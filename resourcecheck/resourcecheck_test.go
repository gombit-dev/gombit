package resourcecheck_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/resourcecheck"
	"gorm.io/gorm"
)

func missing(t *testing.T, model any, body any, managed []string) []string {
	t.Helper()
	got, err := resourcecheck.MissingCreateColumns(model, body, managed)
	if err != nil {
		t.Fatalf("MissingCreateColumns: %v", err)
	}
	return got
}

// A model whose NOT NULL columns are all either in the DTO, nullable, defaulted,
// or auto-managed has no drift.
func TestNoDriftWhenRequiredColumnsAreCovered(t *testing.T) {
	type widget struct {
		ID        uint      `gorm:"primaryKey" json:"id"`
		Name      string    `gorm:"not null" json:"name"`
		Note      *string   `json:"note"`                            // nullable
		Slug      string    `gorm:"not null;default:''" json:"slug"` // DB default
		CreatedAt time.Time `json:"created_at"`                      // auto
		UpdatedAt time.Time `json:"updated_at"`                      // auto
	}
	type body struct {
		Name string `json:"name"`
		Note string `json:"note"`
	}
	if got := missing(t, &widget{}, body{}, nil); len(got) != 0 {
		t.Fatalf("missing = %v, want none", got)
	}
}

// A NOT NULL column with no default, absent from the create DTO, is drift.
func TestDriftWhenRequiredColumnMissingFromDTO(t *testing.T) {
	type widget struct {
		ID       uint   `gorm:"primaryKey" json:"id"`
		Name     string `gorm:"not null" json:"name"`
		TenantID uint   `gorm:"not null" json:"tenant_id"`
	}
	type nameOnly struct {
		Name string `json:"name"`
	}
	got := missing(t, &widget{}, nameOnly{}, nil)
	if !reflect.DeepEqual(got, []string{"tenant_id"}) {
		t.Fatalf("missing = %v, want [tenant_id]", got)
	}
}

// Listing the column as server-managed clears the drift (the handler fills it).
func TestServerManagedColumnIsAKnownSource(t *testing.T) {
	type widget struct {
		ID       uint   `gorm:"primaryKey" json:"id"`
		Name     string `gorm:"not null" json:"name"`
		TenantID uint   `gorm:"not null" json:"tenant_id"`
	}
	type nameOnly struct {
		Name string `json:"name"`
	}
	if got := missing(t, &widget{}, nameOnly{}, []string{"tenant_id"}); len(got) != 0 {
		t.Fatalf("missing = %v, want none (tenant_id server-managed)", got)
	}
}

// Putting the column in the create DTO clears the drift.
func TestColumnInDTOIsAKnownSource(t *testing.T) {
	type widget struct {
		ID       uint `gorm:"primaryKey" json:"id"`
		TenantID uint `gorm:"not null" json:"tenant_id"`
	}
	type withTenant struct {
		TenantID uint `json:"tenant_id"`
	}
	if got := missing(t, &widget{}, withTenant{}, nil); len(got) != 0 {
		t.Fatalf("missing = %v, want none", got)
	}
}

// A belongs_to foreign key column is matched by its json name in the DTO.
func TestForeignKeyColumn(t *testing.T) {
	type category struct {
		ID uint `gorm:"primaryKey" json:"id"`
	}
	type item struct {
		ID         uint     `gorm:"primaryKey" json:"id"`
		CategoryID uint     `gorm:"not null" json:"category_id"`
		Category   category `gorm:"constraint:OnDelete:RESTRICT" json:"-"`
	}
	type withFK struct {
		CategoryID uint `json:"category_id"`
	}
	if got := missing(t, &item{}, withFK{}, nil); len(got) != 0 {
		t.Fatalf("missing = %v, want none (category_id in DTO)", got)
	}
	type noFK struct{}
	if got := missing(t, &item{}, noFK{}, nil); !reflect.DeepEqual(got, []string{"category_id"}) {
		t.Fatalf("missing = %v, want [category_id]", got)
	}
}

// gorm.Model's embedded ID / timestamps / soft-delete are all auto-managed and
// never count as drift.
func TestGormModelEmbeddedFieldsAreAutoManaged(t *testing.T) {
	type post struct {
		gorm.Model
		Title string `gorm:"not null" json:"title"`
	}
	type body struct {
		Title string `json:"title"`
	}
	if got := missing(t, &post{}, body{}, nil); len(got) != 0 {
		t.Fatalf("missing = %v, want none", got)
	}
	type empty struct{}
	if got := missing(t, &post{}, empty{}, nil); !reflect.DeepEqual(got, []string{"title"}) {
		t.Fatalf("missing = %v, want [title] (only the hand-added NOT NULL column)", got)
	}
}

func TestInvalidModelErrors(t *testing.T) {
	if _, err := resourcecheck.MissingCreateColumns(nil, struct{}{}, nil); err == nil {
		t.Fatal("want an error for a nil model")
	}
}
