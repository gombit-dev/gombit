package framework

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/internal/faulttest"
)

// Context fault tests: INV-2 (bounded waiting) and INV-3 (cancellation
// reaches downstream work) through Gombit's own layers: the request-context
// middleware and its per-request timeout, App.Tx, client disconnects, and
// graceful shutdown, down to a database call or an outbound HTTP call.
//
// A handler's outbound HTTP call ending at HTTP.RequestTimeout is
// TestFault_HTTP_HandlerDeadline (CHAOS-3); the disconnect case is here.
//
// Leaks are checked by explicit synchronization, not by counting the
// process's goroutines: the downstream call is a fault injector whose own
// counters say it saw the cancellation, faulttest.Idle says no connection
// was left held, dep.WaitIdle says no outbound call was left running, and
// each handler signals when it returns.

// guard fails t if done does not close within d: a call that should have
// been canceled is still running.
func guard(t *testing.T, what string, done <-chan struct{}, d time.Duration) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not end within %s: its cancellation did not reach it", what, d)
	}
}

// blockedSelect is a statement fault that holds the first n reads of
// fault_tx_parents until their context ends (or the test releases them);
// later reads (the test's own assertions) pass.
// release unblocks them early: a test defers it before closing a server,
// so a regression that leaves a handler stuck fails instead of hanging the
// server's Close.
func blockedSelect(t *testing.T, n int) (faults *faulttest.DBFaults, stmt *faulttest.Injector, release func()) {
	t.Helper()
	ch := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(ch) }) }
	t.Cleanup(release)
	steps := make([]faulttest.Step, n)
	for i := range steps {
		steps[i] = faulttest.Block(ch)
	}
	stmt = faulttest.Sequence(steps...)
	return &faulttest.DBFaults{Statement: stmt, Match: func(q string) bool {
		return strings.HasPrefix(strings.TrimSpace(strings.ToUpper(q)), "SELECT") && strings.Contains(q, "fault_tx_parents")
	}}, stmt, release
}

func requestTimeout(d time.Duration) Option {
	cfg := config.Default()
	cfg.Environment = config.EnvironmentTest
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.RequestTimeout = d
	return WithConfig(cfg)
}

// TestFault_Context_HandlerDeadline_DB: a handler's query on its request
// context ends at the per-request timeout, and the database call sees the
// deadline.
func TestFault_Context_HandlerDeadline_DB(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		gin.SetMode(gin.TestMode)
		faults, stmt, _ := blockedSelect(t, 1)
		app := newFaultApp(t, kind, dsn, faults, requestTimeout(100*time.Millisecond))
		got := make(chan error, 1)
		app.Router().GET("/rows", func(c *gin.Context) {
			var rows []faultTxParent
			err := app.DB().WithContext(c.Request.Context()).Find(&rows).Error
			got <- err
			c.Status(http.StatusGatewayTimeout)
		})
		served := make(chan struct{})
		go func() {
			defer close(served)
			app.Router().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/rows", nil))
		}()
		guard(t, "the request", served, 5*time.Second)
		if err := <-got; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the query = %v, want context.DeadlineExceeded", err)
		}
		if stmt.Failures() != 1 {
			t.Fatalf("the database call saw %d cancellations, want 1", stmt.Failures())
		}
		faulttest.Idle(t, app.Database())
	})
}

// TestFault_Context_HandlerDeadline_Tx: App.Tx carries the request's
// deadline into its transaction, so a stalled statement inside it ends at
// the per-request timeout and the transaction is rolled back.
func TestFault_Context_HandlerDeadline_Tx(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		gin.SetMode(gin.TestMode)
		faults, stmt, _ := blockedSelect(t, 1)
		app := newFaultApp(t, kind, dsn, faults, requestTimeout(100*time.Millisecond))
		got := make(chan error, 1)
		app.Router().POST("/family", func(c *gin.Context) {
			got <- app.Tx(c.Request.Context(), func(tx *gorm.DB) error {
				if err := tx.Create(&faultTxChild{Name: "before"}).Error; err != nil {
					return err
				}
				var rows []faultTxParent
				return tx.Find(&rows).Error // stalls until the deadline
			})
			c.Status(http.StatusGatewayTimeout)
		})
		served := make(chan struct{})
		go func() {
			defer close(served)
			app.Router().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/family", nil))
		}()
		guard(t, "the request", served, 5*time.Second)
		if err := <-got; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("App.Tx = %v, want context.DeadlineExceeded", err)
		}
		if stmt.Failures() != 1 {
			t.Fatalf("the statement saw %d cancellations, want 1", stmt.Failures())
		}
		assertNoFamily(t, app)
	})
}

// TestFault_Context_ClientDisconnect_DB: a client that goes away cancels
// its request's context, and the in-flight query ends with context.Canceled.
func TestFault_Context_ClientDisconnect_DB(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		gin.SetMode(gin.TestMode)
		faults, stmt, release := blockedSelect(t, 1)
		app := newFaultApp(t, kind, dsn, faults)
		got := make(chan error, 1)
		handled := make(chan struct{})
		app.Router().GET("/rows", func(c *gin.Context) {
			defer close(handled)
			var rows []faultTxParent
			got <- app.DB().WithContext(c.Request.Context()).Find(&rows).Error
		})
		srv := httptest.NewServer(app.Router())
		defer srv.Close()
		defer release() // before Close, which waits for a stuck handler
		ctx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/rows", nil)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			if resp, err := srv.Client().Do(req); err == nil {
				_ = resp.Body.Close()
			}
		}()
		<-stmt.Reached(1) // the query is in flight
		cancel()          // the client goes away
		guard(t, "the handler", handled, 5*time.Second)
		if err := <-got; !errors.Is(err, context.Canceled) {
			t.Fatalf("the query = %v, want context.Canceled", err)
		}
		faulttest.Idle(t, app.Database())
	})
}

// TestFault_Context_ClientDisconnect_HTTP: a client that goes away cancels
// the handler's outbound call; the dependency sees it abandoned.
func TestFault_Context_ClientDisconnect_HTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	hang := make(chan struct{})
	dep := faulttest.NewHTTPDependency(t, faulttest.Hang(hang))
	app := newTestApp(t)
	got := make(chan error, 1)
	handled := make(chan struct{})
	app.Router().GET("/proxy", func(c *gin.Context) {
		defer close(handled)
		req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, dep.URL(), nil)
		if err == nil {
			var resp *http.Response
			if resp, err = dep.Client().Do(req); err == nil {
				_ = resp.Body.Close()
			}
		}
		got <- err
	})
	srv := httptest.NewServer(app.Router())
	defer srv.Close()
	defer close(hang) // before Close, which waits for a stuck handler
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/proxy", nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		if resp, err := srv.Client().Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-dep.Reached(1) // the outbound call is in flight
	cancel()
	guard(t, "the handler", handled, 5*time.Second)
	if err := <-got; !errors.Is(err, context.Canceled) {
		t.Fatalf("the outbound call = %v, want context.Canceled", err)
	}
	dep.WaitIdle(t)
	if dep.Abandoned() != 1 {
		t.Fatalf("the dependency saw %d abandoned calls, want 1", dep.Abandoned())
	}
}

// TestFault_Context_ShutdownUnderLoad: shutting down with requests stuck
// in the database drains within the bound: /readyz reports draining, RunContext
// returns within drain delay + shutdown timeout, and the stuck requests'
// queries are canceled (none left running).
func TestFault_Context_ShutdownUnderLoad(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		gin.SetMode(gin.TestMode)
		faults, stmt, release := blockedSelect(t, 3)
		const (
			drainDelay = 500 * time.Millisecond
			timeout    = 300 * time.Millisecond
			inFlight   = 3
		)
		app := newFaultApp(t, kind, dsn, faults, WithShutdownDrainDelay(drainDelay), WithShutdownTimeout(timeout))
		results := make(chan error, inFlight)
		entered := make(chan struct{}, inFlight)
		app.Router().GET("/rows", func(c *gin.Context) {
			entered <- struct{}{}
			var rows []faultTxParent
			results <- app.DB().WithContext(c.Request.Context()).Find(&rows).Error
		})

		runCtx, stop := context.WithCancel(context.Background())
		defer stop()
		defer release()
		ran := make(chan error, 1)
		go func() { ran <- RunContext(runCtx, app) }()
		base := waitForAddr(t, app)

		client := &http.Client{}
		for i := 0; i < inFlight; i++ {
			go func() {
				if resp, err := client.Get(base + "/rows"); err == nil {
					_ = resp.Body.Close()
				}
			}()
		}
		// Every request is stuck in the database: in the held query, or
		// waiting for a connection behind it (SQLite has one).
		for i := 0; i < inFlight; i++ {
			<-entered
		}
		<-stmt.Reached(1)

		start := time.Now()
		stop() // begin graceful shutdown
		// Shutdown flips the drain flag first, then keeps serving for the
		// drain delay: wait for the flag (no request needed), then ask
		// /readyz once while the listener is still open.
		for !app.draining.Load() {
			if time.Since(start) > drainDelay {
				t.Fatal("shutdown did not start draining")
			}
			runtime.Gosched()
		}
		resp, err := client.Get(base + "/readyz")
		if err != nil {
			t.Fatalf("/readyz during the drain delay: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), reasonDraining) {
			t.Fatalf("/readyz while draining = %d %s, want 503 %q", resp.StatusCode, body, reasonDraining)
		}

		select {
		case err := <-ran:
			// Requests were still stuck at the timeout: shutdown says so.
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("RunContext = %v, want the shutdown timeout reported (in-flight requests were cut off)", err)
			}
		case <-time.After(drainDelay + timeout + 5*time.Second):
			t.Fatal("RunContext did not return within the drain delay and shutdown timeout")
		}
		if took := time.Since(start); took > drainDelay+timeout+2*time.Second {
			t.Fatalf("shutdown took %s, want about drain delay %s + timeout %s", took, drainDelay, timeout)
		}
		for i := 0; i < inFlight; i++ {
			select {
			case err := <-results:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("a stuck query ended with %v, want context.Canceled", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%d of %d stuck queries still running after shutdown", inFlight-i, inFlight)
			}
		}
		faulttest.Idle(t, app.Database())
	})
}

// waitForAddr waits for RunContext to bind and returns the base URL.
func waitForAddr(t *testing.T, app *App) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if addr := app.Addr(); addr != "" {
			return "http://" + addr
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("RunContext never bound its listener")
	return ""
}
