package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humagin"
	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/contract"
)

func TestCheckDrift(t *testing.T) {
	requireNode(t)
	root := moduleRoot(t)

	t.Run("committed fixtures match SampleApp", func(t *testing.T) {
		before := fixtureSnapshots(t, root)
		stdout := new(bytes.Buffer)
		err := CheckDrift(context.Background(), DriftOptions{
			WorkDir: root,
			Stdout:  stdout,
			Stderr:  ioDiscard{},
		})
		if err != nil {
			t.Fatalf("CheckDrift() error = %v, want nil", err)
		}
		if !strings.Contains(stdout.String(), "no contract drift") {
			t.Fatalf("stdout = %q, want no contract drift", stdout.String())
		}
		after := fixtureSnapshots(t, root)
		if before != after {
			t.Fatal("CheckDrift() mutated committed sample fixtures")
		}
	})

	t.Run("handler change without regen fails", func(t *testing.T) {
		app, err := SampleApp()
		if err != nil {
			t.Fatalf("SampleApp() error = %v", err)
		}
		huma.Register(app.API(), huma.Operation{
			OperationID: "list-gadgets",
			Method:      http.MethodGet,
			Path:        app.Config().API.Prefix + "/gadgets",
			Summary:     "List gadgets",
		}, func(ctx context.Context, input *struct{}) (*struct {
			Body contract.Data[[]string]
		}, error) {
			return &struct {
				Body contract.Data[[]string]
			}{Body: contract.Data[[]string]{Data: []string{"gadget-1"}}}, nil
		})

		before := fixtureSnapshots(t, root)
		err = CheckDrift(context.Background(), DriftOptions{
			WorkDir: root,
			API:     app.API(),
			Stderr:  ioDiscard{},
		})
		if err == nil {
			t.Fatal("CheckDrift() error = nil, want drift after extra Huma path")
		}
		if !errors.Is(err, ErrDrift) {
			t.Fatalf("CheckDrift() error = %q, want ErrDrift", err)
		}
		if !strings.Contains(err.Error(), "openapi.json") && !strings.Contains(err.Error(), "schema.ts") {
			t.Fatalf("CheckDrift() error = %q, want spec or schema path", err)
		}
		after := fixtureSnapshots(t, root)
		if before != after {
			t.Fatal("CheckDrift() mutated committed sample fixtures")
		}
	})

	t.Run("whitespace-only JSON is not drift", func(t *testing.T) {
		workDir := t.TempDir()
		copySampleFixtures(t, root, workDir)

		specPath := filepath.Join(workDir, "openapi.json")
		pretty := readFile(t, specPath)
		var compact bytes.Buffer
		if err := json.Compact(&compact, []byte(pretty)); err != nil {
			t.Fatalf("json.Compact: %v", err)
		}
		if compact.Len() == 0 || compact.String() == pretty {
			t.Fatal("compacted spec is not a whitespace-only change")
		}
		writeFile(t, specPath, compact.String())

		err := CheckDrift(context.Background(), DriftOptions{
			WorkDir:  workDir,
			SpecPath: "openapi.json",
			OutDir:   "frontend/src/api/generated",
			Stderr:   ioDiscard{},
		})
		if err != nil {
			t.Fatalf("CheckDrift() error = %v, want nil for whitespace-only spec", err)
		}
	})

	t.Run("raw Gin routes absent from regenerated spec", func(t *testing.T) {
		app, err := SampleApp()
		if err != nil {
			t.Fatalf("SampleApp() error = %v", err)
		}
		app.Router().GET("/raw/ping", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"data": gin.H{"status": "ok"}})
		})

		spec, err := contract.OpenAPIJSON(app.API())
		if err != nil {
			t.Fatalf("OpenAPIJSON() error = %v", err)
		}
		if strings.Contains(string(spec), "/raw/ping") {
			t.Fatal("raw Gin route /raw/ping unexpectedly appears in regenerated spec")
		}

		err = CheckDrift(context.Background(), DriftOptions{
			WorkDir: root,
			API:     app.API(),
			Stderr:  ioDiscard{},
		})
		if err != nil {
			t.Fatalf("CheckDrift() error = %v, want nil when only a raw Gin route is added", err)
		}
	})
}

func TestCheckDriftWriteRegeneratesFixtures(t *testing.T) {
	requireNode(t)

	workDir := t.TempDir()
	stdout := new(bytes.Buffer)
	err := CheckDrift(context.Background(), DriftOptions{
		WorkDir:  workDir,
		SpecPath: "openapi.json",
		OutDir:   "frontend/src/api/generated",
		Write:    true,
		Stdout:   stdout,
		Stderr:   ioDiscard{},
	})
	if err != nil {
		t.Fatalf("CheckDrift(write) error = %v", err)
	}
	if !strings.Contains(stdout.String(), "openapi.json") {
		t.Fatalf("stdout = %q, want wrote openapi.json", stdout.String())
	}
	for _, name := range sampleClientFiles {
		path := filepath.Join(workDir, "frontend", "src", "api", "generated", name)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing generated %s: %v", name, err)
		}
	}

	err = CheckDrift(context.Background(), DriftOptions{
		WorkDir:  workDir,
		SpecPath: "openapi.json",
		OutDir:   "frontend/src/api/generated",
		Stderr:   ioDiscard{},
	})
	if err != nil {
		t.Fatalf("CheckDrift() after write error = %v, want nil", err)
	}
}

// TestCheckDriftUsesSpecBytesOverSampleApp models the `gombit client check
// --url` path a generated app uses: it has no Go-level huma.API to pass, so
// the CLI fetches a live spec over HTTP and hands it in as SpecBytes. That
// must take precedence over the API/SampleApp fallback and never touch
// SampleApp's own contract.
func TestCheckDriftUsesSpecBytesOverSampleApp(t *testing.T) {
	requireNode(t)

	router := gin.New()
	api := humagin.New(router, contract.HumaConfig("drift-spec-bytes-test", "0.0.0"))
	huma.Register(api, huma.Operation{
		OperationID: "list-widgets-from-url",
		Method:      http.MethodGet,
		Path:        "/api/v1/widgets",
		Summary:     "List widgets",
	}, func(ctx context.Context, input *struct{}) (*struct {
		Body contract.Data[[]string]
	}, error) {
		return &struct {
			Body contract.Data[[]string]
		}{Body: contract.Data[[]string]{Data: []string{"widget-1"}}}, nil
	})
	specBytes, err := contract.OpenAPIJSON(api)
	if err != nil {
		t.Fatalf("contract.OpenAPIJSON() error = %v", err)
	}

	workDir := t.TempDir()
	stdout := new(bytes.Buffer)
	err = CheckDrift(context.Background(), DriftOptions{
		WorkDir:   workDir,
		SpecPath:  "openapi.json",
		OutDir:    "frontend/src/api/generated",
		SpecBytes: specBytes,
		Write:     true,
		Stdout:    stdout,
		Stderr:    ioDiscard{},
	})
	if err != nil {
		t.Fatalf("CheckDrift(SpecBytes, write) error = %v", err)
	}

	// #nosec G304 -- test-controlled path under t.TempDir()
	written, err := os.ReadFile(filepath.Join(workDir, "openapi.json"))
	if err != nil {
		t.Fatalf("read written spec: %v", err)
	}
	if !strings.Contains(string(written), "list-widgets-from-url") {
		t.Fatalf("written spec = %s, want it to come from SpecBytes, not SampleApp", written)
	}
	if strings.Contains(string(written), "SampleApp") {
		t.Fatal("written spec unexpectedly contains SampleApp content")
	}

	// A subsequent check against the same SpecBytes must report no drift.
	err = CheckDrift(context.Background(), DriftOptions{
		WorkDir:   workDir,
		SpecPath:  "openapi.json",
		OutDir:    "frontend/src/api/generated",
		SpecBytes: specBytes,
		Stderr:    ioDiscard{},
	})
	if err != nil {
		t.Fatalf("CheckDrift(SpecBytes) after write error = %v, want nil", err)
	}
}

func TestCheckDriftRequiresCommittedSpec(t *testing.T) {
	err := CheckDrift(context.Background(), DriftOptions{
		WorkDir:  t.TempDir(),
		SpecPath: "missing.json",
		OutDir:   "generated",
	})
	if err == nil {
		t.Fatal("CheckDrift() error = nil, want missing spec")
	}
	if !strings.Contains(err.Error(), "committed spec") {
		t.Fatalf("CheckDrift() error = %q, want committed spec", err)
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s: %v", root, err)
	}
	return root
}

func copySampleFixtures(t *testing.T, root, dest string) {
	t.Helper()
	copyFile(t, filepath.Join(root, SampleSpecPath), filepath.Join(dest, "openapi.json"))
	for _, name := range sampleClientFiles {
		copyFile(t,
			filepath.Join(root, SampleOutDir, name),
			filepath.Join(dest, "frontend", "src", "api", "generated", name),
		)
	}
}

func copyFile(t *testing.T, src, dest string) {
	t.Helper()
	writeFile(t, dest, readFile(t, src))
}

func fixtureSnapshots(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(readFile(t, filepath.Join(root, SampleSpecPath)))
	for _, name := range sampleClientFiles {
		b.WriteString(readFile(t, filepath.Join(root, SampleOutDir, name)))
	}
	return b.String()
}
