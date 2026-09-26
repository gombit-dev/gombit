package resourcegen

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
)

// idStrategy is the generated resource's primary key. uint is gorm.Model.
// uuid is an application-assigned uuid.UUID. Composite keys are rejected.
type idStrategy string

const (
	idUint idStrategy = "uint"
	idUUID idStrategy = "uuid"
)

func parseIDStrategy(raw string) (idStrategy, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "uint", "integer":
		return idUint, nil
	case "uuid":
		return idUUID, nil
	default:
		return "", fmt.Errorf("resourcegen: --id %q is not supported (supported: uint, uuid)", raw)
	}
}

// pkLookup reports the primary-key Go type of a target model that already
// exists on disk. An empty result means uint.
type pkLookup func(targetPkg, targetType string) (string, error)

// lookupTargetPK reads internal/<pkg> for typeName. A missing package or an
// undeclared name stays uint, so a child can be scaffolded before its target.
// A declared type whose primary key cannot be read, or is not uint or
// uuid.UUID, fails closed. Defined types and aliases (`type User Base`,
// `type User = Base`) are followed. An explicit primaryKey suppresses the
// ID convention, matching gorm.io/gorm v1.31.2 schema.Parse.
func lookupTargetPK(workDir, targetPkg, targetType string) (string, error) {
	dir := filepath.Join(workDir, "internal", targetPkg)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "uint", nil
		}
		return "", fmt.Errorf("resourcegen: read internal/%s: %w", targetPkg, err)
	}
	fset := token.NewFileSet()
	named := map[string]namedDecl{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return "", fmt.Errorf("resourcegen: parse %s: %w", path, err)
		}
		scope := fileScopeOf(file)
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name == nil {
					continue
				}
				named[ts.Name.Name] = namedDecl{expr: ts.Type, scope: scope}
			}
		}
	}
	decl, ok := named[targetType]
	if !ok {
		return "uint", nil
	}
	st, scope, err := resolveStruct(targetType, decl, named, map[string]struct{}{})
	if err != nil {
		return "", err
	}
	return pkFromStruct(targetType, st, scope, named)
}

type fileScope struct {
	names map[string]string
	dots  []string
}

type namedDecl struct {
	expr  ast.Expr
	scope fileScope
}

type pkCandidate struct {
	name     string
	typ      string
	explicit bool
}

func fileScopeOf(file *ast.File) fileScope {
	scope := fileScope{names: map[string]string{}}
	if file == nil {
		return scope
	}
	for _, imp := range file.Imports {
		if imp.Path == nil {
			continue
		}
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path == "" {
			continue
		}
		if imp.Name != nil && imp.Name.Name == "." {
			scope.dots = append(scope.dots, path)
			continue
		}
		if imp.Name != nil && imp.Name.Name == "_" {
			continue
		}
		local := filepath.Base(path)
		if imp.Name != nil && imp.Name.Name != "" {
			local = imp.Name.Name
		}
		scope.names[local] = path
	}
	return scope
}

func resolveStruct(typeName string, decl namedDecl, named map[string]namedDecl, seen map[string]struct{}) (*ast.StructType, fileScope, error) {
	if _, ok := seen[typeName]; ok {
		return nil, fileScope{}, fmt.Errorf("resourcegen: %s embeds itself", typeName)
	}
	seen[typeName] = struct{}{}
	defer delete(seen, typeName)
	switch e := decl.expr.(type) {
	case *ast.StructType:
		return e, decl.scope, nil
	case *ast.Ident:
		next, ok := named[e.Name]
		if !ok {
			return nil, fileScope{}, fmt.Errorf("resourcegen: %s is declared as %s, whose primary key cannot be read", typeName, e.Name)
		}
		return resolveStruct(e.Name, next, named, seen)
	default:
		shown := canonicalType(decl.expr, decl.scope)
		if shown == "" {
			shown = "a non-struct type"
		}
		return nil, fileScope{}, fmt.Errorf("resourcegen: %s is declared as %s, whose primary key cannot be read", typeName, shown)
	}
}

func pkFromStruct(typeName string, st *ast.StructType, scope fileScope, named map[string]namedDecl) (string, error) {
	cands, err := collectPKCandidates(typeName, st, scope, named, map[string]struct{}{})
	if err != nil {
		return "", err
	}
	// gorm.io/gorm v1.31.2 promotes a field named ID only when no field is
	// tagged primaryKey. An explicit key suppresses the convention. A pointer
	// is a different type: *uuid.UUID is not uuid.UUID.
	var explicit, ids []pkCandidate
	for _, cand := range cands {
		if cand.explicit {
			explicit = append(explicit, cand)
			continue
		}
		if cand.name == "ID" {
			ids = append(ids, cand)
		}
	}
	chosen := explicit
	if len(chosen) == 0 {
		chosen = ids
	}
	var keys []string
	seen := map[string]struct{}{}
	for _, cand := range chosen {
		id := cand.name + " " + cand.typ
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		keys = append(keys, cand.typ)
	}
	if len(keys) == 0 {
		return "", fmt.Errorf("resourcegen: %s has no primary key", typeName)
	}
	if len(keys) > 1 {
		return "", fmt.Errorf("resourcegen: %s has a composite primary key, which is not supported", typeName)
	}
	switch keys[0] {
	case "uint", "uuid.UUID":
		return keys[0], nil
	default:
		return "", fmt.Errorf("resourcegen: %s primary key type %s is not supported (supported: uint, uuid.UUID)", typeName, keys[0])
	}
}

func collectPKCandidates(typeName string, st *ast.StructType, scope fileScope, named map[string]namedDecl, seen map[string]struct{}) ([]pkCandidate, error) {
	if _, ok := seen[typeName]; ok {
		return nil, fmt.Errorf("resourcegen: %s embeds itself", typeName)
	}
	seen[typeName] = struct{}{}
	defer delete(seen, typeName)
	if st == nil || st.Fields == nil {
		return nil, nil
	}
	var cands []pkCandidate
	for _, field := range st.Fields.List {
		if gormTagIgnored(field.Tag) {
			continue
		}
		if len(field.Names) == 0 {
			embedded := strings.TrimPrefix(canonicalType(field.Type, scope), "*")
			typeNameOfEmbed := embedded
			if i := strings.LastIndex(embedded, "."); i >= 0 {
				typeNameOfEmbed = embedded[i+1:]
			}
			if !ast.IsExported(typeNameOfEmbed) {
				continue
			}
			if embedded == "gorm.Model" {
				// gorm.Model.ID is tagged primarykey, so it is an explicit key.
				cands = append(cands, pkCandidate{name: "ID", typ: "uint", explicit: true})
				continue
			}
			child, ok := named[typeNameOfEmbed]
			if !ok || typeNameOfEmbed == "" {
				shown := embedded
				if shown == "" {
					shown = "an unnamed type"
				}
				return nil, fmt.Errorf("resourcegen: %s embeds %s, whose primary key cannot be read", typeName, shown)
			}
			childStruct, childScope, err := resolveStruct(typeNameOfEmbed, child, named, map[string]struct{}{})
			if err != nil {
				return nil, err
			}
			nested, err := collectPKCandidates(typeNameOfEmbed, childStruct, childScope, named, seen)
			if err != nil {
				return nil, err
			}
			cands = append(cands, nested...)
			continue
		}
		explicit := gormPrimaryKey(field.Tag)
		for _, name := range field.Names {
			if !ast.IsExported(name.Name) {
				continue
			}
			cands = append(cands, pkCandidate{name: name.Name, typ: canonicalType(field.Type, scope), explicit: explicit})
		}
	}
	return cands, nil
}

func gormTag(tag *ast.BasicLit) string {
	if tag == nil {
		return ""
	}
	raw, err := strconv.Unquote(tag.Value)
	if err != nil {
		return ""
	}
	return reflect.StructTag(raw).Get("gorm")
}

func gormTagIgnored(tag *ast.BasicLit) bool {
	for _, part := range strings.Split(gormTag(tag), ";") {
		if strings.TrimSpace(part) == "-" {
			return true
		}
	}
	return false
}

func gormPrimaryKey(tag *ast.BasicLit) bool {
	for _, part := range strings.Split(gormTag(tag), ";") {
		key, _, _ := strings.Cut(strings.TrimSpace(part), ":")
		switch strings.ToLower(key) {
		case "primarykey", "primary_key":
			return true
		}
	}
	return false
}

func canonicalType(expr ast.Expr, scope fileScope) string {
	switch e := expr.(type) {
	case *ast.Ident:
		if path := dotImport(scope, e.Name); path != "" {
			return canonicalSelector(path, e.Name)
		}
		return e.Name
	case *ast.SelectorExpr:
		pkg, ok := e.X.(*ast.Ident)
		if !ok {
			return ""
		}
		if path := scope.names[pkg.Name]; path != "" {
			return canonicalSelector(path, e.Sel.Name)
		}
		return pkg.Name + "." + e.Sel.Name
	case *ast.StarExpr:
		return "*" + canonicalType(e.X, scope)
	case *ast.ParenExpr:
		return canonicalType(e.X, scope)
	default:
		return ""
	}
}

func dotImport(scope fileScope, name string) string {
	for _, path := range scope.dots {
		switch path {
		case "github.com/google/uuid":
			if name == "UUID" {
				return path
			}
		case "gorm.io/gorm":
			if name == "Model" {
				return path
			}
		}
	}
	return ""
}

func canonicalSelector(path, name string) string {
	switch path {
	case "github.com/google/uuid":
		if name == "UUID" {
			return "uuid.UUID"
		}
	case "gorm.io/gorm":
		if name == "Model" {
			return "gorm.Model"
		}
	}
	return name
}
