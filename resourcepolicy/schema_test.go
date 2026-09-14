package resourcepolicy_test

import (
	"database/sql/driver"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/resourcepolicy"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

func parse(t *testing.T, model any) *schema.Schema {
	t.Helper()
	sch, err := schema.Parse(model, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatalf("schema.Parse: %v", err)
	}
	return sch
}

// field selects a parsed field by Go name for a collision-free test model.
func field(t *testing.T, sch *schema.Schema, goName string) *schema.Field {
	t.Helper()
	for _, f := range sch.Fields {
		if f.Name == goName {
			return f
		}
	}
	t.Fatalf("field %q not found in schema", goName)
	return nil
}

func resolvedByColumn(t *testing.T, resolved []resourcepolicy.Resolved) map[string]resourcepolicy.Resolved {
	t.Helper()
	out := map[string]resourcepolicy.Resolved{}
	for _, r := range resolved {
		out[r.Column] = r
	}
	return out
}

// FromModel end to end: a realistic model resolves its columns with the right
// surfaces, honoring tags, keys, timestamps, soft-delete, and permissions.
func TestResolvedFromModel(t *testing.T) {
	type Book struct {
		gorm.Model        // ID (auto key), CreatedAt/UpdatedAt (auto), DeletedAt (soft-delete)
		Title      string `gorm:"not null"`
		TenantID   uint   `gorm:"not null" gombit:"read,server"`
		Internal   int    `gombit:"-"`
	}
	resolved, err := resourcepolicy.ResolvedFromModel(&Book{})
	if err != nil {
		t.Fatalf("ResolvedFromModel: %v", err)
	}
	byCol := resolvedByColumn(t, resolved)

	// Auto-increment key + timestamps are read-only; soft-delete is not surfaced.
	if k := byCol["id"]; k.InRequest || !k.InResponse {
		t.Fatalf("id = %+v, want response-only", k)
	}
	if _, ok := byCol["deleted_at"]; ok {
		if d := byCol["deleted_at"]; d.InRequest || d.InResponse {
			t.Fatalf("deleted_at = %+v, want hidden", d)
		}
	}
	// Content column: read+write from the request.
	if ti := byCol["title"]; !ti.InRequest || !ti.InResponse || ti.CreateSource != resourcepolicy.CreateSourceRequest {
		t.Fatalf("title = %+v, want read+write from request", ti)
	}
	// Server-managed: not in request, in response, sourced server-side.
	if tn := byCol["tenant_id"]; tn.InRequest || !tn.InResponse || tn.CreateSource != resourcepolicy.CreateSourceServer {
		t.Fatalf("tenant_id = %+v, want read,server", tn)
	}
	// Hidden.
	if in := byCol["internal"]; in.InRequest || in.InResponse {
		t.Fatalf("internal = %+v, want hidden", in)
	}

	if got, want := resourcepolicy.RequestColumns(resolved), []string{"title"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("request columns = %v, want %v", got, want)
	}
	if got, want := resourcepolicy.ServerColumns(resolved), []string{"tenant_id"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("server columns = %v, want %v", got, want)
	}
}

// A model whose policy contradicts itself fails closed through ResolvedFromModel.
func TestResolvedFromModelRejectsContradiction(t *testing.T) {
	type Bad struct {
		ID    uint   `gorm:"primaryKey"`
		Title string `gorm:"not null" gombit:"read"` // required, read-only, no source
	}
	if _, err := resourcepolicy.ResolvedFromModel(&Bad{}); err == nil {
		t.Fatal("want an error: a required read-only column has no create source")
	}
}

// Association target types for the relationship tests.
type relAuthor struct {
	ID uint `gorm:"primaryKey"`
}
type relProfile struct {
	ID     uint `gorm:"primaryKey"`
	BookID uint
}
type relChapter struct {
	ID     uint `gorm:"primaryKey"`
	BookID uint
}
type relTag struct {
	ID uint `gorm:"primaryKey"`
}

// A gombit policy on a relationship field must fail closed (it lives outside
// DBNames, so it would otherwise be silently discarded). Untagged associations
// are fine and only their FK column (if any) is emitted.
func TestRelationshipPolicyFailsClosed(t *testing.T) {
	t.Run("belongs-to", func(t *testing.T) {
		type Book struct {
			ID       uint `gorm:"primaryKey"`
			AuthorID uint
			Author   relAuthor `gombit:"read"`
		}
		if _, err := resourcepolicy.ResolvedFromModel(&Book{}); err == nil {
			t.Fatal("tagged belongs-to must fail closed")
		}
	})
	t.Run("has-one", func(t *testing.T) {
		type Book struct {
			ID      uint       `gorm:"primaryKey"`
			Profile relProfile `gombit:"read"`
		}
		if _, err := resourcepolicy.ResolvedFromModel(&Book{}); err == nil {
			t.Fatal("tagged has-one must fail closed")
		}
	})
	t.Run("has-many", func(t *testing.T) {
		type Book struct {
			ID       uint         `gorm:"primaryKey"`
			Chapters []relChapter `gombit:"read"`
		}
		if _, err := resourcepolicy.ResolvedFromModel(&Book{}); err == nil {
			t.Fatal("tagged has-many must fail closed")
		}
	})
	t.Run("many2many", func(t *testing.T) {
		type Book struct {
			ID   uint     `gorm:"primaryKey"`
			Tags []relTag `gorm:"many2many:book_tags" gombit:"read"`
		}
		if _, err := resourcepolicy.ResolvedFromModel(&Book{}); err == nil {
			t.Fatal("tagged many2many must fail closed")
		}
	})
	t.Run("untagged association: FK column emitted, association not", func(t *testing.T) {
		type Book struct {
			ID       uint `gorm:"primaryKey"`
			AuthorID uint
			Author   relAuthor // untagged belongs-to
		}
		resolved, err := resourcepolicy.ResolvedFromModel(&Book{})
		if err != nil {
			t.Fatalf("untagged association should resolve: %v", err)
		}
		if _, ok := resolvedByColumn(t, resolved)["author_id"]; !ok {
			t.Fatal("belongs-to FK column author_id should be emitted")
		}
		for _, r := range resolved {
			if r.Column == "" {
				t.Fatalf("no column-less field should be emitted, got %+v", r)
			}
		}
	})
}

// A gombit policy on a GORM-ignored field (`gorm:"-"`) is in neither DBNames nor
// Relations, but it IS in sch.Fields — so it must be validated and fail closed,
// not silently ignored.
func TestIgnoredFieldPolicyFailsClosed(t *testing.T) {
	type m struct {
		ID     uint   `gorm:"primaryKey"`
		Hidden string `gorm:"-" gombit:"write"`
	}
	if _, err := resourcepolicy.ResolvedFromModel(&m{}); err == nil {
		t.Fatal(`a gombit policy on a gorm:"-" ignored field must fail closed`)
	}
}

// A gombit policy on a field shadowed by another field mapped to the same column
// can never be honored (only the effective field is emitted) — fail closed rather
// than drop it.
func TestShadowedColumnPolicyFailsClosed(t *testing.T) {
	type m struct {
		ID uint   `gorm:"primaryKey"`
		A  string `gorm:"column:x"`
		B  string `gorm:"column:x" gombit:"read"`
	}
	if _, err := resourcepolicy.ResolvedFromModel(&m{}); err == nil {
		t.Fatal("a gombit policy on a shadowed duplicate-column field must fail closed")
	}
}

// AuditFields is an embeddable container used by the embedded-policy tests.
type AuditFields struct {
	Note string
}

// A gombit policy on an embedded container is discarded by GORM's flattening
// (it drops the container's non-gorm tags), so it must fail closed — named
// (gorm:"embedded") or anonymous.
func TestEmbeddedContainerPolicyFailsClosed(t *testing.T) {
	t.Run("named gorm:embedded", func(t *testing.T) {
		type m struct {
			ID    uint        `gorm:"primaryKey"`
			Audit AuditFields `gorm:"embedded" gombit:"read"`
		}
		if _, err := resourcepolicy.ResolvedFromModel(&m{}); err == nil {
			t.Fatal("gombit policy on a named embedded container must fail closed")
		}
	})
	t.Run("anonymous embed", func(t *testing.T) {
		type m struct {
			ID          uint `gorm:"primaryKey"`
			AuditFields `gombit:"read"`
		}
		if _, err := resourcepolicy.ResolvedFromModel(&m{}); err == nil {
			t.Fatal("gombit policy on an anonymous embedded container must fail closed")
		}
	})
}

// money is a scalar struct type (Valuer/Scanner) GORM persists as one column.
type money struct{ Cents int64 }

func (money) Value() (driver.Value, error) { return nil, nil }
func (*money) Scan(any) error              { return nil }

// GORM keeps scalar struct types (time.Time, a Valuer/Scanner) as columns even
// when embedded anonymously — they are NOT flattened containers, so a policy on
// them is valid and must not be rejected. The guard follows GORM's parsed result,
// not a type guess.
func TestAnonymousScalarStructIsColumnNotContainer(t *testing.T) {
	t.Run("time.Time", func(t *testing.T) {
		type m struct {
			ID        uint `gorm:"primaryKey"`
			time.Time `gombit:"read"`
		}
		resolved, err := resourcepolicy.ResolvedFromModel(&m{})
		if err != nil {
			t.Fatalf("anonymous time.Time is a column, must not be rejected as a container: %v", err)
		}
		if r, ok := resolvedByColumn(t, resolved)["time"]; !ok || !r.InResponse || r.InRequest {
			t.Fatalf("time should be a read-only column, got %+v ok=%v", r, ok)
		}
	})
	t.Run("Valuer/Scanner", func(t *testing.T) {
		type m struct {
			ID    uint  `gorm:"primaryKey"`
			Price money `gombit:"read"`
		}
		resolved, err := resourcepolicy.ResolvedFromModel(&m{})
		if err != nil {
			t.Fatalf("a Valuer/Scanner struct is a scalar column, must not be rejected as a container: %v", err)
		}
		if r, ok := resolvedByColumn(t, resolved)["price"]; !ok || !r.InResponse {
			t.Fatalf("price should be a read column; got %+v ok=%v", r, ok)
		}
	})
}

// An untagged embedded container is fine: its child columns are emitted normally.
func TestUntaggedEmbeddedContainerIsFine(t *testing.T) {
	type m struct {
		ID    uint        `gorm:"primaryKey"`
		Audit AuditFields `gorm:"embedded"`
	}
	resolved, err := resourcepolicy.ResolvedFromModel(&m{})
	if err != nil {
		t.Fatalf("untagged embedded container should resolve: %v", err)
	}
	if _, ok := resolvedByColumn(t, resolved)["note"]; !ok {
		t.Fatal("embedded child column note should be emitted")
	}
}

// Soft-delete is detected by the gorm.DeletedAt TYPE (via IndirectFieldType), not
// the field name — a renamed column, value or pointer, is still hidden.
func TestFactsFromSchemaSoftDeleteByType(t *testing.T) {
	t.Run("value", func(t *testing.T) {
		type m struct {
			ID        uint `gorm:"primaryKey"`
			RemovedAt gorm.DeletedAt
		}
		f := resourcepolicy.FactsFromSchema(field(t, parse(t, &m{}), "RemovedAt"))
		if !f.SoftDelete {
			t.Fatalf("RemovedAt gorm.DeletedAt should map to SoftDelete=true, got %+v", f)
		}
		if r := resolve(t, f, ""); r.InRequest {
			t.Fatalf("renamed soft-delete must not default onto the request, got %+v", r)
		}
	})
	t.Run("pointer", func(t *testing.T) {
		type m struct {
			ID        uint `gorm:"primaryKey"`
			RemovedAt *gorm.DeletedAt
		}
		f := resourcepolicy.FactsFromSchema(field(t, parse(t, &m{}), "RemovedAt"))
		if !f.SoftDelete {
			t.Fatalf("*gorm.DeletedAt should map to SoftDelete=true, got %+v", f)
		}
	})
}

// FromSchema enumerates GORM's effective persisted columns (DBNames /
// FieldsByDBName): two Go fields on one column resolve once, not twice.
func TestFromSchemaCollapsesDuplicateColumn(t *testing.T) {
	type m struct {
		ID uint   `gorm:"primaryKey"`
		A  string `gorm:"column:x"`
		B  string `gorm:"column:x"`
	}
	resolved, err := resourcepolicy.ResolvedFromModel(&m{})
	if err != nil {
		t.Fatalf("ResolvedFromModel: %v", err)
	}
	xCount := 0
	for _, c := range resourcepolicy.RequestColumns(resolved) {
		if c == "x" {
			xCount++
		}
	}
	if xCount != 1 {
		t.Fatalf("column x resolved %d times, want 1 (effective-field enumeration)", xCount)
	}
}

// FromSchema carries each field's gombit tag through to resolution — distinct
// policies must not collapse to kind defaults.
func TestFromSchemaCarriesPolicyTags(t *testing.T) {
	type m struct {
		ID       uint   `gorm:"primaryKey"`
		Title    string // untagged content → read+write
		TenantID uint   `gorm:"not null" gombit:"read,server"`
		Secret   string `gombit:"-"`
	}
	resolved, err := resourcepolicy.ResolvedFromModel(&m{})
	if err != nil {
		t.Fatalf("ResolvedFromModel: %v", err)
	}
	byCol := resolvedByColumn(t, resolved)
	if s := byCol["secret"]; s.InRequest || s.InResponse {
		t.Fatalf(`gombit:"-" must survive FromSchema; got %+v`, s)
	}
	if tn := byCol["tenant_id"]; tn.InRequest || !tn.InResponse || tn.CreateSource != resourcepolicy.CreateSourceServer {
		t.Fatalf(`gombit:"read,server" must survive FromSchema; got %+v`, tn)
	}
	if ti := byCol["title"]; !ti.InRequest || !ti.InResponse {
		t.Fatalf("untagged Title should be read+write; got %+v", ti)
	}
}

// The facts this resolver consumes match GORM's actual create/read permissions.
func TestSchemaDerivedPermissions(t *testing.T) {
	type permModel struct {
		ID        uint   `gorm:"primaryKey"`
		Computed  string `gorm:"->"`                 // read-only: not creatable
		UpdOnly   string `gorm:"<-:update"`          // writable on update only: not creatable
		WriteOnly string `gorm:"->:false;<-:create"` // create-only: not readable
	}
	sch := parse(t, &permModel{})
	facts := func(goName string) resourcepolicy.FieldFacts {
		return resourcepolicy.FactsFromSchema(field(t, sch, goName))
	}

	computed := facts("Computed")
	if computed.Creatable || !computed.Readable {
		t.Fatalf("Computed facts = %+v, want read-only (Creatable=false, Readable=true)", computed)
	}
	if r := resolve(t, computed, ""); r.InRequest || !r.InResponse {
		t.Fatalf("Computed resolved = %+v, want response-only", r)
	}
	wantErr(t, computed, "write", "`->` column is not creatable")

	if upd := facts("UpdOnly"); upd.Creatable {
		t.Fatalf("UpdOnly facts = %+v, want Creatable=false", upd)
	} else if r := resolve(t, upd, ""); r.InRequest {
		t.Fatalf("UpdOnly resolved = %+v, want not in create request", r)
	}

	writeOnly := facts("WriteOnly")
	if !writeOnly.Creatable || writeOnly.Readable {
		t.Fatalf("WriteOnly facts = %+v, want create-only (Creatable=true, Readable=false)", writeOnly)
	}
	if r := resolve(t, writeOnly, ""); r.InResponse || !r.InRequest {
		t.Fatalf("WriteOnly resolved = %+v, want request-only", r)
	}
	wantErr(t, writeOnly, "read", "`->:false` column is not readable")
}

// assertUnsatisfiable proves resolver ⟺ runtime for a model whose named field can
// never receive a valid create value: the resolver rejects it statically, and a
// real GORM+SQLite create hits the NOT NULL constraint.
func assertUnsatisfiable(t *testing.T, model any, fieldName string, createInstance any) {
	t.Helper()
	if _, err := resourcepolicy.Resolve(resourcepolicy.FactsFromSchema(field(t, parse(t, model), fieldName)), ""); err == nil {
		t.Fatalf("resolver must reject %s (no valid create value)", fieldName)
	}
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "t.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(model); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.Create(createInstance).Error; err == nil {
		t.Fatalf("expected GORM create to fail the NOT NULL constraint for %s", fieldName)
	}
}

// managed() must not waive requiredness: an auto-time field GORM won't write and
// a NOT NULL soft-delete both reach INSERT without a value.
func TestUnsatisfiableRequiredColumnsMatchRuntime(t *testing.T) {
	t.Run("not null, non-creatable (<-:update)", func(t *testing.T) {
		type widget struct {
			ID   uint   `gorm:"primaryKey"`
			Code string `gorm:"not null;<-:update"`
		}
		assertUnsatisfiable(t, &widget{}, "Code", &widget{Code: "x"})
	})
	t.Run("not null auto-time GORM won't write", func(t *testing.T) {
		type widget struct {
			ID        uint      `gorm:"primaryKey"`
			CreatedAt time.Time `gorm:"not null;autoCreateTime;<-:update"`
		}
		assertUnsatisfiable(t, &widget{}, "CreatedAt", &widget{})
	})
	t.Run("not null soft-delete", func(t *testing.T) {
		type widget struct {
			ID        uint           `gorm:"primaryKey"`
			Name      string         `gorm:"not null"`
			DeletedAt gorm.DeletedAt `gorm:"not null"`
		}
		assertUnsatisfiable(t, &widget{}, "DeletedAt", &widget{Name: "x"})
	})
}
