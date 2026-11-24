package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestShutdownAndWorkerSettingsDefaults: the defaults are gombit's documented
// figures, and a zero field means the same default, so a Config built in code
// with the field left out behaves as one from Default (HOST-4).
func TestShutdownAndWorkerSettingsDefaults(t *testing.T) {
	cfg := Default()
	if cfg.Shutdown != (ShutdownConfig{Timeout: 10 * time.Second}) {
		t.Errorf("Default().Shutdown = %+v, want a 10s timeout and no drain delay", cfg.Shutdown)
	}
	w := cfg.Jobs.Worker
	if len(w.Queues) != 0 || w.Concurrency != 1 || w.Lease != 5*time.Minute || w.ShutdownTimeout != 30*time.Second {
		t.Errorf("Default().Jobs.Worker = %+v, want no queues, concurrency 1, a 5m lease and a 30s shutdown timeout", w)
	}

	var zero JobsWorkerConfig
	if zero.EffectiveConcurrency() != w.Concurrency || zero.EffectiveLease() != w.Lease || zero.EffectiveShutdownTimeout() != w.ShutdownTimeout {
		t.Errorf("a zero worker config resolves to %d, %s, %s; want the defaults", zero.EffectiveConcurrency(), zero.EffectiveLease(), zero.EffectiveShutdownTimeout())
	}
	if got := (ShutdownConfig{}).EffectiveTimeout(); got != cfg.Shutdown.Timeout {
		t.Errorf("a zero shutdown timeout resolves to %s, want %s", got, cfg.Shutdown.Timeout)
	}
	set := JobsWorkerConfig{Concurrency: 4, Lease: time.Minute, ShutdownTimeout: time.Hour}
	if set.EffectiveConcurrency() != 4 || set.EffectiveLease() != time.Minute || set.EffectiveShutdownTimeout() != time.Hour {
		t.Errorf("a set worker config resolves to %d, %s, %s; want its own values", set.EffectiveConcurrency(), set.EffectiveLease(), set.EffectiveShutdownTimeout())
	}
	if got := (ShutdownConfig{Timeout: time.Minute}).EffectiveTimeout(); got != time.Minute {
		t.Errorf("a set shutdown timeout resolves to %s, want 1m", got)
	}
}

// TestValidateShutdownAndWorkerSettings: a negative duration or concurrency,
// and a worker queue that isn't a valid queue name, are field errors naming
// the variable.
func TestValidateShutdownAndWorkerSettings(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*Config)
		env    string
	}{
		"negative shutdown timeout": {func(c *Config) { c.Shutdown.Timeout = -time.Second }, envShutdownTimeout},
		"negative drain delay":      {func(c *Config) { c.Shutdown.DrainDelay = -time.Second }, envShutdownDrainDelay},
		"invalid worker queue":      {func(c *Config) { c.Jobs.Worker.Queues = []string{"default", "Mail Queue"} }, envJobsWorkerQueues},
		"negative concurrency":      {func(c *Config) { c.Jobs.Worker.Concurrency = -1 }, envJobsWorkerConcurrency},
		"negative lease":            {func(c *Config) { c.Jobs.Worker.Lease = -time.Second }, envJobsWorkerLease},
		"negative worker shutdown":  {func(c *Config) { c.Jobs.Worker.ShutdownTimeout = -time.Second }, envJobsWorkerShutdown},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			var fe FieldErrors
			if err := cfg.Validate(); !errors.As(err, &fe) || !strings.Contains(err.Error(), tc.env) {
				t.Fatalf("Validate() = %v, want a field error naming %s", err, tc.env)
			}
		})
	}
	cfg := Default()
	cfg.Shutdown = ShutdownConfig{}
	cfg.Jobs.Worker = JobsWorkerConfig{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("zero shutdown and worker settings (the defaults): Validate() = %v, want nil", err)
	}
	if err := ValidateJobs(JobsConfig{Driver: JobsDriverRedis, Queue: "default", Namespace: "n", Worker: JobsWorkerConfig{Queues: []string{"-bad"}}}); err == nil {
		t.Fatal("ValidateJobs accepted an invalid worker queue")
	}
}

func TestJobsDriverNeedsWorker(t *testing.T) {
	for driver, want := range map[JobsDriver]bool{JobsDriverSync: false, JobsDriverMemory: false, JobsDriverRedis: true} {
		if got := driver.NeedsWorker(); got != want {
			t.Errorf("%s.NeedsWorker() = %v, want %v", driver, got, want)
		}
	}
}

func TestRequiresRedis(t *testing.T) {
	for name, tc := range map[string]struct {
		cache CacheDriver
		jobs  JobsDriver
		want  bool
	}{
		"defaults":    {CacheDriverMemory, JobsDriverSync, false},
		"noop cache":  {CacheDriverNoop, JobsDriverMemory, false},
		"redis cache": {CacheDriverRedis, JobsDriverSync, true},
		"redis jobs":  {CacheDriverMemory, JobsDriverRedis, true},
		"both":        {CacheDriverRedis, JobsDriverRedis, true},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			cfg.Cache.Driver, cfg.Jobs.Driver = tc.cache, tc.jobs
			if got := cfg.RequiresRedis(); got != tc.want {
				t.Errorf("RequiresRedis() = %v, want %v", got, tc.want)
			}
		})
	}
}
