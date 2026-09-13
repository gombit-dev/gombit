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
