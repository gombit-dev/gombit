package task_test

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTutorialModelSnippetCompiles type-checks the task.go the tutorial tells
// the reader to create. make resource keeps a hand-written model, so a snippet
// that does not compile stops the tutorial at the next makemigrations.
func TestTutorialModelSnippetCompiles(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "docs", "tutorial.md"))
	if err != nil {
		t.Fatal(err)
	}
	const marker = "Create `internal/task/task.go`:"
	i := strings.Index(string(doc), marker)
	if i < 0 {
		t.Fatalf("docs/tutorial.md has no %q step", marker)
	}
	rest := string(doc[i:])
	start := strings.Index(rest, "```go\n")
	end := strings.Index(rest[start+6:], "```")
	if start < 0 || end < 0 {
		t.Fatal("the task.go step has no go code fence")
	}
	src := rest[start+6 : start+6+end]

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "task.go", src, parser.AllErrors)
	if err != nil {
		t.Fatalf("tutorial task.go does not parse: %v\n%s", err, src)
	}
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	if _, err := conf.Check("task", fset, []*ast.File{file}, nil); err != nil {
		t.Fatalf("tutorial task.go does not compile: %v\n%s", err, src)
	}
	if strings.Contains(src, "gorm.Model") || strings.Contains(src, "DeletedAt") {
		t.Fatalf("tutorial task.go soft-deletes (ADR-019):\n%s", src)
	}
}
