package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDefault(t *testing.T) {
	got := Default()

	if got.AppName != "Gombit" {
		t.Fatalf("AppName = %q, want %q", got.AppName, "Gombit")
	}
	if got.Environment != EnvironmentDevelopment {
		t.Fatalf("Environment = %q, want %q", got.Environment, EnvironmentDevelopment)
	}
	if got.HTTP.Addr != ":8080" {
		t.Fatalf("HTTP.Addr = %q, want %q", got.HTTP.Addr, ":8080")
	}
	if got.HTTP.RequestTimeout != 60*time.Second {
		t.Fatalf("HTTP.RequestTimeout = %v, want 60s", got.HTTP.RequestTimeout)
	}
	if got.API.Prefix != "/api/v1" {
		t.Fatalf("API.Prefix = %q, want %q", got.API.Prefix, "/api/v1")
	}
	if !got.API.DocsEnabled {
		t.Fatal("API.DocsEnabled = false, want true")
	}
	if got.Database.Driver != DatabaseDriverSQLite {
		t.Fatalf("Database.Driver = %q, want %q", got.Database.Driver, DatabaseDriverSQLite)
	}
	if got.Database.DSN != "file:gombit.db?cache=shared&_fk=1" {
		t.Fatalf("Database.DSN = %q, want default sqlite DSN", got.Database.DSN)
	}
	if got.Cache.Driver != CacheDriverMemory {
		t.Fatalf("Cache.Driver = %q, want %q", got.Cache.Driver, CacheDriverMemory)
	}
	if got.Cache.Namespace != "gombit:development" {
		t.Fatalf("Cache.Namespace = %q, want gombit:development", got.Cache.Namespace)
	}
	if got.Cache.Redis.Addr != "127.0.0.1:6379" {
		t.Fatalf("Cache.Redis.Addr = %q, want 127.0.0.1:6379", got.Cache.Redis.Addr)
	}
	if got.Logging.Level != LogLevelInfo {
		t.Fatalf("Logging.Level = %q, want %q", got.Logging.Level, LogLevelInfo)
	}
	if got.Logging.Sink != LogSinkStderr {
		t.Fatalf("Logging.Sink = %q, want %q", got.Logging.Sink, LogSinkStderr)
	}
	if got.Auth.JWTSecret != "" {
		t.Fatalf("Auth.JWTSecret = %q, want empty development default", got.Auth.JWTSecret)
	}
	if got.Auth.AccessTokenTTL != DefaultAccessTokenTTL {
		t.Fatalf("Auth.AccessTokenTTL = %v, want %v", got.Auth.AccessTokenTTL, DefaultAccessTokenTTL)
	}
	if got.Auth.RefreshTokenTTL != DefaultRefreshTokenTTL {
		t.Fatalf("Auth.RefreshTokenTTL = %v, want %v", got.Auth.RefreshTokenTTL, DefaultRefreshTokenTTL)
	}
	if got.Auth.Mode != AuthModeJWT {
		t.Fatalf("Auth.Mode = %q, want %q", got.Auth.Mode, AuthModeJWT)
	}
	if !got.Auth.CookieSecure {
		t.Fatal("Auth.CookieSecure = false, want true default")
	}
	if got.Auth.CookieSameSite != CookieSameSiteLax {
		t.Fatalf("Auth.CookieSameSite = %q, want %q", got.Auth.CookieSameSite, CookieSameSiteLax)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

func TestLoadFromEnv(t *testing.T) {
	env := map[string]string{
		envAppName:                 " Example ",
		envEnv:                     " test ",
		envHTTPAddr:                " 127.0.0.1:9000 ",
		envHTTPTrustedProxies:      " 10.0.0.1, 10.0.0.0/24 ",
		envHTTPRequestTimeout:      " 15s ",
		envAPIPrefix:               " /api ",
		envDatabaseDriver:          " postgres ",
		envDatabaseDSN:             " host=localhost user=gombit dbname=app sslmode=disable ",
		envDatabaseMaxOpenConns:    " 20 ",
		envDatabaseMaxIdleConns:    " 4 ",
		envDatabaseConnMaxLifetime: " 45m ",
		envCacheDriver:             " redis ",
		envCacheNamespace:          " example:test ",
		envRedisAddr:               " localhost:6380 ",
		envRedisUsername:           " default ",
		envRedisPassword:           " password ",
		envRedisDB:                 " 2 ",
		envRedisDialTimeout:        " 1s ",
		envRedisReadTimeout:        " 2s ",
		envRedisWriteTimeout:       " 3s ",
		envRedisTLS:                " true ",
		envRedisTLSInsecure:        " yes ",
		envLogLevel:                " debug ",
		envLogSink:                 " stdout ",
		envJWTSecret:               "  test-jwt-secret-from-env  ", // #nosec G101 -- fake test secret.
		envJWTAccessTTL:            " 10m ",
		envJWTRefreshTTL:           " 48h ",
		envAuthMode:                " Cookie ",
		envCookieSecure:            " false ",
		envCookieSameSite:          " Strict ",
	}

	got, err := LoadFromEnv(mapLookup(env))
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v, want nil", err)
	}

	want := Config{
		AppName:     "Example",
		Environment: EnvironmentTest,
		HTTP: HTTPConfig{
			Addr:           "127.0.0.1:9000",
			TrustedProxies: []string{"10.0.0.1", "10.0.0.0/24"},
			RequestTimeout: 15 * time.Second,
		},
		API: APIConfig{
			Prefix:      "/api",
			DocsEnabled: true,
		},
		Database: DatabaseConfig{
			Driver:          DatabaseDriverPostgres,
			Required:        true,
			DSN:             "host=localhost user=gombit dbname=app sslmode=disable",
			MaxOpenConns:    20,
			MaxIdleConns:    4,
			ConnMaxLifetime: 45 * time.Minute,
		},
		Cache: CacheConfig{
			Driver:    CacheDriverRedis,
			Namespace: "example:test",
			Redis: RedisConfig{
				Addr:         "localhost:6380",
				Username:     "default",
				Password:     "password",
				DB:           2,
				DialTimeout:  time.Second,
				ReadTimeout:  2 * time.Second,
				WriteTimeout: 3 * time.Second,
				TLS:          true,
				TLSInsecure:  true,
			},
		},
		Logging: LoggingConfig{
			Level: LogLevelDebug,
			Sink:  LogSinkStdout,
		},
		Auth: AuthConfig{
			JWTSecret:       "test-jwt-secret-from-env",
			AccessTokenTTL:  10 * time.Minute,
			RefreshTokenTTL: 48 * time.Hour,
			Mode:            AuthModeCookie,
			CookieSecure:    false,
			CookieSameSite:  CookieSameSiteStrict,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadFromEnv() = %#v, want %#v", got, want)
	}
}

func TestDefaultForAppliesEnvironmentDocsAndNamespace(t *testing.T) {
	dev := DefaultFor(EnvironmentDevelopment)
	if !dev.API.DocsEnabled {
		t.Fatal("DefaultFor(development) DocsEnabled = false, want true")
	}
	if dev.Cache.Namespace != "gombit:development" {
		t.Fatalf("DefaultFor(development) Namespace = %q", dev.Cache.Namespace)
	}

	prod := DefaultFor(EnvironmentProduction)
	if prod.Environment != EnvironmentProduction {
		t.Fatalf("DefaultFor(production) Environment = %q", prod.Environment)
	}
	if prod.API.DocsEnabled {
		t.Fatal("DefaultFor(production) DocsEnabled = true, want false")
	}
	if prod.Cache.Namespace != "gombit:production" {
		t.Fatalf("DefaultFor(production) Namespace = %q", prod.Cache.Namespace)
	}
	if !Default().API.DocsEnabled {
		t.Fatal("Default() DocsEnabled = false, want development default true")
	}
}

func TestApplyEnvironmentDefaultsSetsDocsFromEnvironment(t *testing.T) {
	cfg := Default()
	cfg.Environment = EnvironmentProduction
	if !cfg.API.DocsEnabled {
		t.Fatal("precondition: Default()+production still has DocsEnabled true")
	}
	got := ApplyEnvironmentDefaults(cfg)
	if got.API.DocsEnabled {
		t.Fatal("ApplyEnvironmentDefaults(production) DocsEnabled = true, want false")
	}
}

func TestLoadFromEnvDisablesDocsInProductionByDefault(t *testing.T) {
	got, err := LoadFromEnv(mapLookup(map[string]string{
		envEnv: string(EnvironmentProduction),
	}))
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v, want nil", err)
	}
	if got.API.DocsEnabled {
		t.Fatal("API.DocsEnabled = true, want false in production when unset")
	}
}

func TestLoadFromEnvAllowsDocsInProductionWhenEnabled(t *testing.T) {
	got, err := LoadFromEnv(mapLookup(map[string]string{
		envEnv:         string(EnvironmentProduction),
		envDocsEnabled: "true",
	}))
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v, want nil", err)
	}
	if !got.API.DocsEnabled {
		t.Fatal("API.DocsEnabled = false, want true when GOMBIT_DOCS_ENABLED=true")
	}
}

func TestLoadFromEnvDisablesDocsWhenExplicitlyOff(t *testing.T) {
	got, err := LoadFromEnv(mapLookup(map[string]string{
		envDocsEnabled: "false",
	}))
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v, want nil", err)
	}
	if got.API.DocsEnabled {
		t.Fatal("API.DocsEnabled = true, want false when GOMBIT_DOCS_ENABLED=false")
	}
}

func TestLoadFromEnvUsesDefaultsWhenUnset(t *testing.T) {
	got, err := LoadFromEnv(mapLookup(nil))
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v, want nil", err)
	}

	if !reflect.DeepEqual(got, Default()) {
		t.Fatalf("LoadFromEnv() = %#v, want default %#v", got, Default())
	}
}

func TestLoadFromEnvAllowsDisabledHTTPRequestTimeout(t *testing.T) {
	got, err := LoadFromEnv(mapLookup(map[string]string{
		envHTTPRequestTimeout: "0",
	}))
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v, want nil", err)
	}
	if got.HTTP.RequestTimeout != 0 {
		t.Fatalf("HTTP.RequestTimeout = %v, want disabled timeout", got.HTTP.RequestTimeout)
	}
}

func TestLoadUsesProcessEnvironment(t *testing.T) {
	t.Setenv(envAppName, "Process Example")
	t.Setenv(envEnv, string(EnvironmentProduction))
	t.Setenv(envHTTPAddr, ":9090")
	t.Setenv(envAPIPrefix, "/api")
	t.Setenv(envDatabaseDriver, string(DatabaseDriverMySQL))
	t.Setenv(envDatabaseDSN, "gombit@tcp(localhost:3306)/app?parseTime=true")
	t.Setenv(envLogLevel, string(LogLevelError))
	t.Setenv(envLogSink, string(LogSinkMongo))
	t.Setenv(envJWTSecret, "production-jwt-secret-32-bytes-min")

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	want := Config{
		AppName:     "Process Example",
		Environment: EnvironmentProduction,
		HTTP: HTTPConfig{
			Addr:           ":9090",
			RequestTimeout: 60 * time.Second,
		},
		API: APIConfig{
			Prefix:      "/api",
			DocsEnabled: false,
		},
		Database: DatabaseConfig{
			Driver:   DatabaseDriverMySQL,
			Required: true,
			DSN:      "gombit@tcp(localhost:3306)/app?parseTime=true",
		},
		Cache: CacheConfig{
			Driver:    CacheDriverMemory,
			Namespace: "process-example:production",
			Redis:     Default().Cache.Redis,
		},
		Logging: LoggingConfig{
			Level: LogLevelError,
			Sink:  LogSinkMongo,
		},
		Auth: AuthConfig{
			JWTSecret:       "production-jwt-secret-32-bytes-min",
			AccessTokenTTL:  DefaultAccessTokenTTL,
			RefreshTokenTTL: DefaultRefreshTokenTTL,
			Mode:            AuthModeJWT,
			CookieSecure:    true,
			CookieSameSite:  CookieSameSiteLax,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load() = %#v, want %#v", got, want)
	}
}

func TestLoadFromEnvRequiresLookup(t *testing.T) {
	_, err := LoadFromEnv(nil)
	if err == nil {
		t.Fatal("LoadFromEnv(nil) error = nil, want error")
	}
	if !strings.Contains(err.Error(), "nil environment lookup") {
		t.Fatalf("LoadFromEnv(nil) error = %q, want nil lookup message", err)
	}
}

func TestValidateReportsExplicitFieldErrors(t *testing.T) {
	cfg := Config{
		AppName:     " ",
		Environment: "staging",
		HTTP: HTTPConfig{
			Addr:           "",
			RequestTimeout: -time.Second,
		},
		API: APIConfig{
			Prefix:      "api",
			DocsEnabled: true,
		},
		Database: DatabaseConfig{
			Driver:          "oracle",
			DSN:             "",
			MaxOpenConns:    -1,
			MaxIdleConns:    2,
			ConnMaxLifetime: -time.Second,
		},
		Cache: CacheConfig{
			Driver:    "disk",
			Namespace: "",
			Redis: RedisConfig{
				Addr:         "",
				DB:           -1,
				DialTimeout:  -time.Second,
				ReadTimeout:  0,
				WriteTimeout: 0,
			},
		},
		Logging: LoggingConfig{
			Level: "trace",
			Sink:  "file",
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want validation errors")
	}

	var fieldErrors FieldErrors
	if !errors.As(err, &fieldErrors) {
		t.Fatalf("Validate() error type = %T, want FieldErrors", err)
	}

	want := []FieldError{
		{Field: "AppName", Env: envAppName, Value: " ", Message: "must not be empty"},
		{
			Field:   "Environment",
			Env:     envEnv,
			Value:   "staging",
			Message: "must be one of development, test, production",
		},
		{Field: "HTTP.Addr", Env: envHTTPAddr, Value: "", Message: "must not be empty"},
		{
			Field:   "HTTP.RequestTimeout",
			Env:     envHTTPRequestTimeout,
			Value:   "-1s",
			Message: "must be greater than or equal to zero",
		},
		{Field: "API.Prefix", Env: envAPIPrefix, Value: "api", Message: "must start with /"},
		{
			Field:   "Database.Driver",
			Env:     envDatabaseDriver,
			Value:   "oracle",
			Message: "must be one of sqlite, postgres, mysql",
		},
		{Field: "Database.DSN", Env: envDatabaseDSN, Message: "must not be empty"},
		{
			Field:   "Database.MaxOpenConns",
			Env:     envDatabaseMaxOpenConns,
			Value:   "-1",
			Message: "must be greater than or equal to zero",
		},
		{
			Field:   "Database.MaxIdleConns",
			Env:     envDatabaseMaxIdleConns,
			Value:   "2",
			Message: "must be less than or equal to Database.MaxOpenConns",
		},
		{
			Field:   "Database.ConnMaxLifetime",
			Env:     envDatabaseConnMaxLifetime,
			Value:   "-1s",
			Message: "must be greater than or equal to zero",
		},
		{
			Field:   "Cache.Driver",
			Env:     envCacheDriver,
			Value:   "disk",
			Message: "must be one of memory, redis, noop",
		},
		{Field: "Cache.Namespace", Env: envCacheNamespace, Value: "", Message: "must not be empty"},
		{Field: "Cache.Redis.Addr", Env: envRedisAddr, Value: "", Message: "must not be empty"},
		{
			Field:   "Cache.Redis.DB",
			Env:     envRedisDB,
			Value:   "-1",
			Message: "must be greater than or equal to zero",
		},
		{
			Field:   "Cache.Redis.DialTimeout",
			Env:     envRedisDialTimeout,
			Value:   "-1s",
			Message: "must be greater than zero",
		},
		{
			Field:   "Cache.Redis.ReadTimeout",
			Env:     envRedisReadTimeout,
			Value:   "0s",
			Message: "must be greater than zero",
		},
		{
			Field:   "Cache.Redis.WriteTimeout",
			Env:     envRedisWriteTimeout,
			Value:   "0s",
			Message: "must be greater than zero",
		},
		{
			Field:   "Logging.Level",
			Env:     envLogLevel,
			Value:   "trace",
			Message: "must be one of debug, info, warn, error",
		},
		{
			Field:   "Logging.Sink",
			Env:     envLogSink,
			Value:   "file",
			Message: "must be one of stderr, stdout, mongo",
		},
	}
	if !reflect.DeepEqual([]FieldError(fieldErrors), want) {
		t.Fatalf("Validate() field errors = %#v, want %#v", []FieldError(fieldErrors), want)
	}

	message := err.Error()
	for _, wantPart := range []string{
		envAppName,
		envEnv,
		envHTTPAddr,
		envHTTPRequestTimeout,
		envAPIPrefix,
		envDatabaseDriver,
		envDatabaseDSN,
		envDatabaseMaxOpenConns,
		envDatabaseMaxIdleConns,
		envDatabaseConnMaxLifetime,
		envCacheDriver,
		envCacheNamespace,
		envRedisAddr,
		envRedisDB,
		envRedisDialTimeout,
		envRedisReadTimeout,
		envRedisWriteTimeout,
		envLogLevel,
		envLogSink,
	} {
		if !strings.Contains(message, wantPart) {
			t.Fatalf("Validate() error = %q, want it to contain %q", message, wantPart)
		}
	}
}

func TestValidateRejectsUnsafeTrustedProxyInProduction(t *testing.T) {
	cfg := Default()
	cfg.Environment = EnvironmentProduction
	cfg.Auth.JWTSecret = "production-jwt-secret-32-bytes-min"
	cfg.HTTP.TrustedProxies = []string{"0.0.0.0/0"}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want trusted proxy validation error")
	}

	var fieldErrors FieldErrors
	if !errors.As(err, &fieldErrors) {
		t.Fatalf("Validate() error type = %T, want FieldErrors", err)
	}

	want := []FieldError{
		{
			Field:   "HTTP.TrustedProxies",
			Env:     envHTTPTrustedProxies,
			Value:   "0.0.0.0/0",
			Message: "must not trust all proxies in production",
		},
	}
	if !reflect.DeepEqual([]FieldError(fieldErrors), want) {
		t.Fatalf("Validate() field errors = %#v, want %#v", []FieldError(fieldErrors), want)
	}
}

func TestValidateRejectsShortJWTSecretInProduction(t *testing.T) {
	cfg := DefaultFor(EnvironmentProduction)
	cfg.Auth.JWTSecret = "short"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want short JWT secret error")
	}

	var fieldErrors FieldErrors
	if !errors.As(err, &fieldErrors) {
		t.Fatalf("Validate() error type = %T, want FieldErrors", err)
	}

	found := false
	for _, got := range fieldErrors {
		if got.Field != "Auth.JWTSecret" {
			continue
		}
		found = true
		if got.Env != envJWTSecret {
			t.Fatalf("Auth.JWTSecret env = %q, want %s", got.Env, envJWTSecret)
		}
		if got.Value != "" {
			t.Fatalf("Auth.JWTSecret FieldError.Value = %q, want empty (do not echo secrets)", got.Value)
		}
		if !strings.Contains(got.Message, "32") {
			t.Fatalf("Auth.JWTSecret message = %q, want length floor", got.Message)
		}
	}
	if !found {
		t.Fatalf("Validate() field errors = %#v, want Auth.JWTSecret", []FieldError(fieldErrors))
	}
}

func TestValidateAllowsEmptyJWTSecretInProduction(t *testing.T) {
	cfg := DefaultFor(EnvironmentProduction)
	if cfg.Auth.Enabled() {
		t.Fatal("precondition: empty JWT secret should disable auth")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil when production JWT secret is unset", err)
	}
}

func TestValidateAcceptsLongJWTSecretInProduction(t *testing.T) {
	cfg := DefaultFor(EnvironmentProduction)
	cfg.Auth.JWTSecret = "production-jwt-secret-32-bytes-min"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

func TestValidateRejectsDevelopmentPlaceholderInProduction(t *testing.T) {
	for _, secret := range []string{DevelopmentJWTPlaceholder, historicalDevelopmentJWTPlaceholder} {
		t.Run(secret, func(t *testing.T) {
			cfg := DefaultFor(EnvironmentProduction)
			cfg.Auth.JWTSecret = secret
			err := cfg.Validate()
			if err == nil {
				t.Fatal("Validate() error = nil, want placeholder JWT secret error")
			}
			var fieldErrors FieldErrors
			if !errors.As(err, &fieldErrors) {
				t.Fatalf("Validate() error type = %T, want FieldErrors", err)
			}
			found := false
			for _, got := range fieldErrors {
				if got.Field != "Auth.JWTSecret" {
					continue
				}
				found = true
				if got.Value != "" {
					t.Fatalf("Auth.JWTSecret FieldError.Value = %q, want empty", got.Value)
				}
				if !strings.Contains(got.Message, "placeholder") {
					t.Fatalf("Auth.JWTSecret message = %q, want placeholder", got.Message)
				}
			}
			if !found {
				t.Fatalf("Validate() field errors = %#v, want Auth.JWTSecret", []FieldError(fieldErrors))
			}
		})
	}
}

func TestLoadFromEnvRejectsShortJWTSecretInProduction(t *testing.T) {
	_, err := LoadFromEnv(mapLookup(map[string]string{
		envEnv:       string(EnvironmentProduction),
		envJWTSecret: "too-short",
	}))
	if err == nil {
		t.Fatal("LoadFromEnv() error = nil, want short JWT secret error")
	}
	var fieldErrors FieldErrors
	if !errors.As(err, &fieldErrors) {
		t.Fatalf("LoadFromEnv() error type = %T, want FieldErrors", err)
	}
	if strings.Contains(err.Error(), "too-short") {
		t.Fatalf("LoadFromEnv() error leaked JWT secret: %v", err)
	}
	found := false
	for _, got := range fieldErrors {
		if got.Field == "Auth.JWTSecret" {
			found = true
			if got.Value != "" {
				t.Fatalf("Auth.JWTSecret FieldError.Value = %q, want empty", got.Value)
			}
		}
	}
	if !found {
		t.Fatalf("LoadFromEnv() field errors = %#v, want Auth.JWTSecret", []FieldError(fieldErrors))
	}
}

func TestAuthEnabled(t *testing.T) {
	if Default().Auth.Enabled() {
		t.Fatal("Default() Auth.Enabled() = true, want false")
	}
	cfg := Default()
	cfg.Auth.JWTSecret = "dev-secret"
	if !cfg.Auth.Enabled() {
		t.Fatal("Auth.Enabled() = false, want true when JWT secret is set")
	}
}

func TestEffectiveModeDefaultsEmptyToJWT(t *testing.T) {
	var cfg AuthConfig
	if got := cfg.EffectiveMode(); got != AuthModeJWT {
		t.Fatalf("EffectiveMode() = %q, want %q", got, AuthModeJWT)
	}
	cfg.Mode = AuthModeCookie
	if got := cfg.EffectiveMode(); got != AuthModeCookie {
		t.Fatalf("EffectiveMode() = %q, want %q", got, AuthModeCookie)
	}
}

func TestEffectiveCookieSameSiteDefaultsEmptyToLax(t *testing.T) {
	var cfg AuthConfig
	if got := cfg.EffectiveCookieSameSite(); got != CookieSameSiteLax {
		t.Fatalf("EffectiveCookieSameSite() = %q, want %q", got, CookieSameSiteLax)
	}
	cfg.CookieSameSite = CookieSameSiteStrict
	if got := cfg.EffectiveCookieSameSite(); got != CookieSameSiteStrict {
		t.Fatalf("EffectiveCookieSameSite() = %q, want %q", got, CookieSameSiteStrict)
	}
}

// TestValidateRejectsInsecureCookieInProduction is the Appendix C case for
// M5-3: cookie auth without Secure under a production deployment must fail
// loud at config.Validate / gombit doctor.
func TestValidateRejectsInsecureCookieInProduction(t *testing.T) {
	cfg := DefaultFor(EnvironmentProduction)
	cfg.Auth.JWTSecret = "production-jwt-secret-32-bytes-min"
	cfg.Auth.Mode = AuthModeCookie
	cfg.Auth.CookieSecure = false

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want insecure cookie error")
	}
	var fieldErrors FieldErrors
	if !errors.As(err, &fieldErrors) {
		t.Fatalf("Validate() error type = %T, want FieldErrors", err)
	}
	want := FieldError{
		Field:   "Auth.CookieSecure",
		Env:     envCookieSecure,
		Value:   "false",
		Message: "must be true in production for cookie auth (Appendix C)",
	}
	for _, got := range fieldErrors {
		if reflect.DeepEqual(got, want) {
			return
		}
	}
	t.Fatalf("Validate() field errors = %#v, want one to equal %#v", []FieldError(fieldErrors), want)
}

func TestValidateAllowsSecureCookieInProduction(t *testing.T) {
	cfg := DefaultFor(EnvironmentProduction)
	cfg.Auth.JWTSecret = "production-jwt-secret-32-bytes-min"
	cfg.Auth.Mode = AuthModeCookie
	cfg.Auth.CookieSecure = true
	cfg.Auth.CookieSameSite = CookieSameSiteStrict
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

func TestValidateAllowsInsecureCookieOutsideProduction(t *testing.T) {
	cfg := DefaultFor(EnvironmentDevelopment)
	cfg.Auth.JWTSecret = "dev-jwt-secret-not-for-production"
	cfg.Auth.Mode = AuthModeCookie
	cfg.Auth.CookieSecure = false
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil (development may disable Secure for plain HTTP)", err)
	}
}

func TestValidateRejectsUnknownAuthMode(t *testing.T) {
	cfg := DefaultFor(EnvironmentTest)
	cfg.Auth.JWTSecret = "test-jwt-secret-32-bytes-minimum!"
	cfg.Auth.Mode = "oauth"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want unknown Auth.Mode error")
	}
	var fieldErrors FieldErrors
	if !errors.As(err, &fieldErrors) {
		t.Fatalf("Validate() error type = %T, want FieldErrors", err)
	}
	found := false
	for _, got := range fieldErrors {
		if got.Field == "Auth.Mode" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Validate() field errors = %#v, want Auth.Mode", []FieldError(fieldErrors))
	}
}

func TestValidateRejectsUnknownCookieSameSite(t *testing.T) {
	cfg := DefaultFor(EnvironmentTest)
	cfg.Auth.JWTSecret = "test-jwt-secret-32-bytes-minimum!"
	cfg.Auth.Mode = AuthModeCookie
	cfg.Auth.CookieSameSite = "none"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want unknown Auth.CookieSameSite error")
	}
	var fieldErrors FieldErrors
	if !errors.As(err, &fieldErrors) {
		t.Fatalf("Validate() error type = %T, want FieldErrors", err)
	}
	found := false
	for _, got := range fieldErrors {
		if got.Field == "Auth.CookieSameSite" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Validate() field errors = %#v, want Auth.CookieSameSite", []FieldError(fieldErrors))
	}
}

// TestValidateIgnoresAuthModeWhenAuthDisabled matches
// TestValidateReportsExplicitFieldErrors: a zero-value AuthConfig (no JWT
// secret) must not produce Auth.Mode/CookieSameSite errors, since no auth
// routes are mounted regardless of Mode.
func TestValidateIgnoresAuthModeWhenAuthDisabled(t *testing.T) {
	cfg := Default()
	cfg.Auth.Mode = "not-a-real-mode"
	cfg.Auth.CookieSameSite = "not-a-real-samesite"
	if cfg.Auth.Enabled() {
		t.Fatal("precondition: empty JWT secret should disable auth")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil when auth is disabled", err)
	}
}

func TestValidateRejectsRedisTLSInsecureInProduction(t *testing.T) {
	cfg := Default()
	cfg.Environment = EnvironmentProduction
	cfg.Auth.JWTSecret = "production-jwt-secret-32-bytes-min"
	cfg.Cache.Driver = CacheDriverRedis
	cfg.Cache.Redis.TLS = true
	cfg.Cache.Redis.TLSInsecure = true

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want validation errors")
	}

	var fieldErrors FieldErrors
	if !errors.As(err, &fieldErrors) {
		t.Fatalf("Validate() error type = %T, want FieldErrors", err)
	}

	want := FieldError{
		Field:   "Cache.Redis.TLSInsecure",
		Env:     envRedisTLSInsecure,
		Value:   "true",
		Message: "must be false in production",
	}
	for _, got := range fieldErrors {
		if reflect.DeepEqual(got, want) {
			return
		}
	}
	t.Fatalf("Validate() field errors = %#v, want one to equal %#v", []FieldError(fieldErrors), want)
}

func TestLoadFromEnvReturnsValidationErrors(t *testing.T) {
	_, err := LoadFromEnv(mapLookup(map[string]string{
		envEnv:       "prod",
		envAPIPrefix: "api",
	}))
	if err == nil {
		t.Fatal("LoadFromEnv() error = nil, want validation errors")
	}

	var fieldErrors FieldErrors
	if !errors.As(err, &fieldErrors) {
		t.Fatalf("LoadFromEnv() error type = %T, want FieldErrors", err)
	}
	if len(fieldErrors) != 2 {
		t.Fatalf("LoadFromEnv() field error count = %d, want 2", len(fieldErrors))
	}
}

func TestLoadFromEnvReturnsParseErrors(t *testing.T) {
	_, err := LoadFromEnv(mapLookup(map[string]string{
		envDatabaseMaxOpenConns:    "many",
		envDatabaseConnMaxLifetime: "later",
		envHTTPRequestTimeout:      "soon",
		envRedisTLS:                "maybe",
	}))
	if err == nil {
		t.Fatal("LoadFromEnv() error = nil, want parse errors")
	}

	var fieldErrors FieldErrors
	if !errors.As(err, &fieldErrors) {
		t.Fatalf("LoadFromEnv() error type = %T, want FieldErrors", err)
	}
	if len(fieldErrors) != 4 {
		t.Fatalf("LoadFromEnv() field error count = %d, want 4", len(fieldErrors))
	}
}

func TestDatabaseRequiredDefaultsTrueAndOverrides(t *testing.T) {
	if !Default().Database.Required {
		t.Fatal("Default().Database.Required = false, want true")
	}

	cfg, err := LoadFromEnv(mapLookup(map[string]string{envDatabaseRequired: "false"}))
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v", err)
	}
	if cfg.Database.Required {
		t.Error("Database.Required = true with GOMBIT_DATABASE_REQUIRED=false, want false")
	}
}

func mapLookup(values map[string]string) EnvLookup {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
