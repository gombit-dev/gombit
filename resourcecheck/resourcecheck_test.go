package resourcecheck_test

import (
	"reflect"
	"testing"
	"time"

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

func createPersists(t *testing.T, src string, serverManaged ...string) (bool, string) {
	t.Helper()
	ok, detail, err := resourcecheck.CreatePersistsConstructor([]byte(src), "Handler", "create", "buildBookForCreate", serverManaged)
	if err != nil {
		t.Fatalf("CreatePersistsConstructor: %v", err)
	}
	return ok, detail
}

// The generated create method routes the persisted value through the constructor.
func TestCreatePersistsConstructorHappyPath(t *testing.T) {
	src := `package book
func (h *Handler) create(ctx context.Context, input *createBookInput) (*createBookOutput, error) {
	row := buildBookForCreate(input)
	if err := h.DB.WithContext(ctx).Create(&row).Error; err != nil {
		return nil, err
	}
	return nil, nil
}`
	if ok, detail := createPersists(t, src); !ok {
		t.Fatalf("want persists=true; detail=%q", detail)
	}
}

// Review's failure A: the handler builds the persisted value inline, bypassing
// the constructor. The constructor may still return TenantID, but it is not what
// gets persisted — must be rejected.
func TestCreatePersistsConstructorRejectsBypass(t *testing.T) {
	src := `package book
func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := Book{Title: input.Body.Title}
	return h.DB.WithContext(ctx).Create(&row).Error
}`
	if ok, _ := createPersists(t, src); ok {
		t.Fatal("want persists=false when create builds the value inline (bypassing the constructor)")
	}
}

// Review's failure B: the handler mutates the constructor result before Create.
func TestCreatePersistsConstructorRejectsMutation(t *testing.T) {
	src := `package book
func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(input)
	row.TenantID = 0
	return h.DB.WithContext(ctx).Create(&row).Error
}`
	if ok, _ := createPersists(t, src); ok {
		t.Fatal("want persists=false when the constructor result is mutated before Create")
	}
}

func TestCreatePersistsConstructorRejectsReassign(t *testing.T) {
	src := `package book
func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(input)
	row = Book{}
	return h.DB.WithContext(ctx).Create(&row).Error
}`
	if ok, _ := createPersists(t, src); ok {
		t.Fatal("want persists=false when the persisted variable is reassigned")
	}
}

func TestCreatePersistsConstructorRejectsMissingCreate(t *testing.T) {
	src := `package book
func (h *Handler) create(ctx context.Context, input *createBookInput) error { return nil }`
	if ok, _ := createPersists(t, src); ok {
		t.Fatal("want persists=false when there is no Create call")
	}
	if ok, _, err := resourcecheck.CreatePersistsConstructor([]byte("package book\n"), "Handler", "create", "buildBookForCreate", nil); err != nil || ok {
		t.Fatalf("want persists=false when the create method is absent; ok=%v err=%v", ok, err)
	}
}

// Review's ordering attack: the constructor assignment exists, but it runs
// AFTER Create, which already persisted the zero value. Syntactic presence is
// not dataflow — must be rejected.
func TestCreatePersistsConstructorRejectsAssignAfterCreate(t *testing.T) {
	src := `package book
func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	var row Book
	_ = h.DB.WithContext(ctx).Create(&row).Error
	row = buildBookForCreate(input)
	return nil
}`
	if ok, _ := createPersists(t, src); ok {
		t.Fatal("want persists=false when the constructor assignment runs after Create")
	}
}

// Review's conditional attack: the constructor assignment only happens on one
// branch, so the other path persists a zero value. A non-dominating assignment
// must be rejected.
func TestCreatePersistsConstructorRejectsConditionalAssign(t *testing.T) {
	src := `package book
func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	var row Book
	if input.Body.Title != "" {
		row = buildBookForCreate(input)
	}
	return h.DB.WithContext(ctx).Create(&row).Error
}`
	if ok, _ := createPersists(t, src); ok {
		t.Fatal("want persists=false when the constructor assignment is under a conditional")
	}
}

// Review's closure/dead-code concern: an assignment buried in a function literal
// does not dominate Create on the enclosing path.
func TestCreatePersistsConstructorRejectsAssignInClosure(t *testing.T) {
	src := `package book
func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	var row Book
	func() { row = buildBookForCreate(input) }()
	return h.DB.WithContext(ctx).Create(&row).Error
}`
	if ok, _ := createPersists(t, src); ok {
		t.Fatal("want persists=false when the constructor assignment is inside a closure")
	}
}

// Two Create calls make "the persisted value" ambiguous — reject conservatively.
func TestCreatePersistsConstructorRejectsSecondCreate(t *testing.T) {
	src := `package book
func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(input)
	_ = h.DB.WithContext(ctx).Create(&Book{}).Error
	return h.DB.WithContext(ctx).Create(&row).Error
}`
	if ok, _ := createPersists(t, src); ok {
		t.Fatal("want persists=false when a second Create call is present")
	}
}

// Finding 2: receiver pinning. A decoy create on another type is declared first
// with a valid shape; the registered Handler.create bypasses the constructor.
// The checker must judge Handler.create, not the decoy.
func TestCreatePersistsConstructorPinsReceiver(t *testing.T) {
	src := `package book
func (p *Probe) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(input)
	return p.DB.WithContext(ctx).Create(&row).Error
}
func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := Book{Title: input.Body.Title}
	return h.DB.WithContext(ctx).Create(&row).Error
}`
	if ok, _ := createPersists(t, src); ok {
		t.Fatal("want persists=false: Handler.create (not the decoy) bypasses the constructor")
	}
}

func TestCreatePersistsConstructorRequiresExpectedReceiver(t *testing.T) {
	src := `package book
func (p *Probe) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(input)
	return p.DB.WithContext(ctx).Create(&row).Error
}`
	if ok, _ := createPersists(t, src); ok {
		t.Fatal("want persists=false when no Handler.create exists")
	}
}

// Finding 2: the persistence call must be rooted at the handler receiver, so an
// unrelated object's .Create cannot masquerade as the GORM write.
func TestCreatePersistsConstructorRejectsNonReceiverCreate(t *testing.T) {
	src := `package book
func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(input)
	return other.WithContext(ctx).Create(&row).Error
}`
	if ok, _ := createPersists(t, src); ok {
		t.Fatal("want persists=false when Create is not rooted at the handler receiver")
	}
}

// Finding 1: a pointer-receiver method call implicitly takes &row and can mutate
// it, with no &row syntax and no assignment node. The allowlist still rejects it.
func TestCreatePersistsConstructorRejectsPointerReceiverMethod(t *testing.T) {
	src := `package book
func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(input)
	row.clearTenant()
	return h.DB.WithContext(ctx).Create(&row).Error
}`
	if ok, _ := createPersists(t, src); ok {
		t.Fatal("want persists=false when a method is invoked on row before Create")
	}
}

// Finding 1: `row.Field++` is an *ast.IncDecStmt, not an AssignStmt.
func TestCreatePersistsConstructorRejectsIncDec(t *testing.T) {
	src := `package book
func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(input)
	row.Views++
	return h.DB.WithContext(ctx).Create(&row).Error
}`
	if ok, _ := createPersists(t, src); ok {
		t.Fatal("want persists=false when a field of row is incremented before Create")
	}
}

// Finding 3: assigning a declared server-managed column after construction is the
// documented, supported flow and must be permitted.
func TestCreatePersistsConstructorPermitsServerManagedAssignment(t *testing.T) {
	src := `package book
func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(input)
	row.TenantID = tenantFrom(ctx)
	return h.DB.WithContext(ctx).Create(&row).Error
}`
	if ok, detail := createPersists(t, src, "tenant_id"); !ok {
		t.Fatalf("want persists=true when the mutated column is server-managed; detail=%q", detail)
	}
}

// Finding 3: assigning a column that is NOT declared server-managed is still a
// silent divergence and must be rejected.
func TestCreatePersistsConstructorRejectsUndeclaredServerManagedAssignment(t *testing.T) {
	src := `package book
func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(input)
	row.OwnerID = userFrom(ctx)
	return h.DB.WithContext(ctx).Create(&row).Error
}`
	if ok, _ := createPersists(t, src, "tenant_id"); ok {
		t.Fatal("want persists=false when assigning a field whose column is not server-managed")
	}
}

// Review's aliasing attack: mutate the persisted row through a pointer alias
// before Create. The direct `row.X =` check misses `p.X =`, so the guard instead
// fails closed on any `&row` taken outside the Create call.
func TestCreatePersistsConstructorRejectsPointerAlias(t *testing.T) {
	src := `package book
func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(input)
	p := &row
	p.Title = ""
	return h.DB.WithContext(ctx).Create(&row).Error
}`
	if ok, _ := createPersists(t, src); ok {
		t.Fatal("want persists=false when an alias to the persisted row is created")
	}
}

// Passing &row to a helper that could mutate it is indirect mutation the AST
// cannot follow — fail closed.
func TestCreatePersistsConstructorRejectsAddressPassedToHelper(t *testing.T) {
	src := `package book
func (h *Handler) create(ctx context.Context, input *createBookInput) error {
	row := buildBookForCreate(input)
	normalize(&row)
	return h.DB.WithContext(ctx).Create(&row).Error
}`
	if ok, _ := createPersists(t, src); ok {
		t.Fatal("want persists=false when &row is handed to another function before Create")
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
