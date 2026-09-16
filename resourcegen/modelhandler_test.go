package resourcegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
`
