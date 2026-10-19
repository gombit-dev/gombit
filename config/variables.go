package config

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// Variable is an environment variable LoadFromEnv reads.
type Variable struct {
	// Name is the variable, e.g. GOMBIT_HTTP_ADDR.
	Name string `json:"name"`
	// Secret is whether its value is a credential (or may embed one, as a
	// DSN does): tooling never prints it.
	Secret bool `json:"secret"`
	// Default is what it is when unset in development, TestDefault in
	// test and ProductionDefault in production, written as a .env file
	// would hold it. A secret's default never holds a credential: only the DSN has
	// one (a local SQLite file).
	Default           string `json:"default"`
	TestDefault       string `json:"test_default"`
	ProductionDefault string `json:"production_default"`
}

// variable binds an environment variable to the Config field it sets.
type variable struct {
	name   string
	secret bool
	get    func(Config) string
}

// variables are every variable LoadFromEnv reads (a test holds this to the
// env* constants), and how to read each one's value back from a Config.
var variables = []variable{
	{envAppName, false, func(c Config) string { return c.AppName }},
	{envEnv, false, func(c Config) string { return string(c.Environment) }},
	{envHTTPAddr, false, func(c Config) string { return c.HTTP.Addr }},
	{envHTTPTrustedProxies, false, func(c Config) string { return strings.Join(c.HTTP.TrustedProxies, ",") }},
	{envHTTPRequestTimeout, false, func(c Config) string { return duration(c.HTTP.RequestTimeout) }},
	{envAPIPrefix, false, func(c Config) string { return c.API.Prefix }},
	{envDocsEnabled, false, func(c Config) string { return strconv.FormatBool(c.API.DocsEnabled) }},
	{envDatabaseDriver, false, func(c Config) string { return string(c.Database.Driver) }},
	{envDatabaseRequired, false, func(c Config) string { return strconv.FormatBool(c.Database.Required) }},
	{envDatabaseDSN, true, func(c Config) string { return c.Database.DSN }},
	{envDatabaseMaxOpenConns, false, func(c Config) string { return strconv.Itoa(c.Database.MaxOpenConns) }},
	{envDatabaseMaxIdleConns, false, func(c Config) string { return strconv.Itoa(c.Database.MaxIdleConns) }},
	{envDatabaseConnMaxLifetime, false, func(c Config) string { return duration(c.Database.ConnMaxLifetime) }},
	{envCacheDriver, false, func(c Config) string { return string(c.Cache.Driver) }},
	{envCacheNamespace, false, func(c Config) string { return c.Cache.Namespace }},
	{envJobsDriver, false, func(c Config) string { return string(c.Jobs.Driver) }},
	{envJobsQueue, false, func(c Config) string { return c.Jobs.Queue }},
	{envJobsNamespace, false, func(c Config) string { return c.Jobs.Namespace }},
	{envStorageDriver, false, func(c Config) string { return string(c.Storage.Driver) }},
	{envStorageLocalRoot, false, func(c Config) string { return c.Storage.Local.Root }},
	{envStorageS3Endpoint, false, func(c Config) string { return c.Storage.S3.Endpoint }},
	{envStorageS3Region, false, func(c Config) string { return c.Storage.S3.Region }},
	{envStorageS3Bucket, false, func(c Config) string { return c.Storage.S3.Bucket }},
	{envStorageS3Prefix, false, func(c Config) string { return c.Storage.S3.Prefix }},
	{envStorageS3AccessKeyID, true, func(c Config) string { return c.Storage.S3.AccessKeyID }},
	{envStorageS3SecretKey, true, func(c Config) string { return c.Storage.S3.SecretAccessKey }},
	{envStorageS3PathStyle, false, func(c Config) string { return strconv.FormatBool(c.Storage.S3.ForcePathStyle) }},
	{envStorageS3PublicURL, false, func(c Config) string { return c.Storage.S3.PublicURL }},
	{envStoragePublicPrefix, false, func(c Config) string { return c.Storage.PublicPrefix }},
	{envStorageURLSecret, true, func(c Config) string { return c.Storage.URLSecret }},
	{envStorageLocalURL, false, func(c Config) string { return c.Storage.Local.URL }},
	{envRedisAddr, false, func(c Config) string { return c.Cache.Redis.Addr }},
	{envRedisUsername, false, func(c Config) string { return c.Cache.Redis.Username }},
	{envRedisPassword, true, func(c Config) string { return c.Cache.Redis.Password }},
	{envRedisDB, false, func(c Config) string { return strconv.Itoa(c.Cache.Redis.DB) }},
	{envRedisDialTimeout, false, func(c Config) string { return duration(c.Cache.Redis.DialTimeout) }},
	{envRedisReadTimeout, false, func(c Config) string { return duration(c.Cache.Redis.ReadTimeout) }},
	{envRedisWriteTimeout, false, func(c Config) string { return duration(c.Cache.Redis.WriteTimeout) }},
	{envRedisTLS, false, func(c Config) string { return strconv.FormatBool(c.Cache.Redis.TLS) }},
	{envRedisTLSInsecure, false, func(c Config) string { return strconv.FormatBool(c.Cache.Redis.TLSInsecure) }},
	{envLogLevel, false, func(c Config) string { return string(c.Logging.Level) }},
	{envLogSink, false, func(c Config) string { return string(c.Logging.Sink) }},
	{envJWTSecret, true, func(c Config) string { return c.Auth.JWTSecret }},
	{envJWTAccessTTL, false, func(c Config) string { return duration(c.Auth.AccessTokenTTL) }},
	{envJWTRefreshTTL, false, func(c Config) string { return duration(c.Auth.RefreshTokenTTL) }},
	{envAuthMode, false, func(c Config) string { return string(c.Auth.Mode) }},
	{envCookieSecure, false, func(c Config) string { return strconv.FormatBool(c.Auth.CookieSecure) }},
	{envCookieSameSite, false, func(c Config) string { return string(c.Auth.CookieSameSite) }},
	{envSecuritySanitizeInput, false, func(c Config) string { return strconv.FormatBool(c.Security.SanitizeInput) }},
}

// duration writes d as GOMBIT_* durations are written: "0" for none.
func duration(d time.Duration) string {
	if d == 0 {
		return "0"
	}
	return d.String()
}

// Variables are the environment variables LoadFromEnv reads, by name, with
// their defaults per environment: the configuration surface the upgrade tooling compares
// across releases (a renamed, removed or re-defaulted variable must be
// declared in the compatibility manifest).
func Variables() []Variable {
	dev, test, prod := DefaultFor(EnvironmentDevelopment), DefaultFor(EnvironmentTest), DefaultFor(EnvironmentProduction)
	out := make([]Variable, 0, len(variables))
	for _, v := range variables {
		out = append(out, Variable{Name: v.name, Secret: v.secret, Default: v.get(dev), TestDefault: v.get(test), ProductionDefault: v.get(prod)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
