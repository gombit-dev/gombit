package appcontract

import "testing"

func validInputs() Inputs {
	return Inputs{
		FrameworkVersion: "v0.5.0",
		HTTPAddr:         ":8080",
		DatabaseDriver:   "sqlite",
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
