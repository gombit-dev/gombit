package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClientTemplateCopiesAgree checks every checked-in copy of the generated
// client.ts against clientTS, the string gombit client generate writes: the
// scaffold template gombit new installs, plus the example and golden trees.
// Editing the template and leaving a copy behind is how the two drift apart,
// and an app only notices when it runs gombit client check.
func TestClientTemplateCopiesAgree(t *testing.T) {
	root := moduleRoot(t)
	paths := []string{
		filepath.Join("scaffold", "templates", "frontend", "src", "api", "generated", "client.ts.tmpl"),
		filepath.Join(SampleOutDir, "client.ts"),
	}
	for _, name := range []string{"client", "new", "new-mui", "new-cookie", "make-resource", "make-command"} {
		p := filepath.Join("goldentest", "testdata", "golden", name, "client.ts")
		if name != "client" {
			p = filepath.Join("goldentest", "testdata", "golden", name, "frontend", "src", "api", "generated", "client.ts")
		}
		paths = append(paths, p)
	}

	for _, rel := range paths {
		// #nosec G304 -- fixed repo-relative path under the module root
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Errorf("read %s: %v", rel, err)
			continue
		}
		if string(data) != clientTS {
			t.Errorf("%s differs from clientTS; regenerate it or update the template", rel)
		}
	}
}

// TestUnwrapResolvesNoContent pins unwrap's contract for 204 No Content (#505):
// openapi-fetch reports data: undefined there because a 204 has no body by
// definition, so a successful no-content response must resolve instead of
// throwing empty_response. The generated client is typechecked but never
// executed by this package's tests, so this asserts the emitted template the
// same way the scaffold tests assert generated frontend source.
func TestUnwrapResolvesNoContent(t *testing.T) {
	start := strings.Index(clientTS, "export async function unwrap")
	if start < 0 {
		t.Fatal("clientTS has no unwrap")
	}
	body := clientTS[start:]

	empty := strings.Index(body, `throw new ContractError("empty_response"`)
	if empty < 0 {
		t.Fatal("unwrap no longer rejects a 2xx response without content")
	}
	noContent := strings.Index(body, "result.response.status === 204")
	if noContent < 0 {
		t.Fatal("unwrap does not special-case 204 No Content")
	}
	if noContent > empty {
		t.Error("unwrap throws empty_response before checking for 204, so a successful no-content response still fails")
	}
	if !strings.Contains(body, "return undefined as T;") {
		t.Error("the 204 branch does not resolve with undefined")
	}
}
