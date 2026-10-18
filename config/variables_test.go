package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// TestVariablesCoverEveryEnvConstant: the catalog lists exactly the
// variables config.go names (its env* constants), so a variable LoadFromEnv
// starts reading cannot be missing from what the upgrade tooling compares.
func TestVariablesCoverEveryEnvConstant(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "config.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	consts := map[string]bool{}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "env") || i >= len(vs.Values) {
					continue
				}
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					v, _ := strconv.Unquote(lit.Value)
					consts[v] = true
				}
			}
		}
	}
	listed := map[string]bool{}
	for _, v := range Variables() {
		if listed[v.Name] {
			t.Errorf("%s is listed twice", v.Name)
		}
		listed[v.Name] = true
		if !consts[v.Name] {
			t.Errorf("%s is listed but no env* constant names it", v.Name)
		}
	}
	for name := range consts {
		if !listed[name] {
			t.Errorf("%s is an env* constant but not in Variables()", name)
		}
	}
	if len(consts) < 40 {
		t.Fatalf("found %d env* constants; the parse is wrong", len(consts))
	}
}

// TestVariablesReadBackWhatLoadFromEnvSets: each catalog entry reads the
// field its variable sets. Setting the variable to a value distinct from
// its default changes exactly that entry's reading, so a binding to the
// wrong field fails.
func TestVariablesReadBackWhatLoadFromEnvSets(t *testing.T) {
	samples := map[string]string{
		envAppName: "demo", envEnv: "test", envHTTPAddr: ":9090", envHTTPTrustedProxies: "10.0.0.1",
		envHTTPRequestTimeout: "7s", envAPIPrefix: "/api/v2", envDocsEnabled: "false",
		envDatabaseDriver: "postgres", envDatabaseRequired: "false", envDatabaseDSN: "postgres://db.example.com/app",
		envDatabaseMaxOpenConns: "9", envDatabaseMaxIdleConns: "3", envDatabaseConnMaxLifetime: "9m0s",
		envCacheDriver: "redis", envCacheNamespace: "ns:x", envJobsDriver: "memory", envJobsQueue: "q2",
		envJobsNamespace: "jobs:x", envStorageDriver: "memory", envStorageLocalRoot: "files",
		envStorageS3Endpoint: "https://s3.example.com", envStorageS3Region: "eu-west-1", envStorageS3Bucket: "b",
		envStorageS3Prefix: "p/", envStorageS3AccessKeyID: "AKIA", envStorageS3SecretKey: "sk",
		envStorageS3PathStyle: "true", envStorageS3PublicURL: "https://cdn.example.com",
		envStoragePublicPrefix: "pub/", envStorageURLSecret: strings.Repeat("u", 32), envStorageLocalURL: "/files",
		envRedisAddr: "redis:6379", envRedisUsername: "ru", envRedisPassword: "rp", envRedisDB: "2",
		envRedisDialTimeout: "6s", envRedisReadTimeout: "4s", envRedisWriteTimeout: "2s", envRedisTLS: "true",
		envRedisTLSInsecure: "true", envLogLevel: "debug", envLogSink: "stdout",
		envJWTSecret: strings.Repeat("j", 32), envJWTAccessTTL: "5m0s", envJWTRefreshTTL: "24h0m0s",
		envAuthMode: "cookie", envCookieSecure: "false", envCookieSameSite: "strict",
		envSecuritySanitizeInput: "true",
	}
	base := map[string]string{envEnv: "test", envJWTSecret: strings.Repeat("j", 32)}
	load := func(extra map[string]string) Config {
		t.Helper()
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		for k, v := range extra {
			env[k] = v
		}
		cfg, err := LoadFromEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
		if err != nil {
			t.Fatalf("LoadFromEnv(%v): %v", extra, err)
		}
		return cfg
	}
	before := load(nil)
	for _, v := range variables {
		sample, ok := samples[v.name]
		if !ok {
			t.Errorf("%s: no sample value", v.name)
			continue
		}
		extra := map[string]string{v.name: sample}
		if v.name == envRedisTLSInsecure { // valid only with TLS on
			extra[envRedisTLS] = "true"
		}
		after := load(extra)
		if got := v.get(after); got != sample {
			t.Errorf("%s: reads %q after setting %q", v.name, got, sample)
		}
		for _, other := range variables {
			if other.name == v.name || other.name == envEnv || dependsOn(other.name, v.name) {
				continue
			}
			if other.get(after) != other.get(before) {
				t.Errorf("setting %s changed %s too", v.name, other.name)
			}
		}
	}
}

// dependsOn: variables whose value derives from another when unset.
func dependsOn(derived, from string) bool {
	switch derived {
	case envCacheNamespace, envJobsNamespace:
		return from == envAppName
	case envRedisTLS:
		return from == envRedisTLSInsecure
	}
	return false
}

// TestVariablesDefaults: defaults are read from DefaultFor, per
// environment, and no secret ships a default.
func TestVariablesDefaults(t *testing.T) {
	byName := map[string]Variable{}
	for _, v := range Variables() {
		byName[v.Name] = v
		if v.Secret && (v.Default != "" || v.TestDefault != "" || v.ProductionDefault != "") && v.Name != envDatabaseDSN {
			t.Errorf("%s is secret but has a default", v.Name)
		}
	}
	if v := byName[envDocsEnabled]; v.Default != "true" || v.TestDefault != "true" || v.ProductionDefault != "false" {
		t.Errorf("%s defaults = %+v", envDocsEnabled, v)
	}
	if v := byName[envJWTAccessTTL]; v.Default != "15m0s" {
		t.Errorf("%s default = %q", envJWTAccessTTL, v.Default)
	}
	if v := byName[envHTTPRequestTimeout]; v.Default != "0" {
		t.Errorf("%s default = %q", envHTTPRequestTimeout, v.Default)
	}
}
