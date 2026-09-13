package resourcepolicy_test

import (
	"reflect"
	"testing"

	"github.com/gombit-dev/gombit/resourcepolicy"
)

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

// A regular content column with no tag is read+write from the request.
func TestDefaultContentColumnIsReadWrite(t *testing.T) {
	r := resolve(t, content("Title", "title"), "")
	if !r.InRequest || !r.InResponse || r.CreateSource != resourcepolicy.CreateSourceRequest {
		t.Fatalf("got %+v, want in request+response, create source request", r)
	}
}

func TestHiddenTag(t *testing.T) {
	r := resolve(t, content("Internal", "internal"), "-")
	if r.InRequest || r.InResponse || r.CreateSource != resourcepolicy.CreateSourceNone {
		t.Fatalf("got %+v, want fully hidden", r)
	}
}

// `server` means set server-side: not in the request, create source server,
// still surfaced when `read` is asked.
func TestServerManagedTag(t *testing.T) {
	r := resolve(t, content("TenantID", "tenant_id"), "read,server")
	if r.InRequest {
		t.Fatal("server-managed field must not be in the request")
	}
	if !r.InResponse {
		t.Fatal("read,server should be in the response")
	}
	if r.CreateSource != resourcepolicy.CreateSourceServer {
		t.Fatalf("create source = %s, want server", r.CreateSource)
	}
}

// An explicit tag is authoritative about surfaces: read-only, no write.
func TestReadOnlyTag(t *testing.T) {
	r := resolve(t, content("Slug", "slug"), "read")
	if r.InRequest || !r.InResponse {
		t.Fatalf("got %+v, want response only", r)
	}
	if r.CreateSource != resourcepolicy.CreateSourceNone {
		t.Fatalf("create source = %s, want none (not writable, not server)", r.CreateSource)
	}
}

func TestWriteServerConflict(t *testing.T) {
	if _, err := resourcepolicy.Resolve(content("X", "x"), "write,server"); err == nil {
		t.Fatal("want an error for write+server")
	}
}

func TestHiddenWithOtherTokensConflict(t *testing.T) {
	if _, err := resourcepolicy.Resolve(content("X", "x"), "read,-"); err == nil {
		t.Fatal("want an error for combining - with read")
	}
}

func TestUnknownToken(t *testing.T) {
	if _, err := resourcepolicy.Resolve(content("X", "x"), "reed"); err == nil {
		t.Fatal("want an error for an unknown token")
	}
}

// Auto-managed columns are never writable.
func TestPrimaryKeyDefaultsReadOnly(t *testing.T) {
	r := resolve(t, resourcepolicy.FieldFacts{GoName: "ID", Column: "id", DBBacked: true, PrimaryKey: true}, "")
	if r.InRequest || !r.InResponse {
		t.Fatalf("got %+v, want response-only PK", r)
	}
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

func TestWritingAutoManagedIsError(t *testing.T) {
	pk := resourcepolicy.FieldFacts{GoName: "ID", Column: "id", DBBacked: true, PrimaryKey: true}
	if _, err := resourcepolicy.Resolve(pk, "write"); err == nil {
		t.Fatal("want an error writing an auto-managed column")
	}
}

// A relationship field is not a scalar wire column and cannot carry policy.
func TestRelationshipHasNoPolicy(t *testing.T) {
	rel := resourcepolicy.FieldFacts{GoName: "Category", DBBacked: false}
	r := resolve(t, rel, "")
	if r.InRequest || r.InResponse {
		t.Fatalf("got %+v, want relationship excluded from DTOs", r)
	}
	if _, err := resourcepolicy.Resolve(rel, "read"); err == nil {
		t.Fatal("want an error tagging a relationship with API policy")
	}
}

func TestResolveAllAndColumnSurfaces(t *testing.T) {
	fields := []resourcepolicy.FieldFacts{
		{GoName: "ID", Column: "id", DBBacked: true, PrimaryKey: true},
		{GoName: "Title", Column: "title", DBBacked: true},
		{GoName: "TenantID", Column: "tenant_id", DBBacked: true},
		{GoName: "Internal", Column: "internal", DBBacked: true},
		{GoName: "Category", DBBacked: false},
	}
	tags := map[string]string{
		"TenantID": "read,server",
		"Internal": "-",
	}
	resolved, err := resourcepolicy.ResolveAll(fields, tags)
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
