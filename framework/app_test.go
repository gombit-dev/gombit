package framework

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"go.uber.org/zap"
)

func TestRunContextClosesServerAfterShutdownTimeout(t *testing.T) {
	app := newTestApp(t, WithShutdownTimeout(200*time.Millisecond))
	started := make(chan struct{})
	var startOnce sync.Once
	app.Router().GET("/slow", func(c *gin.Context) {
		startOnce.Do(func() { close(started) })
		time.Sleep(5 * time.Second)
		c.Status(http.StatusOK)
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunContext(ctx, app)
	}()

	waitForHTTP(t, app, "/livez")

	reqDone := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Get("http://" + app.Addr() + "/slow")
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		reqDone <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("slow handler did not start")
	}

	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("RunContext() error = nil, want shutdown timeout")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("RunContext() error = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunContext() did not return after shutdown timeout")
	}

	select {
	case err := <-reqDone:
		if err == nil {
			t.Fatal("in-flight GET /slow error = nil, want connection closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight GET /slow still held after RunContext returned; server.Close() was not called")
	}
}

func TestRunContextBootsAndShutsDown(t *testing.T) {
	app := newTestApp(t)
	app.Router().GET("/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, map[string]any{
			"data": map[string]string{"status": "ok"},
		})
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunContext(ctx, app)
	}()

	waitForHTTP(t, app, "/livez")
	waitForHTTP(t, app, "/ping")
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunContext() error = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunContext() did not shut down after context cancellation")
	}
}

func TestHooksRunInDeterministicOrder(t *testing.T) {
	app := newTestApp(t)
	var (
		mu    sync.Mutex
		order []string
	)
	record := func(value string) Hook {
		return func(ctx context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, value)
			return nil
		}
	}

	app.OnStart(record("start-1"))
	app.OnStart(record("start-2"))
	app.OnStop(record("stop-1"))
	app.OnStop(record("stop-2"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunContext(ctx, app)
	}()

	waitForHTTP(t, app, "/readyz")
	cancel()
	if err := waitRun(done); err != nil {
		t.Fatalf("RunContext() error = %v, want nil", err)
	}

	want := []string{"start-1", "start-2", "stop-2", "stop-1"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("hook order = %#v, want %#v", order, want)
	}
}

func TestStartFailureRunsStopHooks(t *testing.T) {
	app := newTestApp(t)
	startErr := errors.New("start failed")
	var order []string

	app.OnStart(func(ctx context.Context) error {
		order = append(order, "start-1")
		return nil
	})
	app.OnStart(func(ctx context.Context) error {
		order = append(order, "start-2")
		return startErr
	})
	app.OnStop(func(ctx context.Context) error {
		order = append(order, "stop-1")
		return nil
	})
	app.OnStop(func(ctx context.Context) error {
		order = append(order, "stop-2")
		return nil
	})

	err := RunContext(context.Background(), app)
	if !errors.Is(err, startErr) {
		t.Fatalf("RunContext() error = %v, want %v", err, startErr)
	}

	want := []string{"start-1", "start-2", "stop-2", "stop-1"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("hook order = %#v, want %#v", order, want)
	}
}

func TestStopHooksReceiveBoundedShutdownContext(t *testing.T) {
	app := newTestApp(t, WithShutdownTimeout(250*time.Millisecond))
	deadlineSeen := make(chan bool, 1)
	app.OnStop(func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		// Bounded, and usable: a deadline in the past is no budget at all.
		deadlineSeen <- ok && ctx.Err() == nil && time.Until(deadline) > 0
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunContext(ctx, app)
	}()

	waitForHTTP(t, app, "/livez")
	cancel()
	if err := waitRun(done); err != nil {
		t.Fatalf("RunContext() error = %v, want nil", err)
	}

	select {
	case got := <-deadlineSeen:
		if !got {
			t.Fatal("stop hook context had no deadline, or one already passed")
		}
	default:
		t.Fatal("stop hook did not run")
	}
}

// TestStopHooksGetTheirOwnBudgetAfterAnOverrunDrain locks issue #431: a request
// that outlives the shutdown timeout uses up the HTTP drain's budget, and the
// stop hooks must still get a live context bounded by their own timeout, not
// the drain's expired one. Shutdown still reports the cut-off drain.
func TestStopHooksGetTheirOwnBudgetAfterAnOverrunDrain(t *testing.T) {
	const timeout = 200 * time.Millisecond
	app := newTestApp(t, WithShutdownTimeout(timeout))
	release := make(chan struct{})
	entered := make(chan struct{})
	app.Router().GET("/slow", func(c *gin.Context) {
		close(entered)
		<-release
		c.Status(http.StatusOK)
	})
	t.Cleanup(func() { close(release) })

	type seen struct {
		err       error
		remaining time.Duration
		ok        bool
	}
	hook := make(chan seen, 1)
	app.OnStop(func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		hook <- seen{err: ctx.Err(), remaining: time.Until(deadline), ok: ok}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunContext(ctx, app) }()
	waitForHTTP(t, app, "/livez")
	go func() {
		if resp, err := http.Get("http://" + app.Addr() + "/slow"); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	cancel()
	if err := waitRun(done); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunContext() error = %v, want the cut-off drain reported (context.DeadlineExceeded)", err)
	}

	s := <-hook
	if !s.ok || s.err != nil {
		t.Fatalf("stop hook context: deadline=%v err=%v, want a live bounded context", s.ok, s.err)
	}
	// The hooks' budget is their own timeout, not what the drain left over.
	if s.remaining < timeout/2 || s.remaining > timeout {
		t.Fatalf("stop hook had %v left, want about its own %v", s.remaining, timeout)
	}
}

func TestDefaultRouterRecoversFromPanics(t *testing.T) {
	app := newTestApp(t)
	app.Router().GET("/panic", func(c *gin.Context) {
		panic("boom")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunContext(ctx, app)
	}()

	waitForHTTP(t, app, "/livez")
	resp := getHTTP(t, app, "/panic")
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("GET /panic status = %d, want %d", resp.StatusCode, http.StatusInternalServerError)
	}

	cancel()
	if err := waitRun(done); err != nil {
		t.Fatalf("RunContext() error = %v, want nil", err)
	}
}

func TestProductionConfigSetsGinReleaseMode(t *testing.T) {
	previousMode := gin.Mode()
	t.Cleanup(func() {
		gin.SetMode(previousMode)
	})

	cfg := config.Default()
	cfg.Environment = config.EnvironmentProduction
	cfg.HTTP.Addr = "127.0.0.1:0"

	_, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	if got := gin.Mode(); got != gin.ReleaseMode {
		t.Fatalf("gin.Mode() = %q, want %q", got, gin.ReleaseMode)
	}
}

func TestAppExposesDatabaseEscapeHatch(t *testing.T) {
	db, err := database.Open(config.DatabaseConfig{
		Driver: config.DatabaseDriverSQLite,
		DSN:    "file::memory:?cache=shared&_fk=1",
	})
	if err != nil {
		t.Fatalf("database.Open() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("database.Close() error = %v, want nil", err)
		}
	})

	app := newTestApp(t, WithDatabase(db))
	if got := app.Database(); got != db {
		t.Fatalf("Database() = %p, want %p", got, db)
	}
	if got := app.DB(); got != db.DB {
		t.Fatalf("DB() = %p, want %p", got, db.DB)
	}
}

func TestAppExposesDefaultMemoryCache(t *testing.T) {
	app := newTestApp(t)
	if app.Cache() == nil {
		t.Fatal("Cache() = nil, want default memory cache")
	}
	if app.Redis() != nil {
		t.Fatalf("Redis() = %v, want nil for memory cache", app.Redis())
	}

	ctx := context.Background()
	if err := app.Cache().Set(ctx, "answer", 42, time.Minute); err != nil {
		t.Fatalf("Cache().Set() error = %v, want nil", err)
	}
	var got int
	found, err := app.Cache().Get(ctx, "answer", &got)
	if err != nil {
		t.Fatalf("Cache().Get() error = %v, want nil", err)
	}
	if !found || got != 42 {
		t.Fatalf("Cache().Get() = (%t, %d), want (true, 42)", found, got)
	}
}

func TestAppExposesRedisEscapeHatchWhenConfigured(t *testing.T) {
	cfg := config.Default()
	cfg.Environment = config.EnvironmentTest
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Cache.Driver = config.CacheDriverRedis
	cfg.Cache.Namespace = config.DefaultCacheNamespace(cfg.AppName, cfg.Environment)

	app, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	if app.Cache() == nil {
		t.Fatal("Cache() = nil, want Redis-backed cache")
	}
	if app.Redis() == nil {
		t.Fatal("Redis() = nil, want Redis escape hatch")
	}
	t.Cleanup(func() {
		if err := app.Redis().Close(); err != nil {
			t.Fatalf("Redis().Close() error = %v, want nil", err)
		}
	})
}

func TestNewBuildsDefaultLoggerWithoutMongo(t *testing.T) {
	app := newTestApp(t)
	if app.Logger() == nil {
		t.Fatal("Logger() = nil, want logger")
	}
}

func TestAppExposesLogger(t *testing.T) {
	logger := zap.NewNop()
	app := newTestApp(t, WithLogger(logger))
	if got := app.Logger(); got != logger {
		t.Fatalf("Logger() = %p, want %p", got, logger)
	}
}

func TestAppBootsWithMongoSinkWhenLoggerProvided(t *testing.T) {
	cfg := config.Default()
	cfg.Environment = config.EnvironmentTest
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Logging.Sink = config.LogSinkMongo

	app, err := New(WithConfig(cfg), WithLogger(zap.NewNop()))
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	if app.Logger() == nil {
		t.Fatal("Logger() = nil, want logger")
	}
}

func TestNewWithMongoSinkRequiresProvidedLogger(t *testing.T) {
	cfg := config.Default()
	cfg.Environment = config.EnvironmentTest
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Logging.Sink = config.LogSinkMongo

	_, err := New(WithConfig(cfg))
	if err == nil {
		t.Fatal("New(WithConfig(mongo sink)) error = nil, want error")
	}
}

func TestNewValidatesOptions(t *testing.T) {
	_, err := New(WithShutdownTimeout(0))
	if err == nil {
		t.Fatal("New(WithShutdownTimeout(0)) error = nil, want error")
	}

	_, err = New(WithDatabase(nil))
	if err == nil {
		t.Fatal("New(WithDatabase(nil)) error = nil, want error")
	}

	_, err = New(WithCache(nil))
	if err == nil {
		t.Fatal("New(WithCache(nil)) error = nil, want error")
	}

	_, err = New(WithLogger(nil))
	if err == nil {
		t.Fatal("New(WithLogger(nil)) error = nil, want error")
	}

	_, err = New(WithEmbeddedFrontend(nil))
	if err == nil {
		t.Fatal("New(WithEmbeddedFrontend(nil)) error = nil, want error")
	}
}

func newTestApp(t *testing.T, options ...Option) *App {
	t.Helper()

	cfg := config.Default()
	cfg.Environment = config.EnvironmentTest
	cfg.HTTP.Addr = "127.0.0.1:0"

	options = append([]Option{WithConfig(cfg)}, options...)
	app, err := New(options...)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	return app
}

func waitForHTTP(t *testing.T, app *App, path string) {
	t.Helper()

	client := &http.Client{Timeout: 100 * time.Millisecond}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		addr := app.Addr()
		if addr == "" {
			time.Sleep(10 * time.Millisecond)
			continue
		}

		resp, err := client.Get("http://" + addr + path)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s on %s", path, app.Addr())
}

func getHTTP(t *testing.T, app *App, path string) *http.Response {
	t.Helper()

	client := &http.Client{Timeout: 100 * time.Millisecond}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		addr := app.Addr()
		if addr == "" {
			time.Sleep(10 * time.Millisecond)
			continue
		}

		resp, err := client.Get("http://" + addr + path)
		if err == nil {
			return resp
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s on %s", path, app.Addr())
	return nil
}

func waitRun(done <-chan error) error {
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		return errors.New("RunContext did not return")
	}
}
