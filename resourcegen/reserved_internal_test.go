package resourcegen

import (
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/types"
	"gorm.io/gorm"
)

// TestPlannedOperationIDsCoverBothStorageKinds pins the planner side of the
// operation-ID invariant to the storage kinds the handler registers uploads for:
// `file` and `image` are both storage-backed, so both must appear in the planned
// set (the framework's reserved IDs are checked against exactly this set).
func TestPlannedOperationIDsCoverBothStorageKinds(t *testing.T) {
	fields, err := parseFields([]string{"attachment:file:required", "cover:image"}, "document")
	if err != nil {
		t.Fatalf("parseFields: %v", err)
	}
	name, err := parseResourceName("Document")
	if err != nil {
		t.Fatalf("parseResourceName: %v", err)
	}

	ids := resourceOperationIDs(name, fields)
	for _, want := range []string{"upload-document-attachment", "upload-document-cover"} {
		if !containsString(ids, want) {
			t.Fatalf("resourceOperationIDs() = %v, want it to include %q", ids, want)
		}
	}
}

// storageKindsModel carries both storage-backed column kinds, so the test can
// compare the IDs the generator actually mounts with the IDs the planner reserves.
type storageKindsModel struct {
	gorm.Model
	Title      string      `gorm:"not null"`
	Attachment types.File  `gorm:"not null"`
	Cover      *types.Image
}

// TestGeneratedHandlerRegistersUploadForBothStorageKinds is the generator side of
// the same invariant: a resource with a file and an image column registers an
// upload operation for each, which is what makes the planned set the right one to
// check against reservedOperationIDs.
func TestGeneratedHandlerRegistersUploadForBothStorageKinds(t *testing.T) {
	res, err := buildModelResource(&storageKindsModel{}, "storagekinds")
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	src, err := renderModelHandler(res)
	if err != nil {
		t.Fatalf("renderModelHandler: %v", err)
	}
	assertParses(t, src)

	for _, want := range []string{`"upload-storage_kinds_model-attachment"`, `"upload-storage_kinds_model-cover"`} {
		if !strings.Contains(src, want) {
			t.Fatalf("generated handler is missing operation ID %s:\n%s", want, src)
		}
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
