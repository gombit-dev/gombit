package resourcegen

import (
	"database/sql"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	collidepkg1box "github.com/gombit-dev/gombit/resourcegen/testdata/collidepkg1/box"
	collidepkg2box "github.com/gombit-dev/gombit/resourcegen/testdata/collidepkg2/box"
	"github.com/gombit-dev/gombit/types"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
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

// namedStringSlice is an UNEXPORTED defined slice type. Local to its own
// package it is perfectly valid (the generated file lives there too, so no
// qualification is needed); referenced from outside its package it cannot be
// named at all — see TestTypeRendererRejectsUnexportedExternalType.
type namedStringSlice []string

// NamedStringSlice is the exported counterpart, used to prove the "named type
// rendered by identity" contract on a type that CAN legally be qualified from
// another package (issue #352 review: the previous version of this test
// asserted `resourcegen.namedStringSlice` as correct output — an identifier no
// other package can compile against).
type NamedStringSlice []string

// A named type — even a defined slice — is rendered by identity, not decomposed
// into its underlying type. From another package it is qualified and imported;
// local to the model's package it is unqualified with no import (the generated
// file lives in that package, so importing it would be a self-import).
func TestTypeRendererNamedComposite(t *testing.T) {
	nss := reflect.TypeOf(NamedStringSlice{})

	extExpr, extImports := renderExternal(t, nss)
	if extExpr != "resourcegen.NamedStringSlice" {
		t.Fatalf("external expr = %q, want qualified", extExpr)
	}
	if !reflect.DeepEqual(extImports, []importSpec{{"resourcegen", "github.com/gombit-dev/gombit/resourcegen"}}) {
		t.Fatalf("external imports = %v", extImports)
	}

	// Local to the model's package: unqualified, no import (no self-import).
	// Exported/unexported does not matter here — nothing needs to name it from
	// outside its own package, so the UNEXPORTED namedStringSlice is used to
	// prove that export status only gates the external path.
	local := reflect.TypeOf(namedStringSlice{})
	trLocal := newTypeRenderer("github.com/gombit-dev/gombit/resourcegen", "resourcegen")
	localExpr, err := trLocal.render(local)
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

// render must fail closed on an unexported type referenced from outside its
// package: alias.namedStringSlice is not an identifier any other package can
// compile against. Reachable through a type alias to an unexported type in a
// dependency (`type Exported = unexported`); rare, but silently emitting it
// produced uncompilable source (issue #352 review).
func TestTypeRendererRejectsUnexportedExternalType(t *testing.T) {
	tr := newTypeRenderer("example.com/some/othermodel", "othermodel")
	_, err := tr.render(reflect.TypeOf(namedStringSlice{}))
	if err == nil {
		t.Fatal("want an error: unexported external type cannot be qualified")
	}
	if !strings.Contains(err.Error(), "unexported") {
		t.Fatalf("error should name the problem, got: %v", err)
	}
}

// A path whose last segment is not a Go identifier (e.g. .../x.v2) still yields a
// usable, uniquified alias rather than an invalid qualifier — and distinct paths
// sharing a base get distinct aliases, so output stays deterministic and compiles.
func TestTypeRendererAliasFallback(t *testing.T) {
	tr := newTypeRenderer("example.com/model", "model")
	a1 := tr.aliasFor("example.com/x.v2")
	a2 := tr.aliasFor("example.com/other/x.v2")
	if !isGoIdent(a1) || !isGoIdent(a2) {
		t.Fatalf("aliases must be identifiers, got %q %q", a1, a2)
	}
	if a1 == a2 {
		t.Fatalf("distinct paths must get distinct aliases, both %q", a1)
	}
}

// A package-path segment that is a real Go keyword ("map", "type", "select", …)
// is a valid identifier by isGoIdent's character-class rule but an invalid
// import alias — "import map \"...\"" does not parse. aliasFor must fall back
// to the synthetic name the same way it does for a non-identifier segment
// (issue #352 review, finding 6).
func TestTypeRendererAliasFallsBackOnKeywordSegment(t *testing.T) {
	tr := newTypeRenderer("example.com/model", "model")
	alias := tr.aliasFor("example.com/x/map")
	if alias == "map" {
		t.Fatalf("alias = %q, a Go keyword cannot be used as an import alias", alias)
	}
	if !isUsableIdent(alias) {
		t.Fatalf("alias = %q, want a usable (non-keyword) identifier", alias)
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
	res, err := buildModelResource(&Book{}, "resourcegen")
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
	res, err := buildModelResource(&Widget{}, "resourcegen")
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	if res.TypeName != "Widget" || res.dataType() != "widgetData" {
		t.Fatalf("identity = %q/%q, want Widget/widgetData", res.TypeName, res.dataType())
	}
}

// The destination package is taken verbatim from the caller, never guessed from
// reflection — reflect.Type.PkgPath is an import path, not a declared package
// name, and the two can differ (issue #352 review, finding 2). Widget's real
// package here is "resourcegen"; the caller supplying an unrelated package
// proves buildModelResource never derives it, and pkgBase(PkgPath()) would have
// silently returned "resourcegen" instead.
func TestBuildModelResourceUsesCallerSuppliedPackage(t *testing.T) {
	type Widget struct {
		ID uint `gorm:"primaryKey"`
	}
	res, err := buildModelResource(&Widget{}, "somewhereelse")
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	if res.Package != "somewhereelse" {
		t.Fatalf("Package = %q, want the caller-supplied %q, not one derived from reflection", res.Package, "somewhereelse")
	}
}

// An unusable destination package — not a Go identifier, or a reserved keyword
// (a valid identifier by character class, but not a legal package clause) —
// must fail closed rather than emit `package 1bad` or `package map`.
func TestBuildModelResourceRejectsUnusablePackage(t *testing.T) {
	type Widget struct {
		ID uint `gorm:"primaryKey"`
	}
	for _, bad := range []string{"", "1bad", "bad-pkg", "map", "select"} {
		if _, err := buildModelResource(&Widget{}, bad); err == nil {
			t.Errorf("pkg %q: want an error, package clause would not compile", bad)
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
	if _, err := buildModelResource(&Bad{}, "resourcegen"); err == nil {
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
	if _, err := buildModelResource(&Dup{}, "resourcegen"); err == nil {
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
	_, err := buildModelResource(&Doc{}, "resourcegen")
	if err == nil {
		t.Fatal("want an error: pointer embed would panic the mapper")
	}
	if !strings.Contains(err.Error(), "pointer embed") {
		t.Fatalf("error should name the pointer embed, got: %v", err)
	}
}

// A two-level value embed (A -> B -> leaf) must resolve cleanly: every segment
// of the bind path is declared directly at its own level, which is exactly what
// fieldDeclaredAt looks up (issue #352 review, finding 7 — the nested case the
// single-level pointer-embed test above cannot exercise).
func TestBuildModelResourceAllowsNestedValueEmbed(t *testing.T) {
	type Leaf struct{ X string }
	type Mid struct {
		Leaf Leaf `gorm:"embedded"`
	}
	type Outer struct {
		ID  uint `gorm:"primaryKey"`
		Mid Mid  `gorm:"embedded"`
	}
	res, err := buildModelResource(&Outer{}, "resourcegen")
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	found := false
	for _, f := range res.Fields {
		if f.GoName == "X" {
			found = true
			if f.AccessPath != "Mid.Leaf.X" {
				t.Fatalf("AccessPath = %q, want Mid.Leaf.X", f.AccessPath)
			}
		}
	}
	if !found {
		t.Fatal("nested value-embedded field X was not resolved")
	}
}

// A pointer embed at the SECOND level (Outer -> Mid value-embedded -> Mid.Leaf
// pointer-embedded) must still be rejected. reflect.Type.FieldByName's
// promotion-aware search can find "Leaf" through a different path than the
// literal bind-path segment being checked; fieldDeclaredAt must not repeat that
// mistake at a non-first level (issue #352 review, finding 7).
func TestBuildModelResourceRejectsPointerEmbedAtIntermediateLevel(t *testing.T) {
	type Leaf struct{ X string }
	type Mid struct {
		Leaf *Leaf `gorm:"embedded"`
	}
	type Outer struct {
		ID  uint `gorm:"primaryKey"`
		Mid Mid  `gorm:"embedded"`
	}
	_, err := buildModelResource(&Outer{}, "resourcegen")
	if err == nil {
		t.Fatal("want an error: the second-level embed is a pointer")
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
	res, err := buildModelResource(&Book{}, "resourcegen")
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

// The wire (JSON) field name must follow the Go field name, not the DB column
// name. A `gorm:"column:..."` override is a persistence decision (ADR-016:
// column name is "GORM schema owns"); routing it into the JSON tag would
// silently promote it to a public API decision, which ADR-016's ownership
// table puts under "policy owns" instead (issue #352 review, finding 8).
func TestRenderModelDTOsJSONNameFollowsGoNameNotColumn(t *testing.T) {
	type Book struct {
		ID    uint   `gorm:"primaryKey"`
		Title string `gorm:"column:custom_title_column"`
	}
	res, err := buildModelResource(&Book{}, "resourcegen")
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	src := renderModelDTOs(res)

	if !strings.Contains(src, `Title string `+"`"+`json:"title" doc:"Title"`+"`") {
		t.Fatalf("JSON tag must derive from GoName (\"title\"), not Column (\"custom_title_column\"):\n%s", src)
	}
	if strings.Contains(src, "custom_title_column") {
		t.Fatalf("the DB column override must not leak into the wire contract:\n%s", src)
	}
	// The mapper still reaches the field through the model's Go field, which is
	// unaffected by the column override — the persistence side keeps working.
	if !strings.Contains(src, "Title: row.Title") || !strings.Contains(src, "row.Title = body.Title") {
		t.Fatalf("mappers must still bind through the Go field name:\n%s", src)
	}
	assertParses(t, src)
}

// slugType is a defined string type: it renders by identity ("slugType") yet has
// reflect.Kind String, so the NOT NULL minLength constraint must still apply.
type slugType string

// A NOT NULL string column gets minLength:"1" in the create body so an empty
// string cannot silently satisfy the column (issue #218). The test keys off the
// field's reflect.Kind, not the rendered GoType text: a defined `type slugType
// string` renders as "slugType" but is Kind String and zero-fills to "" just the
// same, so it must get the same constraint. Keying off GoType == "string" would
// skip every named string type — the Slug/Email/Username a real model defines
// (issue #352 review, finding 1).
func TestSemanticFormatReachesTheRequest(t *testing.T) {
	type emailAddr string
	type Person struct {
		ID      uint      `gorm:"primaryKey"`
		Contact string    `gorm:"not null;size:255" format:"email"`
		Handle  string    `gorm:"size:255" pattern:"^[-a-zA-Z0-9_]+$"`
		Inbox   emailAddr `gorm:"size:255" format:"email"`
		Site    *string   `gorm:"size:255" format:"uri"`
		Ref     *string   `format:"uri-reference"`
	}
	res, err := buildModelResource(&Person{}, "resourcegen")
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	src := renderModelDTOs(res)
	if !strings.Contains(src, `json:"contact" format:"email" minLength:"1" maxLength:"255" doc:"Contact"`) {
		t.Fatalf("email format missing:\n%s", src)
	}
	if strings.Contains(src, `Handle *string`) || strings.Contains(src, "nilIfBlank") || strings.Contains(src, `*emailAddr`) {
		t.Fatalf("a plain string stays a string:\n%s", src)
	}
	if !strings.Contains(src, `Handle string `+"`"+`json:"handle" pattern:"^[-a-zA-Z0-9_]+$" maxLength:"255" doc:"Handle"`) {
		t.Fatalf("plain slug pattern missing:\n%s", src)
	}
	if !strings.Contains(src, "Inbox emailAddr ") || !strings.Contains(src, `json:"inbox" format:"email" maxLength:"255" doc:"Inbox"`) {
		t.Fatalf("named string must keep its type:\n%s", src)
	}
	if strings.Contains(src, `json:"inbox" format:"email" nullable:"true"`) {
		t.Fatalf("named string must not be nullable:\n%s", src)
	}
	if !strings.Contains(src, `Site *string`) || !strings.Contains(src, `json:"site" format:"uri" nullable:"true" maxLength:"255" doc:"Site"`) || !strings.Contains(src, "Site: row.Site") {
		t.Fatalf("pointer column must stay a nullable pointer:\n%s", src)
	}
	if !strings.Contains(src, `Ref *string`) || !strings.Contains(src, `format:"uri-reference"`) {
		t.Fatalf("uri-reference must stay a pointer string with its format:\n%s", src)
	}
}

func TestSemanticBlankAndBadValue(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles and runs a temp module; skipped in -short")
	}
	type Person struct {
		ID     uint    `gorm:"primaryKey"`
		Work   string  `gorm:"not null;size:255" format:"email"`
		Site   *string `gorm:"size:255" format:"uri"`
		Handle *string `gorm:"size:255" pattern:"^[-a-zA-Z0-9_]+$"`
		Addr   *string `gorm:"size:255" format:"ip"`
		Code   string  `gorm:"size:255" pattern:"^[a-z]+$"`
	}
	res, err := buildModelResource(&Person{}, "personpkg")
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	dto := string(mustFormatGo(renderModelDTOs(res)))
	if strings.Contains(dto, "nilIfBlank") || !strings.Contains(dto, "*string `json:\"site\"") {
		t.Fatalf("optional semantic columns must stay pointers:\n%s", dto)
	}
	dir := t.TempDir()
	pkgDir := filepath.Join(dir, res.Package)
	if err := os.MkdirAll(pkgDir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "go.mod"),
		"module personmod\n\ngo 1.23\n\nrequire github.com/gombit-dev/gombit v0.0.0\n\nreplace github.com/gombit-dev/gombit => "+resourcegenModuleRoot(t)+"\n")
	writeFile(t, filepath.Join(pkgDir, "model.go"), `package personpkg

type Person struct {
	ID     uint `+"`gorm:\"primaryKey\"`"+`
	Work   string
	Site   *string
	Handle *string
	Addr   *string
	Code   string
}
`)
	writeFile(t, filepath.Join(pkgDir, "dto.gen.go"), dto)
	writeFile(t, filepath.Join(pkgDir, "run_test.go"), `package personpkg

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humagin"
	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/contract"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestRun(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "t.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Person{}); err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	api := humagin.New(router, contract.HumaConfig("person", "0.0.0"))
	type input struct {
		Body personCreateBody
	}
	huma.Register(api, huma.Operation{
		OperationID: "create-person",
		Method:      http.MethodPost,
		Path:        "/people",
	}, func(_ context.Context, in *input) (*struct{ Body personData }, error) {
		row := personFromCreateBody(in.Body)
		if err := db.Create(&row).Error; err != nil {
			return nil, err
		}
		var got Person
		if err := db.First(&got, row.ID).Error; err != nil {
			return nil, err
		}
		if in.Body.Site == nil && (got.Site != nil || got.Handle != nil || got.Addr != nil) {
			t.Fatalf("null stored as site=%v handle=%v addr=%v", got.Site, got.Handle, got.Addr)
		}
		out := toPersonData(got)
		return &struct{ Body personData }{Body: out}, nil
	})
	post := func(raw string) (int, map[string]any) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/people", strings.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}
	code, body := post(`+"`{\"work\":\"ada@example.com\",\"site\":null,\"handle\":null,\"addr\":null,\"code\":\"ab\"}`"+`)
	if code != http.StatusOK || body["site"] != nil || body["handle"] != nil || body["addr"] != nil || body["work"] != "ada@example.com" || body["code"] != "ab" {
		t.Fatalf("blank optional: status %d body %#v", code, body)
	}
	if code, body = post(`+"`{\"work\":\"ada@example.com\",\"site\":null,\"handle\":null,\"addr\":null,\"code\":\"\"}`"+`); code != http.StatusUnprocessableEntity {
		t.Fatalf("blank plain pattern: status %d body %#v", code, body)
	}
	if code, body = post(`+"`{\"work\":\"not-an-email\",\"site\":null,\"handle\":null,\"addr\":null,\"code\":\"ab\"}`"+`); code != http.StatusUnprocessableEntity {
		t.Fatalf("bad email: status %d body %#v", code, body)
	}
	if code, body = post(`+"`{\"work\":\"ada@example.com\",\"site\":\"example.com\",\"handle\":null,\"addr\":null,\"code\":\"ab\"}`"+`); code != http.StatusUnprocessableEntity {
		t.Fatalf("bad url: status %d body %#v", code, body)
	}
	if code, body = post(`+"`{\"work\":\"ada@example.com\",\"site\":null,\"handle\":\"has space\",\"addr\":null,\"code\":\"ab\"}`"+`); code != http.StatusUnprocessableEntity {
		t.Fatalf("bad slug: status %d body %#v", code, body)
	}
	if code, body = post(`+"`{\"work\":\"ada@example.com\",\"site\":null,\"handle\":null,\"addr\":\"nope\",\"code\":\"ab\"}`"+`); code != http.StatusUnprocessableEntity {
		t.Fatalf("bad ip: status %d body %#v", code, body)
	}
	code, body = post(`+"`{\"work\":\"ada@example.com\",\"site\":\"https://example.com\",\"handle\":\"ada\",\"addr\":\"127.0.0.1\",\"code\":\"ab\"}`"+`)
	if code != http.StatusOK || body["site"] != "https://example.com" || body["handle"] != "ada" || body["addr"] != "127.0.0.1" || body["code"] != "ab" {
		t.Fatalf("good values: status %d body %#v", code, body)
	}
}
`)
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("semantic round trip failed: %v\n%s\n--- generated ---\n%s", err, out, dto)
	}
}

func TestRequestTagMinLengthKeysOnKindNotText(t *testing.T) {
	type Article struct {
		ID    uint     `gorm:"primaryKey"`
		Name  slugType `gorm:"not null"` // named string, NOT NULL -> minLength
		Plain string   `gorm:"not null"` // plain string, NOT NULL -> minLength
		Opt   slugType // nullable named string -> no minLength
	}
	res, err := buildModelResource(&Article{}, "resourcegen")
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	src := renderModelDTOs(res)

	if !strings.Contains(src, "Name slugType `"+`json:"name" minLength:"1" doc:"Name"`+"`") {
		t.Fatalf("named NOT NULL string (rendered by identity) must still get minLength:\n%s", src)
	}
	if !strings.Contains(src, "Plain string `"+`json:"plain" minLength:"1" doc:"Plain"`+"`") {
		t.Fatalf("plain NOT NULL string must get minLength:\n%s", src)
	}
	if !strings.Contains(src, "Opt slugType `"+`json:"opt" doc:"Opt"`+"`") {
		t.Fatalf("nullable named string must NOT get minLength:\n%s", src)
	}
}

// Every import is emitted with an explicit alias, including standard-library
// paths: a bare import binds the package's DECLARED name, which reflection cannot
// see and need not equal the path basename — a non-domain module path (gombit new
// --module myapp) can hold an internal package whose folder name differs from its
// package clause. line() must never drop the alias (issue #352 review, finding 2).
func TestImportSpecLineAlwaysAliased(t *testing.T) {
	cases := []struct {
		spec importSpec
		want string
	}{
		{importSpec{"time", "time"}, `time "time"`},
		{importSpec{"sql", "database/sql"}, `sql "database/sql"`},
		{importSpec{"types", "github.com/gombit-dev/gombit/types"}, `types "github.com/gombit-dev/gombit/types"`},
		{importSpec{"box1", "myapp/box"}, `box1 "myapp/box"`},
	}
	for _, tc := range cases {
		if got := tc.spec.line(); got != tc.want {
			t.Errorf("line(%+v) = %q, want %q", tc.spec, got, tc.want)
		}
	}
}

// --- exact output + determinism (preconditions for generate --check) ---

// goldenBookDTOs is the exact formatted output for goldenBook. The package clause
// is the model's own package (resourcegen, here the test package), since the file
// is generated beside the model. Title carries minLength:"1" in the create body
// because it is `gorm:"not null"`: without that constraint an empty string from
// the client passes resourcepolicy (a create source exists) and reaches the NOT
// NULL column as valid-but-wrong data — the exact silent zero-fill issue #218
// exists to eliminate (issue #352 review, finding 5). TenantID is NOT NULL too
// but response-only (read,server), so it never reaches the create body at all.
const goldenBookDTOs = `// Code generated by gombit make resource. DO NOT EDIT.
package resourcegen

import time "time"

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
	Title    string ` + "`json:\"title\" minLength:\"1\" doc:\"Title\"`" + `
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

func TestRequestTagReadsValidateConstraints(t *testing.T) {
	type Person struct {
		ID     uint   `gorm:"primaryKey"`
		Age    int    `gorm:"not null;check:age >= 0 AND age <= 150" validate:"min=0;max=150"`
		Code   string `gorm:"size:8;not null" validate:"max_length=8;pattern=^[a-z]+$"`
		Status string `gorm:"size:16" validate:"enum=draft,published;default=draft"`
		Count  uint   `validate:"min=2"`
	}
	res, err := buildModelResource(&Person{}, "resourcegen")
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	src := renderModelDTOs(res)
	for _, want := range []string{
		`json:"age" minimum:"0" maximum:"150" doc:"Age"`,
		`json:"code" minLength:"1" maxLength:"8" pattern:"^[a-z]+$" doc:"Code"`,
		`Status *string`,
		`json:"status,omitempty" maxLength:"16" enum:"draft,published" doc:"Status"`,
		`json:"count" minimum:"2" doc:"Count"`,
		`row.Status = "draft"`,
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("DTOs missing %q:\n%s", want, src)
		}
	}
	if strings.Contains(src, `default:"draft"`) {
		t.Fatalf("request tag must not use Huma default:\n%s", src)
	}
}

func TestGeneratedDefaultsKeepExplicitZero(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles and runs a temp module; skipped in -short")
	}
	type Person struct {
		ID     uint          `gorm:"primaryKey"`
		Count  int           `validate:"min=0;default=1"`
		Active bool          `validate:"default=true"`
		Price  types.Decimal `validate:"max=10"`
	}
	res, err := buildModelResource(&Person{}, "personpkg")
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	dto := string(mustFormatGo(renderModelDTOs(res)))
	if strings.Contains(dto, `default:"`) {
		t.Fatalf("Huma default tag rewrites zeros:\n%s", dto)
	}
	if !strings.Contains(dto, `json:"count,omitempty"`) || !strings.Contains(dto, `json:"active,omitempty"`) {
		t.Fatalf("defaulted create fields must be omitempty so a missing key is legal:\n%s", dto)
	}
	if strings.Contains(dto, `minimum:"10"`) || strings.Contains(dto, `maximum:"10"`) {
		t.Fatalf("decimal bounds must not be minimum/maximum on a string schema:\n%s", dto)
	}

	dir := t.TempDir()
	pkgDir := filepath.Join(dir, res.Package)
	if err := os.MkdirAll(pkgDir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "go.mod"),
		"module personmod\n\ngo 1.23\n\nrequire github.com/gombit-dev/gombit v0.0.0\n\nreplace github.com/gombit-dev/gombit => "+resourcegenModuleRoot(t)+"\n")
	writeFile(t, filepath.Join(pkgDir, "model.go"), `package personpkg

import "github.com/gombit-dev/gombit/types"

type Person struct {
	ID     uint `+"`gorm:\"primaryKey\"`"+`
	Count  int
	Active bool
	Price  types.Decimal
}
`)
	writeFile(t, filepath.Join(pkgDir, "dto.gen.go"), dto)
	writeFile(t, filepath.Join(pkgDir, "run_test.go"), `package personpkg

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humagin"
	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestRun(t *testing.T) {
	zero := 0
	active := false
	explicit := personFromCreateBody(personCreateBody{
		Count:  &zero,
		Active: &active,
		Price:  types.MustDecimal("1"),
	})
	if explicit.Count != 0 || explicit.Active != false {
		t.Fatalf("explicit zero rewritten before save: %+v", explicit)
	}
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "t.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Person{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&explicit).Error; err != nil {
		t.Fatal(err)
	}
	var got Person
	if err := db.First(&got, explicit.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Count != 0 || got.Active != false {
		t.Fatalf("explicit zero rewritten on insert: %+v", got)
	}

	omitted := personFromCreateBody(personCreateBody{Price: types.MustDecimal("1")})
	if omitted.Count != 1 || omitted.Active != true {
		t.Fatalf("omitted fields did not take the default: %+v", omitted)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	api := humagin.New(router, contract.HumaConfig("person", "0.0.0"))
	type input struct {
		Body personCreateBody
	}
	type created struct {
		Count  int  `+"`json:\"count\"`"+`
		Active bool `+"`json:\"active\"`"+`
	}
	huma.Register(api, huma.Operation{
		OperationID: "create-person",
		Method:      http.MethodPost,
		Path:        "/people",
	}, func(_ context.Context, in *input) (*struct{ Body created }, error) {
		row := personFromCreateBody(in.Body)
		if err := db.Create(&row).Error; err != nil {
			return nil, err
		}
		return &struct{ Body created }{Body: created{Count: row.Count, Active: row.Active}}, nil
	})
	post := func(raw string) (int, string, created) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/people", strings.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		var got created
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode %s: %v body %s", raw, err, rec.Body.String())
			}
		}
		return rec.Code, rec.Body.String(), got
	}
	if code, body, got := post(`+"`{\"price\":\"1\"}`"+`); code != http.StatusOK || got.Count != 1 || got.Active != true {
		t.Fatalf("omitted default over HTTP: status %d body %s parsed %+v", code, body, got)
	}
	if code, body, got := post(`+"`{\"count\":0,\"active\":false,\"price\":\"1\"}`"+`); code != http.StatusOK || got.Count != 0 || got.Active != false {
		t.Fatalf("explicit zero over HTTP: status %d body %s parsed %+v", code, body, got)
	}

	over := personCreateBody{Price: types.MustDecimal("999")}
	if errs := over.Resolve(nil); len(errs) == 0 {
		t.Fatal("decimal 999 with max 10 must fail Resolve")
	}
}
`)
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s\n--- generated ---\n%s", err, out, dto)
	}
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated defaults failed: %v\n%s\n--- generated ---\n%s", err, out, dto)
	}
}

func TestRenderModelDTOsGolden(t *testing.T) {
	res, err := buildModelResource(goldenBook(), "resourcegen")
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
	res, err := buildModelResource(&Book{}, "resourcegen")
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
		res, err := buildModelResource(goldenBook(), "resourcegen")
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
	res, err := buildModelResource(&execModel{}, "execpkg")
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

// execImportsModel exercises every import shape the golden/parse-only tests
// above only render, never compile (issue #352 review, finding 4): a
// standard-library type (time.Time), a third-party named type (types.Decimal),
// and two distinct import paths that share a basename ("box") and must resolve
// to distinct, uniquified aliases. Every import is written with an explicit
// alias (issue #352 review: a bare import binds the package's declared name,
// which reflection cannot see and need not equal the path basename). Before
// this test, aliasFor -> importSpec.line -> renderImports produced no source
// any test compiled — an incorrect alias/import shape (swapped alias/path, a
// missing uniquifier) would still have gone green.
type execImportsModel struct {
	ID     uint `gorm:"primaryKey"`
	When   time.Time
	Amount types.Decimal
	BoxA   collidepkg1box.Box
	BoxB   collidepkg2box.Box
}

func TestGeneratedDTOsWithImportsCompileAndRun(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles and runs a temp module; skipped in -short")
	}
	res, err := buildModelResource(&execImportsModel{}, "execimportspkg")
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	dto := string(mustFormatGo(renderModelDTOs(res)))

	if !strings.Contains(dto, `time "time"`) {
		t.Fatalf("expected an explicitly aliased std import for time.Time:\n%s", dto)
	}
	if !strings.Contains(dto, `types "github.com/gombit-dev/gombit/types"`) {
		t.Fatalf("expected an aliased third-party import for types.Decimal:\n%s", dto)
	}
	if !strings.Contains(dto, `box "github.com/gombit-dev/gombit/resourcegen/testdata/collidepkg1/box"`) ||
		!strings.Contains(dto, `box1 "github.com/gombit-dev/gombit/resourcegen/testdata/collidepkg2/box"`) {
		t.Fatalf("expected the basename collision uniquified to box/box1:\n%s", dto)
	}

	dir := t.TempDir()
	pkgDir := filepath.Join(dir, res.Package)
	if err := os.MkdirAll(pkgDir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "go.mod"),
		"module execimportsmod\n\ngo 1.23\n\nrequire github.com/gombit-dev/gombit v0.0.0\n\nreplace github.com/gombit-dev/gombit => "+resourcegenModuleRoot(t)+"\n")
	// The model as the generated code expects it. Its own import aliases are
	// independent of dto.gen.go's (each file has its own import block); what
	// must match is the underlying type identity (import path + name).
	writeFile(t, filepath.Join(pkgDir, "model.go"),
		"package "+res.Package+"\n\n"+
			"import (\n"+
			"\t\"time\"\n\n"+
			"\t\"github.com/gombit-dev/gombit/types\"\n"+
			"\tcp1 \"github.com/gombit-dev/gombit/resourcegen/testdata/collidepkg1/box\"\n"+
			"\tcp2 \"github.com/gombit-dev/gombit/resourcegen/testdata/collidepkg2/box\"\n"+
			")\n\n"+
			"type execImportsModel struct {\n"+
			"\tID     uint\n"+
			"\tWhen   time.Time\n"+
			"\tAmount types.Decimal\n"+
			"\tBoxA   cp1.Box\n"+
			"\tBoxB   cp2.Box\n"+
			"}\n")
	writeFile(t, filepath.Join(pkgDir, "dto.gen.go"), dto)
	// Executes both generated mappers (unexported, so this must live in the
	// same package) with real values for every imported type, round-tripping
	// through the response and create mappers.
	writeFile(t, filepath.Join(pkgDir, "run_test.go"),
		"package "+res.Package+"\n\n"+
			"import (\n"+
			"\t\"testing\"\n"+
			"\t\"time\"\n\n"+
			"\t\"github.com/gombit-dev/gombit/types\"\n"+
			"\tcp1 \"github.com/gombit-dev/gombit/resourcegen/testdata/collidepkg1/box\"\n"+
			"\tcp2 \"github.com/gombit-dev/gombit/resourcegen/testdata/collidepkg2/box\"\n"+
			")\n\n"+
			"func TestRun(t *testing.T) {\n"+
			"\twhen := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)\n"+
			"\tamount, err := types.NewDecimalFromString(\"12.34\")\n"+
			"\tif err != nil {\n\t\tt.Fatal(err)\n\t}\n"+
			"\trow := execImportsModel{ID: 7, When: when, Amount: amount, BoxA: cp1.Box(\"a\"), BoxB: cp2.Box(\"b\")}\n"+
			"\td := toexecImportsModelData(row)\n"+
			"\tif d.ID != 7 || !d.When.Equal(when) || !d.Amount.Equal(amount.Decimal) || d.BoxA != cp1.Box(\"a\") || d.BoxB != cp2.Box(\"b\") {\n"+
			"\t\tt.Fatalf(\"response mapper: %+v\", d)\n\t}\n"+
			"\tbuilt := execImportsModelFromCreateBody(execImportsModelCreateBody{When: when, Amount: amount, BoxA: cp1.Box(\"a\"), BoxB: cp2.Box(\"b\")})\n"+
			"\tif !built.When.Equal(when) || !built.Amount.Equal(amount.Decimal) || built.BoxA != cp1.Box(\"a\") || built.BoxB != cp2.Box(\"b\") {\n"+
			"\t\tt.Fatalf(\"create mapper: %+v\", built)\n\t}\n"+
			"}\n")

	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated DTOs failed to compile/run: %v\n%s\n--- generated ---\n%s", err, out, dto)
	}
}

// resourcegenModuleRoot is this repo's module root, for a temp module's
// `replace` directive back to the real framework code — the same technique
// cmd/gombit's scaffold-compile tests use (cmdModuleRoot), one directory
// level shallower since resourcegen sits directly under the module root.
func resourcegenModuleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s: %v", root, err)
	}
	return root
}

// --- slice 4.5: query-capability type appropriateness ---

// A query capability whose column type cannot support it fails closed at build
// time (matching the legacy field-grammar type rules).
func TestBuildModelResourceRejectsBadQueryTypes(t *testing.T) {
	t.Run("aggregatable string", func(t *testing.T) {
		type m struct {
			ID    uint   `gorm:"primaryKey"`
			Title string `gombit:"read,write,aggregatable"`
		}
		if _, err := buildModelResource(&m{}, "m"); err == nil || !strings.Contains(err.Error(), "aggregatable") {
			t.Fatalf("aggregatable string must fail closed, got: %v", err)
		}
	})
	t.Run("searchable int", func(t *testing.T) {
		type m struct {
			ID    uint  `gorm:"primaryKey"`
			Count int64 `gombit:"read,write,searchable"`
		}
		if _, err := buildModelResource(&m{}, "m"); err == nil || !strings.Contains(err.Error(), "searchable") {
			t.Fatalf("searchable int must fail closed, got: %v", err)
		}
	})
	t.Run("filterable time", func(t *testing.T) {
		type m struct {
			ID   uint      `gorm:"primaryKey"`
			When time.Time `gombit:"read,write,filterable"`
		}
		if _, err := buildModelResource(&m{}, "m"); err == nil || !strings.Contains(err.Error(), "filterable") {
			t.Fatalf("filterable time must fail closed, got: %v", err)
		}
	})
}

// The CLI grammar and the model-first generator must accept or reject the same
// column. Each path used to lock its own predicate, so a text filter could be
// illegal in parseFields and legal in buildModelResource.
func TestQueryPolicyAgreesAcrossGeneratorPaths(t *testing.T) {
	t.Run("text filterable", func(t *testing.T) {
		if _, err := parseFields([]string{"body:text:filterable"}, "posts"); err == nil || !strings.Contains(err.Error(), "filterable") {
			t.Fatalf("parseFields text filterable = %v", err)
		}
		type m struct {
			ID   uint   `gorm:"primaryKey"`
			Body string `gorm:"type:text" gombit:"read,write,filterable"`
		}
		if _, err := buildModelResource(&m{}, "posts"); err == nil || !strings.Contains(err.Error(), "filterable") {
			t.Fatalf("buildModelResource text filterable = %v", err)
		}
	})
	t.Run("url searchable", func(t *testing.T) {
		if _, err := parseFields([]string{"site:url:searchable"}, "posts"); err == nil || !strings.Contains(err.Error(), "searchable") {
			t.Fatalf("parseFields url searchable = %v", err)
		}
		type m struct {
			ID   uint   `gorm:"primaryKey"`
			Site string `gorm:"size:255" format:"uri" gombit:"read,write,searchable"`
		}
		if _, err := buildModelResource(&m{}, "posts"); err == nil || !strings.Contains(err.Error(), "searchable") {
			t.Fatalf("buildModelResource url searchable = %v", err)
		}
	})
	t.Run("email filterable", func(t *testing.T) {
		if _, err := parseFields([]string{"contact:email:filterable"}, "posts"); err == nil || !strings.Contains(err.Error(), "filterable") {
			t.Fatalf("parseFields email filterable = %v", err)
		}
		type m struct {
			ID      uint   `gorm:"primaryKey"`
			Contact string `gorm:"size:255" format:"email" gombit:"read,write,filterable"`
		}
		if _, err := buildModelResource(&m{}, "posts"); err == nil || !strings.Contains(err.Error(), "filterable") {
			t.Fatalf("buildModelResource email filterable = %v", err)
		}
	})
	t.Run("string regex matching the slug alphabet stays filterable", func(t *testing.T) {
		if _, err := parseFields([]string{`token:string:filterable,regex=^[-a-zA-Z0-9_]+$`}, "posts"); err != nil {
			t.Fatalf("parseFields string regex filterable = %v", err)
		}
		type m struct {
			ID    uint   `gorm:"primaryKey"`
			Token string `gorm:"size:255" validate:"pattern=^[-a-zA-Z0-9_]+$" gombit:"read,write,filterable"`
		}
		if _, err := buildModelResource(&m{}, "posts"); err != nil {
			t.Fatalf("buildModelResource string regex filterable = %v", err)
		}
	})
	t.Run("string filterable", func(t *testing.T) {
		if _, err := parseFields([]string{"title:string:filterable"}, "posts"); err != nil {
			t.Fatalf("parseFields string filterable = %v", err)
		}
		type m struct {
			ID    uint   `gorm:"primaryKey"`
			Title string `gombit:"read,write,filterable"`
		}
		if _, err := buildModelResource(&m{}, "posts"); err != nil {
			t.Fatalf("buildModelResource string filterable = %v", err)
		}
	})
	t.Run("int aggregatable", func(t *testing.T) {
		if _, err := parseFields([]string{"qty:int:aggregatable"}, "posts"); err != nil {
			t.Fatalf("parseFields int aggregatable = %v", err)
		}
		type m struct {
			ID  uint `gorm:"primaryKey"`
			Qty int  `gombit:"read,write,aggregatable"`
		}
		if _, err := buildModelResource(&m{}, "posts"); err != nil {
			t.Fatalf("buildModelResource int aggregatable = %v", err)
		}
	})
	t.Run("float aggregatable", func(t *testing.T) {
		// float is not emitted by make resource yet, and the catalog says it is
		// not aggregatable. Both paths reject the column.
		if _, err := parseFields([]string{"score:float:aggregatable"}, "posts"); err == nil {
			t.Fatal("parseFields float aggregatable succeeded")
		}
		type m struct {
			ID    uint    `gorm:"primaryKey"`
			Score float64 `gombit:"read,write,aggregatable"`
		}
		if _, err := buildModelResource(&m{}, "posts"); err == nil || !strings.Contains(err.Error(), "aggregatable") {
			t.Fatalf("buildModelResource float aggregatable = %v", err)
		}
	})
	t.Run("decimal aggregatable", func(t *testing.T) {
		if _, err := parseFields([]string{"total:decimal:aggregatable"}, "posts"); err != nil {
			t.Fatalf("parseFields decimal aggregatable = %v", err)
		}
		type framework struct {
			ID     uint          `gorm:"primaryKey"`
			Amount types.Decimal `gombit:"read,write,aggregatable"`
		}
		if _, err := buildModelResource(&framework{}, "posts"); err != nil {
			t.Fatalf("buildModelResource types.Decimal aggregatable = %v", err)
		}
		type shopspring struct {
			ID     uint            `gorm:"primaryKey"`
			Amount decimal.Decimal `gombit:"read,write,aggregatable"`
		}
		if _, err := buildModelResource(&shopspring{}, "posts"); err != nil {
			t.Fatalf("buildModelResource decimal.Decimal aggregatable = %v", err)
		}
	})
}

func TestBuildModelResourceScalarGaps(t *testing.T) {
	type m struct {
		ID     uint             `gorm:"primaryKey"`
		Score  float64          `gombit:"read,write,sortable"`
		Born   *types.Date      `gorm:"type:date" gombit:"read,write,sortable"`
		Due    types.Date       `gorm:"type:date;not null" gombit:"read,write,sortable"`
		Token  *uuid.UUID       `gorm:"type:char(36)" gombit:"read,write,sortable"`
		Meta   types.NullJSON   `gorm:"type:text" gombit:"read,write"`
		Body   types.JSON       `gorm:"type:text;not null" gombit:"read,write"`
		At     time.Time        `gombit:"read,write,sortable"`
		Opens  *types.TimeOfDay `gorm:"type:char(8)" gombit:"read,write,sortable"`
		Shift  types.TimeOfDay  `gorm:"type:char(8);not null" gombit:"read,write,sortable"`
		Length *types.Duration  `gorm:"type:bigint" gombit:"read,write,sortable"`
		Span   types.Duration   `gorm:"type:bigint;not null" validate:"default=30m" gombit:"read,write,sortable"`
	}
	res, err := buildModelResource(&m{}, "m")
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	src := string(mustFormatGo(renderModelDTOs(res)))
	for _, want := range []string{
		`json:"score"`,
		"*types.Date",
		`format:"date" nullable:"true"`,
		`format:"date"`,
		"*uuid.UUID",
		`format:"uuid" nullable:"true"`,
		"types.NullJSON",
		"types.JSON",
		"time.Time",
		"*types.TimeOfDay",
		`pattern:"^([01][0-9]|2[0-3]):[0-5][0-9]`,
		`nullable:"true"`,
		"*types.Duration",
		`format:"duration" nullable:"true"`,
		"types.MustDuration(\"30m\")",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("dto missing %q:\n%s", want, src)
		}
	}
	for _, line := range strings.Split(src, "\n") {
		if !strings.Contains(line, "json:") {
			continue
		}
		if strings.Contains(line, "Due ") && strings.Contains(line, `nullable:"true"`) {
			t.Fatalf("required date is nullable:\n%s", line)
		}
		if strings.Contains(line, "Due ") && !strings.Contains(line, `format:"date"`) {
			t.Fatalf("required date missing format:\n%s", line)
		}
		if strings.Contains(line, "Shift ") && strings.Contains(line, `nullable:"true"`) {
			t.Fatalf("required time of day is nullable:\n%s", line)
		}
		if strings.Contains(line, "Span ") && strings.Contains(line, "omitempty") && !strings.Contains(line, `nullable:"true"`) {
			t.Fatalf("defaulted duration does not accept null:\n%s", line)
		}
		if strings.Contains(line, "Body ") && strings.Contains(line, `nullable:"true"`) {
			t.Fatalf("required JSON is nullable:\n%s", line)
		}
	}
}

// A decimal column IS aggregatable (numeric), even though its Go kind is a struct.
func TestBuildModelResourceAllowsDecimalAggregate(t *testing.T) {
	type m struct {
		ID     uint          `gorm:"primaryKey"`
		Amount types.Decimal `gombit:"read,write,aggregatable"`
	}
	res, err := buildModelResource(&m{}, "m")
	if err != nil {
		t.Fatalf("decimal must be aggregatable: %v", err)
	}
	if len(res.aggregateFields()) != 1 {
		t.Fatalf("Amount should be aggregatable, got %d aggregate fields", len(res.aggregateFields()))
	}
}

// The response-visible rule propagates through buildModelResource: a hidden field
// with a query capability fails closed.
func TestBuildModelResourceRejectsHiddenQueryable(t *testing.T) {
	type m struct {
		ID     uint   `gorm:"primaryKey"`
		Secret string `gombit:"-,searchable"`
	}
	if _, err := buildModelResource(&m{}, "m"); err == nil {
		t.Fatal("a hidden searchable field must fail closed")
	}
}

// Nullability is orthogonal to query capability: a nullable (*T or sql.Null*)
// column is just as filterable/searchable/aggregatable as its non-nullable form,
// classified by its underlying scalar kind (parity with the legacy grammar).
func TestBuildModelResourceAllowsNullableQueryTypes(t *testing.T) {
	type m struct {
		ID    uint           `gorm:"primaryKey"`
		Name  *string        `gombit:"read,write,filterable,searchable,sortable"`
		Price *int64         `gombit:"read,write,filterable,aggregatable"`
		Flag  *bool          `gombit:"read,write,filterable"`
		Label sql.NullString `gombit:"read,write,searchable"`
		Money *types.Decimal `gombit:"read,write,aggregatable"`
	}
	res, err := buildModelResource(&m{}, "m")
	if err != nil {
		t.Fatalf("nullable query columns must resolve: %v", err)
	}
	if n := len(res.filterFields()); n != 3 {
		t.Fatalf("filterable count = %d, want 3 (name, price, flag)", n)
	}
	if n := len(res.aggregateFields()); n != 2 {
		t.Fatalf("aggregatable count = %d, want 2 (price, money)", n)
	}
	// filterKindExpr must key off the unwrapped kind: a nullable *int64 coerces as
	// an int64, never as a string.
	for _, f := range res.filterFields() {
		if f.Column == "price" && filterKindExpr(f) != "database.FilterInt64" {
			t.Fatalf("nullable *int64 filter kind = %q, want database.FilterInt64", filterKindExpr(f))
		}
		if f.Column == "flag" && filterKindExpr(f) != "database.FilterBool" {
			t.Fatalf("nullable *bool filter kind = %q, want database.FilterBool", filterKindExpr(f))
		}
	}
}
