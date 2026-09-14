package resourcepolicy_test

import (
	"reflect"
	"testing"

	"github.com/gombit-dev/gombit/resourcepolicy"
)

// content is a nullable, non-key content column unless the caller sets more.
func content(name, col string) resourcepolicy.FieldFacts {
	return resourcepolicy.FieldFacts{GoName: name, Column: col, DBBacked: true}
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

// A required (NOT NULL, no default) column made read-only has no create source
// and would be silently zero-filled — rejected.
func TestRequiredReadOnlyIsRejected(t *testing.T) {
	f := content("Title", "title")
	f.NotNull = true
	wantErr(t, f, "read", "required column with no create source")
}

// A required column hidden with no server source is likewise rejected.
func TestRequiredHiddenIsRejected(t *testing.T) {
	f := content("Title", "title")
	f.NotNull = true
	wantErr(t, f, "-", "required column hidden with no source")
}

// A NOT NULL column WITH a database default may be omitted from create.
func TestRequiredWithDefaultIsAllowed(t *testing.T) {
	f := content("Status", "status")
	f.NotNull = true
	f.HasDefault = true
	r := resolve(t, f, "read")
	if r.CreateSource != resourcepolicy.CreateSourceNone || !r.InResponse {
		t.Fatalf("got %+v, want response-only, no create source (DB default)", r)
	}
}

// --- primary keys: manual vs auto-generated ---

// A manual (non-auto) primary key needs a create value; the client may supply it.
func TestManualKeyDefaultsWritable(t *testing.T) {
	pk := resourcepolicy.FieldFacts{GoName: "ID", Column: "id", DBBacked: true, PrimaryKey: true, NotNull: true}
	r := resolve(t, pk, "")
	if !r.InRequest || r.CreateSource != resourcepolicy.CreateSourceRequest {
		t.Fatalf("got %+v, want writable manual key sourced from request", r)
	}
}

// A manual key may instead be set server-side (e.g. a UUID minted in a hook).
func TestManualKeyServerManaged(t *testing.T) {
	pk := resourcepolicy.FieldFacts{GoName: "ID", Column: "id", DBBacked: true, PrimaryKey: true, NotNull: true}
	r := resolve(t, pk, "read,server")
	if r.InRequest || r.CreateSource != resourcepolicy.CreateSourceServer {
		t.Fatalf("got %+v, want server-sourced manual key", r)
	}
}

// An auto-increment key is DB-generated: read-only, never writable.
func TestAutoIncrementKeyIsReadOnly(t *testing.T) {
	pk := resourcepolicy.FieldFacts{GoName: "ID", Column: "id", DBBacked: true, PrimaryKey: true, AutoIncrement: true, NotNull: true}
	r := resolve(t, pk, "")
	if r.InRequest || !r.InResponse {
		t.Fatalf("got %+v, want response-only auto key", r)
	}
	wantErr(t, pk, "write", "cannot write an auto-increment key")
}

func TestTimestampsDefaultReadOnly(t *testing.T) {
	r := resolve(t, resourcepolicy.FieldFacts{GoName: "CreatedAt", Column: "created_at", DBBacked: true, AutoTime: true}, "")
	if r.InRequest || !r.InResponse {
		t.Fatalf("got %+v, want response-only timestamp", r)
	}
}

func TestSoftDeleteDefaultsHidden(t *testing.T) {
	r := resolve(t, resourcepolicy.FieldFacts{GoName: "DeletedAt", Column: "deleted_at", DBBacked: true, SoftDelete: true}, "")
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

// --- embedded-name collision: GORM flattens two fields to the same Go name ---

// ResolveAll must key policy by each field's own facts+tag, not by Go name, so
// two flattened "Code" fields with different columns and tags stay distinct.
func TestResolveAllPreservesEmbeddedNameCollision(t *testing.T) {
	fields := []resourcepolicy.Field{
		{FieldFacts: resourcepolicy.FieldFacts{GoName: "Code", Column: "public_code", DBBacked: true}, Tag: "read"},
		{FieldFacts: resourcepolicy.FieldFacts{GoName: "Code", Column: "secret_code", DBBacked: true}, Tag: "-"},
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
	fields := []resourcepolicy.Field{
		{FieldFacts: resourcepolicy.FieldFacts{GoName: "ID", Column: "id", DBBacked: true, PrimaryKey: true, AutoIncrement: true, NotNull: true}},
		{FieldFacts: content("Title", "title"), Tag: ""},
		{FieldFacts: func() resourcepolicy.FieldFacts { f := content("TenantID", "tenant_id"); f.NotNull = true; return f }(), Tag: "read,server"},
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
