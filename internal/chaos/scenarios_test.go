//go:build chaos

package chaos

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/framework"
	"github.com/gombit-dev/gombit/internal/faulttest"
)

func init() {
	register(Scenario{Name: "database/failure-boundary", Component: "database", Run: failureBoundary})
	register(Scenario{Name: "database/concurrent-writers", Component: "database", Run: concurrentWriters})
	register(Scenario{Name: "context/cancellation-stress", Component: "context", Run: cancellationStress})
	register(Scenario{Name: "postgres/interruption", Component: "postgres", Run: postgresInterruption})
	register(Scenario{Name: "http/dependency-faults", Component: "http", Run: httpDependencyFaults})
}

type chaosRow struct {
	ID    uint `gorm:"primaryKey"`
	Batch string
	N     int
}

func (chaosRow) TableName() string { return "chaos_rows" }

type chaosCounter struct {
	ID uint `gorm:"primaryKey"`
	N  int
}

func (chaosCounter) TableName() string { return "chaos_counters" }

// pickDB draws the database: Postgres (when configured) or a fresh SQLite.
func pickDB(t *testing.T, env Environment) (database.Driver, string) {
	t.Helper()
	if env.PostgresDSN != "" && env.Rand.IntN(2) == 0 {
		return database.DriverPostgres, env.PostgresDSN
	}
	return database.DriverSQLite, "file:" + filepath.Join(t.TempDir(), "chaos.db") + "?_fk=1"
}

// newApp is an App over kind's database wrapped with faults, with fresh
// chaos tables; faults are disarmed while the tables are made.
func newApp(t *testing.T, kind database.Driver, dsn string, faults *faulttest.DBFaults, opts ...framework.Option) (*framework.App, *database.DB) {
	t.Helper()
	faults.Disarm()
	db, err := faulttest.OpenDB(kind, dsn, faults)
	if err != nil {
		t.Fatal(err)
	}
	drop := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = db.WithContext(ctx).Migrator().DropTable(&chaosRow{}, &chaosCounter{})
	}
	t.Cleanup(func() {
		faults.Disarm()
		drop()
		_ = db.Close()
	})
	drop()
	if err := db.AutoMigrate(&chaosRow{}, &chaosCounter{}); err != nil {
		t.Fatal(err)
	}
	faults.Arm()
	cfg := config.Default()
	cfg.Environment = config.EnvironmentTest
	cfg.HTTP.Addr = "127.0.0.1:0"
	app, err := framework.New(append([]framework.Option{framework.WithConfig(cfg), framework.WithDatabase(db)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return app, db
}

func countBatch(t *testing.T, app *framework.App, batch string) int64 {
	t.Helper()
	var n int64
	if err := app.DB().Model(&chaosRow{}).Where("batch = ?", batch).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

// failureBoundary: a transaction of n inserts fails at a random statement,
// at COMMIT, or not at all. Invariant: all or nothing (INV-1), and the
// next transaction goes through.
func failureBoundary(t *testing.T, env Environment) {
	kind, dsn := pickDB(t, env)
	n := 2 + env.Rand.IntN(5)
	faults := &faulttest.DBFaults{Match: faulttest.Inserts("chaos_rows")}
	var boundary string
	switch env.Rand.IntN(3) {
	case 0:
		k := 1 + env.Rand.IntN(n)
		faults.Statement = faulttest.FailOnCall(k, faulttest.ErrInjected)
		boundary = fmt.Sprintf("insert %d of %d", k, n)
	case 1:
		faults.Commit = faulttest.FailOnce(faulttest.ErrInjected)
		boundary = fmt.Sprintf("COMMIT after %d inserts", n)
	default:
		boundary = "none"
	}
	env.Drew(t, "%s, %d inserts, fault at %s", kind, n, boundary)
	app, db := newApp(t, kind, dsn, faults)
	batch := fmt.Sprintf("iter-%d", env.Iteration)
	err := app.Tx(context.Background(), func(tx *gorm.DB) error {
		for i := 0; i < n; i++ {
			if err := tx.Create(&chaosRow{Batch: batch, N: i}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	got := countBatch(t, app, batch)
	if boundary == "none" {
		if err != nil || got != int64(n) {
			env.Mismatch(t, fmt.Sprintf("%s: a transaction with no fault", kind), fmt.Sprintf("success and %d rows", n), fmt.Sprintf("%v and %d rows", err, got))
		}
	} else if !errors.Is(err, faulttest.ErrInjected) || got != 0 {
		env.Mismatch(t, fmt.Sprintf("%s: a transaction failing at %s", kind, boundary), "the injected fault and 0 rows", fmt.Sprintf("%v and %d rows", err, got))
	}
	if err := app.Tx(context.Background(), func(tx *gorm.DB) error {
		return tx.Create(&chaosRow{Batch: batch + "-next", N: 0}).Error
	}); err != nil {
		env.Mismatch(t, "the next transaction", "success", err.Error())
	}
	faulttest.Idle(t, db)
}

// concurrentWriters: 2..8 transactions increment one counter at once, a
// random subset of their COMMITs failing. Invariant: the counter equals the
// number of transactions that reported success, and every failure is the
// injected one. The seed fixes how many writers there are and which COMMITs
// (in commit order) fail; which goroutine reaches which COMMIT is the
// scheduler's, and the invariant holds for every assignment.
func concurrentWriters(t *testing.T, env Environment) {
	kind, dsn := pickDB(t, env)
	writers := 2 + env.Rand.IntN(7)
	steps := make([]faulttest.Step, writers)
	failing := 0
	for i := range steps {
		if env.Rand.IntN(10) < 3 {
			steps[i] = faulttest.Failure(faulttest.ErrInjected)
			failing++
		} else {
			steps[i] = faulttest.Success()
		}
	}
	env.Drew(t, "%s, %d writers, %d COMMITs failing", kind, writers, failing)
	faults := &faulttest.DBFaults{Commit: faulttest.Sequence(steps...)}
	app, db := newApp(t, kind, dsn, faults)
	faults.Disarm()
	counter := chaosCounter{}
	if err := app.DB().Create(&counter).Error; err != nil {
		t.Fatal(err)
	}
	faults.Arm()

	results := make(chan error, writers)
	for i := 0; i < writers; i++ {
		go func() {
			results <- app.Tx(context.Background(), func(tx *gorm.DB) error {
				return tx.Model(&chaosCounter{}).Where("id = ?", counter.ID).UpdateColumn("n", gorm.Expr("n + 1")).Error
			})
		}()
	}
	succeeded := 0
	for i := 0; i < writers; i++ {
		select {
		case err := <-results:
			switch {
			case err == nil:
				succeeded++
			case !errors.Is(err, faulttest.ErrInjected):
				env.Mismatch(t, fmt.Sprintf("%s: a concurrent writer", kind), "success or the injected COMMIT fault", err.Error())
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%d of %d writers still running", writers-i, writers)
		}
	}
	var final chaosCounter
	if err := app.DB().First(&final, counter.ID).Error; err != nil {
		t.Fatal(err)
	}
	if succeeded != writers-failing || final.N != succeeded {
		env.Mismatch(t, fmt.Sprintf("%s: %d writers, %d COMMITs failing", kind, writers, failing),
			fmt.Sprintf("%d successes and the counter at %d", writers-failing, writers-failing),
			fmt.Sprintf("%d successes and the counter at %d", succeeded, final.N))
	}
	faulttest.Idle(t, db)
}

// cancellationStress: a transaction of n inserts has its context canceled
// while a random insert, or its COMMIT, is in flight. Invariant: the answer
// matches what persisted (an error means no rows, success means all), and
// no connection is left held.
func cancellationStress(t *testing.T, env Environment) {
	kind, dsn := pickDB(t, env)
	n := 1 + env.Rand.IntN(5)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	faults := &faulttest.DBFaults{Match: faulttest.Inserts("chaos_rows")}
	var reached <-chan struct{}
	var at string
	if k := env.Rand.IntN(n + 1); k < n { // cancel during insert k+1
		steps := make([]faulttest.Step, k+1)
		for i := range steps[:k] {
			steps[i] = faulttest.Success()
		}
		steps[k] = faulttest.Block(release)
		faults.Statement = faulttest.Sequence(steps...)
		reached = faults.Statement.Reached(k + 1)
		at = fmt.Sprintf("insert %d of %d", k+1, n)
	} else { // cancel during COMMIT
		faults.Commit = faulttest.Sequence(faulttest.Block(release))
		reached = faults.Commit.Reached(1)
		at = "COMMIT"
	}
	env.Drew(t, "%s, %d inserts, cancel during %s", kind, n, at)
	app, db := newApp(t, kind, dsn, faults)
	batch := fmt.Sprintf("iter-%d", env.Iteration)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- app.Tx(ctx, func(tx *gorm.DB) error {
			for i := 0; i < n; i++ {
				if err := tx.Create(&chaosRow{Batch: batch, N: i}).Error; err != nil {
					return err
				}
			}
			return nil
		})
	}()
	select {
	case <-reached:
	case err := <-result:
		t.Fatalf("the transaction ended (%v) before reaching %s", err, at)
	case <-time.After(10 * time.Second):
		t.Fatalf("the transaction never reached %s", at)
	}
	cancel()
	close(release) // a held COMMIT (no context) proceeds; a held insert already saw the cancel
	var err error
	select {
	case err = <-result:
	case <-time.After(10 * time.Second):
		t.Fatalf("the transaction canceled at %s did not return", at)
	}
	got := countBatch(t, app, batch)
	if (err == nil && got != int64(n)) || (err != nil && got != 0) {
		env.Mismatch(t, fmt.Sprintf("%s: a transaction canceled during %s", kind, at),
			fmt.Sprintf("success with %d rows, or an error with 0", n), fmt.Sprintf("%v with %d rows", err, got))
	}
	faulttest.Idle(t, db)
}

// postgresInterruption: through a TCP proxy, the database stalls, drops a
// connection mid-query (FIN or RST), or restarts (refuses and drops
// everything). Invariant: the call ends within its deadline (INV-2) and,
// once the fault clears, the next call succeeds without a restart (INV-6).
func postgresInterruption(t *testing.T, env Environment) {
	if env.PostgresDSN == "" {
		t.Skip("set CHAOS_POSTGRES_DSN for the Postgres scenarios")
	}
	upstream, err := faulttest.PostgresHostPort(env.PostgresDSN)
	if err != nil {
		t.Fatal(err)
	}
	proxy := faulttest.NewTCPProxy(t, upstream)
	dsn, err := faulttest.PostgresDSNVia(env.PostgresDSN, proxy.Addr())
	if err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(config.DatabaseConfig{Driver: config.DatabaseDriverPostgres, DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Heal(); _ = db.Close() })
	ping := func(ctx context.Context) error { return db.WithContext(ctx).Exec("SELECT 1").Error }
	if err := ping(context.Background()); err != nil {
		t.Fatal(err)
	}

	deadline := time.Duration(100+env.Rand.IntN(400)) * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	fault := [...]string{"stall", "cut", "reset", "restart"}[env.Rand.IntN(4)]
	env.Drew(t, "postgres via proxy, fault %s, deadline %s", fault, deadline)
	start := time.Now()
	switch fault {
	case "stall":
		proxy.Hold()
		err = ping(ctx)
	case "cut", "reset":
		proxy.Hold()
		done := make(chan error, 1)
		go func() { done <- ping(ctx) }()
		select {
		case <-proxy.Held():
		case err := <-done:
			t.Fatalf("the query returned (%v) before reaching the proxy", err)
		case <-time.After(10 * time.Second):
			t.Fatal("the query never reached the proxy")
		}
		proxy.Refuse()
		if fault == "cut" {
			proxy.Cut()
		} else {
			proxy.Reset()
		}
		err = <-done
	case "restart":
		proxy.Refuse()
		proxy.Cut()
		err = ping(ctx)
	}
	if took := time.Since(start); err == nil || took > deadline+time.Second {
		env.Mismatch(t, fmt.Sprintf("a query against a Postgres that %s", fault),
			fmt.Sprintf("an error within the %s deadline", deadline), fmt.Sprintf("%v after %s", err, took))
	}
	if err := proxy.Heal(); err != nil {
		t.Fatal(err)
	}
	rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rcancel()
	if err := ping(rctx); err != nil {
		env.Mismatch(t, fmt.Sprintf("the first query after the %s cleared", fault), "success, with no restart", err.Error())
	}
}

// httpDependencyFaults: a handler behind HTTP.RequestTimeout calls a
// dependency that answers with a random script of faults. Invariant: every
// request ends within the timeout's bound, every response body is closed,
// and no call is left running on the dependency.
func httpDependencyFaults(t *testing.T, env Environment) {
	gin.SetMode(gin.TestMode)
	menu := []func() faulttest.HTTPStep{
		faulttest.ServerError,
		func() faulttest.HTTPStep { return faulttest.TooManyRequests(time.Second) },
		func() faulttest.HTTPStep { return faulttest.Hang(make(chan struct{})) },
		func() faulttest.HTTPStep { return faulttest.CutBody(`{"partial":`) },
		faulttest.ResetConnection,
		func() faulttest.HTTPStep { return faulttest.Respond(http.StatusOK, `{}`) },
	}
	calls := 1 + env.Rand.IntN(5)
	steps := make([]faulttest.HTTPStep, calls)
	for i := range steps {
		steps[i] = menu[env.Rand.IntN(len(menu))]()
	}
	names := make([]string, len(steps))
	for i, step := range steps {
		names[i] = step.String()
	}
	dep := faulttest.NewHTTPDependency(t, steps...)
	tracker := faulttest.TrackBodies(dep.Client().Transport)
	client := &http.Client{Transport: tracker}

	cfg := config.Default()
	cfg.Environment = config.EnvironmentTest
	cfg.HTTP.Addr = "127.0.0.1:0"
	timeout := time.Duration(100+env.Rand.IntN(300)) * time.Millisecond
	cfg.HTTP.RequestTimeout = timeout
	env.Drew(t, "request timeout %s, dependency script %v", timeout, names)
	app, err := framework.New(framework.WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	app.Router().GET("/call", func(c *gin.Context) {
		req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, dep.URL(), nil)
		if err == nil {
			var resp *http.Response
			if resp, err = client.Do(req); err == nil {
				_ = resp.Body.Close()
			}
		}
		if err != nil {
			c.Status(http.StatusBadGateway)
			return
		}
		c.Status(http.StatusOK)
	})
	for i, step := range steps {
		start := time.Now()
		done := make(chan struct{})
		go func() {
			defer close(done)
			app.Router().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/call", nil))
		}()
		select {
		case <-done:
		case <-time.After(timeout + 5*time.Second):
			env.Mismatch(t, fmt.Sprintf("call %d (%s)", i+1, step), fmt.Sprintf("the request ends within its %s timeout", timeout), "still running")
			return
		}
		if took := time.Since(start); took > timeout+time.Second {
			env.Mismatch(t, fmt.Sprintf("call %d (%s)", i+1, step), fmt.Sprintf("the request ends within its %s timeout", timeout), fmt.Sprintf("took %s", took))
		}
	}
	if tracker.Open() != 0 {
		env.Mismatch(t, "response bodies", "all closed", fmt.Sprintf("%d open", tracker.Open()))
	}
	dep.WaitIdle(t)
}
