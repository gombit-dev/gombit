package resourcegen

import (
	"database/sql"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/types"
	"gorm.io/gorm"
)

// --- renderGoType: reflect.Type -> Go type expression + imports ---

func TestRenderGoType(t *testing.T) {
	cases := []struct {
		name    string
		typ     reflect.Type
		expr    string
		imports []string
	}{
		{"string", reflect.TypeOf(""), "string", nil},
		{"int", reflect.TypeOf(int(0)), "int", nil},
		{"uint", reflect.TypeOf(uint(0)), "uint", nil},
		{"bool", reflect.TypeOf(false), "bool", nil},
		{"float64", reflect.TypeOf(float64(0)), "float64", nil},
		{"time.Time", reflect.TypeOf(time.Time{}), "time.Time", []string{"time"}},
		{"pointer time", reflect.TypeOf((*time.Time)(nil)), "*time.Time", []string{"time"}},
		{"sql.NullString", reflect.TypeOf(sql.NullString{}), "sql.NullString", []string{"database/sql"}},
		{"types.Decimal", reflect.TypeOf(types.Decimal{}), "types.Decimal", []string{"github.com/gombit-dev/gombit/types"}},
		{"byte slice", reflect.TypeOf([]byte(nil)), "[]byte", nil},
		{"pointer string", reflect.TypeOf((*string)(nil)), "*string", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expr, imports, err := renderGoType(tc.typ)
			if err != nil {
				t.Fatalf("renderGoType(%s): %v", tc.name, err)
			}
			if expr != tc.expr {
				t.Fatalf("expr = %q, want %q", expr, tc.expr)
			}
			if !reflect.DeepEqual(normalizeNil(imports), normalizeNil(tc.imports)) {
				t.Fatalf("imports = %v, want %v", imports, tc.imports)
			}
		})
	}
}

func normalizeNil(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

// --- policy split: which columns land in which DTO ---

func TestBuildModelResourceSplitsByPolicy(t *testing.T) {
	type Book struct {
		gorm.Model        // ID + timestamps read-only, DeletedAt hidden
		Title      string `gorm:"not null"`                      // read+write content
		TenantID   uint   `gorm:"not null" gombit:"read,server"` // response-only, server-set
		Password   string `gombit:"write"`                       // request-only
		Internal   string `gombit:"-"`                           // hidden
	}
	res, err := buildModelResource("book", "Book", &Book{})
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}

	req := map[string]bool{}
	for _, f := range res.requestFields() {
		req[f.GoName] = true
	}
	resp := map[string]bool{}
	for _, f := range res.responseFields() {
		resp[f.GoName] = true
	}

	// Content: both surfaces.
	if !req["Title"] || !resp["Title"] {
		t.Fatalf("Title should be in request and response; req=%v resp=%v", req, resp)
	}
	// Server-managed: response only.
	if req["TenantID"] || !resp["TenantID"] {
		t.Fatalf("TenantID (read,server) should be response-only; req=%v resp=%v", req, resp)
	}
	// Write-only: request only.
	if !req["Password"] || resp["Password"] {
		t.Fatalf("Password (write) should be request-only; req=%v resp=%v", req, resp)
	}
	// Read-only auto key + timestamps: response only, never request.
	for _, name := range []string{"ID", "CreatedAt", "UpdatedAt"} {
		if req[name] || !resp[name] {
			t.Fatalf("%s should be response-only; req=%v resp=%v", name, req, resp)
		}
	}
	// Hidden and soft-delete: neither surface.
	for _, name := range []string{"Internal", "DeletedAt"} {
		if req[name] || resp[name] {
			t.Fatalf("%s should appear in no DTO; req=%v resp=%v", name, req, resp)
		}
	}
}

// A model whose policy is unsatisfiable fails closed at build time (same contract
// as ResolveAll): a required read-only column has no create source.
func TestBuildModelResourceFailsClosed(t *testing.T) {
	type Bad struct {
		ID    uint   `gorm:"primaryKey"`
		Title string `gorm:"not null" gombit:"read"`
	}
	if _, err := buildModelResource("bad", "Bad", &Bad{}); err == nil {
		t.Fatal("want an error: required read-only column has no create source")
	}
}

// Distinct embedded structs with a same-named leaf collide in the flat DTO and
// must fail closed rather than emit a struct with duplicate fields.
func TestBuildModelResourceRejectsDuplicateLeaf(t *testing.T) {
	type A struct{ Note string }
	type B struct {
		Note string `gorm:"column:note_b"`
	}
	type m struct {
		ID uint `gorm:"primaryKey"`
		A  A    `gorm:"embedded"`
		B  B    `gorm:"embedded"`
	}
	if _, err := buildModelResource("m", "M", &m{}); err == nil {
		t.Fatal("want an error: two columns map to DTO field \"Note\"")
	}
}

// --- rendered source: mappers reach columns through the model bind path ---

func TestRenderModelDTOsEmbeddedAccessPaths(t *testing.T) {
	type Audit struct{ Note string }
	type Book struct {
		gorm.Model
		Title string
		Audit Audit `gorm:"embedded"`
	}
	res, err := buildModelResource("book", "Book", &Book{})
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	src := renderModelDTOs(res)

	// Response mapper reads the promoted key through its embed and the named embed
	// through its field.
	for _, want := range []string{"ID: row.Model.ID", "Title: row.Title", "Note: row.Audit.Note"} {
		if !strings.Contains(src, want) {
			t.Fatalf("response mapper missing %q in:\n%s", want, src)
		}
	}
	// Create mapper assigns through the bind path (not a composite-literal key).
	for _, want := range []string{"row.Title = body.Title", "row.Audit.Note = body.Note"} {
		if !strings.Contains(src, want) {
			t.Fatalf("create mapper missing %q in:\n%s", want, src)
		}
	}
	// It never tries to set a read-only key from the request.
	if strings.Contains(src, "row.Model.ID = body.ID") {
		t.Fatalf("create mapper must not set the read-only ID:\n%s", src)
	}
	assertParses(t, src)
}

func TestRenderModelDTOsBanner(t *testing.T) {
	type Book struct {
		ID    uint `gorm:"primaryKey"`
		Title string
	}
	res, err := buildModelResource("book", "Book", &Book{})
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	src := renderModelDTOs(res)
	if !strings.HasPrefix(src, "// "+GeneratedBanner+"\n") {
		t.Fatalf("generated file must start with the DO-NOT-EDIT banner:\n%s", src)
	}
}

// The exact formatted output for a representative model. It locks the generated
// shape (read/write/server split, response-only timestamps, hidden soft-delete)
// and, together with TestRenderModelDTOsDeterministic, the byte-stability that
// gombit generate --check (slice 5) will depend on.
const goldenBookDTOs = `// Code generated by gombit make resource. DO NOT EDIT.
package book

import "time"

// bookData is the response body for a Book.
type bookData struct {
	ID        uint      ` + "`json:\"id\" doc:\"ID\"`" + `
	CreatedAt time.Time ` + "`json:\"created_at\" doc:\"CreatedAt\"`" + `
	UpdatedAt time.Time ` + "`json:\"updated_at\" doc:\"UpdatedAt\"`" + `
	Title     string    ` + "`json:\"title\" doc:\"Title\"`" + `
	TenantID  uint      ` + "`json:\"tenant_id\" doc:\"TenantID\"`" + `
}

// bookCreateBody is the request body for creating a Book.
type bookCreateBody struct {
	Title    string ` + "`json:\"title\" doc:\"Title\"`" + `
	Password string ` + "`json:\"password\" doc:\"Password\"`" + `
}

// toBookData projects a Book into its response DTO.
func toBookData(row Book) bookData {
	return bookData{
		ID:        row.Model.ID,
		CreatedAt: row.Model.CreatedAt,
		UpdatedAt: row.Model.UpdatedAt,
		Title:     row.Title,
		TenantID:  row.TenantID,
	}
}

// bookFromCreateBody builds a new Book from a create request.
func bookFromCreateBody(body bookCreateBody) Book {
	var row Book
	row.Title = body.Title
	row.Password = body.Password
	return row
}
`

func goldenBook() any {
	type Book struct {
		gorm.Model
		Title    string `gorm:"not null"`
		TenantID uint   `gorm:"not null" gombit:"read,server"`
		Password string `gombit:"write"`
		Internal string `gombit:"-"`
	}
	return &Book{}
}

func TestRenderModelDTOsGolden(t *testing.T) {
	res, err := buildModelResource("book", "Book", goldenBook())
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	got := string(mustFormatGo(renderModelDTOs(res)))
	if got != goldenBookDTOs {
		t.Fatalf("generated DTOs differ from golden:\n--- got ---\n%s\n--- want ---\n%s", got, goldenBookDTOs)
	}
}

// Generation must be byte-deterministic (no map-iteration order leaking into
// output) — the precondition for a regenerate-and-compare drift gate.
func TestRenderModelDTOsDeterministic(t *testing.T) {
	first := ""
	for i := 0; i < 8; i++ {
		res, err := buildModelResource("book", "Book", goldenBook())
		if err != nil {
			t.Fatalf("buildModelResource: %v", err)
		}
		got := string(mustFormatGo(renderModelDTOs(res)))
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("generation is not deterministic; run %d differs:\n%s", i, got)
		}
	}
}

func assertParses(t *testing.T, src string) {
	t.Helper()
	if _, err := parser.ParseFile(token.NewFileSet(), "dto.gen.go", src, parser.AllErrors); err != nil {
		t.Fatalf("generated source does not parse: %v\n%s", err, src)
	}
}

// --- the generated DTOs + mappers actually compile against a matching model ---

// compileModel is defined identically in the temp module's model.go below; keep
// the two in sync. Stdlib-only field types keep the temp module hermetic (no
// external requires, so `go build` needs no network).
type compileModel struct {
	ID        uint
	Title     string
	CreatedAt time.Time
}

func TestGeneratedDTOsCompile(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a temp module; skipped in -short")
	}
	res, err := buildModelResource("book", "Book", &compileModel{})
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	// The temp module defines the model as `Book`; generation used compileModel,
	// so rename the referenced type to match. buildModelResource took the fields
	// from compileModel but the emitted type name is whatever we pass ("Book").
	src := string(mustFormatGo(renderModelDTOs(res)))

	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "book")
	if err := os.MkdirAll(pkgDir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "go.mod"), "module bookmod\n\ngo 1.23\n")
	writeFile(t, filepath.Join(pkgDir, "model.go"), "package book\n\nimport \"time\"\n\ntype Book struct {\n\tID uint\n\tTitle string\n\tCreatedAt time.Time\n}\n")
	writeFile(t, filepath.Join(pkgDir, "dto.gen.go"), src)

	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated DTOs do not compile: %v\n%s\n--- generated ---\n%s", err, out, src)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
