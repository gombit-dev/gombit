package appcontract

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func validInputs() Inputs {
	return Inputs{
		FrameworkVersion: "v0.5.0",
		HTTPAddr:         ":8080",
		DatabaseDriver:   "sqlite",
		WebStopTimeout:   22 * time.Second,
		ProjectorVersion: "v0.5.0",
	}
}

func TestProjectFillsFixedAndDefaultFields(t *testing.T) {
	got, err := Project(validInputs())
	if err != nil {
		t.Fatalf("Project() error = %v, want nil", err)
	}

	if got.ContractVersion != ContractVersion {
		t.Errorf("contract_version = %d, want %d", got.ContractVersion, ContractVersion)
	}
	if got.Framework != (Framework{Name: "gombit", Version: "v0.5.0"}) {
		t.Errorf("framework = %+v", got.Framework)
	}
	if got.Build != (Build{Command: DefaultBuildCommand, Artifact: DefaultBuildArtifact}) {
		t.Errorf("build = %+v, want documented defaults", got.Build)
	}
	if got.Runtime.HTTPPort != 8080 {
		t.Errorf("http_port = %d, want 8080", got.Runtime.HTTPPort)
	}
	if got.Runtime.Health != (Health{Live: "/livez", Ready: "/readyz"}) {
		t.Errorf("health = %+v, want the HOST-2 probe paths", got.Runtime.Health)
	}
	if got.Migrations.Path != DefaultMigrationsPath {
		t.Errorf("migrations.path = %q, want %q", got.Migrations.Path, DefaultMigrationsPath)
	}
}

func TestProjectDatabaseFromInputs(t *testing.T) {
	in := validInputs()
	in.DatabaseDriver = "postgres"
	in.DatabaseRequired = true

	got, err := Project(in)
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}
	if got.Database != (Database{Required: true, Driver: "postgres"}) {
		t.Errorf("database = %+v, want {true, postgres}", got.Database)
	}
}

func TestProjectHonorsBuildAndMigrationsOverrides(t *testing.T) {
	in := validInputs()
	in.BuildCommand = "make build"
	in.BuildArtifact = "dist/app"
	in.MigrationsPath = "db/migrations"

	got, err := Project(in)
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}
	if got.Build.Command != "make build" || got.Build.Artifact != "dist/app" {
		t.Errorf("build = %+v, want overrides", got.Build)
	}
	if got.Migrations.Path != "db/migrations" {
		t.Errorf("migrations.path = %q, want override", got.Migrations.Path)
	}
}

func TestProjectRejectsMissingRequiredInputs(t *testing.T) {
	tests := map[string]func(*Inputs){
		"no framework version": func(in *Inputs) { in.FrameworkVersion = "" },
		"no database driver":   func(in *Inputs) { in.DatabaseDriver = "" },
		"no http addr":         func(in *Inputs) { in.HTTPAddr = "" },
		"no web stop timeout":  func(in *Inputs) { in.WebStopTimeout = 0 },
		"no worker stop timeout": func(in *Inputs) {
			in.Worker = true
			in.WorkerStopTimeout = 0
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			in := validInputs()
			mutate(&in)
			if _, err := Project(in); err == nil {
				t.Fatalf("Project() error = nil, want error for %s", name)
			}
		})
	}
}

func TestProjectParsesPort(t *testing.T) {
	tests := map[string]struct {
		addr    string
		want    int
		wantErr bool
	}{
		"bare port":    {":8080", 8080, false},
		"host+port":    {"127.0.0.1:9000", 9000, false},
		"no port":      {"localhost", 0, true},
		"bad port":     {":notaport", 0, true},
		"out of range": {":70000", 0, true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			in := validInputs()
			in.HTTPAddr = tc.addr
			got, err := Project(in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Project(%q) error = nil, want error", tc.addr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Project(%q) error = %v", tc.addr, err)
			}
			if got.Runtime.HTTPPort != tc.want {
				t.Errorf("http_port = %d, want %d", got.Runtime.HTTPPort, tc.want)
			}
		})
	}
}

// An app without a worker declares the web process alone.
func TestProjectWebProcessOnly(t *testing.T) {
	got, err := Project(validInputs())
	if err != nil {
		t.Fatal(err)
	}
	want := []Process{{Name: ProcessWeb, Args: []string{}, HTTP: true, StopTimeoutSeconds: 22}}
	if !reflect.DeepEqual(got.Processes, want) {
		t.Errorf("processes = %+v, want %+v", got.Processes, want)
	}
	if got.Redis == nil || got.Redis.Required {
		t.Errorf("redis = %+v, want not required", got.Redis)
	}
}

// An app whose jobs need a worker declares it after the web process, with
// the worker command and its own stop timeout.
func TestProjectWorkerProcess(t *testing.T) {
	in := validInputs()
	in.Worker = true
	in.WorkerStopTimeout = 58 * time.Second
	in.RedisRequired = true
	got, err := Project(in)
	if err != nil {
		t.Fatal(err)
	}
	want := []Process{
		{Name: ProcessWeb, Args: []string{}, HTTP: true, StopTimeoutSeconds: 22},
		{Name: ProcessWorker, Args: []string{WorkerCommand}, HTTP: false, StopTimeoutSeconds: 58},
	}
	if !reflect.DeepEqual(got.Processes, want) {
		t.Errorf("processes = %+v, want %+v", got.Processes, want)
	}
	if got.Redis == nil || !got.Redis.Required {
		t.Errorf("redis = %+v, want required", got.Redis)
	}
}

// A stop timeout that isn't a whole number of seconds rounds up: a host that
// waits a second too little cuts the shutdown off.
func TestProjectRoundsStopTimeoutsUp(t *testing.T) {
	in := validInputs()
	in.WebStopTimeout = 20*time.Second + time.Millisecond
	in.Worker = true
	in.WorkerStopTimeout = 59*time.Second + 999*time.Millisecond
	got, err := Project(in)
	if err != nil {
		t.Fatal(err)
	}
	if web, worker := got.Processes[0].StopTimeoutSeconds, got.Processes[1].StopTimeoutSeconds; web != 21 || worker != 60 {
		t.Errorf("stop timeouts = %ds and %ds, want 21s and 60s", web, worker)
	}
}

// The JSON a host reads: the web process's args are an empty list, not null.
func TestContractJSONDeclaresProcessesAndRedis(t *testing.T) {
	got, err := Project(validInputs())
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"processes":[{"name":"web","args":[],"http":true,"stop_timeout_seconds":22}]`,
		`"redis":{"required":false}`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("contract JSON %s lacks %s", data, want)
		}
	}
}

// Projected by a gombit of another release, the contract leaves out the
// processes and the Redis dependency, which describe a release's runtime,
// and needs no stop timeouts: it reads as one that predates them.
func TestProjectLeavesOutTheRuntimeForAnotherRelease(t *testing.T) {
	in := validInputs()
	in.ProjectorVersion = "v0.6.0"
	in.WebStopTimeout = 0
	in.Worker = true
	in.RedisRequired = true
	if in.DescribesRuntime() {
		t.Fatal("DescribesRuntime() = true for another release")
	}
	got, err := Project(in)
	if err != nil {
		t.Fatalf("Project() error = %v, want the contract without its runtime", err)
	}
	if got.Processes != nil || got.Redis != nil {
		t.Errorf("processes = %+v, redis = %+v; want both left out", got.Processes, got.Redis)
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"processes"`, `"redis"`} {
		if strings.Contains(string(data), field) {
			t.Errorf("contract JSON %s has %s, want it absent", data, field)
		}
	}
	// What every release projects alike is still there.
	if got.Database.Driver != "sqlite" || got.Runtime.HTTPPort != 8080 {
		t.Errorf("contract = %+v, want its other fields", got)
	}
}
