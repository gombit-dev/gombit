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

// lookupTargetPK reads internal/<pkg> for typeName. A missing package or type
// stays uint, so a child can be scaffolded before its target. A type that
// exists and whose primary key is not uint or uuid.UUID fails closed. The
// scan walks embedded structs and reads gorm primaryKey tags, so the key is
// the one GORM would use, not only a field literally named ID.
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
	structs := map[string]*ast.StructType{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return "", fmt.Errorf("resourcegen: parse %s: %w", path, err)
		}
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
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				structs[ts.Name.Name] = st
			}
		}
	}
	st, ok := structs[targetType]
	if !ok {
		return "uint", nil
	}
	return pkFromStruct(targetType, st, structs)
}

type pkCandidate struct {
	name     string
	typ      string
	explicit bool
}

func pkFromStruct(typeName string, st *ast.StructType, structs map[string]*ast.StructType) (string, error) {
	cands, err := collectPKCandidates(typeName, st, structs, map[string]struct{}{})
	if err != nil {
		return "", err
	}
	var keys []string
	seen := map[string]struct{}{}
	for _, cand := range cands {
		// GORM's primary key is every field tagged primaryKey, and every field
		// named ID. A pointer is a different type: *uuid.UUID is not uuid.UUID.
		if !cand.explicit && cand.name != "ID" {
			continue
		}
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

func collectPKCandidates(typeName string, st *ast.StructType, structs map[string]*ast.StructType, seen map[string]struct{}) ([]pkCandidate, error) {
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
			embedded := exprType(field.Type)
			embedded = strings.TrimPrefix(embedded, "*")
			if embedded == "gorm.Model" {
				cands = append(cands, pkCandidate{name: "ID", typ: "uint"})
				continue
			}
			local := embedded
			if i := strings.LastIndex(embedded, "."); i >= 0 {
				local = embedded[i+1:]
			}
			child, ok := structs[local]
			if !ok || local == "" {
				shown := embedded
				if shown == "" {
					shown = "an unnamed type"
				}
				return nil, fmt.Errorf("resourcegen: %s embeds %s, whose primary key cannot be resolved", typeName, shown)
			}
			nested, err := collectPKCandidates(local, child, structs, seen)
			if err != nil {
				return nil, err
			}
			cands = append(cands, nested...)
			continue
		}
		explicit := gormPrimaryKey(field.Tag)
		for _, name := range field.Names {
			cands = append(cands, pkCandidate{name: name.Name, typ: exprType(field.Type), explicit: explicit})
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
		if key == "primaryKey" || key == "primary_key" {
			return true
		}
	}
	return false
}

func exprType(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		pkg, ok := e.X.(*ast.Ident)
		if !ok {
			return ""
		}
		return pkg.Name + "." + e.Sel.Name
	case *ast.StarExpr:
		return "*" + exprType(e.X)
	default:
		return ""
	}
}
