package upgrade

import (
	"strings"
	"testing"
)

// TestActionWithoutApplyIsNotImplemented: an action registered without an
// implementation is not one automatic changes may name.
func TestActionWithoutApplyIsNotImplemented(t *testing.T) {
	actions["described-only"] = Action{Description: "says what it would do"}
	defer delete(actions, "described-only")
	if _, ok := LookupAction("described-only"); ok {
		t.Fatal("LookupAction found an action with no Apply")
	}
	data := "format: 1\nreleases:\n  - version: v0.1.0\n  - version: v0.2.0\n    changes:\n      - {id: x, kind: automatic, action: described-only, area: api, summary: s}\n"
	if _, err := ParseManifest([]byte(data)); err == nil || !strings.Contains(err.Error(), "not one this gombit implements") {
		t.Fatalf("ParseManifest = %v; want the action refused", err)
	}
}
