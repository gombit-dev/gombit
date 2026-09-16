package resourcepolicy_test

import (
	"reflect"
	"testing"

	"github.com/gombit-dev/gombit/resourcepolicy"
)

// content is a normal (creatable, readable, nullable, non-key) content column
// unless the caller sets more.
func content(name, col string) resourcepolicy.FieldFacts {
	return resourcepolicy.FieldFacts{GoName: name, Column: col, Creatable: true, Readable: true}
}

func manualKey() resourcepolicy.FieldFacts {
	return resourcepolicy.FieldFacts{GoName: "ID", Column: "id", PrimaryKey: true, NotNull: true, Creatable: true, Readable: true}
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
// included field.
func TestNoColumnCannotBeIncluded(t *testing.T) {
	r := resolve(t, resourcepolicy.FieldFacts{GoName: "Ghost", Creatable: true, Readable: true}, "")
	if r.InRequest || r.InResponse || r.CreateSource != resourcepolicy.CreateSourceNone {
		t.Fatalf("got %+v, want a column-less field excluded from the API", r)
	}
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
	rel := resourcepolicy.FieldFacts{GoName: "Category"}
	r := resolve(t, rel, "")
	if r.InRequest || r.InResponse {
		t.Fatalf("got %+v, want relationship excluded from DTOs", r)
	}
	wantErr(t, rel, "read", "tagging a relationship with API policy")
}

// --- GORM create/read permissions (`->`, `<-`) must be honored ---

// A non-creatable, nullable column (`gorm:"->"`) defaults to response-only.
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
// unsatisfiable: GORM omits it from INSERT and nothing supplies it.
func TestNonCreatableRequiredIsRejected(t *testing.T) {
	f := content("Code", "code")
	f.Creatable = false
	f.NotNull = true // no default, not auto-generated
	wantErr(t, f, "", "non-creatable required column with no default is unsatisfiable")
}

// A non-readable column (`gorm:"->:false;<-:create"`) defaults to request-only.
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

// --- embedded-name collision: policy is keyed by field, not Go name ---

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

// --- query capabilities (slice 4.5) ---

func TestQueryCapabilitiesResolve(t *testing.T) {
	r := resolve(t, content("Genre", "genre"), "read,write,filterable,sortable,searchable")
	if !r.Filterable || !r.Sortable || !r.Searchable {
		t.Fatalf("got %+v, want filterable+sortable+searchable", r)
	}
	if r.Aggregatable {
		t.Fatalf("aggregatable must be off when not declared: %+v", r)
	}
	if !r.InResponse || !r.InRequest {
		t.Fatalf("read,write should keep the field in both surfaces: %+v", r)
	}
}

func TestAggregatableResolves(t *testing.T) {
	r := resolve(t, content("Price", "price"), "read,aggregatable")
	if !r.Aggregatable || !r.InResponse {
		t.Fatalf("got %+v, want aggregatable + response-visible", r)
	}
}

// Query capabilities are opt-in: an untagged field has none.
func TestUntaggedHasNoQuerySurface(t *testing.T) {
	r := resolve(t, content("Title", "title"), "")
	if r.Filterable || r.Sortable || r.Searchable || r.Aggregatable {
		t.Fatalf("untagged field must have no query surface: %+v", r)
	}
}

// A queryable field must be response-visible: a hidden or write-only column with
// a query surface leaks through membership/counts/ordering and must fail closed.
func TestQueryableMustBeResponseVisible(t *testing.T) {
	// Hidden + searchable.
	wantErr(t, content("Secret", "secret"), "-,searchable", "hidden field cannot be searchable")
	// Write-only (not in response) + filterable.
	wantErr(t, content("Password", "password"), "write,filterable", "write-only field cannot be filterable")
	// server (response-visible only if read) + sortable, without read.
	f := content("TenantID", "tenant_id")
	f.NotNull = true
	wantErr(t, f, "server,sortable", "server-only (not readable) field cannot be sortable")
}

func TestUnknownQueryTokenStillRejected(t *testing.T) {
	wantErr(t, content("X", "x"), "read,groupable", "groupable is not a known token")
}
