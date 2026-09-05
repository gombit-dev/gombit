package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/config"
)

func stubLoadConfig(t *testing.T, cfg config.Config) {
	t.Helper()
	prev := LoadConfig
	t.Cleanup(func() { LoadConfig = prev })
	LoadConfig = func() (config.Config, error) { return cfg, nil }
}

func writeGoMod(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

type appContractJSON struct {
	ContractVersion int `json:"contract_version"`
	Framework       struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"framework"`
	Runtime struct {
		HTTPPort int `json:"http_port"`
		Health   struct {
			Live  string `json:"live"`
			Ready string `json:"ready"`
		} `json:"health"`
	} `json:"runtime"`
	Database struct {
		Required bool   `json:"required"`
		Driver   string `json:"driver"`
	} `json:"database"`
}

func TestRunContractAppEmitsProjectedJSON(t *testing.T) {
	cfg := config.Default() // Database.Required defaults to true
	cfg.HTTP.Addr = ":8080"
	cfg.Database.Driver = config.DatabaseDriverPostgres
	stubLoadConfig(t, cfg)
	dir := writeGoMod(t, "module x\nrequire github.com/gombit-dev/gombit v0.5.0\n")

	var out bytes.Buffer
	if err := runContractApp(&out, dir, ""); err != nil {
		t.Fatalf("runContractApp() error = %v", err)
	}

	var got appContractJSON
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal contract %q: %v", out.String(), err)
	}
	if got.ContractVersion != 1 {
		t.Errorf("contract_version = %d, want 1", got.ContractVersion)
	}
	if got.Framework.Name != "gombit" || got.Framework.Version != "v0.5.0" {
		t.Errorf("framework = %+v, want {gombit v0.5.0}", got.Framework)
	}
	if got.Runtime.HTTPPort != 8080 {
		t.Errorf("http_port = %d, want 8080", got.Runtime.HTTPPort)
	}
	if got.Runtime.Health.Live != "/livez" || got.Runtime.Health.Ready != "/readyz" {
		t.Errorf("health = %+v, want /livez + /readyz", got.Runtime.Health)
	}
	if got.Database.Driver != "postgres" {
		t.Errorf("database.driver = %q, want postgres", got.Database.Driver)
	}
	if !got.Database.Required {
		t.Error("database.required = false, want true (the default) — a postgres app needs a datastore")
	}
}

func TestRunContractAppRequiredReflectsConfig(t *testing.T) {
	// database.required is a declared config value, not a proxy for auth: an app
	// that opts out reports false even with a driver configured.
	cfg := config.Default()
	cfg.Database.Required = false
	stubLoadConfig(t, cfg)
	dir := writeGoMod(t, "module x\nrequire github.com/gombit-dev/gombit v0.5.0\n")

	var out bytes.Buffer
	if err := runContractApp(&out, dir, ""); err != nil {
		t.Fatalf("runContractApp() error = %v", err)
	}
	var got appContractJSON
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Database.Required {
		t.Error("database.required = true, want false when the app declares it does not need a database")
	}
}

func TestRunContractAppWritesFile(t *testing.T) {
	stubLoadConfig(t, config.Default())
	dir := writeGoMod(t, "module x\nrequire github.com/gombit-dev/gombit v0.5.0\n")
	outPath := filepath.Join(t.TempDir(), "app.json")

	var stdout bytes.Buffer
	if err := runContractApp(&stdout, dir, outPath); err != nil {
		t.Fatalf("runContractApp() error = %v", err)
	}
	if !strings.Contains(stdout.String(), outPath) {
		t.Errorf("stdout = %q, want mention of %q", stdout.String(), outPath)
	}
	data, err := os.ReadFile(outPath) // #nosec G304 -- outPath is a t.TempDir path in this test.
	if err != nil {
		t.Fatalf("read out file: %v", err)
	}
	var got appContractJSON
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal file: %v", err)
	}
	if got.ContractVersion != 1 {
		t.Errorf("contract_version = %d, want 1", got.ContractVersion)
	}
}

func TestRunContractAppUnresolvedFrameworkFailsLoudly(t *testing.T) {
	stubLoadConfig(t, config.Default())
	dir := writeGoMod(t, "module x\nrequire github.com/gombit-dev/gombit v0.5.0\nreplace github.com/gombit-dev/gombit => ../gombit\n")

	var out bytes.Buffer
	err := runContractApp(&out, dir, "")
	if err == nil {
		t.Fatal("runContractApp() error = nil, want error for replaced framework")
	}
	if !strings.Contains(err.Error(), "unresolved") {
		t.Errorf("error = %v, want it to explain the unresolved version", err)
	}
}

func TestRunContractAppMissingGoModFails(t *testing.T) {
	stubLoadConfig(t, config.Default())
	var out bytes.Buffer
	if err := runContractApp(&out, t.TempDir(), ""); err == nil {
		t.Fatal("runContractApp() error = nil, want error for missing go.mod")
	}
}
