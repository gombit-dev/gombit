package resourcepolicy_test

import (
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

// factsOf maps a parsed GORM field to FieldFacts — the mapping slice 2 will use
// to feed real schema facts into the resolver.
func factsOf(t *testing.T, sch *schema.Schema, goName string) resourcepolicy.FieldFacts {
	t.Helper()
	f := sch.FieldsByName[goName]
	if f == nil {
		t.Fatalf("field %q not found in schema", goName)
	}
	return resourcepolicy.FieldFacts{
		GoName:        f.Name,
		Column:        f.DBName,
		DBBacked:      f.DBName != "",
		PrimaryKey:    f.PrimaryKey,
		AutoIncrement: f.AutoIncrement,
		AutoTime:      f.AutoCreateTime != 0 || f.AutoUpdateTime != 0,
		SoftDelete:    f.Name == "DeletedAt",
		NotNull:       f.NotNull,
		HasDefault:    f.HasDefaultValue,
		DefaultValue:  f.DefaultValue,
		Creatable:     f.Creatable,
		Readable:      f.Readable,
	}
}

// content is a normal (creatable, readable, nullable, non-key) content column
// unless the caller sets more.
func content(name, col string) resourcepolicy.FieldFacts {
	return resourcepolicy.FieldFacts{GoName: name, Column: col, DBBacked: true, Creatable: true, Readable: true}
}

func resolve(t *testing.T, f resourcepolicy.FieldFacts, tag string) resourcepolicy.Resolved {
	t.Helper()
	r, err := resourcepolicy.Resolve(f, tag)
	if err != nil {
		t.Fatalf("Resolve(%q, %q): %v", f.GoName, tag, err)
	}
	return r
}

func wantErr(t *testing.T, f resourcepolicy.FieldFacts, tag, why string) {
	t.Helper()
	if _, err := resourcepolicy.Resolve(f, tag); err == nil {
		t.Fatalf("Resolve(%q, %q): want error (%s)", f.GoName, tag, why)
	}
}

func TestDefaultContentColumnIsReadWrite(t *testing.T) {
	r := resolve(t, content("Title", "title"), "")
	if !r.InRequest || !r.InResponse || r.CreateSource != resourcepolicy.CreateSourceRequest {
		t.Fatalf("got %+v, want in request+response, create source request", r)
	}
}

func TestHiddenTag(t *testing.T) {
	r := resolve(t, content("Note", "note"), "-")
	if r.InRequest || r.InResponse || r.CreateSource != resourcepolicy.CreateSourceNone {
		t.Fatalf("got %+v, want fully hidden", r)
	}
}

func TestServerManagedTag(t *testing.T) {
	f := content("TenantID", "tenant_id")
	f.NotNull = true
	r := resolve(t, f, "read,server")
	if r.InRequest || !r.InResponse || r.CreateSource != resourcepolicy.CreateSourceServer {
		t.Fatalf("got %+v, want response-only, source server", r)
	}
}

// `-,server`: hidden from the API but set by a hook — the two are orthogonal.
func TestHiddenServerManaged(t *testing.T) {
	f := content("SecretRef", "secret_ref")
	f.NotNull = true
	r := resolve(t, f, "-,server")
	if r.InRequest || r.InResponse {
		t.Fatalf("got %+v, want hidden from the API", r)
	}
	if r.CreateSource != resourcepolicy.CreateSourceServer {
		t.Fatalf("create source = %s, want server", r.CreateSource)
	}
}

func TestReadOnlyNullableIsAllowed(t *testing.T) {
	r := resolve(t, content("Slug", "slug"), "read") // nullable, so no create source needed
	if r.InRequest || !r.InResponse || r.CreateSource != resourcepolicy.CreateSourceNone {
		t.Fatalf("got %+v, want response-only", r)
	}
}

func TestWriteServerConflict(t *testing.T) {
	wantErr(t, content("X", "x"), "write,server", "two sources")
}

func TestHiddenReadConflict(t *testing.T) { wantErr(t, content("X", "x"), "read,-", "- with read") }

func TestUnknownToken(t *testing.T) { wantErr(t, content("X", "x"), "reed", "unknown token") }

// --- required-column create-source validation (#352) ---

func TestRequiredReadOnlyIsRejected(t *testing.T) {
	f := content("Title", "title")
	f.NotNull = true
	wantErr(t, f, "read", "required column with no create source")
}

func TestRequiredHiddenIsRejected(t *testing.T) {
	f := content("Title", "title")
	f.NotNull = true
	wantErr(t, f, "-", "required column hidden with no source")
}

func TestRequiredWithDefaultIsAllowed(t *testing.T) {
	f := content("Status", "status")
	f.NotNull = true
	f.HasDefault = true
	f.DefaultValue = "'pending'"
	r := resolve(t, f, "read")
	if r.CreateSource != resourcepolicy.CreateSourceNone || !r.InResponse {
		t.Fatalf("got %+v, want response-only, no create source (DB default)", r)
	}
}

// A `default:null` clause does not satisfy NOT NULL, so it must not exempt a
// required, non-source column from the create-value check.
func TestRequiredWithNullDefaultIsRejected(t *testing.T) {
	f := content("Status", "status")
	f.NotNull = true
	f.HasDefault = true
	f.DefaultValue = "null"
	wantErr(t, f, "read", "a NULL default does not satisfy NOT NULL")
}

// --- primary keys: manual vs auto-generated ---

func manualKey() resourcepolicy.FieldFacts {
	return resourcepolicy.FieldFacts{GoName: "ID", Column: "id", DBBacked: true, PrimaryKey: true, NotNull: true, Creatable: true, Readable: true}
}

func TestManualKeyDefaultsWritable(t *testing.T) {
	r := resolve(t, manualKey(), "")
	if !r.InRequest || r.CreateSource != resourcepolicy.CreateSourceRequest {
		t.Fatalf("got %+v, want writable manual key sourced from request", r)
	}
}

func TestManualKeyServerManaged(t *testing.T) {
	r := resolve(t, manualKey(), "read,server")
	if r.InRequest || r.CreateSource != resourcepolicy.CreateSourceServer {
		t.Fatalf("got %+v, want server-sourced manual key", r)
	}
}

func TestAutoIncrementKeyIsReadOnly(t *testing.T) {
	pk := manualKey()
	pk.AutoIncrement = true
	r := resolve(t, pk, "")
	if r.InRequest || !r.InResponse {
		t.Fatalf("got %+v, want response-only auto key", r)
	}
	wantErr(t, pk, "write", "cannot write an auto-increment key")
}

func TestTimestampsDefaultReadOnly(t *testing.T) {
	f := content("CreatedAt", "created_at")
	f.AutoTime = true
	r := resolve(t, f, "")
	if r.InRequest || !r.InResponse {
		t.Fatalf("got %+v, want response-only timestamp", r)
	}
}

func TestSoftDeleteDefaultsHidden(t *testing.T) {
	f := content("DeletedAt", "deleted_at")
	f.SoftDelete = true
	r := resolve(t, f, "")
	if r.InRequest || r.InResponse {
		t.Fatalf("got %+v, want hidden soft-delete", r)
	}
}

func TestRelationshipHasNoPolicy(t *testing.T) {
	rel := resourcepolicy.FieldFacts{GoName: "Category", DBBacked: false}
	r := resolve(t, rel, "")
	if r.InRequest || r.InResponse {
		t.Fatalf("got %+v, want relationship excluded from DTOs", r)
	}
	wantErr(t, rel, "read", "tagging a relationship with API policy")
}

// --- GORM create/read permissions (`->`, `<-`) must be honored ---

// A non-creatable, nullable column (`gorm:"->"`) defaults to response-only: GORM
// omits it from INSERT, and being nullable it needs no create value.
func TestNonCreatableNullableDefaultsResponseOnly(t *testing.T) {
	f := content("Computed", "computed")
	f.Creatable = false // read-only/computed; nullable, so create need not supply it
	r := resolve(t, f, "")
	if r.InRequest || r.CreateSource != resourcepolicy.CreateSourceNone {
		t.Fatalf("got %+v, want no request/create source (non-creatable)", r)
	}
	if !r.InResponse {
		t.Fatalf("got %+v, want response (readable)", r)
	}
	wantErr(t, f, "write", "cannot write a non-creatable column")
	wantErr(t, f, "server", "server value would be discarded on a non-creatable column")
}

// A NOT NULL non-creatable column with no default/auto-generation is
// unsatisfiable: GORM omits it from INSERT and nothing supplies it, so every
// create hits the NOT NULL constraint. Reject the model statically.
func TestNonCreatableRequiredIsRejected(t *testing.T) {
	f := content("Code", "code")
	f.Creatable = false
	f.NotNull = true // no default, not auto-generated
	wantErr(t, f, "", "non-creatable required column with no default is unsatisfiable")
}

// A non-readable column (`gorm:"->:false;<-:create"`) defaults to request-only:
// GORM skips it on SELECT, so it cannot be surfaced in the response.
func TestNonReadableDefaultsRequestOnly(t *testing.T) {
	f := content("Secret", "secret")
	f.Readable = false
	r := resolve(t, f, "")
	if r.InResponse {
		t.Fatalf("got %+v, want not in response (non-readable)", r)
	}
	if !r.InRequest || r.CreateSource != resourcepolicy.CreateSourceRequest {
		t.Fatalf("got %+v, want request-only", r)
	}
	wantErr(t, f, "read", "cannot read a non-readable column")
}

// Built from a real gorm/schema.Parse so the facts this resolver consumes match
// GORM's actual permission semantics (what slices 2–4 will feed in).
func TestSchemaDerivedPermissions(t *testing.T) {
	type permModel struct {
		ID        uint   `gorm:"primaryKey"`
		Computed  string `gorm:"->"`                 // read-only: not creatable
		UpdOnly   string `gorm:"<-:update"`          // writable on update only: not creatable
		WriteOnly string `gorm:"->:false;<-:create"` // create-only: not readable
	}
	sch, err := schema.Parse(&permModel{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatalf("schema.Parse: %v", err)
	}
	facts := func(goName string) resourcepolicy.FieldFacts { return factsOf(t, sch, goName) }

	computed := facts("Computed")
	if computed.Creatable || !computed.Readable {
		t.Fatalf("Computed facts = %+v, want read-only (Creatable=false, Readable=true)", computed)
	}
	if r := resolve(t, computed, ""); r.InRequest || !r.InResponse {
		t.Fatalf("Computed resolved = %+v, want response-only", r)
	}
	wantErr(t, computed, "write", "`->` column is not creatable")

	upd := facts("UpdOnly")
	if upd.Creatable {
		t.Fatalf("UpdOnly facts = %+v, want Creatable=false", upd)
	}
	if r := resolve(t, upd, ""); r.InRequest {
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

// assertUnsatisfiable proves resolver ⟺ runtime for a model whose named field
// can never receive a valid create value: the resolver rejects it statically, and
// a real GORM+SQLite create hits the NOT NULL constraint.
func assertUnsatisfiable(t *testing.T, model any, fieldName string, createInstance any) {
	t.Helper()
	sch, err := schema.Parse(model, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatalf("schema.Parse: %v", err)
	}
	if _, err := resourcepolicy.Resolve(factsOf(t, sch, fieldName), ""); err == nil {
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

// Columns that can never get a valid value on create must be rejected — and the
// resolver's rejection must match what GORM+SQLite actually do. managed() must
// not waive requiredness: an auto-time field GORM won't write, and a NOT NULL
// soft-delete, both reach INSERT without a value.
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
	t.Run("not null with a NULL default, non-creatable", func(t *testing.T) {
		type widget struct {
			ID   uint   `gorm:"primaryKey"`
			Code string `gorm:"not null;default:null;<-:update"`
		}
		assertUnsatisfiable(t, &widget{}, "Code", &widget{Code: "x"})
	})
}

// --- embedded-name collision: GORM flattens two fields to the same Go name ---

func TestResolveAllPreservesEmbeddedNameCollision(t *testing.T) {
	fields := []resourcepolicy.Field{
		{FieldFacts: content("Code", "public_code"), Tag: "read"},
		{FieldFacts: content("Code", "secret_code"), Tag: "-"},
	}
	resolved, err := resourcepolicy.ResolveAll(fields)
	if err != nil {
		t.Fatalf("ResolveAll: %v", err)
	}
	if !resolved[0].InResponse || resolved[1].InResponse {
		t.Fatalf("collision mishandled: public=%+v secret=%+v", resolved[0], resolved[1])
	}
	if got, want := resourcepolicy.ResponseColumns(resolved), []string{"public_code"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("response columns = %v, want %v (secret_code must stay hidden)", got, want)
	}
}

func TestResolveAllColumnSurfaces(t *testing.T) {
	autoID := manualKey()
	autoID.AutoIncrement = true
	tenant := content("TenantID", "tenant_id")
	tenant.NotNull = true
	fields := []resourcepolicy.Field{
		{FieldFacts: autoID},
		{FieldFacts: content("Title", "title"), Tag: ""},
		{FieldFacts: tenant, Tag: "read,server"},
		{FieldFacts: content("Internal", "internal"), Tag: "-"},
		{FieldFacts: resourcepolicy.FieldFacts{GoName: "Category", DBBacked: false}},
	}
	resolved, err := resourcepolicy.ResolveAll(fields)
	if err != nil {
		t.Fatalf("ResolveAll: %v", err)
	}
	if got, want := resourcepolicy.RequestColumns(resolved), []string{"title"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("request columns = %v, want %v", got, want)
	}
	if got, want := resourcepolicy.ResponseColumns(resolved), []string{"id", "tenant_id", "title"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("response columns = %v, want %v", got, want)
	}
	if got, want := resourcepolicy.ServerColumns(resolved), []string{"tenant_id"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("server columns = %v, want %v", got, want)
	}
}
