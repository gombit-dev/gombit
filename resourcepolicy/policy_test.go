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

// factsOf maps a parsed GORM *schema.Field to FieldFacts — a lossless,
// name-independent projection. Soft-delete is detected by the gorm.DeletedAt TYPE
// via IndirectFieldType (GORM dispatches its clauses on reflect.New of the
// indirect type), so both `gorm.DeletedAt` and `*gorm.DeletedAt` are recognized.
//
// Slice 2 should enumerate GORM's EFFECTIVE persisted columns — iterate
// sch.DBNames and take sch.FieldsByDBName[name] (see canonicalFields) — NOT raw
// sch.Fields, which retains duplicate-column/shadowed fields GORM does not
// persist. Relationships (no column) are handled separately via sch.Relationships.
func factsOf(f *schema.Field) resourcepolicy.FieldFacts {
	return resourcepolicy.FieldFacts{
		GoName:        f.Name,
		Column:        f.DBName,
		PrimaryKey:    f.PrimaryKey,
		AutoIncrement: f.AutoIncrement,
		AutoTime:      f.AutoCreateTime != 0 || f.AutoUpdateTime != 0,
		SoftDelete:    f.IndirectFieldType == reflect.TypeOf(gorm.DeletedAt{}),
		NotNull:       f.NotNull,
		HasDefault:    f.HasDefaultValue,
		Creatable:     f.Creatable,
		Readable:      f.Readable,
	}
}

// canonicalFields maps a schema to resolver input the way slice 2 should: over
// GORM's effective persisted columns (ordered sch.DBNames → sch.FieldsByDBName),
// so a duplicate-column or shadowed field is resolved once, not twice — and it
// carries each selected field's own `gombit` tag through to the resolver (the
// policy must not vanish between selection and resolution).
func canonicalFields(sch *schema.Schema) []resourcepolicy.Field {
	out := make([]resourcepolicy.Field, 0, len(sch.DBNames))
	for _, name := range sch.DBNames {
		f := sch.FieldsByDBName[name]
		out = append(out, resourcepolicy.Field{FieldFacts: factsOf(f), Tag: f.Tag.Get("gombit")})
	}
	return out
}

// field selects a parsed field by Go name for a collision-free test model. The
// mapping (factsOf) never keys by name; enumeration uses canonicalFields.
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

// content is a normal (creatable, readable, nullable, non-key) content column
// unless the caller sets more.
func content(name, col string) resourcepolicy.FieldFacts {
	return resourcepolicy.FieldFacts{GoName: name, Column: col, Creatable: true, Readable: true}
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

// read/write/server are independent directions. `write` alone (a password) is
// request-only, NOT in the response — an `InResponse = tag.read || tag.write`
// implementation would fail this.
func TestWriteOnlyTag(t *testing.T) {
	r := resolve(t, content("Password", "password"), "write")
	if !r.InRequest || r.InResponse {
		t.Fatalf("got %+v, want request-only (write-only field)", r)
	}
	if r.CreateSource != resourcepolicy.CreateSourceRequest {
		t.Fatalf("create source = %s, want request", r.CreateSource)
	}
}

// Bare `server` (hook-set, not returned) is neither in the request nor response.
func TestBareServerTag(t *testing.T) {
	f := content("AuditToken", "audit_token")
	f.NotNull = true
	r := resolve(t, f, "server")
	if r.InRequest || r.InResponse {
		t.Fatalf("got %+v, want not in request or response", r)
	}
	if r.CreateSource != resourcepolicy.CreateSourceServer {
		t.Fatalf("create source = %s, want server", r.CreateSource)
	}
}

func TestWriteServerConflict(t *testing.T) {
	wantErr(t, content("X", "x"), "write,server", "two sources")
}

func TestHiddenReadConflict(t *testing.T) { wantErr(t, content("X", "x"), "read,-", "- with read") }

func TestUnknownToken(t *testing.T) { wantErr(t, content("X", "x"), "reed", "unknown token") }

// A stray comma must not silently become an alternate spelling: every empty token
// (leading, interior, trailing, or a lone ",") is rejected.
func TestEmptyTokensRejected(t *testing.T) {
	for _, tag := range []string{",", "read,,write", ",read", "read,", " , "} {
		wantErr(t, content("Note", "note"), tag, "empty token in gombit tag "+tag)
	}
}

// A field with no column is a relationship; it can never be resolved as an
// included (request/response) field, so the column helpers never silently drop an
// included field. This closes the DBBacked-vs-Column malformed state.
func TestNoColumnCannotBeIncluded(t *testing.T) {
	r := resolve(t, resourcepolicy.FieldFacts{GoName: "Ghost", Creatable: true, Readable: true}, "")
	if r.InRequest || r.InResponse || r.CreateSource != resourcepolicy.CreateSourceNone {
		t.Fatalf("got %+v, want a column-less field excluded from the API", r)
	}
	// And every included field the resolver produces has a column.
	resolved, err := resourcepolicy.ResolveAll([]resourcepolicy.Field{
		{FieldFacts: content("Title", "title")},
		{FieldFacts: resourcepolicy.FieldFacts{GoName: "Ghost", Creatable: true, Readable: true}},
	})
	if err != nil {
		t.Fatalf("ResolveAll: %v", err)
	}
	for _, rf := range resolved {
		if (rf.InRequest || rf.InResponse) && rf.Column == "" {
			t.Fatalf("included field %q has no column", rf.GoName)
		}
	}
}

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

// A `default:` clause defers to the database: this layer does not require an API
// source for it (and does not evaluate whether the default's SQL is non-null —
// the DB enforces NOT NULL and fails loudly if it isn't).
func TestRequiredWithDefaultDefersToDatabase(t *testing.T) {
	f := content("Status", "status")
	f.NotNull = true
	f.HasDefault = true
	r := resolve(t, f, "read")
	if r.CreateSource != resourcepolicy.CreateSourceNone || !r.InResponse {
		t.Fatalf("got %+v, want response-only, no create source (DB default)", r)
	}
}

// --- primary keys: manual vs auto-generated ---

func manualKey() resourcepolicy.FieldFacts {
	return resourcepolicy.FieldFacts{GoName: "ID", Column: "id", PrimaryKey: true, NotNull: true, Creatable: true, Readable: true}
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

// Soft-delete is detected by the gorm.DeletedAt TYPE, not the field name, so a
// renamed soft-delete column is still hidden (not defaulted onto the request).
func TestSoftDeleteDetectedByType(t *testing.T) {
	type m struct {
		ID        uint `gorm:"primaryKey"`
		RemovedAt gorm.DeletedAt
	}
	sch, err := schema.Parse(&m{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatalf("schema.Parse: %v", err)
	}
	f := factsOf(field(t, sch, "RemovedAt"))
	if !f.SoftDelete {
		t.Fatalf("RemovedAt gorm.DeletedAt should map to SoftDelete=true, got %+v", f)
	}
	if r := resolve(t, f, ""); r.InRequest {
		t.Fatalf("renamed soft-delete column must not default onto the create request, got %+v", r)
	}
}

// A pointer soft-delete field is dispatched by GORM via the indirect type, so it
// must also be detected (FieldType is *gorm.DeletedAt; IndirectFieldType is not).
func TestPointerSoftDeleteDetected(t *testing.T) {
	type m struct {
		ID        uint `gorm:"primaryKey"`
		RemovedAt *gorm.DeletedAt
	}
	sch, err := schema.Parse(&m{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatalf("schema.Parse: %v", err)
	}
	f := factsOf(field(t, sch, "RemovedAt"))
	if !f.SoftDelete {
		t.Fatalf("*gorm.DeletedAt should map to SoftDelete=true, got %+v", f)
	}
	if r := resolve(t, f, ""); r.InRequest {
		t.Fatalf("pointer soft-delete column must not default onto the create request, got %+v", r)
	}
}

// Two Go fields mapped to the same column appear twice in sch.Fields but once in
// GORM's effective set (DBNames/FieldsByDBName). canonicalFields must resolve the
// column once — proving slice 2 should enumerate the effective set, not sch.Fields.
func TestCanonicalFieldsCollapseDuplicateColumn(t *testing.T) {
	type m struct {
		ID uint   `gorm:"primaryKey"`
		A  string `gorm:"column:x"`
		B  string `gorm:"column:x"`
	}
	sch, err := schema.Parse(&m{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatalf("schema.Parse: %v", err)
	}
	resolved, err := resourcepolicy.ResolveAll(canonicalFields(sch))
	if err != nil {
		t.Fatalf("ResolveAll: %v", err)
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

// The adapter must carry each field's gombit tag through to the resolver:
// distinct policies (hidden, server-managed) must survive canonicalFields, not
// collapse to kind defaults.
func TestCanonicalFieldsCarryPolicyTags(t *testing.T) {
	type m struct {
		ID       uint   `gorm:"primaryKey"`
		Title    string // untagged content → read+write
		TenantID uint   `gorm:"not null" gombit:"read,server"`
		Secret   string `gombit:"-"`
	}
	sch, err := schema.Parse(&m{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatalf("schema.Parse: %v", err)
	}
	resolved, err := resourcepolicy.ResolveAll(canonicalFields(sch))
	if err != nil {
		t.Fatalf("ResolveAll: %v", err)
	}
	byCol := map[string]resourcepolicy.Resolved{}
	for _, r := range resolved {
		byCol[r.Column] = r
	}
	if s := byCol["secret"]; s.InRequest || s.InResponse {
		t.Fatalf(`gombit:"-" must survive the adapter; got %+v`, s)
	}
	if tn := byCol["tenant_id"]; tn.InRequest || !tn.InResponse || tn.CreateSource != resourcepolicy.CreateSourceServer {
		t.Fatalf(`gombit:"read,server" must survive the adapter; got %+v`, tn)
	}
	if ti := byCol["title"]; !ti.InRequest || !ti.InResponse {
		t.Fatalf("untagged Title should be read+write; got %+v", ti)
	}
}

func TestRelationshipHasNoPolicy(t *testing.T) {
	rel := resourcepolicy.FieldFacts{GoName: "Category"}
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
	facts := func(goName string) resourcepolicy.FieldFacts { return factsOf(field(t, sch, goName)) }

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
	if _, err := resourcepolicy.Resolve(factsOf(field(t, sch, fieldName)), ""); err == nil {
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
		{FieldFacts: resourcepolicy.FieldFacts{GoName: "Category"}},
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
