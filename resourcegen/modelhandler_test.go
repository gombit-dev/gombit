package resourcegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/types"
	"gorm.io/gorm"
)

// handlerModel is the model the handler tests generate against: a NOT NULL
// content column (Title), a NOT NULL server-managed column (TenantID) that must
// stay out of the request body and be set by the create hook, and a hidden
// column (Secret).
type handlerModel struct {
	gorm.Model
	Title    string `gorm:"not null"`
	TenantID uint   `gorm:"not null" gombit:"read,server"`
	Secret   string `gombit:"-"`
}

// Book is the compile-run model for TestGeneratedHandlerCompilesAndRuns. It is a
// package-level type (not function-local) so its Go type name is literally "Book",
// which the generated identifiers and the temp module's hand-written model.go /
// run_test.go must agree on. Same shape as handlerModel.
type Book struct {
	gorm.Model
	Title    string `gorm:"not null"`
	TenantID uint   `gorm:"not null" gombit:"read,server"`
	Secret   string `gombit:"-"`
}

func buildHandlerModel(t *testing.T, pkg string) modelResource {
	t.Helper()
	res, err := buildModelResource(&handlerModel{}, pkg)
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	return res
}

func TestRenderModelHandlerStructure(t *testing.T) {
	res := buildHandlerModel(t, "handlermodel")
	src, err := renderModelHandler(res)
	if err != nil {
		t.Fatalf("renderModelHandler: %v", err)
	}
	assertParses(t, src)

	// Generator-owned: DO-NOT-EDIT banner.
	if !strings.HasPrefix(src, "// "+GeneratedBanner+"\n") {
		t.Fatalf("handler must carry the DO-NOT-EDIT banner:\n%s", src)
	}
	// The hooks interface is per-resource and typed on the model + create body.
	if !strings.Contains(src, "type handlerModelHooks interface {") ||
		!strings.Contains(src, "BeforeCreate(ctx context.Context, row *handlerModel, body handlerModelCreateBody) error") {
		t.Fatalf("handler must declare the typed per-resource hooks interface:\n%s", src)
	}
	// The Handler holds DB and the hooks interface.
	if !strings.Contains(src, "Hooks handlerModelHooks") {
		t.Fatalf("Handler must hold the hooks interface:\n%s", src)
	}
	// create maps the body, runs the hook, THEN persists — in that order.
	iBuild := strings.Index(src, "handlerModelFromCreateBody(input.Body)")
	iHook := strings.Index(src, "h.Hooks.BeforeCreate(ctx, &row, input.Body)")
	iCreate := strings.Index(src, "Create(&row)")
	if iBuild < 0 || iHook < 0 || iCreate < 0 {
		t.Fatalf("create must build, run the hook, and persist (build=%d hook=%d create=%d):\n%s", iBuild, iHook, iCreate, src)
	}
	if iBuild >= iHook || iHook >= iCreate {
		t.Fatalf("create must build -> hook -> persist, in that order (build=%d hook=%d create=%d):\n%s", iBuild, iHook, iCreate, src)
	}
	// The hook is mandatory: a nil Hooks fails closed BEFORE building/persisting,
	// so a server-managed column is never silently zero-filled (#218). The guard
	// must precede the build so no row is constructed on the misconfigured path.
	iGuard := strings.Index(src, "if h.Hooks == nil {")
	if iGuard < 0 || iGuard >= iBuild {
		t.Fatalf("create must fail closed on a nil Hooks before building the row (guard=%d build=%d):\n%s", iGuard, iBuild, src)
	}
	if !strings.Contains(src, `contract.Internal("create handlermodel: Handler.Hooks is not set")`) {
		t.Fatalf("nil-Hooks guard must return an internal error:\n%s", src)
	}
	// Register wires the human-owned default Hooks.
	if !strings.Contains(src, "h := &Handler{DB: app.DB(), Hooks: Hooks{}}") {
		t.Fatalf("Register must wire the human-owned Hooks{}:\n%s", src)
	}
	// Operation IDs match the legacy scheme (so swapping handlers in slice 5 keeps
	// the OpenAPI contract): list is plural kebab, get/create singular snake.
	for _, want := range []string{`"list-handler-models"`, `"get-handler_model"`, `"create-handler_model"`} {
		if !strings.Contains(src, want) {
			t.Fatalf("missing operation id %s:\n%s", want, src)
		}
	}
	// This slice is create + read only — no update/delete plumbing.
	if strings.Contains(src, "http.MethodPut") || strings.Contains(src, "http.MethodDelete") {
		t.Fatalf("slice 4 is create+read only; found update/delete:\n%s", src)
	}
}

func TestRenderModelHooksStructure(t *testing.T) {
	res := buildHandlerModel(t, "handlermodel")
	src := renderModelHooks(res)
	assertParses(t, src)

	// Human-owned: it is the customization seam, so it must NOT be DO-NOT-EDIT.
	if strings.Contains(src, GeneratedBanner) {
		t.Fatalf("the hooks file is human-owned and must not carry the DO-NOT-EDIT banner:\n%s", src)
	}
	if !strings.Contains(src, "type Hooks struct{}") {
		t.Fatalf("hooks file must declare the concrete Hooks type:\n%s", src)
	}
	// The default implementation is a no-op the developer edits.
	if !strings.Contains(src, "func (Hooks) BeforeCreate(ctx context.Context, row *handlerModel, body handlerModelCreateBody) error {") ||
		!strings.Contains(src, "return nil") {
		t.Fatalf("hooks file must implement a no-op BeforeCreate:\n%s", src)
	}
}

// Both generated files must be byte-deterministic — the precondition for the
// regenerate-and-compare drift gate (slice 5).
func TestRenderModelHandlerDeterministic(t *testing.T) {
	var firstHandler, firstHooks string
	for i := 0; i < 8; i++ {
		res := buildHandlerModel(t, "handlermodel")
		h, err := renderModelHandler(res)
		if err != nil {
			t.Fatalf("renderModelHandler: %v", err)
		}
		hooks := renderModelHooks(res)
		if i == 0 {
			firstHandler, firstHooks = h, hooks
			continue
		}
		if h != firstHandler {
			t.Fatalf("handler generation is not deterministic; run %d differs", i)
		}
		if hooks != firstHooks {
			t.Fatalf("hooks generation is not deterministic; run %d differs", i)
		}
	}
}

// goldenBookHooks locks the human-owned hooks seam (short, and the file a
// developer sees first). The package clause is the caller-supplied package.
const goldenBookHooks = `package book

import "context"

// Hooks implements BookHooks. This file is generated once and
// is yours to edit: set server-managed columns (tenant, owner, timestamps not
// handled by GORM, …) on row in BeforeCreate. Regeneration does not overwrite it.
type Hooks struct{}

// BeforeCreate runs after the request is mapped onto row and before it is
// persisted. The default is a no-op; add server-derived values here.
func (Hooks) BeforeCreate(ctx context.Context, row *Book, body bookCreateBody) error {
	return nil
}
`

func TestRenderModelHooksGolden(t *testing.T) {
	res, err := buildModelResource(goldenBook(), "book")
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	got := string(mustFormatGo(renderModelHooks(res)))
	if got != goldenBookHooks {
		t.Fatalf("hooks file differs from golden:\n--- got ---\n%s\n--- want ---\n%s", got, goldenBookHooks)
	}
}

// TestGeneratedHandlerCompilesAndRuns generates the DTO + handler + hooks for a
// model with a NOT NULL server-managed column, compiles them against the REAL
// framework (contract/database/framework/huma/gorm via a replace directive), and
// EXECUTES the create/get/list operations on a SQLite-backed Handler. A custom
// hook sets the server column that the request body cannot carry — proving the
// #218 fix end to end: the column is absent from the request DTO, populated by
// BeforeCreate, satisfies NOT NULL, is persisted, and appears in the response.
func TestGeneratedHandlerCompilesAndRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles and runs a temp module against the framework; skipped in -short")
	}
	// Identity is derived from the model type, so the compile-run model must be a
	// type literally named Book for the generated names (bookData, BookHooks, …) to
	// match the temp module's model.go and run_test.go below.
	res, err := buildModelResource(&Book{}, "book")
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	dto := string(mustFormatGo(renderModelDTOs(res)))
	handler, err := renderModelHandler(res)
	if err != nil {
		t.Fatalf("renderModelHandler: %v", err)
	}
	handler = string(mustFormatGo(handler))
	hooks := string(mustFormatGo(renderModelHooks(res)))

	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "book")
	if err := os.MkdirAll(pkgDir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "go.mod"),
		"module bookapp\n\ngo 1.23\n\nrequire github.com/gombit-dev/gombit v0.0.0\n\nreplace github.com/gombit-dev/gombit => "+resourcegenModuleRoot(t)+"\n")
	// The model as the generated code expects it (matching field types; the gombit
	// tag drove generation already and is not needed at compile time).
	writeFile(t, filepath.Join(pkgDir, "model.go"),
		"package book\n\nimport \"gorm.io/gorm\"\n\ntype Book struct {\n\tgorm.Model\n\tTitle    string `gorm:\"not null\"`\n\tTenantID uint   `gorm:\"not null\"`\n\tSecret   string\n}\n")
	writeFile(t, filepath.Join(pkgDir, "dto.gen.go"), dto)
	writeFile(t, filepath.Join(pkgDir, "handler.gen.go"), handler)
	writeFile(t, filepath.Join(pkgDir, "hooks.go"), hooks)
	// Executes the generated operations. serverHooks stands in for a customized
	// hooks.go, setting the server column the request body cannot carry.
	writeFile(t, filepath.Join(pkgDir, "run_test.go"), handlerRunTest)

	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated handler failed to compile/run: %v\n%s\n--- handler ---\n%s", err, out, handler)
	}
}

const handlerRunTest = `package book

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// serverHooks stands in for a customized hooks.go: it sets the server-managed
// column the request body does not carry.
type serverHooks struct{}

func (serverHooks) BeforeCreate(ctx context.Context, row *Book, body bookCreateBody) error {
	row.TenantID = 42
	return nil
}

func TestGeneratedCRUD(t *testing.T) {
	// The generated default Hooks satisfies the interface and compiles.
	var _ BookHooks = Hooks{}

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "t.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Book{}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: db, Hooks: serverHooks{}}
	ctx := context.Background()

	out, err := h.create(ctx, &createBookInput{Body: bookCreateBody{Title: "hello"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if out.Body.Data.Title != "hello" {
		t.Fatalf("title not mapped: %+v", out.Body.Data)
	}
	// The server column was absent from the request body, set by the hook,
	// persisted through NOT NULL, and surfaced in the response.
	if out.Body.Data.TenantID != 42 {
		t.Fatalf("server column not set by hook: %+v", out.Body.Data)
	}
	if out.Body.Data.ID == 0 {
		t.Fatalf("id not assigned: %+v", out.Body.Data)
	}

	got, err := h.get(ctx, &getBookInput{ID: strconv.FormatUint(uint64(out.Body.Data.ID), 10)})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Body.Data.Title != "hello" || got.Body.Data.TenantID != 42 {
		t.Fatalf("get returned wrong row: %+v", got.Body.Data)
	}

	listed, err := h.list(ctx, &listBooksInput{Page: 1, PerPage: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed.Body.Data) != 1 || listed.Body.Meta.Total != 1 {
		t.Fatalf("list: got %d rows, total %d", len(listed.Body.Data), listed.Body.Meta.Total)
	}
}

// A Handler with no Hooks is misconfigured: create must fail closed rather than
// skip the hook and persist a zero-filled server column (#218). Nothing must be
// written on that path.
func TestCreateFailsClosedWithoutHooks(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "t.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Book{}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: db} // Hooks left nil

	if _, err := h.create(context.Background(), &createBookInput{Body: bookCreateBody{Title: "hello"}}); err == nil {
		t.Fatal("create must return an error when Hooks is nil, not silently zero-fill the server column")
	}
	var count int64
	db.Model(&Book{}).Count(&count)
	if count != 0 {
		t.Fatalf("create must not persist a row on the misconfigured path; found %d", count)
	}
}
`

// --- slice 4.5: declared list-query surface ---

// queryModel declares each query capability via the gombit tag; the generated
// handler must emit the matching query params, sort/aggregate lists, and
// ListMeta, driven by resolved policy (not by re-reading tags in the handler).
type queryModel struct {
	gorm.Model
	Title  string        `gorm:"not null" gombit:"read,write,searchable,sortable"`
	Genre  string        `gombit:"read,write,filterable,sortable"`
	Price  int64         `gombit:"read,write,filterable,aggregatable"`
	Active bool          `gombit:"read,write,filterable"`
	Amount types.Decimal `gombit:"read,write,aggregatable"`
}

func TestRenderModelHandlerQuerySurface(t *testing.T) {
	res, err := buildModelResource(&queryModel{}, "querymodel")
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	src, err := renderModelHandler(res)
	if err != nil {
		t.Fatalf("renderModelHandler: %v", err)
	}
	assertParses(t, src)

	for _, want := range []string{
		// Aggregatable present -> ListMeta.
		"contract.DataMeta[[]queryModelData, contract.ListMeta]",
		// Search / ordering / aggregate param tags (alignment-independent).
		"`query:\"search\" doc:\"Search term matched across searchable fields\"`",
		"`query:\"ordering\" doc:\"Field to order by; prefix with - for DESC (allowed: title, genre)\"`",
		"e.g. sum:price (funcs: sum, avg, min, max; fields: price, amount)",
		// Filter param tags, with the bool one carrying a true/false enum.
		"`query:\"genre\" doc:\"Filter by Genre (exact match)\"`",
		"`query:\"price\" doc:\"Filter by Price (exact match)\"`",
		"`query:\"active\" enum:\"true,false\" doc:\"Filter by Active (exact match)\"`",
		// List body: filter coercion + search + aggregate + ordering.
		"database.FilterEq(ctx, q, \"genre\", database.FilterString, input.Genre)",
		"database.FilterEq(ctx, q, \"price\", database.FilterInt64, input.Price)",
		"database.FilterEq(ctx, q, \"active\", database.FilterBool, input.Active)",
		"database.Search(q, []string{\"title\"}, input.Search)",
		"database.ParseAggregates(ctx, input.Aggregate,",
		"database.Ordering(ctx, q, input.Ordering, []string{\"title\", \"genre\"}, \"id\")",
		"Aggregates: aggregates",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("query handler missing %q in:\n%s", want, src)
		}
	}
}

// With no query capabilities declared, the handler is byte-identical to the plain
// paginated list (PageMeta, no query params) — a no-modifier resource is unchanged.
func TestRenderModelHandlerNoQuerySurface(t *testing.T) {
	res := buildHandlerModel(t, "handlermodel")
	src, err := renderModelHandler(res)
	if err != nil {
		t.Fatalf("renderModelHandler: %v", err)
	}
	if !strings.Contains(src, "contract.DataMeta[[]handlerModelData, contract.PageMeta]") {
		t.Fatalf("no-query resource should use PageMeta:\n%s", src)
	}
	for _, absent := range []string{"query:\"search\"", "query:\"ordering\"", "query:\"aggregate\"", "database.FilterEq"} {
		if strings.Contains(src, absent) {
			t.Fatalf("no-query resource must not emit %q:\n%s", absent, src)
		}
	}
}

// Item is the compile-run model for the query surface: searchable/sortable Name,
// filterable Kind, filterable+aggregatable Price. Package-level so its Go type is
// literally "Item", matching the temp module's model.go.
type Item struct {
	gorm.Model
	Name  string `gorm:"not null" gombit:"read,write,searchable,sortable"`
	Kind  string `gombit:"read,write,filterable"`
	Price int64  `gombit:"read,write,filterable,sortable,aggregatable"`
}

// TestGeneratedQueryHandlerCompilesAndRuns generates a resource with the full
// query surface, compiles it against the real framework, and EXECUTES filter,
// search, ordering, and aggregate through the generated list handler on SQLite —
// proving the ported query codegen references database.FilterEq/Search/Ordering/
// ParseAggregates/Aggregate correctly and behaves the same as the legacy handler.
func TestGeneratedQueryHandlerCompilesAndRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles and runs a temp module against the framework; skipped in -short")
	}
	res, err := buildModelResource(&Item{}, "item")
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	dto := string(mustFormatGo(renderModelDTOs(res)))
	handler, err := renderModelHandler(res)
	if err != nil {
		t.Fatalf("renderModelHandler: %v", err)
	}
	handler = string(mustFormatGo(handler))
	hooks := string(mustFormatGo(renderModelHooks(res)))

	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "item")
	if err := os.MkdirAll(pkgDir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "go.mod"),
		"module itemapp\n\ngo 1.23\n\nrequire github.com/gombit-dev/gombit v0.0.0\n\nreplace github.com/gombit-dev/gombit => "+resourcegenModuleRoot(t)+"\n")
	writeFile(t, filepath.Join(pkgDir, "model.go"),
		"package item\n\nimport \"gorm.io/gorm\"\n\ntype Item struct {\n\tgorm.Model\n\tName  string `gorm:\"not null\"`\n\tKind  string\n\tPrice int64\n}\n")
	writeFile(t, filepath.Join(pkgDir, "dto.gen.go"), dto)
	writeFile(t, filepath.Join(pkgDir, "handler.gen.go"), handler)
	writeFile(t, filepath.Join(pkgDir, "hooks.go"), hooks)
	writeFile(t, filepath.Join(pkgDir, "run_test.go"), queryRunTest)

	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated query handler failed to compile/run: %v\n%s\n--- handler ---\n%s", err, out, handler)
	}
}

const queryRunTest = `package item

import (
	"context"
	"path/filepath"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestQuery(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "t.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Item{}); err != nil {
		t.Fatal(err)
	}
	for _, it := range []Item{
		{Name: "apple", Kind: "fruit", Price: 10},
		{Name: "banana", Kind: "fruit", Price: 20},
		{Name: "carrot", Kind: "veg", Price: 5},
	} {
		if err := db.Create(&it).Error; err != nil {
			t.Fatal(err)
		}
	}
	h := &Handler{DB: db}
	ctx := context.Background()

	// filter: Kind=fruit -> 2 rows.
	out, err := h.list(ctx, &listItemsInput{Page: 1, PerPage: 50, Kind: "fruit"})
	if err != nil {
		t.Fatalf("filter list: %v", err)
	}
	if out.Body.Meta.Total != 2 || len(out.Body.Data) != 2 {
		t.Fatalf("filter Kind=fruit: total=%d rows=%d", out.Body.Meta.Total, len(out.Body.Data))
	}

	// search: "app" -> apple only.
	s, err := h.list(ctx, &listItemsInput{Page: 1, PerPage: 50, Search: "app"})
	if err != nil {
		t.Fatalf("search list: %v", err)
	}
	if len(s.Body.Data) != 1 || s.Body.Data[0].Name != "apple" {
		t.Fatalf("search app: %+v", s.Body.Data)
	}

	// ordering: -price -> banana (20) first.
	o, err := h.list(ctx, &listItemsInput{Page: 1, PerPage: 50, Ordering: "-price"})
	if err != nil {
		t.Fatalf("ordering list: %v", err)
	}
	if len(o.Body.Data) != 3 || o.Body.Data[0].Name != "banana" {
		t.Fatalf("ordering -price: %+v", o.Body.Data)
	}

	// aggregate: sum:price over Kind=fruit -> 30.
	a, err := h.list(ctx, &listItemsInput{Page: 1, PerPage: 50, Kind: "fruit", Aggregate: "sum:price"})
	if err != nil {
		t.Fatalf("aggregate list: %v", err)
	}
	sum, ok := a.Body.Meta.Aggregates["sum:price"]
	if !ok {
		t.Fatalf("aggregate sum:price missing: %+v", a.Body.Meta.Aggregates)
	}
	if sum.String() != "30" {
		t.Fatalf("sum:price = %s, want 30", sum.String())
	}
}
`
