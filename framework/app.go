package framework

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
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
	"github.com/gombit-dev/gombit/logging"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

const defaultShutdownTimeout = 10 * time.Second

// Hook is an application lifecycle callback.
type Hook func(context.Context) error

// Option configures an App.
type Option func(*App) error

// App owns Gombit's runtime lifecycle and HTTP router.
type App struct {
	cfg              config.Config
	cfgSet           bool
	cache            cache.Cache
	cacheStore       *cache.Store
	cacheOwned       bool
	redis            *redis.Client
	db               *database.DB
	logger           *zap.Logger
	router           *gin.Engine
	api              huma.API
	startHooks       []Hook
	stopHooks        []Hook
	shutdownTimeout  time.Duration
	embeddedFrontend fs.FS
	csrfExemptPaths  []string
	rawBodyPaths     []string

	mu     sync.RWMutex
	server *http.Server
	addr   string
}

type namedMiddleware struct {
	name    string
	handler gin.HandlerFunc
}

// New creates an application using process configuration and the default router.
func New(options ...Option) (*App, error) {
	app := &App{
		cfg:             config.Default(),
		shutdownTimeout: defaultShutdownTimeout,
	}

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
		router, err := newRouter(app.cfg, app.csrfExemptPaths, app.rawBodyPaths)
		if err != nil {
			return nil, err
		}
		app.router = router
	} else if err := configureTrustedProxies(app.router, app.cfg.HTTP.TrustedProxies); err != nil {
		return nil, err
	}
	if app.api == nil {
		contract.Install(contract.InstallOptions{
			RequestID: GetRequestIDFromContext,
		})
		// OpenAPI Info.Version stays 0.0.0 until runtime versioning lands.
		app.api = humagin.New(app.router, contract.HumaConfigFor(app.cfg.AppName, "0.0.0", app.cfg.API.DocsEnabled))
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
			mountAdminSPA(app.router, adminui.FS(), app.cfg.API.Prefix)
		}
	}
	mountEmbeddedFrontend(app.router, app.embeddedFrontend, app.cfg.API.Prefix)

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

// Run runs app until an interrupt or terminate signal is received.
func Run(app *App) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return RunContext(ctx, app)
}

// RunContext runs app until ctx is canceled or the HTTP server fails.
func RunContext(ctx context.Context, app *App) error {
	if ctx == nil {
		return errors.New("framework: nil context")
	}
	if app == nil {
		return errors.New("framework: nil app")
	}

	listener, err := net.Listen("tcp", app.Config().HTTP.Addr)
	if err != nil {
		return fmt.Errorf("framework: listen: %w", err)
	}

	server := &http.Server{
		Handler:           app.Router(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       app.Config().HTTP.RequestTimeout,
		WriteTimeout:      app.Config().HTTP.RequestTimeout,
		IdleTimeout:       app.Config().HTTP.RequestTimeout,
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
	a.mu.RLock()
	server := a.server
	timeout := a.shutdownTimeout
	a.mu.RUnlock()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if server != nil {
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return errors.Join(
				fmt.Errorf("framework: shutdown: %w", err),
				a.runStopHooksWithContext(shutdownCtx),
				a.closeOwnedCache(),
			)
		}
	}

	return errors.Join(a.runStopHooksWithContext(shutdownCtx), a.closeOwnedCache())
}

func (a *App) runStopHooks() error {
	a.mu.RLock()
	timeout := a.shutdownTimeout
	a.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return errors.Join(a.runStopHooksWithContext(ctx), a.closeOwnedCache())
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

func newRouter(cfg config.Config, csrfExemptPaths, rawBodyPaths []string) (*gin.Engine, error) {
	router := gin.New()
	enableMethodNotAllowed(router)
	if err := configureTrustedProxies(router, cfg.HTTP.TrustedProxies); err != nil {
		return nil, err
	}

	metrics := newHTTPMetrics()
	router.Use(middlewareHandlers(runtimeMiddlewareStack(cfg, metrics, csrfExemptPaths, rawBodyPaths))...)
	router.GET("/livez", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"data": gin.H{
				"status": "ok",
			},
		})
	})
	router.GET("/readyz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"data": gin.H{
				"status": "ok",
			},
		})
	})
	router.GET("/metrics", metrics.handler)
	return router, nil
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

func runtimeMiddlewareStack(cfg config.Config, metrics *httpMetrics, csrfExemptPaths, rawBodyPaths []string) []namedMiddleware {
	stack := []namedMiddleware{
		{name: "recovery", handler: gin.Recovery()},
		{name: "request_context", handler: requestContextMiddleware()},
		{name: "metrics", handler: metricsMiddleware(metrics)},
		{
			name:    "security_headers",
			handler: securityHeadersMiddleware(cfg.Environment == config.EnvironmentProduction),
		},
		// Raw-body paths (webhooks) skip input sanitization so their body reaches
		// the handler unmodified for signature verification (WithRawBodyPaths).
		{name: "xss", handler: xssMiddleware(rawBodyPaths...)},
	}
	// CSRF must run as global Gin middleware, not just on the auth Huma
	// routes: it covers every state-changing request (M5-3), including
	// application feature routes registered later via app.Router(). See
	// docs/auth-cookie.md. Raw-body paths are also CSRF-exempt: a webhook that
	// needs its raw body cannot do the double-submit either.
	if cfg.Auth.Enabled() && cfg.Auth.EffectiveMode() == config.AuthModeCookie {
		csrfExempt := append(append([]string{}, csrfExemptPaths...), rawBodyPaths...)
		stack = append(stack, namedMiddleware{name: "csrf", handler: auth.CSRFMiddleware(cfg, csrfExempt...)})
	}
	stack = append(stack, namedMiddleware{name: "request_timeout", handler: requestTimeoutMiddleware(cfg.HTTP.RequestTimeout)})
	return stack
}

func middlewareHandlers(stack []namedMiddleware) []gin.HandlerFunc {
	handlers := make([]gin.HandlerFunc, 0, len(stack))
	for _, middleware := range stack {
		handlers = append(handlers, middleware.handler)
	}
	return handlers
}
