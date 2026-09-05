// Package appcontract projects a stable, machine-readable description of a
// Gombit application — the HOST-1 application contract (ADR-015). A deployment
// host (e.g. Gombit Cloud) reads it to learn how to build, health-check, and
// migrate an ordinary handwritten Gombit app without inferring anything from
// repository trivia.
//
// Every field is projected from the app's *declared* configuration (typed
// config.Config / gombit.yaml, the resolved framework version, documented
// defaults) — never guessed by walking the source tree (build plan §10, 6.2).
// The contract is versioned by ContractVersion, independent of the framework
// version, so a host can fail loudly on an unknown shape (DESIGN.md §53, §55).
package appcontract

import (
	"errors"
	"fmt"
	"net"
	"strconv"
)

// ContractVersion is the schema version of the emitted contract. It is
// independent of the framework version; bump it only on a breaking shape
// change, and document the change.
const ContractVersion = 1

// FrameworkName is the framework a Gombit application contract describes.
const FrameworkName = "gombit"

// Health probe paths are the stable operational contract from HOST-2
// (ADR-015, docs/health.md). They are fixed so a host can rely on them.
const (
	LivenessPath  = "/livez"
	ReadinessPath = "/readyz"
)

// Documented build defaults (build plan M5-5; build.DefaultOut = "bin/server").
const (
	DefaultBuildCommand  = "gombit build --embed"
	DefaultBuildArtifact = "bin/server"
	// DefaultMigrationsPath mirrors migrations.defaultMigrationDir.
	DefaultMigrationsPath = "database/migrations"
)

// Contract is the machine-readable application description (DESIGN.md §9).
type Contract struct {
	ContractVersion int        `json:"contract_version"`
	Framework       Framework  `json:"framework"`
	Build           Build      `json:"build"`
	Runtime         Runtime    `json:"runtime"`
	Database        Database   `json:"database"`
	Migrations      Migrations `json:"migrations"`
}

// Framework identifies the framework and the version the app builds against.
type Framework struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Build is the command that produces the deployable artifact and where it lands.
type Build struct {
	Command  string `json:"command"`
	Artifact string `json:"artifact"`
}

// Runtime describes how the built app is run and probed.
type Runtime struct {
	HTTPPort int    `json:"http_port"`
	Health   Health `json:"health"`
}

// Health is the pair of operational probe paths (HOST-2).
type Health struct {
	Live  string `json:"live"`
	Ready string `json:"ready"`
}

// Database is the app's datastore dependency. Required is true when the
// framework refuses to start without a database for the declared configuration.
type Database struct {
	Required bool   `json:"required"`
	Driver   string `json:"driver"`
}

// Migrations locates the app's versioned migrations.
type Migrations struct {
	Path string `json:"path"`
}

// Inputs are the declared values a Contract is projected from. The CLI maps a
// typed config.Config onto these; keeping Project free of config keeps it
// trivially testable and lets a host reuse the type.
type Inputs struct {
	// FrameworkVersion the app builds against (e.g. "v0.5.0"). Required — an
	// empty value is a hard error, never a silent default (DESIGN.md §55).
	FrameworkVersion string
	// HTTPAddr is the declared listen address (config.HTTP.Addr, e.g. ":8080").
	HTTPAddr string
	// DatabaseDriver is the configured driver (config.Database.Driver).
	DatabaseDriver string
	// DatabaseRequired is whether the framework requires a database to start
	// for this configuration (currently: auth enabled — framework/app.go).
	DatabaseRequired bool
	// MigrationsPath locates versioned migrations; DefaultMigrationsPath when "".
	MigrationsPath string
	// BuildCommand / BuildArtifact override the documented defaults when set.
	BuildCommand  string
	BuildArtifact string
}

// Project builds the application contract from declared inputs, applying the
// fixed/default fields. It fails loudly on inputs a host could not act on.
func Project(in Inputs) (Contract, error) {
	if in.FrameworkVersion == "" {
		return Contract{}, errors.New("appcontract: framework version is required")
	}
	if in.DatabaseDriver == "" {
		return Contract{}, errors.New("appcontract: database driver is required")
	}
	port, err := parsePort(in.HTTPAddr)
	if err != nil {
		return Contract{}, err
	}

	return Contract{
		ContractVersion: ContractVersion,
		Framework: Framework{
			Name:    FrameworkName,
			Version: in.FrameworkVersion,
		},
		Build: Build{
			Command:  orDefault(in.BuildCommand, DefaultBuildCommand),
			Artifact: orDefault(in.BuildArtifact, DefaultBuildArtifact),
		},
		Runtime: Runtime{
			HTTPPort: port,
			Health: Health{
				Live:  LivenessPath,
				Ready: ReadinessPath,
			},
		},
		Database: Database{
			Required: in.DatabaseRequired,
			Driver:   in.DatabaseDriver,
		},
		Migrations: Migrations{
			Path: orDefault(in.MigrationsPath, DefaultMigrationsPath),
		},
	}, nil
}

// parsePort extracts the TCP port from a listen address such as ":8080" or
// "127.0.0.1:8080".
func parsePort(addr string) (int, error) {
	if addr == "" {
		return 0, errors.New("appcontract: http address is required")
	}
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, fmt.Errorf("appcontract: parse http address %q: %w", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("appcontract: http address %q has no valid port", addr)
	}
	return port, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
