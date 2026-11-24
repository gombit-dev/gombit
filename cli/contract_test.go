package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/appcontract"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/framework"
)

func stubLoadConfig(t *testing.T, cfg config.Config) {
	t.Helper()
	prev := LoadConfigFromDir
	t.Cleanup(func() { LoadConfigFromDir = prev })
	LoadConfigFromDir = func(string) (config.Config, error) { return cfg, nil }
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

// The processes and the Redis dependency are projected from the declared
// jobs and cache drivers (HOST-4): a worker only where the jobs driver's
// queue is worked outside the web process, with the stop timeouts the
// framework computes from the declared shutdown settings.
func TestRunContractAppDeclaresProcesses(t *testing.T) {
	type process struct {
		Name               string   `json:"name"`
		Args               []string `json:"args"`
		HTTP               bool     `json:"http"`
		StopTimeoutSeconds int      `json:"stop_timeout_seconds"`
	}
	web := process{Name: "web", Args: []string{}, HTTP: true, StopTimeoutSeconds: 22}
	for name, tc := range map[string]struct {
		jobs      config.JobsDriver
		cache     config.CacheDriver
		processes []process
		redis     bool
	}{
		"sync jobs":   {config.JobsDriverSync, config.CacheDriverMemory, []process{web}, false},
		"memory jobs": {config.JobsDriverMemory, config.CacheDriverMemory, []process{web}, false},
		"redis cache": {config.JobsDriverSync, config.CacheDriverRedis, []process{web}, true},
		"redis jobs": {config.JobsDriverRedis, config.CacheDriverMemory, []process{web,
			{Name: "worker", Args: []string{"worker"}, StopTimeoutSeconds: 58}}, true},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Jobs.Driver, cfg.Cache.Driver = tc.jobs, tc.cache
			stubLoadConfig(t, cfg)
			dir := writeGoMod(t, "module x\nrequire github.com/gombit-dev/gombit v0.5.0\n")
			var out bytes.Buffer
			if err := runContractApp(&out, dir, ""); err != nil {
				t.Fatal(err)
			}
			var got struct {
				Processes []process `json:"processes"`
				Redis     struct {
					Required bool `json:"required"`
				} `json:"redis"`
			}
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Processes, tc.processes) {
				t.Errorf("processes = %+v, want %+v", got.Processes, tc.processes)
			}
			if got.Redis.Required != tc.redis {
				t.Errorf("redis.required = %v, want %v", got.Redis.Required, tc.redis)
			}
		})
	}
}

// The stop timeouts follow the declared shutdown and worker settings, by the
// framework's own computation.
func TestRunContractAppStopTimeoutsFollowTheConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Jobs.Driver = config.JobsDriverRedis
	cfg.Shutdown = config.ShutdownConfig{Timeout: 15 * time.Second, DrainDelay: 3 * time.Second}
	cfg.Jobs.Worker.ShutdownTimeout = 2 * time.Minute
	stubLoadConfig(t, cfg)
	dir := writeGoMod(t, "module x\nrequire github.com/gombit-dev/gombit v0.5.0\n")
	var out bytes.Buffer
	if err := runContractApp(&out, dir, ""); err != nil {
		t.Fatal(err)
	}
	var got appcontract.Contract
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// 3s + 2×15s + 2s, and 2m + 18s + 15s.
	if len(got.Processes) != 2 || got.Processes[0].StopTimeoutSeconds != 35 || got.Processes[1].StopTimeoutSeconds != 153 {
		t.Fatalf("processes = %+v, want the web process at 35s and the worker at 153s", got.Processes)
	}
	if web, worker := framework.WebKillAfter(cfg), framework.WorkerKillAfterFor(cfg, nil); web != 35*time.Second || worker != 153*time.Second {
		t.Fatalf("framework figures %s and %s, want 35s and 153s", web, worker)
	}
}

// The contract's worker argument is the framework's worker command.
func TestContractWorkerCommandIsTheFrameworks(t *testing.T) {
	if appcontract.WorkerCommand != framework.WorkerCommand {
		t.Fatalf("appcontract.WorkerCommand = %q, framework.WorkerCommand = %q", appcontract.WorkerCommand, framework.WorkerCommand)
	}
}
