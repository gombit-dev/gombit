package resourcegen

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
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
// stays uint, so a child can be scaffolded before its target. A primary key
// that is neither uint nor uuid.UUID fails closed.
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
				if !ok || ts.Name == nil || ts.Name.Name != targetType {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				return pkFromStruct(targetType, st)
			}
		}
	}
	return "uint", nil
}

func pkFromStruct(typeName string, st *ast.StructType) (string, error) {
	if st.Fields == nil {
		return "uint", nil
	}
	embedsModel := false
	idType := ""
	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			if exprName(field.Type) == "gorm.Model" {
				embedsModel = true
			}
			continue
		}
		for _, name := range field.Names {
			if name.Name == "ID" {
				idType = exprName(field.Type)
			}
		}
	}
	if idType == "" {
		if embedsModel {
			return "uint", nil
		}
		return "uint", nil
	}
	switch idType {
	case "uint":
		return "uint", nil
	case "uuid.UUID":
		return "uuid.UUID", nil
	default:
		return "", fmt.Errorf("resourcegen: %s primary key type %s is not supported (supported: uint, uuid.UUID)", typeName, idType)
	}
}

func exprName(expr ast.Expr) string {
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
		return exprName(e.X)
	default:
		return ""
	}
}
