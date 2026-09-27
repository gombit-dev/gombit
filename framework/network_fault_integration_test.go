//go:build integration

package framework

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/internal/faulttest"
)

// Network fault tests: an App over a real Postgres reached through a
// faulttest.TCPProxy, with transport faults toggled at explicit points.
// INV-2 (bounded waiting: a call ends at its deadline, never waits out a
// dead connection) and INV-6 (recovery: once the fault is lifted the next
// operation succeeds without restarting the app).

type netFaultRow struct {
	ID   uint `gorm:"primaryKey"`
	Name string
}

func (netFaultRow) TableName() string { return "fault_net_rows" }

// proxiedApp is an App whose database is Postgres through a fault proxy,
// with a fresh fault_net_rows table. maxOpen bounds the pool (0: Open's
// default).
func proxiedApp(t *testing.T, maxOpen int) (*App, *faulttest.TCPProxy) {
	t.Helper()
	if *postgresDSN == "" {
		t.Skip("set -framework.postgres-dsn to run network fault tests")
	}
	upstream, err := faulttest.PostgresHostPort(*postgresDSN)
	if err != nil {
		t.Fatal(err)
	}
	proxy := faulttest.NewTCPProxy(t, upstream)
	dsn, err := faulttest.PostgresDSNVia(*postgresDSN, proxy.Addr())
	if err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(config.DatabaseConfig{Driver: config.DatabaseDriverPostgres, DSN: dsn, MaxOpenConns: maxOpen})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = proxy.Heal()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = db.WithContext(ctx).Migrator().DropTable(&netFaultRow{})
		_ = db.Close()
	})
	if err := db.Migrator().DropTable(&netFaultRow{}); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&netFaultRow{}); err != nil {
		t.Fatal(err)
	}
	return newTestApp(t, WithDatabase(db)), proxy
}

// ping runs one statement on ctx through the app's pool.
func ping(ctx context.Context, app *App) error {
	return app.DB().WithContext(ctx).Exec("SELECT 1").Error
}

// withinDeadline runs fn on a context with deadline d and fails t if fn
// outlasts it by more than a second: the deadline, not the dead dependency,
// must end the call (INV-2).
func withinDeadline(t *testing.T, d time.Duration, fn func(ctx context.Context) error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	start := time.Now()
	err := fn(ctx)
	if took := time.Since(start); took > d+time.Second {
		t.Fatalf("the call took %s against a %s deadline", took, d)
	}
	return err
}

// assertRecovers fails t unless, with the fault lifted, the app's next
// independent operation succeeds, with no restart (INV-6).
func assertRecovers(t *testing.T, app *App, proxy *faulttest.TCPProxy) {
	t.Helper()
	if err := proxy.Heal(); err != nil {
		t.Fatal(err)
	}
	if err := withinDeadline(t, 5*time.Second, func(ctx context.Context) error { return ping(ctx, app) }); err != nil {
		t.Fatalf("the first operation after recovery = %v, want success without restarting the app", err)
	}
}

// awaitHeld waits until the proxy holds a call's bytes (sent by the client,
// not yet forwarded to the dependency), failing t if the call returns first
// or never reaches the proxy.
func awaitHeld(t *testing.T, proxy *faulttest.TCPProxy, done <-chan error) {
	t.Helper()
	select {
	case <-proxy.Held():
	case err := <-done:
		t.Fatalf("the call returned (%v) before reaching the held dependency", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the call never reached the held dependency")
	}
}

// TestFault_Network_Unavailable: the database refusing connections (with
// its open ones dropped) fails the next operation at once, not after a
// timeout.
func TestFault_Network_Unavailable(t *testing.T) {
	app, proxy := proxiedApp(t, 0)
	proxy.Refuse()
	proxy.Cut()
	start := time.Now()
	err := withinDeadline(t, 5*time.Second, func(ctx context.Context) error { return ping(ctx, app) })
	if err == nil {
		t.Fatal("an operation against a refusing database succeeded")
	}
	if took := time.Since(start); took >= time.Second {
		t.Fatalf("a refused connection took %s to report, want it at once", took)
	}
	assertRecovers(t, app, proxy)
}

// TestFault_Network_Latency: a database that stops answering (latency past
// any deadline) ends the call at the caller's deadline.
func TestFault_Network_Latency(t *testing.T) {
	app, proxy := proxiedApp(t, 0)
	if err := ping(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	proxy.Hold()
	err := withinDeadline(t, 200*time.Millisecond, func(ctx context.Context) error { return ping(ctx, app) })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a stalled query = %v, want context.DeadlineExceeded", err)
	}
	assertRecovers(t, app, proxy)
}

// TestFault_Network_ConnectionLost: a connection lost while the client is
// waiting on a query, dropped (Cut) or reset, with the database still
// unreachable, fails that query promptly, and the pool recovers. The query
// is held in the proxy (the server has not seen it), so database/sql may
// retry it on a fresh connection; refusing new ones makes the loss final.
// (A connection lost after the server executed a statement is
// TestFault_Network_ConnectionLostMidTransaction.)
func TestFault_Network_ConnectionLost(t *testing.T) {
	for name, drop := range map[string]func(*faulttest.TCPProxy){
		"cut":   (*faulttest.TCPProxy).Cut,
		"reset": (*faulttest.TCPProxy).Reset,
	} {
		t.Run(name, func(t *testing.T) {
			app, proxy := proxiedApp(t, 0)
			if err := ping(context.Background(), app); err != nil {
				t.Fatal(err)
			}
			proxy.Hold()
			done := make(chan error, 1)
			go func() { done <- ping(context.Background(), app) }()
			awaitHeld(t, proxy, done) // the client has sent the query; the proxy holds it
			proxy.Refuse()
			drop(proxy)
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("a query whose connection was lost succeeded")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("a query whose connection was lost did not return")
			}
			assertRecovers(t, app, proxy)
		})
	}
}

// TestFault_Network_ConnectionLostMidTransaction: a transaction whose
// connection drops before COMMIT persists nothing (INV-1 over the wire).
func TestFault_Network_ConnectionLostMidTransaction(t *testing.T) {
	app, proxy := proxiedApp(t, 0)
	err := app.Tx(context.Background(), func(tx *gorm.DB) error {
		if err := tx.Create(&netFaultRow{Name: "lost"}).Error; err != nil {
			return err
		}
		proxy.Cut()
		return nil // App.Tx commits over the dropped connection
	})
	if err == nil {
		t.Fatal("a transaction whose connection dropped before COMMIT reported success")
	}
	assertRecovers(t, app, proxy)
	var n int64
	if err := app.DB().Model(&netFaultRow{}).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d rows persisted from a transaction that never committed", n)
	}
}

// TestFault_Network_PoolExhaustion: with every pooled connection busy, a
// caller waiting for one honors its deadline instead of queueing forever.
func TestFault_Network_PoolExhaustion(t *testing.T) {
	app, proxy := proxiedApp(t, 2)
	var txs []*gorm.DB
	for i := 0; i < 2; i++ {
		tx := app.DB().Begin() // holds one of the two connections
		if tx.Error != nil {
			t.Fatal(tx.Error)
		}
		txs = append(txs, tx)
	}
	release := func() {
		for _, tx := range txs {
			tx.Rollback()
		}
		txs = nil
	}
	defer release() // even when an assertion below fails
	err := withinDeadline(t, 200*time.Millisecond, func(ctx context.Context) error { return ping(ctx, app) })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting on an exhausted pool = %v, want context.DeadlineExceeded", err)
	}
	release()
	assertRecovers(t, app, proxy)
}

// TestFault_Network_ReadyzRecovery: /readyz reports 503 not_ready while the
// database is unreachable or stalled (a fixed reason, no DSN), and 200 again
// once it is back, with no restart.
func TestFault_Network_ReadyzRecovery(t *testing.T) {
	prev := readinessTimeout
	readinessTimeout = 300 * time.Millisecond
	t.Cleanup(func() { readinessTimeout = prev })
	app, proxy := proxiedApp(t, 0)
	readyz := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		app.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return rec
	}
	assertNotReady := func(t *testing.T) {
		t.Helper()
		start := time.Now()
		rec := readyz()
		if took := time.Since(start); took > readinessTimeout+time.Second {
			t.Fatalf("/readyz took %s against a %s probe timeout", took, readinessTimeout)
		}
		body := rec.Body.String()
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(body, `"code":"not_ready"`) ||
			!strings.Contains(body, reasonDatastore) {
			t.Fatalf("/readyz during an outage = %d %s, want 503 not_ready %q", rec.Code, body, reasonDatastore)
		}
		for _, leak := range []string{proxy.Addr(), "postgres://", "password", "gombit:gombit"} {
			if strings.Contains(body, leak) {
				t.Fatalf("/readyz leaked %q: %s", leak, body)
			}
		}
	}
	if rec := readyz(); rec.Code != http.StatusOK {
		t.Fatalf("/readyz before the outage = %d %s", rec.Code, rec.Body.String())
	}

	t.Run("unreachable", func(t *testing.T) {
		proxy.Refuse()
		proxy.Cut()
		assertNotReady(t)
		assertRecovers(t, app, proxy)
		if rec := readyz(); rec.Code != http.StatusOK {
			t.Fatalf("/readyz after recovery = %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("stalled", func(t *testing.T) {
		proxy.Hold()
		assertNotReady(t)
		assertRecovers(t, app, proxy)
		if rec := readyz(); rec.Code != http.StatusOK {
			t.Fatalf("/readyz after recovery = %d %s", rec.Code, rec.Body.String())
		}
	})
}
