package framework

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humagin"
	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/admin"
	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/cache"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/internal/adminui"
	"github.com/gombit-dev/gombit/jobs"
	"github.com/gombit-dev/gombit/logging"
	"github.com/gombit-dev/gombit/storage"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

const defaultShutdownTimeout = 10 * time.Second

// defaultHTTPServerTimeout is the connection-level read/write/idle timeout the
// http.Server falls back to when the per-handler request timeout is disabled
// (HTTP.RequestTimeout <= 0, the default since issue #270 / PERF-12). The
// per-handler context deadline is opt-in, but the connection-level safety net
// against slow or stuck sockets is not — a disabled per-handler deadline must
// not mean an unbounded ReadTimeout/WriteTimeout/IdleTimeout. When
// RequestTimeout is set, it drives all three, WriteTimeout plus
// requestTimeoutWriteGrace.
const defaultHTTPServerTimeout = 60 * time.Second

// requestTimeoutWriteGrace is how far the connection write deadline outlasts
// the per-handler deadline when HTTP.RequestTimeout is set (issue #430).
//
// net/http arms the write deadline when it finishes reading the request
// headers, which is before request_context derives the handler's context. With
// equal durations the write deadline therefore expires first, and a handler
// that honors its context and then writes its timeout response (504, D10
// envelope with request_id) has that write dropped: the client sees the
// connection close instead. The grace leaves the handler time to return and
// write its response after its own deadline fires.
const requestTimeoutWriteGrace = 5 * time.Second

// Hook is an application lifecycle callback.
type Hook func(context.Context) error

// Option configures an App.
type Option func(*App) error

// App owns Gombit's runtime lifecycle and HTTP router.
type App struct {
	cfg        config.Config
	cfgSet     bool
	cache      cache.Cache
	cacheStore *cache.Store
	cacheOwned bool
	redis      *redis.Client
	jobs       *jobs.Dispatcher
	jobsOwned  bool
	jobMetrics *jobs.Metrics
	storage    storage.Storage
	// storageOrigins are the store's URL origins for the SPA pages' CSP
	// (spaStorageOrigins).
	storageOrigins     []string
	storageOriginsOnce sync.Once
	workerQueues       []string // consumed by RunWorker in this process
	db                 *database.DB
	logger             *zap.Logger
	router             *gin.Engine
	storageRoute       *storageRoute // the storage route New mounted, if any (nil with WithRouter)
	api                huma.API
	startHooks         []Hook
	stopHooks          []Hook
	shutdownTimeout    time.Duration
	shutdownDrainDelay time.Duration
	embeddedFrontend   fs.FS
	csrfExemptPaths    []string
	rawBodyPaths       []string

	mu     sync.RWMutex
	server *http.Server
	addr   string

	// draining is set when graceful shutdown begins so /readyz reports 503
	// and a host deregisters the instance before in-flight requests finish
	// (HOST-2 / ADR-015).
	draining atomic.Bool

	// readyProbe checks the configured datastore for /readyz. nil when no
	// datastore is attached (readiness then depends only on draining).
	// Defaults to pingDatabase; overridable in tests.
	readyProbe func(context.Context) error
}

type namedMiddleware struct {
	name    string
	handler gin.HandlerFunc
}

// New creates an application using process configuration and the default router.
func New(options ...Option) (_ *App, err error) {
	app := &App{
		cfg:             config.Default(),
		shutdownTimeout: defaultShutdownTimeout,
		jobMetrics:      jobs.NewMetrics(),
	}
	// A failed New never hands the caller an *App, so nothing else can release
	// what it opened: the cache (whose in-memory driver runs a janitor
	// goroutine, and whose Redis driver holds a pool) and the job dispatcher
	// (issue #435). Only what New opened itself is closed; a cache or
	// dispatcher passed in with WithCache/WithJobs belongs to the caller.
	defer func() {
		if err != nil {
			if releaseErr := app.releaseOwned(); releaseErr != nil {
				err = errors.Join(err, releaseErr)
			}
		}
	}()

	for _, option := range options {
		if option == nil {
			continue
		}
		if err := option(app); err != nil {
			return nil, err
		}
	}

	if !app.cfgSet {
		cfg, err := config.Load()
		if err != nil {
			return nil, err
		}
		app.cfg = cfg
	}

	if err := app.cfg.Validate(); err != nil {
		return nil, err
	}
	if app.shutdownTimeout <= 0 {
		return nil, errors.New("framework: shutdown timeout must be positive")
	}
	configureHTTPMode(app.cfg)
	if app.logger == nil {
		logger, err := logging.New(app.cfg.Logging)
		if err != nil {
			return nil, err
		}
		app.logger = logger
		app.OnStop(func(context.Context) error {
			return syncLogger(logger)
		})
	}
	if app.router == nil {
		router, route, err := newRouter(app.cfg, app.csrfExemptPaths, app.rawBodyPaths, app.handleReadyz, app.writeJobMetrics)
		if err != nil {
			return nil, err
		}
		app.router = router
		app.storageRoute = route
	} else if err := configureTrustedProxies(app.router, app.cfg.HTTP.TrustedProxies); err != nil {
		return nil, err
	}
	if app.api == nil {
		contract.Install(contract.InstallOptions{
			RequestID: GetRequestIDFromContext,
		})
		// OpenAPI Info.Version stays 0.0.0 until runtime versioning lands.
		humaConfig := contract.HumaConfigFor(app.cfg.AppName, "0.0.0", app.cfg.API.DocsEnabled)
		// A response that cannot be encoded is answered as a D10 500; log why
		// through the app's logger (#442).
		logger := app.logger
		humaConfig.Formats = contract.JSONFormats(func(requestID string, err error) {
			logger.Error("http: response could not be encoded", zap.String("request_id", requestID), zap.Error(err))
		})
		app.api = humagin.New(app.router, humaConfig)
	}
	if app.cache == nil {
		store, err := cache.Open(app.cfg.Cache)
		if err != nil {
			return nil, err
		}
		app.cache = store
		app.cacheStore = store
		app.cacheOwned = true
		app.redis = store.Redis()
	}
	if app.jobs == nil {
		registry := jobs.NewRegistry(jobs.WithPropagator(JobPropagator()), jobs.WithPropagator(jobs.OTelPropagator()))
		var dispatcher *jobs.Dispatcher
		var err error
		if app.cfg.Jobs.Driver == config.JobsDriverRedis && app.redis != nil {
			// The app already has a Redis client (WithRedis, or the cache's):
			// queue against the server it talks to rather than dial
			// GOMBIT_REDIS_*, which could even be a different one. The queue
			// gets its own pool, tuned for queue calls.
			dispatcher, err = jobs.OpenWithRedis(app.cfg.Jobs, app.redis, registry)
		} else {
			dispatcher, err = jobs.Open(app.cfg.Jobs, app.cfg.Cache.Redis, registry)
		}
		if err != nil {
			return nil, err
		}
		app.jobs = dispatcher
		app.jobsOwned = true
	}
	if app.storage == nil {
		store, signer, err := openStorage(app.cfg, app.logger)
		if err != nil {
			return nil, err
		}
		app.storage = store
		if signer != nil {
			// The local and memory drivers' URLs, served by the app. An app
			// that passes its own store (WithStorage) mounts presign.Handler
			// itself if it wants them.
			if err := mountStorageURLs(app.router, store, signer, app.storageRoute); err != nil {
				return nil, err
			}
		}
	}
	if app.cfg.Auth.Enabled() {
		if app.db == nil || app.db.DB == nil {
			return nil, errors.New("framework: JWT secret is set but no database is attached")
		}
		if err := auth.Mount(app.api, app.db.DB, app.cfg); err != nil {
			return nil, err
		}
		// Admin introspection + data plane mount only in cookie mode
		// (ADR-013). JWT-only apps do not grow admin routes.
		if app.cfg.Auth.EffectiveMode() == config.AuthModeCookie {
			if err := admin.Mount(app); err != nil {
				return nil, err
			}
			// Framework-owned admin SPA (ADMIN-2 / ADR-013). Explicit Gin
			// routes so /admin wins over Huma and over the app NoRoute SPA.
			mountAdminSPA(app.router, adminui.FS(), app.cfg.API.Prefix, app.spaStorageOrigins()...)
		}
	}
	if app.embeddedFrontend != nil {
		mountEmbeddedFrontend(app.router, app.embeddedFrontend, app.cfg.API.Prefix, app.spaStorageOrigins()...)
	}

	// A /readyz datastore probe exists only when a database is attached
	// (HOST-2 / ADR-015). Apps with no datastore are ready on the drain flag
	// alone.
	if app.readyProbe == nil && app.db != nil && app.db.DB != nil {
		app.readyProbe = app.pingDatabase
	}

	// Database logging goes through the app's logger, so it honors
	// GOMBIT_LOG_SINK / GOMBIT_LOG_LEVEL and never prints parameter values
	// (issue #439). Done last: the database is the caller's, and a New that
	// fails must not leave it logging through an app that never ran. A GORM
	// logger the caller set on it is kept.
	if app.db != nil {
		app.db.ReplaceDefaultLogger(database.NewLogger(app.logger.Named("database")))
	}

	return app, nil
}

// WithConfig sets the app configuration.
// DocsEnabled is taken as given: Default() leaves /docs on even if you later
// set Environment to production. Use config.DefaultFor(env) or set
// API.DocsEnabled yourself.
func WithConfig(cfg config.Config) Option {
	return func(app *App) error {
		if err := cfg.Validate(); err != nil {
			return err
		}
		app.cfg = cfg
		app.cfgSet = true
		return nil
	}
}

// WithCache attaches a cache implementation the caller opened. Unlike the
// cache App opens for itself via cache.Open (closed automatically on
// shutdown), App does not call Close on a cache attached this way — the
// caller keeps ownership and is responsible for closing it, including
// stopping a cache.Memory janitor goroutine started with cache.WithJanitor.
func WithCache(c cache.Cache) Option {
	return func(app *App) error {
		if c == nil {
			return errors.New("framework: nil cache")
		}
		app.cache = c
		if store, ok := c.(*cache.Store); ok {
			app.cacheStore = store
			app.redis = store.Redis()
		}
		return nil
	}
}

// WithJobs attaches a job dispatcher the caller opened, instead of the one
// App opens from Config.Jobs. App does not close a dispatcher attached this
// way.
func WithJobs(dispatcher *jobs.Dispatcher) Option {
	return func(app *App) error {
		if dispatcher == nil {
			return errors.New("framework: nil job dispatcher")
		}
		app.jobs = dispatcher
		app.jobsOwned = false
		return nil
	}
}

// WithStorage attaches an object store the caller opened (a driver, or a
// test double such as storage/memory), instead of the one App opens from
// Config.Storage.
func WithStorage(s storage.Storage) Option {
	return func(app *App) error {
		if s == nil {
			return errors.New("framework: nil storage")
		}
		app.storage = s
		return nil
	}
}

// WithRedis attaches an application-owned Redis client as the app cache.
func WithRedis(client *redis.Client) Option {
	return func(app *App) error {
		if client == nil {
			return errors.New("framework: nil redis client")
		}
		app.cache = cache.NewRedis(client)
		app.redis = client
		return nil
	}
}

// WithDatabase attaches an opened database handle to the app.
func WithDatabase(db *database.DB) Option {
	return func(app *App) error {
		if db == nil || db.DB == nil {
			return errors.New("framework: nil database")
		}
		app.db = db
		return nil
	}
}

// WithLogger attaches a Zap logger to the app.
func WithLogger(logger *zap.Logger) Option {
	return func(app *App) error {
		if logger == nil {
			return errors.New("framework: nil logger")
		}
		app.logger = logger
		return nil
	}
}

// WithRouter sets the app router.
func WithRouter(router *gin.Engine) Option {
	return func(app *App) error {
		if router == nil {
			return errors.New("framework: nil router")
		}
		app.router = router
		return nil
	}
}

// WithShutdownDrainDelay sets a delay between the start of graceful shutdown —
// when /readyz begins returning 503 (HOST-2 / ADR-015) — and the moment the
// HTTP listener stops accepting connections. During the delay the server keeps
// serving, so a host that gates traffic on /readyz observes the 503 and
// deregisters the instance before any request is connection-refused. Without a
// delay (the default, 0) the flag still sheds in-flight and keep-alive pollers
// during the shutdown grace period, but a host opening a fresh probe connection
// after shutdown begins sees connection-refused rather than a 503. Set this to
// a host's readiness poll interval (a few seconds) for a clean drain.
func WithShutdownDrainDelay(delay time.Duration) Option {
	return func(app *App) error {
		if delay < 0 {
			return errors.New("framework: shutdown drain delay must not be negative")
		}
		app.shutdownDrainDelay = delay
		return nil
	}
}

// WithShutdownTimeout sets the bounded shutdown timeout.
func WithShutdownTimeout(timeout time.Duration) Option {
	return func(app *App) error {
		if timeout <= 0 {
			return errors.New("framework: shutdown timeout must be positive")
		}
		app.shutdownTimeout = timeout
		return nil
	}
}

// WithCSRFExemptPaths marks exact request paths that opt out of cookie-mode
// CSRF enforcement on unsafe methods. Use it for non-browser endpoints that
// cannot participate in the double-submit defense — webhooks, server-to-server
// callbacks — which must authenticate themselves by other means (e.g. HMAC
// signature verification). Paths match the request path exactly, including the
// API prefix, e.g. "/api/v1/webhooks/github". No effect in JWT mode or when a
// custom router is supplied via WithRouter.
func WithCSRFExemptPaths(paths ...string) Option {
	return func(app *App) error {
		app.csrfExemptPaths = append(app.csrfExemptPaths, paths...)
		return nil
	}
}

// WithRawBodyPaths marks exact request paths whose request body must reach the
// handler byte-for-byte unmodified — webhooks and other server-to-server
// endpoints that verify a signature over the raw body (e.g. GitHub's
// X-Hub-Signature-256 HMAC). The XSS input sanitizer, which otherwise
// re-encodes JSON request bodies, is skipped for these paths.
//
// Such an endpoint also cannot participate in the cookie CSRF double-submit, so
// raw-body paths are additionally CSRF-exempt (the union with
// WithCSRFExemptPaths) — declaring a webhook path here is enough. The handler
// must authenticate the caller itself. Paths match the request path exactly,
// including the API prefix, e.g. "/api/v1/webhooks/github". No effect when a
// custom router is supplied via WithRouter.
func WithRawBodyPaths(paths ...string) Option {
	return func(app *App) error {
		app.rawBodyPaths = append(app.rawBodyPaths, paths...)
		return nil
	}
}

// Config returns the typed app configuration.
func (a *App) Config() config.Config {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg
}

// Jobs returns the job dispatcher: register jobs on Jobs().Registry() at
// startup, and dispatch with Jobs().Dispatch. The driver comes from
// Config.Jobs (GOMBIT_JOBS_DRIVER, sync by default), and the registry
// carries request and trace IDs into job handlers (JobPropagator).
func (a *App) Jobs() *jobs.Dispatcher {
	return a.jobs
}

// Storage returns the app's object store: the driver Config.Storage names
// (local files by default), or the one attached with WithStorage.
func (a *App) Storage() storage.Storage {
	return a.storage
}

// Cache returns the configured cache implementation.
func (a *App) Cache() cache.Cache {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cache
}

// Redis returns the underlying go-redis client when Redis is enabled.
func (a *App) Redis() *redis.Client {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.redis
}

// Logger returns the app's Zap logger.
func (a *App) Logger() *zap.Logger {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.logger
}

// Database returns the opened database handle with driver metadata.
func (a *App) Database() *database.DB {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.db
}

// DB returns the underlying GORM database escape hatch.
func (a *App) DB() *gorm.DB {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.db == nil {
		return nil
	}
	return a.db.DB
}

// Tx runs fn inside a single database transaction: it commits when fn returns
// nil and rolls back on any error or panic. It is the framework home for
// transactional multi-model writes (atomically update A + B + C) and for
// cross-row invariants that must read and write consistently. Model Validate
// hooks (database.Validator) run inside this transaction too. Use struct writes
// (Create/Save/Updates(struct)) so Validate sees the values being written — an
// invariant checked in Validate then cannot commit alongside a change that
// violates it.
//
//	err := app.Tx(ctx, func(tx *gorm.DB) error {
//	    if err := tx.Create(&a).Error; err != nil { return err }
//	    b.Count++
//	    return tx.Save(&b).Error
//	})
func (a *App) Tx(ctx context.Context, fn func(tx *gorm.DB) error) error {
	db := a.DB()
	if db == nil {
		return errors.New("framework: no database attached")
	}
	if fn == nil {
		return errors.New("framework: nil transaction function")
	}
	return db.WithContext(ctx).Transaction(fn)
}

// Router returns the underlying Gin router escape hatch.
func (a *App) Router() *gin.Engine {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.router
}

// API returns the Huma API used for contract-typed route registration.
func (a *App) API() huma.API {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.api
}

// Addr returns the bound HTTP address after the app has started.
func (a *App) Addr() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.addr
}

// OnStart registers a start hook. Hooks run in registration order.
func (a *App) OnStart(hook Hook) {
	if hook == nil {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.startHooks = append(a.startHooks, hook)
}

// OnStop registers a stop hook. Hooks run in reverse registration order.
func (a *App) OnStop(hook Hook) {
	if hook == nil {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopHooks = append(a.stopHooks, hook)
}

// Run runs app until an interrupt or terminate signal is received: the HTTP
// server, or, when the process's first argument is "worker", the jobs worker
// (see RunWorker; `./server worker --help` lists its flags). Other arguments
// are ignored, as before.
func Run(app *App) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(os.Args) > 1 && os.Args[1] == WorkerCommand {
		return runWorkerCommand(ctx, app, os.Args[2:], os.Stderr)
	}
	return RunContext(ctx, app)
}

// RunContext runs app until ctx is canceled or the HTTP server fails. Every
// return runs the stop hooks and closes what the app opened, so an App runs
// once: it cannot be run again after RunContext returns (issue #435).
func RunContext(ctx context.Context, app *App) error {
	if app == nil {
		return errors.New("framework: nil app")
	}
	if ctx == nil {
		return errors.Join(errors.New("framework: nil context"), app.runStopHooks())
	}

	listener, err := net.Listen("tcp", app.Config().HTTP.Addr)
	if err != nil {
		return errors.Join(fmt.Errorf("framework: listen: %w", err), app.runStopHooks())
	}

	// The per-handler context deadline (HTTP.RequestTimeout) is opt-in and off by
	// default (issue #270), but the connection-level timeouts are a safety net
	// that must stay on. When RequestTimeout is set it drives all three, and the
	// write timeout gets requestTimeoutWriteGrace on top so the handler's own
	// timeout response can still be written (issue #430); when it is disabled
	// they fall back to defaultHTTPServerTimeout rather than 0 (unbounded), and
	// there is no handler deadline to outlast.
	serverTimeout := app.Config().HTTP.RequestTimeout
	writeTimeout := serverTimeout + requestTimeoutWriteGrace
	if serverTimeout <= 0 {
		serverTimeout = defaultHTTPServerTimeout
		writeTimeout = defaultHTTPServerTimeout
	}
	server := &http.Server{
		Handler:           app.Router(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       serverTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       serverTimeout,
	}
	app.setServer(server, listener.Addr().String())

	if err := app.runStartHooks(ctx); err != nil {
		_ = listener.Close()
		return errors.Join(err, app.runStopHooks())
	}

	serverErr := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	select {
	case <-ctx.Done():
		err := app.shutdown()
		if serveErr := <-serverErr; serveErr != nil {
			err = errors.Join(err, fmt.Errorf("framework: serve: %w", serveErr))
		}
		return err
	case err := <-serverErr:
		stopErr := app.runStopHooks()
		if err != nil {
			return errors.Join(fmt.Errorf("framework: serve: %w", err), stopErr)
		}
		return stopErr
	}
}

func (a *App) setServer(server *http.Server, addr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.server = server
	a.addr = addr
}

func (a *App) snapshotStartHooks() []Hook {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return append([]Hook(nil), a.startHooks...)
}

func (a *App) snapshotStopHooks() []Hook {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return append([]Hook(nil), a.stopHooks...)
}

func (a *App) runStartHooks(ctx context.Context) error {
	for i, hook := range a.snapshotStartHooks() {
		if err := hook(ctx); err != nil {
			return fmt.Errorf("framework: start hook %d: %w", i+1, err)
		}
	}
	return nil
}

func (a *App) shutdown() error {
	// Flip readiness to 503 before draining so a host deregisters the instance
	// while in-flight requests finish (HOST-2 / ADR-015).
	a.draining.Store(true)

	a.mu.RLock()
	server := a.server
	timeout := a.shutdownTimeout
	drainDelay := a.shutdownDrainDelay
	a.mu.RUnlock()

	// Keep serving for the drain delay so a host polling /readyz sees the 503
	// and stops routing before the listener closes.
	if drainDelay > 0 {
		timer := time.NewTimer(drainDelay)
		<-timer.C
	}

	// The HTTP drain and the stop hooks get separate budgets (issue #431). A
	// request that outlives the drain consumes all of its context, and hooks
	// handed that same context would start with it already expired: any hook
	// that honors its context (flush a buffer, close a pool) would fail at once,
	// exactly when shutdown is already going badly. runStopHooks gives them their
	// own shutdownTimeout, as the worker's shutdown already does.
	var drainErr error
	if server != nil {
		drainCtx, cancel := context.WithTimeout(context.Background(), timeout)
		err := server.Shutdown(drainCtx)
		cancel()
		if err != nil {
			_ = server.Close()
			drainErr = fmt.Errorf("framework: shutdown: %w", err)
		}
	}

	return errors.Join(drainErr, a.runStopHooks())
}

func (a *App) runStopHooks() error {
	a.mu.RLock()
	timeout := a.shutdownTimeout
	a.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return errors.Join(a.runStopHooksWithContext(ctx), a.releaseOwned())
}

// releaseOwned closes the job dispatcher and cache the app opened itself, in
// that order (see closeOwnedJobs). Each is closed at most once.
func (a *App) releaseOwned() error {
	return errors.Join(a.closeOwnedJobs(), a.closeOwnedCache())
}

func (a *App) closeOwnedCache() error {
	a.mu.Lock()
	store := a.cacheStore
	owned := a.cacheOwned
	if owned {
		a.cacheOwned = false
	}
	a.mu.Unlock()
	if !owned || store == nil {
		return nil
	}
	return store.Close()
}

// closeOwnedJobs runs before closeOwnedCache: a Redis job queue may borrow
// the cache's client.
func (a *App) closeOwnedJobs() error {
	a.mu.Lock()
	dispatcher := a.jobs
	owned := a.jobsOwned
	a.jobsOwned = false
	a.mu.Unlock()
	if !owned || dispatcher == nil {
		return nil
	}
	return dispatcher.Close()
}

func (a *App) runStopHooksWithContext(ctx context.Context) error {
	hooks := a.snapshotStopHooks()
	var joined error
	for i := len(hooks) - 1; i >= 0; i-- {
		if err := hooks[i](ctx); err != nil {
			joined = errors.Join(joined, fmt.Errorf("framework: stop hook %d: %w", i+1, err))
		}
	}
	return joined
}

func syncLogger(logger *zap.Logger) error {
	if err := logger.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) {
		return fmt.Errorf("framework: sync logger: %w", err)
	}
	return nil
}

func newRouter(cfg config.Config, csrfExemptPaths, rawBodyPaths []string, readyz gin.HandlerFunc, extraMetrics func(context.Context, io.Writer)) (*gin.Engine, *storageRoute, error) {
	router := gin.New()
	enableMethodNotAllowed(router)
	// Unmatched paths get the D10 404 (issue #438). WithEmbeddedFrontend
	// replaces this NoRoute with the SPA fallback, which answers reserved and
	// API paths the same way.
	router.NoRoute(abortNotFound)
	if err := configureTrustedProxies(router, cfg.HTTP.TrustedProxies); err != nil {
		return nil, nil, err
	}

	metrics := newHTTPMetrics()
	route := new(storageRoute) // set only if New mounts the storage route
	router.Use(middlewareHandlers(runtimeMiddlewareStack(cfg, metrics, csrfExemptPaths, rawBodyPaths, route))...)
	// Operational probes (HOST-2 / ADR-015). Raw Gin, out of OpenAPI — like
	// /metrics and the admin SPA. /livez is liveness (process up); /readyz is
	// readiness (safe to receive traffic). Hosts gate traffic on /readyz.
	router.GET("/livez", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"data": gin.H{
				"status": "ok",
			},
		})
	})
	router.GET("/readyz", readyz)
	router.GET("/metrics", func(c *gin.Context) {
		var b strings.Builder
		b.WriteString(metrics.render())
		if extraMetrics != nil {
			extraMetrics(c.Request.Context(), &b)
		}
		c.Data(http.StatusOK, "text/plain; version=0.0.4; charset=utf-8", []byte(b.String()))
	})
	return router, route, nil
}

// addWorkerQueues records queues a worker in this process consumes, for the
// queue gauges on /metrics.
func (a *App) addWorkerQueues(queues []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, q := range queues {
		if !slices.Contains(a.workerQueues, q) {
			a.workerQueues = append(a.workerQueues, q)
		}
	}
}

// writeJobMetrics appends the metrics of workers running in this process
// (RunWorker), when there are any, with the depth of the queues they
// consume. A queue whose stats cannot be read is left out, logged, and
// reported by gombit_jobs_queue_stats_up{queue} 0, rather than failing the
// app's whole scrape (HTTP metrics included) during a queue outage.
func (a *App) writeJobMetrics(ctx context.Context, w io.Writer) {
	a.mu.RLock()
	queues := slices.Clone(a.workerQueues)
	a.mu.RUnlock()
	if a.jobMetrics == nil || (a.jobMetrics.Empty() && len(queues) == 0) {
		return
	}
	var stats map[string]jobs.QueueStats
	var up []string
	if len(queues) > 0 {
		ctx, cancel := context.WithTimeout(ctx, queueStatsTimeout)
		defer cancel()
		var err error
		stats, err = readQueueStats(ctx, a.jobs.Queue(), queues)
		if err != nil {
			a.Logger().Warn("metrics: reading job queue stats failed", zap.Error(err))
		}
		for _, q := range queues {
			v := "1"
			if _, ok := stats[q]; !ok {
				v = "0"
			}
			up = append(up, fmt.Sprintf("gombit_jobs_queue_stats_up{queue=%q} %s\n", q, v))
		}
	}
	_ = a.jobMetrics.WritePrometheus(w, stats, time.Now())
	if len(up) > 0 {
		_, _ = io.WriteString(w, "# HELP gombit_jobs_queue_stats_up Whether the queue's stats were read for this scrape.\n# TYPE gombit_jobs_queue_stats_up gauge\n"+strings.Join(up, ""))
	}
}

// JobMetrics returns the metrics workers in this process record into.
func (a *App) JobMetrics() *jobs.Metrics { return a.jobMetrics }

// readinessTimeout bounds the datastore probe so a hung datastore cannot hang
// the /readyz handler (and, with it, a host's traffic-gating decision). A var,
// not a const, so tests can shrink it.
var readinessTimeout = 2 * time.Second

// Stable, non-sensitive reasons surfaced on the public /readyz probe. The
// underlying cause (which may embed a DSN — user=, host=, database=) is logged,
// never returned to the caller.
const (
	reasonDraining  = "shutting down"
	reasonDatastore = "datastore unavailable"
)

// handleReadyz serves the /readyz readiness probe (HOST-2 / ADR-015). It
// reports 200 only when the app is not shutting down and the configured
// datastore is reachable; otherwise 503 with a D10 not_ready envelope whose
// message is a fixed reason (never the raw datastore error). Start hooks need
// no flag here: RunContext serves only after they succeed, so any request
// reaching this handler is already past startup.
func (a *App) handleReadyz(c *gin.Context) {
	if reason, ok := a.readiness(c.Request.Context()); !ok {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{
				"code":       "not_ready",
				"message":    reason,
				"request_id": GetRequestID(c),
			},
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"status": "ready",
		},
	})
}

// readiness reports whether the app can receive traffic, with a stable reason
// when it cannot. Readiness is: not draining, and (when a datastore is
// attached) the datastore probe succeeds within readinessTimeout. The probe's
// error is logged, not returned, so a DSN never reaches the public probe.
func (a *App) readiness(ctx context.Context) (reason string, ok bool) {
	if a.draining.Load() {
		return reasonDraining, false
	}

	a.mu.RLock()
	probe := a.readyProbe
	a.mu.RUnlock()

	if probe == nil {
		return "", true
	}

	probeCtx, cancel := context.WithTimeout(ctx, readinessTimeout)
	defer cancel()
	if err := probe(probeCtx); err != nil {
		if a.logger != nil {
			a.logger.Warn("readiness: datastore probe failed", zap.Error(err))
		}
		return reasonDatastore, false
	}
	return "", true
}

// pingDatabase is the default datastore readiness probe: a bounded ping of the
// attached database's connection pool.
func (a *App) pingDatabase(ctx context.Context) error {
	a.mu.RLock()
	db := a.db
	a.mu.RUnlock()
	if db == nil || db.DB == nil {
		return nil
	}
	sqlDB, err := db.SQLDB()
	if err != nil {
		return err
	}
	return sqlDB.PingContext(ctx)
}

func configureHTTPMode(cfg config.Config) {
	if cfg.Environment == config.EnvironmentProduction {
		gin.SetMode(gin.ReleaseMode)
	}
}

func configureTrustedProxies(engine *gin.Engine, proxies []string) error {
	if err := engine.SetTrustedProxies(proxies); err != nil {
		return fmt.Errorf("framework: trusted proxies: %w", err)
	}
	return nil
}

func runtimeMiddlewareStack(cfg config.Config, metrics *httpMetrics, csrfExemptPaths, rawBodyPaths []string, route *storageRoute) []namedMiddleware {
	stack := []namedMiddleware{
		{name: "recovery", handler: recoverWithEnvelope()},
		// request_context also imposes the per-handler timeout (issue #268): the
		// two IDs and the deadline ride one Request.WithContext, and the former
		// standalone request_timeout layer is gone.
		{name: "request_context", handler: requestContextMiddleware(cfg.HTTP.RequestTimeout)},
		{name: "metrics", handler: metricsMiddleware(metrics)},
		{
			name:    "security_headers",
			handler: securityHeadersMiddleware(cfg.Environment == config.EnvironmentProduction, cfg.API.DocsEnabled),
		},
		// Request-body size limit: an oversized JSON body is rejected with a D10
		// 413 before any handler runs, bounding the memory a single request can
		// force — including on raw app.Router() routes that call ShouldBindJSON,
		// and on raw-body webhooks (bounding the read does not alter accepted
		// bytes, so a signature still verifies). It is "always on" only in the
		// sense that it is part of this default runtime stack; an app that brings
		// its own router via WithRouter owns its middleware and this layer is not
		// installed for it. This is separate from sanitization: the bound used to
		// live incidentally inside the input sanitizer, which #271 made opt-in, so
		// it now stands on its own. It bounds JSON bodies only; non-JSON bodies
		// (uploads, text/plain) are not size-limited here — a general body-size
		// middleware is deferred. See requestBodyLimitMiddleware.
		// The storage route's direct uploads are bounded by their signed
		// length instead, and must be stored byte for byte.
		{name: "request_body_limit", handler: skipStorageRoute(route, requestBodyLimitMiddleware())},
	}
	// Input sanitization is opt-in (issue #271 / PERF-13). The default pipeline
	// does not rewrite request input: XSS is an output-encoding concern — the
	// generated React frontend escapes text (JSX), the framework admin SPA
	// renders values as React text (not HTML), a JSON API response is not an
	// HTML sink, and the response CSP is a backstop. Stripping markup on ingress
	// corrupts faithful values like {"description":"x < y"} while costing
	// allocations on every write request. Apps that want ingress
	// stripping set Security.SanitizeInput (GOMBIT_SECURITY_SANITIZE_INPUT);
	// WithRawBodyPaths still exempts webhook/signature paths. For a single field,
	// call framework.SanitizeHTML from the handler instead. See
	// docs/adr/018-input-sanitization-opt-in.md.
	if cfg.Security.SanitizeInput {
		stack = append(stack, namedMiddleware{name: "xss", handler: skipStorageRoute(route, xssMiddleware(rawBodyPaths...))})
	}
	// CSRF must run as global Gin middleware, not just on the auth Huma
	// routes: it covers every state-changing request (M5-3), including
	// application feature routes registered later via app.Router(). See
	// docs/auth-cookie.md. Raw-body paths are also CSRF-exempt: a webhook that
	// needs its raw body cannot do the double-submit either.
	if cfg.Auth.Enabled() && cfg.Auth.EffectiveMode() == config.AuthModeCookie {
		csrfExempt := append(append([]string{}, csrfExemptPaths...), rawBodyPaths...)
		stack = append(stack, namedMiddleware{name: "csrf", handler: skipStorageRoute(route, auth.CSRFMiddleware(cfg, csrfExempt...))})
	}
	// The per-handler timeout is opt-in (issue #270 / PERF-12): HTTP.RequestTimeout
	// defaults to 0. There is no separate request_timeout layer to omit — #268
	// folded the deadline into request_context, and applyTimeout is a true no-op
	// when the timeout is <= 0 (no timerCtx, no timer), so a disabled deadline
	// costs nothing on the request path. The http.Server read/write/idle timeouts
	// remain the connection-level safety net (see RunContext);
	// docs/adr/017-request-timeout-opt-in.md.
	return stack
}

func middlewareHandlers(stack []namedMiddleware) []gin.HandlerFunc {
	handlers := make([]gin.HandlerFunc, 0, len(stack))
	for _, middleware := range stack {
		handlers = append(handlers, middleware.handler)
	}
	return handlers
}
