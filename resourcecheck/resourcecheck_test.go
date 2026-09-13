package resourcecheck_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/resourcecheck"
	"gorm.io/gorm"
)

func missing(t *testing.T, model any, assigned []string, managed []string) []string {
	t.Helper()
	got, err := resourcecheck.MissingCreateColumns(model, assigned, managed)
	if err != nil {
		t.Fatalf("MissingCreateColumns: %v", err)
	}
	return got
}

// No drift when every required column is assigned by the handler.
func TestNoDriftWhenRequiredColumnsAreAssigned(t *testing.T) {
	type widget struct {
		ID        uint      `gorm:"primaryKey"`
		Name      string    `gorm:"not null"`
		Note      *string   // nullable
		Slug      string    `gorm:"not null;default:''"` // DB default
		CreatedAt time.Time // auto
		UpdatedAt time.Time // auto
	}
	if got := missing(t, &widget{}, []string{"Name"}, nil); len(got) != 0 {
		t.Fatalf("missing = %v, want none", got)
	}
}

// The review's concrete failure: a required column the handler does NOT assign is
// drift, even if the DTO declares it. Matching is on the handler's assigned Go
// fields, not DTO membership.
func TestDriftWhenRequiredColumnNotAssigned(t *testing.T) {
	type widget struct {
		ID       uint   `gorm:"primaryKey"`
		Name     string `gorm:"not null"`
		TenantID uint   `gorm:"not null"`
	}
	// The handler assigns only Name (it forgot TenantID, regardless of the DTO).
	got := missing(t, &widget{}, []string{"Name"}, nil)
	if !reflect.DeepEqual(got, []string{"tenant_id"}) {
		t.Fatalf("missing = %v, want [tenant_id]", got)
	}
}

func TestAssignedColumnIsCovered(t *testing.T) {
	type widget struct {
		ID       uint `gorm:"primaryKey"`
		TenantID uint `gorm:"not null"`
	}
	if got := missing(t, &widget{}, []string{"TenantID"}, nil); len(got) != 0 {
		t.Fatalf("missing = %v, want none (TenantID assigned)", got)
	}
}

func TestServerManagedColumnIsAKnownSource(t *testing.T) {
	type widget struct {
		ID       uint   `gorm:"primaryKey"`
		Name     string `gorm:"not null"`
		TenantID uint   `gorm:"not null"`
	}
	// TenantID is not assigned by the handler but is set server-side.
	if got := missing(t, &widget{}, []string{"Name"}, []string{"tenant_id"}); len(got) != 0 {
		t.Fatalf("missing = %v, want none (tenant_id server-managed)", got)
	}
}

func TestForeignKeyColumn(t *testing.T) {
	type category struct {
		ID uint `gorm:"primaryKey"`
	}
	type item struct {
		ID         uint     `gorm:"primaryKey"`
		CategoryID uint     `gorm:"not null"`
		Category   category `gorm:"constraint:OnDelete:RESTRICT"`
	}
	if got := missing(t, &item{}, []string{"CategoryID"}, nil); len(got) != 0 {
		t.Fatalf("missing = %v, want none (CategoryID assigned)", got)
	}
	if got := missing(t, &item{}, nil, nil); !reflect.DeepEqual(got, []string{"category_id"}) {
		t.Fatalf("missing = %v, want [category_id]", got)
	}
}

// gorm.Model's embedded ID / timestamps / soft-delete are auto-managed.
func TestGormModelEmbeddedFieldsAreAutoManaged(t *testing.T) {
	type post struct {
		gorm.Model
		Title string `gorm:"not null"`
	}
	if got := missing(t, &post{}, []string{"Title"}, nil); len(got) != 0 {
		t.Fatalf("missing = %v, want none", got)
	}
	if got := missing(t, &post{}, nil, nil); !reflect.DeepEqual(got, []string{"title"}) {
		t.Fatalf("missing = %v, want [title] (only the hand-added NOT NULL column)", got)
	}
}

func TestInvalidModelErrors(t *testing.T) {
	if _, err := resourcecheck.MissingCreateColumns(nil, nil, nil); err == nil {
		t.Fatal("want an error for a nil model")
	}
}

// ConstructorCreateFields reads the fields the named constructor RETURNS.
func TestConstructorCreateFieldsParsesReturnedLiteral(t *testing.T) {
	src := []byte(`package book

func buildBookForCreate(input *createBookInput) Book {
	return Book{
		Title:      input.Body.Title,
		CategoryID: input.Body.CategoryID,
	}
}

func (h *Handler) create() Book { return buildBookForCreate(nil) }
func otherThing() Book          { return Book{Ignored: 1} }
`)
	got, err := resourcecheck.ConstructorCreateFields(src, "buildBookForCreate", "Book")
	if err != nil {
		t.Fatalf("ConstructorCreateFields: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"CategoryID", "Title"}) {
		t.Fatalf("assigned = %v, want [CategoryID Title]", got)
	}
}

// The review's decoy attack: a temporary `Book{...}` that is never returned must
// not certify a field the persisted value omits.
func TestConstructorCreateFieldsIgnoresDecoyLiterals(t *testing.T) {
	src := []byte(`package book

func buildBookForCreate(input *createBookInput) Book {
	_ = Book{TenantID: input.Body.TenantID} // decoy: validated but not persisted
	return Book{Title: input.Body.Title}
}
`)
	got, err := resourcecheck.ConstructorCreateFields(src, "buildBookForCreate", "Book")
	if err != nil {
		t.Fatalf("ConstructorCreateFields: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"Title"}) {
		t.Fatalf("assigned = %v, want [Title] (decoy TenantID literal is not returned)", got)
	}
}

func TestConstructorCreateFieldsEmptyWhenNoReturnedLiteral(t *testing.T) {
	// A constructor that returns a named local (not an inline literal) is treated
	// conservatively: no fields are certified.
	src := []byte("package book\n\nfunc buildBookForCreate() Book { b := Book{Title: x}; return b }\n")
	got, err := resourcecheck.ConstructorCreateFields(src, "buildBookForCreate", "Book")
	if err != nil {
		t.Fatalf("ConstructorCreateFields: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("assigned = %v, want none (returned value is not an inline literal)", got)
	}
}

// Review's multiple-return attack: a field set on only one return branch must
// not be certified, because the other path persists its zero value. The result
// is the intersection of the return paths, not their union.
func TestConstructorCreateFieldsIntersectsMultipleReturns(t *testing.T) {
	src := []byte(`package book
func buildBookForCreate(input *createBookInput) Book {
	if input.Body.CategoryID == 0 {
		return Book{Title: input.Body.Title}
	}
	return Book{Title: input.Body.Title, CategoryID: input.Body.CategoryID}
}`)
	got, err := resourcecheck.ConstructorCreateFields(src, "buildBookForCreate", "Book")
	if err != nil {
		t.Fatalf("ConstructorCreateFields: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"Title"}) {
		t.Fatalf("assigned = %v, want [Title] (CategoryID is set on only one branch)", got)
	}
}

// A return path that is not an inline literal (a named local) empties the
// intersection: nothing is certified, so every required column fails closed.
func TestConstructorCreateFieldsNonLiteralReturnFailsClosed(t *testing.T) {
	src := []byte(`package book
func buildBookForCreate(input *createBookInput) Book {
	if input.Body.Title == "" {
		b := Book{Title: "untitled"}
		return b
	}
	return Book{Title: input.Body.Title}
}`)
	got, err := resourcecheck.ConstructorCreateFields(src, "buildBookForCreate", "Book")
	if err != nil {
		t.Fatalf("ConstructorCreateFields: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("assigned = %v, want none (a non-literal return path fails closed)", got)
	}
}

// goodCtor is the canonical context-aware constructor: a single unconditional
// return literal. withCtor composes a book package from it plus a create method.
const goodCtor = "func buildBookForCreate(ctx context.Context, input *createBookInput) Book {\n\treturn Book{Title: input.Body.Title}\n}\n"

func withCtor(method string) string {
	return "package book\n" + goodCtor + method
}

func grammarOK(t *testing.T, src string) (bool, string) {
	t.Helper()
	ok, detail, err := resourcecheck.ValidateCreateGrammar([]byte(src), "Handler", "create", "buildBookForCreate", "Book")
	if err != nil {
		t.Fatalf("ValidateCreateGrammar: %v", err)
	}
	return ok, detail
}

// The canonical generated create shape is accepted.
func TestValidateCreateGrammarHappyPath(t *testing.T) {
	src := withCtor(`func (h *Handler) create(ctx context.Context, input *createBookInput) (*createBookOutput, error) {
	row := buildBookForCreate(ctx, input)
	if err := h.DB.WithContext(ctx).Create(&row).Error; err != nil {
		return nil, err
	}
	return createBookOutput{}, nil
}`)
	if ok, detail := grammarOK(t, src); !ok {
		t.Fatalf("want ok; detail=%q", detail)
	}
}

// The grammar is exact: even a bare `return h.DB.….Create(&row).Error` (no
// checked-error if) is not what the generator emits, so it is rejected.
func TestValidateCreateGrammarRejectsBareReturnCreate(t *testing.T) {
	src := withCtor(`func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(ctx, input)
	return h.DB.WithContext(ctx).Create(&row).Error
}`)
	if ok, _ := grammarOK(t, src); ok {
		t.Fatal("want rejected: the exact grammar requires the checked-error if, not a bare return")
	}
}

// Finding 1 (this round): a Create guarded by a business conditional leaves a
// runtime path that returns success without persisting. Rejected.
func TestValidateCreateGrammarRejectsConditionalCreate(t *testing.T) {
	src := withCtor(`func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(ctx, input)
	if input.Body.Title != "" {
		return h.DB.WithContext(ctx).Create(&row).Error
	}
	return nil
}`)
	if ok, _ := grammarOK(t, src); ok {
		t.Fatal("want rejected: one path returns success without persisting")
	}
}

// Finding 1 (this round): the Create runs but its error is ignored, so the only
// write can fail while the handler reports success. Rejected.
func TestValidateCreateGrammarRejectsUncheckedCreate(t *testing.T) {
	src := withCtor(`func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(ctx, input)
	h.DB.WithContext(ctx).Create(&row)
	return nil
}`)
	if ok, _ := grammarOK(t, src); ok {
		t.Fatal("want rejected: the Create error is not checked")
	}
}

// Finding 2 (this round): `h.Audit.DB.….Create` writes through a nested database,
// not the handler's own h.DB. Rejected — .DB appearing somewhere in the chain is
// not the receiver's direct DB field.
func TestValidateCreateGrammarRejectsNestedDBCreate(t *testing.T) {
	src := withCtor(`func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(ctx, input)
	if err := h.Audit.DB.WithContext(ctx).Create(&row).Error; err != nil {
		return err
	}
	return nil
}`)
	if ok, _ := grammarOK(t, src); ok {
		t.Fatal("want rejected: Create must be through the receiver's own DB field, not h.Audit.DB")
	}
}

// Finding 2 (round 8): a DryRun session returns without inserting — a successful
// non-write. The pinned h.DB.WithContext(ctx) chain rejects the intervening call.
func TestValidateCreateGrammarRejectsDryRunCreate(t *testing.T) {
	src := withCtor(`func (h *Handler) create(ctx context.Context, input *createBookInput) (*createBookOutput, error) {
	row := buildBookForCreate(ctx, input)
	if err := h.DB.Session(&gorm.Session{DryRun: true}).Create(&row).Error; err != nil {
		return nil, err
	}
	return nil, nil
}`)
	if ok, _ := grammarOK(t, src); ok {
		t.Fatal("want rejected: a DryRun session is a successful non-write")
	}
}

// Finding 2 (round 8): an else branch can undo the successful create.
func TestValidateCreateGrammarRejectsElseBranch(t *testing.T) {
	src := withCtor(`func (h *Handler) create(ctx context.Context, input *createBookInput) (*createBookOutput, error) {
	row := buildBookForCreate(ctx, input)
	if err := h.DB.WithContext(ctx).Create(&row).Error; err != nil {
		return nil, err
	} else {
		h.DB.Delete(&row)
	}
	return nil, nil
}`)
	if ok, _ := grammarOK(t, src); ok {
		t.Fatal("want rejected: an else branch may delete the created row")
	}
}

// Finding 2 (round 8): the error branch reports success (return nil, nil) instead
// of returning the persistence error.
func TestValidateCreateGrammarRejectsErrorBranchWithoutErr(t *testing.T) {
	src := withCtor(`func (h *Handler) create(ctx context.Context, input *createBookInput) (*createBookOutput, error) {
	row := buildBookForCreate(ctx, input)
	if err := h.DB.WithContext(ctx).Create(&row).Error; err != nil {
		return nil, nil
	}
	return nil, nil
}`)
	if ok, _ := grammarOK(t, src); ok {
		t.Fatal("want rejected: the error branch does not return the persistence error")
	}
}

// Finding 3 (this round): a same-named constructor METHOD decoy is declared
// before the package function. The validator must judge the package function
// (which the unqualified call resolves to), and it omits TenantID.
func TestValidateCreateGrammarIgnoresConstructorMethodDecoy(t *testing.T) {
	src := `package book
func (p *Probe) buildBookForCreate(ctx context.Context, input *createBookInput) Book {
	return Book{Title: input.Body.Title, TenantID: input.Body.TenantID}
}
func buildBookForCreate(ctx context.Context, input *createBookInput) Book {
	return Book{Title: input.Body.Title}
}
func (h *Handler) create(ctx context.Context, input *createBookInput) (*createBookOutput, error) {
	row := buildBookForCreate(ctx, input)
	if err := h.DB.WithContext(ctx).Create(&row).Error; err != nil {
		return nil, err
	}
	return nil, nil
}`
	if ok, detail := grammarOK(t, src); !ok {
		t.Fatalf("want ok (the package constructor is valid); detail=%q", detail)
	}
	// The field collector must certify only the package function's fields.
	got, err := resourcecheck.ConstructorCreateFields([]byte(src), "buildBookForCreate", "Book")
	if err != nil {
		t.Fatalf("ConstructorCreateFields: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"Title"}) {
		t.Fatalf("assigned = %v, want [Title] (the method decoy's TenantID must not be certified)", got)
	}
}

// Building the value inline bypasses the constructor.
func TestValidateCreateGrammarRejectsBypass(t *testing.T) {
	src := withCtor(`func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := Book{Title: input.Body.Title}
	return h.DB.WithContext(ctx).Create(&row).Error
}`)
	if ok, _ := grammarOK(t, src); ok {
		t.Fatal("want rejected when create builds the value inline")
	}
}

// No post-construction mutation is permitted — server values belong in the ctor.
func TestValidateCreateGrammarRejectsPostConstructAssignment(t *testing.T) {
	src := withCtor(`func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(ctx, input)
	row.TenantID = tenantFrom(ctx)
	return h.DB.WithContext(ctx).Create(&row).Error
}`)
	if ok, _ := grammarOK(t, src); ok {
		t.Fatal("want rejected when row is mutated after construction")
	}
}

func TestValidateCreateGrammarRejectsReassign(t *testing.T) {
	src := withCtor(`func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(ctx, input)
	row = Book{}
	return h.DB.WithContext(ctx).Create(&row).Error
}`)
	if ok, _ := grammarOK(t, src); ok {
		t.Fatal("want rejected when the persisted variable is reassigned")
	}
}

func TestValidateCreateGrammarRejectsMissingCreate(t *testing.T) {
	src := withCtor(`func (h *Handler) create(ctx context.Context, input *createBookInput) error { return nil }`)
	if ok, _ := grammarOK(t, src); ok {
		t.Fatal("want rejected when there is no Create call")
	}
}

// The constructor assignment runs AFTER Create; source order is not runtime order.
func TestValidateCreateGrammarRejectsAssignAfterCreate(t *testing.T) {
	src := withCtor(`func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	var row Book
	_ = h.DB.WithContext(ctx).Create(&row).Error
	row = buildBookForCreate(ctx, input)
	return nil
}`)
	if ok, _ := grammarOK(t, src); ok {
		t.Fatal("want rejected when the constructor assignment runs after Create")
	}
}

func TestValidateCreateGrammarRejectsConditionalAssign(t *testing.T) {
	src := withCtor(`func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	var row Book
	if input.Body.Title != "" {
		row = buildBookForCreate(ctx, input)
	}
	return h.DB.WithContext(ctx).Create(&row).Error
}`)
	if ok, _ := grammarOK(t, src); ok {
		t.Fatal("want rejected when the constructor assignment is under a conditional")
	}
}

func TestValidateCreateGrammarRejectsAssignInClosure(t *testing.T) {
	src := withCtor(`func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	var row Book
	func() { row = buildBookForCreate(ctx, input) }()
	return h.DB.WithContext(ctx).Create(&row).Error
}`)
	if ok, _ := grammarOK(t, src); ok {
		t.Fatal("want rejected when the constructor assignment is inside a closure")
	}
}

func TestValidateCreateGrammarRejectsSecondCreate(t *testing.T) {
	src := withCtor(`func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(ctx, input)
	_ = h.DB.WithContext(ctx).Create(&Book{}).Error
	return h.DB.WithContext(ctx).Create(&row).Error
}`)
	if ok, _ := grammarOK(t, src); ok {
		t.Fatal("want rejected when a second h.DB.Create call is present")
	}
}

// Finding 4 (this round): a deferred Create runs on return, so a later
// `row.clearTenant()` mutates the value first. Source position is not runtime
// order — reject defer/go Create.
func TestValidateCreateGrammarRejectsDeferCreate(t *testing.T) {
	src := withCtor(`func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(ctx, input)
	defer h.DB.WithContext(ctx).Create(&row)
	row.clearTenant()
	return nil
}`)
	if ok, _ := grammarOK(t, src); ok {
		t.Fatal("want rejected when the Create is deferred")
	}
}

// Finding 3 (this round): the only Create is on h.Audit, not h.DB, so it is not
// the GORM persistence call. Reject: the real write is not through DB.
func TestValidateCreateGrammarRejectsNonDBCreate(t *testing.T) {
	src := withCtor(`func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(ctx, input)
	return h.Audit.WithContext(ctx).Create(&row).Error
}`)
	if ok, _ := grammarOK(t, src); ok {
		t.Fatal("want rejected when Create is not rooted through h.DB")
	}
}

// Receiver pinning: a decoy create on another type is declared first with a valid
// shape; the checker judges Handler.create (which bypasses), not the decoy.
func TestValidateCreateGrammarPinsReceiver(t *testing.T) {
	src := withCtor(`func (p *Probe) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(ctx, input)
	return p.DB.WithContext(ctx).Create(&row).Error
}
func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := Book{Title: input.Body.Title}
	return h.DB.WithContext(ctx).Create(&row).Error
}`)
	if ok, _ := grammarOK(t, src); ok {
		t.Fatal("want rejected: Handler.create (not the decoy) bypasses the constructor")
	}
}

func TestValidateCreateGrammarRequiresExpectedReceiver(t *testing.T) {
	src := withCtor(`func (p *Probe) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(ctx, input)
	return p.DB.WithContext(ctx).Create(&row).Error
}`)
	if ok, _ := grammarOK(t, src); ok {
		t.Fatal("want rejected when no Handler.create exists")
	}
}

// Implicit mutation: a pointer-receiver method call takes &row implicitly.
func TestValidateCreateGrammarRejectsPointerReceiverMethod(t *testing.T) {
	src := withCtor(`func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(ctx, input)
	row.clearTenant()
	return h.DB.WithContext(ctx).Create(&row).Error
}`)
	if ok, _ := grammarOK(t, src); ok {
		t.Fatal("want rejected when a method is invoked on row before Create")
	}
}

func TestValidateCreateGrammarRejectsIncDec(t *testing.T) {
	src := withCtor(`func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(ctx, input)
	row.Views++
	return h.DB.WithContext(ctx).Create(&row).Error
}`)
	if ok, _ := grammarOK(t, src); ok {
		t.Fatal("want rejected when a field of row is incremented before Create")
	}
}

func TestValidateCreateGrammarRejectsPointerAlias(t *testing.T) {
	src := withCtor(`func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(ctx, input)
	p := &row
	p.Title = ""
	return h.DB.WithContext(ctx).Create(&row).Error
}`)
	if ok, _ := grammarOK(t, src); ok {
		t.Fatal("want rejected when an alias to the persisted row is created")
	}
}

func TestValidateCreateGrammarRejectsAddressPassedToHelper(t *testing.T) {
	src := withCtor(`func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(ctx, input)
	normalize(&row)
	return h.DB.WithContext(ctx).Create(&row).Error
}`)
	if ok, _ := grammarOK(t, src); ok {
		t.Fatal("want rejected when &row is handed to another function before Create")
	}
}

// The constructor itself must be a single unconditional return literal.
func TestValidateCreateGrammarRejectsBranchingConstructor(t *testing.T) {
	src := `package book
func buildBookForCreate(ctx context.Context, input *createBookInput) Book {
	if input.Body.Title == "" {
		return Book{Title: "untitled"}
	}
	return Book{Title: input.Body.Title}
}
func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(ctx, input)
	return h.DB.WithContext(ctx).Create(&row).Error
}`
	if ok, _ := grammarOK(t, src); ok {
		t.Fatal("want rejected when the constructor branches instead of a single return literal")
	}
}

func TestValidateCreateGrammarRejectsNonLiteralConstructor(t *testing.T) {
	src := `package book
func buildBookForCreate(ctx context.Context, input *createBookInput) Book {
	b := Book{Title: input.Body.Title}
	return b
}
func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(ctx, input)
	return h.DB.WithContext(ctx).Create(&row).Error
}`
	if ok, _ := grammarOK(t, src); ok {
		t.Fatal("want rejected when the constructor returns a named local, not a literal")
	}
}

// End to end, including the decoy: the decoy'd TenantID is still reported as
// drift because only the RETURNED literal counts.
func TestConstructorFieldsFeedMissingColumns(t *testing.T) {
	type widget struct {
		ID       uint   `gorm:"primaryKey"`
		TenantID uint   `gorm:"not null"`
		Name     string `gorm:"not null"`
	}
	both := []byte("package w\nfunc buildWidgetForCreate(in any) widget { return widget{Name: x, TenantID: y} }\n")
	fields, err := resourcecheck.ConstructorCreateFields(both, "buildWidgetForCreate", "widget")
	if err != nil {
		t.Fatalf("ConstructorCreateFields: %v", err)
	}
	if got := missing(t, &widget{}, fields, nil); len(got) != 0 {
		t.Fatalf("missing = %v, want none when both returned", got)
	}

	decoy := []byte("package w\nfunc buildWidgetForCreate(in any) widget { _ = widget{TenantID: y}; return widget{Name: x} }\n")
	fields, err = resourcecheck.ConstructorCreateFields(decoy, "buildWidgetForCreate", "widget")
	if err != nil {
		t.Fatalf("ConstructorCreateFields: %v", err)
	}
	if got := missing(t, &widget{}, fields, nil); !reflect.DeepEqual(got, []string{"tenant_id"}) {
		t.Fatalf("missing = %v, want [tenant_id] (decoy TenantID must not certify)", got)
	}
}

// --- #218 finding 4: model↔wire-contract drift ---

// wireBook and its DTOs mirror the generated shapes. The response DTO wraps the
// payload in the D10 contract.Data envelope, exactly like generated code.
type wireBook struct {
	gorm.Model
	Title string `gorm:"not null"`
	Note  *string
}

type createWireBookInput struct {
	Body struct {
		Title string `json:"title"`
	}
}

type wireBookData struct {
	ID    uint   `json:"id"`
	Title string `json:"title"`
}

type wireBookOutput struct {
	Body contract.Data[wireBookData]
}

type wireHandler struct{}

func (*wireHandler) create(ctx context.Context, input *createWireBookInput) (*wireBookOutput, error) {
	return nil, nil
}
func (*wireHandler) get(ctx context.Context, input *createWireBookInput) (*wireBookOutput, error) {
	return nil, nil
}

// A content column (nullable Note) exposed by neither the request nor the
// response wire contract is write- and read-drift. Title is exposed on both.
func TestModelWireDriftFlagsMissingWireFields(t *testing.T) {
	h := &wireHandler{}
	write, read, err := resourcecheck.ModelWireDrift(&wireBook{}, h.create, h.get, nil, nil, nil)
	if err != nil {
		t.Fatalf("ModelWireDrift: %v", err)
	}
	if !reflect.DeepEqual(write, []string{"note"}) {
		t.Fatalf("writeDrift = %v, want [note]", write)
	}
	if !reflect.DeepEqual(read, []string{"note"}) {
		t.Fatalf("readDrift = %v, want [note]", read)
	}
}

func TestModelWireDriftRespectsExemptions(t *testing.T) {
	h := &wireHandler{}
	write, read, err := resourcecheck.ModelWireDrift(&wireBook{}, h.create, h.get, nil, []string{"note"}, []string{"note"})
	if err != nil {
		t.Fatalf("ModelWireDrift: %v", err)
	}
	if len(write) != 0 || len(read) != 0 {
		t.Fatalf("want no drift with note exempt; write=%v read=%v", write, read)
	}
}

// json:"-" hides a field from the wire even though the Go field exists, so a
// model column covered only by such a field is still drift.
type dashInput struct {
	Body struct {
		Title string `json:"title"`
		Note  string `json:"-"`
	}
}
type dashOutput struct {
	Body contract.Data[struct {
		ID    uint   `json:"id"`
		Title string `json:"title"`
		Note  string `json:"-"`
	}]
}
type dashHandler struct{}

func (*dashHandler) create(ctx context.Context, input *dashInput) (*dashOutput, error) {
	return nil, nil
}
func (*dashHandler) get(ctx context.Context, input *dashInput) (*dashOutput, error) { return nil, nil }

func TestModelWireDriftHonorsJSONDash(t *testing.T) {
	h := &dashHandler{}
	write, read, err := resourcecheck.ModelWireDrift(&wireBook{}, h.create, h.get, nil, nil, nil)
	if err != nil {
		t.Fatalf("ModelWireDrift: %v", err)
	}
	// Note is present as a Go field but json:"-", so it is not on the wire.
	if !reflect.DeepEqual(write, []string{"note"}) {
		t.Fatalf("writeDrift = %v, want [note] (json:\"-\" is not on the wire)", write)
	}
	if !reflect.DeepEqual(read, []string{"note"}) {
		t.Fatalf("readDrift = %v, want [note]", read)
	}
}

// Association/relationship fields carry no DB column and must not be reported as
// drift (they are not scalar content columns).
type wireCategory struct {
	gorm.Model
	Name string `gorm:"not null"`
}
type wireItem struct {
	gorm.Model
	Name       string `gorm:"not null"`
	CategoryID uint
	Category   wireCategory // belongs_to association object (no column)
}
type wireItemInput struct {
	Body struct {
		Name       string `json:"name"`
		CategoryID uint   `json:"category_id"`
	}
}
type wireItemData struct {
	ID         uint   `json:"id"`
	Name       string `json:"name"`
	CategoryID uint   `json:"category_id"`
}
type wireItemOutput struct {
	Body contract.Data[wireItemData]
}
type itemHandler struct{}

func (*itemHandler) create(ctx context.Context, input *wireItemInput) (*wireItemOutput, error) {
	return nil, nil
}
func (*itemHandler) get(ctx context.Context, input *wireItemInput) (*wireItemOutput, error) {
	return nil, nil
}

func TestModelWireDriftIgnoresAssociationsAndAutoManaged(t *testing.T) {
	h := &itemHandler{}
	// The DTOs expose name and category_id; the Category association and
	// gorm.Model fields are not columns. Expect no drift and, crucially, no "".
	write, read, err := resourcecheck.ModelWireDrift(&wireItem{}, h.create, h.get, nil, nil, nil)
	if err != nil {
		t.Fatalf("ModelWireDrift: %v", err)
	}
	if len(write) != 0 || len(read) != 0 {
		t.Fatalf("want no drift (association is not a content column); write=%v read=%v", write, read)
	}
}
