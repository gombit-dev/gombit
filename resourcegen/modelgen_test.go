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

// --- typeRenderer: reflect.Type -> Go type expression + imports ---

// renderExternal renders t as if it came from a package other than the model's,
// so every named type takes the import/alias path.
func renderExternal(t *testing.T, typ reflect.Type) (string, []importSpec) {
	t.Helper()
	tr := newTypeRenderer("example.com/some/othermodel", "othermodel")
	expr, err := tr.render(typ)
	if err != nil {
		t.Fatalf("render(%s): %v", typ, err)
	}
	return expr, tr.importSpecs()
}

func TestTypeRendererExternal(t *testing.T) {
	cases := []struct {
		name    string
		typ     reflect.Type
		expr    string
		imports []importSpec
	}{
		{"string", reflect.TypeOf(""), "string", nil},
		{"int", reflect.TypeOf(int(0)), "int", nil},
		{"uint", reflect.TypeOf(uint(0)), "uint", nil},
		{"bool", reflect.TypeOf(false), "bool", nil},
		{"float64", reflect.TypeOf(float64(0)), "float64", nil},
		{"time.Time", reflect.TypeOf(time.Time{}), "time.Time", []importSpec{{"time", "time"}}},
		{"pointer time", reflect.TypeOf((*time.Time)(nil)), "*time.Time", []importSpec{{"time", "time"}}},
		{"sql.NullString", reflect.TypeOf(sql.NullString{}), "sql.NullString", []importSpec{{"sql", "database/sql"}}},
		{"types.Decimal", reflect.TypeOf(types.Decimal{}), "types.Decimal", []importSpec{{"types", "github.com/gombit-dev/gombit/types"}}},
		{"byte slice", reflect.TypeOf([]byte(nil)), "[]byte", nil},
		{"pointer string", reflect.TypeOf((*string)(nil)), "*string", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expr, imports := renderExternal(t, tc.typ)
			if expr != tc.expr {
				t.Fatalf("expr = %q, want %q", expr, tc.expr)
			}
			if !reflect.DeepEqual(normalizeImports(imports), normalizeImports(tc.imports)) {
				t.Fatalf("imports = %v, want %v", imports, tc.imports)
			}
		})
	}
}

func normalizeImports(s []importSpec) []importSpec {
	if len(s) == 0 {
		return nil
	}
	return s
}

// namedStringSlice is a defined slice type: its identity (and any JSON/DB
// behavior it carries) must be preserved, not decomposed into []string.
type namedStringSlice []string

// A named type — even a defined slice — is rendered by identity, not decomposed
// into its underlying type. From another package it is qualified and imported;
// local to the model's package it is unqualified with no import (the generated
// file lives in that package, so importing it would be a self-import).
func TestTypeRendererNamedComposite(t *testing.T) {
	nss := reflect.TypeOf(namedStringSlice{})

	extExpr, extImports := renderExternal(t, nss)
	if extExpr != "resourcegen.namedStringSlice" {
		t.Fatalf("external expr = %q, want qualified", extExpr)
	}
	if !reflect.DeepEqual(extImports, []importSpec{{"resourcegen", "github.com/gombit-dev/gombit/resourcegen"}}) {
		t.Fatalf("external imports = %v", extImports)
	}

	// Local to the model's package: unqualified, no import (no self-import).
	trLocal := newTypeRenderer("github.com/gombit-dev/gombit/resourcegen", "resourcegen")
	localExpr, err := trLocal.render(nss)
	if err != nil {
		t.Fatalf("render local: %v", err)
	}
	if localExpr != "namedStringSlice" {
		t.Fatalf("local expr = %q, want unqualified namedStringSlice", localExpr)
	}
	if len(trLocal.importSpecs()) != 0 {
		t.Fatalf("local named type must not be imported, got %v", trLocal.importSpecs())
	}
}

// A path whose last segment is not a Go identifier (e.g. .../x.v2) still yields a
// usable, uniquified alias rather than an invalid qualifier — and distinct paths
// sharing a base get distinct aliases, so output stays deterministic and compiles.
func TestTypeRendererAliasFallback(t *testing.T) {
	tr := newTypeRenderer("example.com/model", "model")
	a1, _ := tr.aliasFor("example.com/x.v2")
	a2, _ := tr.aliasFor("example.com/other/x.v2")
	if !isGoIdent(a1) || !isGoIdent(a2) {
		t.Fatalf("aliases must be identifiers, got %q %q", a1, a2)
	}
	if a1 == a2 {
		t.Fatalf("distinct paths must get distinct aliases, both %q", a1)
	}
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
	res, err := buildModelResource(&Book{})
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	if res.TypeName != "Book" {
		t.Fatalf("TypeName = %q, want Book (derived from the model)", res.TypeName)
	}

	req := map[string]bool{}
	for _, f := range res.requestFields() {
		req[f.GoName] = true
	}
	resp := map[string]bool{}
	for _, f := range res.responseFields() {
		resp[f.GoName] = true
	}

	if !req["Title"] || !resp["Title"] {
		t.Fatalf("Title should be in request and response; req=%v resp=%v", req, resp)
	}
	if req["TenantID"] || !resp["TenantID"] {
		t.Fatalf("TenantID (read,server) should be response-only; req=%v resp=%v", req, resp)
	}
	if !req["Password"] || resp["Password"] {
		t.Fatalf("Password (write) should be request-only; req=%v resp=%v", req, resp)
	}
	for _, name := range []string{"ID", "CreatedAt", "UpdatedAt"} {
		if req[name] || !resp[name] {
			t.Fatalf("%s should be response-only; req=%v resp=%v", name, req, resp)
		}
	}
	for _, name := range []string{"Internal", "DeletedAt"} {
		if req[name] || resp[name] {
			t.Fatalf("%s should appear in no DTO; req=%v resp=%v", name, req, resp)
		}
	}
}

// Identity is derived from the model, so a modelResource cannot describe one
// model's fields under a different type name. This closes the caller-supplied
// typeName drift ADR-016 forbids.
func TestBuildModelResourceDerivesIdentity(t *testing.T) {
	type Widget struct {
		ID    uint `gorm:"primaryKey"`
		Label string
	}
	res, err := buildModelResource(&Widget{})
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	if res.TypeName != "Widget" || res.dataType() != "widgetData" {
		t.Fatalf("identity = %q/%q, want Widget/widgetData", res.TypeName, res.dataType())
	}
}

// A model whose policy is unsatisfiable fails closed at build time (same contract
// as ResolveAll): a required read-only column has no create source.
func TestBuildModelResourceFailsClosed(t *testing.T) {
	type Bad struct {
		ID    uint   `gorm:"primaryKey"`
		Title string `gorm:"not null" gombit:"read"`
	}
	if _, err := buildModelResource(&Bad{}); err == nil {
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
	type Dup struct {
		ID uint `gorm:"primaryKey"`
		A  A    `gorm:"embedded"`
		B  B    `gorm:"embedded"`
	}
	if _, err := buildModelResource(&Dup{}); err == nil {
		t.Fatal("want an error: two columns map to DTO field \"Note\"")
	}
}

// A column reached through a pointer embed is rejected: the generated mapper would
// dereference a nil pointer after zero-initializing the model.
func TestBuildModelResourceRejectsPointerEmbed(t *testing.T) {
	type Audit struct{ Note string }
	type Doc struct {
		ID    uint   `gorm:"primaryKey"`
		Audit *Audit `gorm:"embedded"`
	}
	_, err := buildModelResource(&Doc{})
	if err == nil {
		t.Fatal("want an error: pointer embed would panic the mapper")
	}
	if !strings.Contains(err.Error(), "pointer embed") {
		t.Fatalf("error should name the pointer embed, got: %v", err)
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
	res, err := buildModelResource(&Book{})
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	src := renderModelDTOs(res)

	for _, want := range []string{"ID: row.Model.ID", "Title: row.Title", "Note: row.Audit.Note"} {
		if !strings.Contains(src, want) {
			t.Fatalf("response mapper missing %q in:\n%s", want, src)
		}
	}
	for _, want := range []string{"row.Title = body.Title", "row.Audit.Note = body.Note"} {
		if !strings.Contains(src, want) {
			t.Fatalf("create mapper missing %q in:\n%s", want, src)
		}
	}
	if strings.Contains(src, "row.Model.ID = body.ID") {
		t.Fatalf("create mapper must not set the read-only ID:\n%s", src)
	}
	assertParses(t, src)
}

// --- exact output + determinism (preconditions for generate --check) ---

// goldenBookDTOs is the exact formatted output for goldenBook. The package clause
// is the model's own package (resourcegen, here the test package), since the file
// is generated beside the model.
const goldenBookDTOs = `// Code generated by gombit make resource. DO NOT EDIT.
package resourcegen

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
	res, err := buildModelResource(goldenBook())
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	got := string(mustFormatGo(renderModelDTOs(res)))
	if got != goldenBookDTOs {
		t.Fatalf("generated DTOs differ from golden:\n--- got ---\n%s\n--- want ---\n%s", got, goldenBookDTOs)
	}
}

func TestRenderModelDTOsBanner(t *testing.T) {
	type Book struct {
		ID    uint `gorm:"primaryKey"`
		Title string
	}
	res, err := buildModelResource(&Book{})
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	if !strings.HasPrefix(renderModelDTOs(res), "// "+GeneratedBanner+"\n") {
		t.Fatal("generated file must start with the DO-NOT-EDIT banner")
	}
}

// Generation must be byte-deterministic (no map-iteration order leaking into
// output) — the precondition for a regenerate-and-compare drift gate.
func TestRenderModelDTOsDeterministic(t *testing.T) {
	first := ""
	for i := 0; i < 8; i++ {
		res, err := buildModelResource(goldenBook())
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

// --- the generated DTOs + mappers compile AND execute against a matching model ---

// execMeta is a value embed used by TestGeneratedDTOsCompileAndRun; its column
// (note) is reached through a value bind path (Meta.Note), which must map without
// panicking. All field types are predeclared, so the temp module needs no
// external requires and `go test` runs offline.
type execMeta struct{ Note string }

type execModel struct {
	ID    uint `gorm:"primaryKey"`
	Title string
	Meta  execMeta `gorm:"embedded"`
}

func TestGeneratedDTOsCompileAndRun(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles and runs a temp module; skipped in -short")
	}
	res, err := buildModelResource(&execModel{})
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	dto := string(mustFormatGo(renderModelDTOs(res)))

	dir := t.TempDir()
	pkgDir := filepath.Join(dir, res.Package)
	if err := os.MkdirAll(pkgDir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "go.mod"), "module execmod\n\ngo 1.23\n")
	// The model as the generated code expects it, in the generated package.
	writeFile(t, filepath.Join(pkgDir, "model.go"),
		"package "+res.Package+"\n\ntype execMeta struct{ Note string }\n\ntype execModel struct {\n\tID uint\n\tTitle string\n\tMeta execMeta\n}\n")
	writeFile(t, filepath.Join(pkgDir, "dto.gen.go"), dto)
	// An internal test that EXECUTES both generated mappers (they are unexported,
	// so it must live in the same package) and checks the value-embed round-trips.
	writeFile(t, filepath.Join(pkgDir, "run_test.go"),
		"package "+res.Package+"\n\nimport \"testing\"\n\n"+
			"func TestRun(t *testing.T) {\n"+
			"\trow := execModel{ID: 7, Title: \"t\"}\n\trow.Meta.Note = \"n\"\n"+
			"\td := toexecModelData(row)\n"+
			"\tif d.ID != 7 || d.Title != \"t\" || d.Note != \"n\" {\n\t\tt.Fatalf(\"response mapper: %+v\", d)\n\t}\n"+
			"\tbuilt := execModelFromCreateBody(execModelCreateBody{Title: \"x\", Note: \"y\"})\n"+
			"\tif built.Title != \"x\" || built.Meta.Note != \"y\" {\n\t\tt.Fatalf(\"create mapper: %+v\", built)\n\t}\n"+
			"}\n")

	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated DTOs failed to compile/run: %v\n%s\n--- generated ---\n%s", err, out, dto)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
