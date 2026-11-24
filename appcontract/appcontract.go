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
//
// A field can be added within a version; only a breaking change of shape
// bumps it (ADR-015). A host that does not know an added field ignores it and
// behaves as before, and a host that reads one finds it absent in a contract
// from an older gombit. Version 1 gained processes and redis (HOST-4, after
// v0.10.0).
//
// The contract describes the configuration it was projected from: the
// process environment over the app's .env, nothing set in the app's code. A
// host that injects configuration of its own projects the contract with that
// environment, the one it will start the processes with.
//
// The processes and the Redis dependency describe what one release's runtime
// does with that configuration: which settings it reads, how long each
// process takes to stop, what needs Redis. Only the gombit of the release the
// app runs can describe them, so they are projected only when the projecting
// gombit is that release (Inputs.ProjectorVersion), and left out otherwise:
// absent, as in a contract that predates them.
package appcontract

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"
)

// ContractVersion is the schema version of the emitted contract. It is
// independent of the framework version; bump it only on a breaking shape
// change, and document the change. Fields added within it are documented in
// the package doc.
const ContractVersion = 1

// FrameworkName is the framework a Gombit application contract describes.
const FrameworkName = "gombit"

// Health probe paths are the stable operational contract from HOST-2
// (ADR-015, docs/health.md). They are fixed so a host can rely on them.
const (
	LivenessPath  = "/livez"
	ReadinessPath = "/readyz"
)

// Process names (Process.Name).
const (
	// ProcessWeb is the HTTP server: the artifact run with no arguments.
	ProcessWeb = "web"
	// ProcessWorker is the jobs worker: the artifact run with WorkerCommand.
	ProcessWorker = "worker"
)

// WorkerCommand is the artifact's argument that runs the jobs worker
// (framework.WorkerCommand).
const WorkerCommand = "worker"

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
	// Processes are the processes a host runs from the artifact: ProcessWeb,
	// then ProcessWorker when the app's jobs need one. Nil, and absent from
	// the JSON, when the contract does not describe them: it was projected by
	// a gombit other than the app's release, or predates them.
	Processes []Process `json:"processes,omitempty"`
	// Redis is nil, and absent, exactly when Processes is.
	Redis *Redis `json:"redis,omitempty"`
}

// Process is one process a host runs from the build artifact.
type Process struct {
	// Name is ProcessWeb or ProcessWorker.
	Name string `json:"name"`
	// Args are the artifact's arguments: none for the web process,
	// [WorkerCommand] for the worker. Each process reads the rest of its
	// settings from the environment the contract was projected from.
	Args []string `json:"args"`
	// HTTP is whether the process serves runtime.http_port and the health
	// probes. Only the web process does.
	HTTP bool `json:"http"`
	// StopTimeoutSeconds is how long the process needs, from the stop signal
	// (SIGTERM or SIGINT) to its exit, rounded up to a whole second. A host
	// that kills it sooner cuts off its graceful shutdown: a request, a stop
	// hook, or a job's ack.
	StopTimeoutSeconds int `json:"stop_timeout_seconds"`
}

// Redis is the app's Redis dependency. Required is true when the app's cache
// or its job queue is on the redis driver; the host provides the server
// (GOMBIT_REDIS_*).
type Redis struct {
	Required bool `json:"required"`
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
	// WebStopTimeout is how long the web process needs to stop
	// (framework.WebKillAfter). Required.
	WebStopTimeout time.Duration
	// Worker is whether the app runs a jobs worker process: its jobs driver
	// is worked outside the web process (config.JobsDriver.NeedsWorker).
	Worker bool
	// WorkerStopTimeout is how long the worker needs to stop
	// (framework.WorkerKillAfterFor). Required when Worker is set.
	WorkerStopTimeout time.Duration
	// RedisRequired is whether the app needs a Redis server
	// (config.Config.RequiresRedis).
	RedisRequired bool
	// ProjectorVersion is the release of the gombit projecting the contract
	// (`gombit version`). The processes and the Redis dependency are
	// projected only when it is FrameworkVersion: the stop timeouts and the
	// settings behind them are that gombit's, which describe another release
	// wrongly. Otherwise the stop timeouts are not required and the contract
	// leaves both out.
	ProjectorVersion string
}

// DescribesRuntime reports whether a contract projected from in describes
// the app's processes and Redis dependency: the projecting gombit is the
// release the app runs.
func (in Inputs) DescribesRuntime() bool {
	return in.ProjectorVersion == in.FrameworkVersion
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
	var processes []Process
	var redis *Redis
	if in.DescribesRuntime() {
		if in.WebStopTimeout <= 0 {
			return Contract{}, errors.New("appcontract: the web process's stop timeout is required")
		}
		processes = []Process{{
			Name:               ProcessWeb,
			Args:               []string{},
			HTTP:               true,
			StopTimeoutSeconds: wholeSeconds(in.WebStopTimeout),
		}}
		if in.Worker {
			if in.WorkerStopTimeout <= 0 {
				return Contract{}, errors.New("appcontract: the worker's stop timeout is required")
			}
			processes = append(processes, Process{
				Name:               ProcessWorker,
				Args:               []string{WorkerCommand},
				StopTimeoutSeconds: wholeSeconds(in.WorkerStopTimeout),
			})
		}
		redis = &Redis{Required: in.RedisRequired}
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
		Processes: processes,
		Redis:     redis,
	}, nil
}

// wholeSeconds is d in seconds, rounded up.
func wholeSeconds(d time.Duration) int {
	return int((d + time.Second - 1) / time.Second)
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
