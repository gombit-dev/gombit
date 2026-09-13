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

// AssignedCreateFields reads the fields the generated create constructor assigns.
func TestAssignedCreateFieldsParsesCreateConstructor(t *testing.T) {
	src := []byte(`package book

type Handler struct{}

func (h *Handler) create(ctx any, input *createBookInput) error {
	row := Book{
		Title:      input.Body.Title,
		CategoryID: input.Body.CategoryID,
	}
	_ = row
	return nil
}

func (h *Handler) list() { _ = Book{Ignored: 1} }
`)
	got, err := resourcecheck.AssignedCreateFields(src, "Book")
	if err != nil {
		t.Fatalf("AssignedCreateFields: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"CategoryID", "Title"}) {
		t.Fatalf("assigned = %v, want [CategoryID Title] (only create's Book{...}, not list's)", got)
	}
}

func TestAssignedCreateFieldsEmptyWhenNoConstructor(t *testing.T) {
	src := []byte("package book\n\nfunc (h *Handler) create() {}\n")
	got, err := resourcecheck.AssignedCreateFields(src, "Book")
	if err != nil {
		t.Fatalf("AssignedCreateFields: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("assigned = %v, want none", got)
	}
}

// End to end: an assigned field clears the drift the same column shows when the
// handler's create constructor omits it.
func TestAssignedFieldsFeedMissingColumns(t *testing.T) {
	type widget struct {
		ID       uint   `gorm:"primaryKey"`
		TenantID uint   `gorm:"not null"`
		Name     string `gorm:"not null"`
	}
	assignsBoth := []byte("package w\nfunc (h *Handler) create() { _ = widget{Name: x, TenantID: y} }\n")
	fields, err := resourcecheck.AssignedCreateFields(assignsBoth, "widget")
	if err != nil {
		t.Fatalf("AssignedCreateFields: %v", err)
	}
	if got := missing(t, &widget{}, fields, nil); len(got) != 0 {
		t.Fatalf("missing = %v, want none when both assigned", got)
	}

	assignsName := []byte("package w\nfunc (h *Handler) create() { _ = widget{Name: x} }\n")
	fields, err = resourcecheck.AssignedCreateFields(assignsName, "widget")
	if err != nil {
		t.Fatalf("AssignedCreateFields: %v", err)
	}
	if got := missing(t, &widget{}, fields, nil); !reflect.DeepEqual(got, []string{"tenant_id"}) {
		t.Fatalf("missing = %v, want [tenant_id] when TenantID not assigned", got)
	}
}
