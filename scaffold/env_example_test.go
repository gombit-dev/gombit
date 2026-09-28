package scaffold

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestEnvExampleListsEveryConfigVariable keeps .env.example honest about its
// claim to list the GOMBIT_* server variables the config package reads: every
// GOMBIT_* env-name constant in config/config.go must appear in the template,
// set or commented out.
func TestEnvExampleListsEveryConfigVariable(t *testing.T) {
	tmpl, err := templateFS.ReadFile("templates/env.example.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	vars := configEnvVars(t)
	if len(vars) < 30 {
		t.Fatalf("found only %d GOMBIT_* constants in config/config.go; the parse is broken", len(vars))
	}
	for _, name := range vars {
		// A line setting it (NAME=...) or a commented example (# NAME=...).
		line := regexp.MustCompile(`(?m)^(# )?` + regexp.QuoteMeta(name) + `=`)
		if !line.Match(tmpl) {
			t.Errorf("templates/env.example.tmpl does not list %s (config/config.go reads it)", name)
		}
	}
}

// configEnvVars returns the value of every string constant in
// config/config.go that names a GOMBIT_* environment variable.
func configEnvVars(t *testing.T) []string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(self), "..", "config", "config.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var vars []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			for _, value := range spec.(*ast.ValueSpec).Values {
				lit, ok := value.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				s, err := strconv.Unquote(lit.Value)
				if err == nil && strings.HasPrefix(s, "GOMBIT_") {
					vars = append(vars, s)
				}
			}
		}
	}
	return vars
}
