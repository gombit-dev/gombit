package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/config"
)

func TestCheckInsecureFlagsInsecureCookieInProduction(t *testing.T) {
	// config.Validate already rejects this at config.Load time (see
	// config_test.go TestValidateRejectsInsecureCookieInProduction), so
	// runDoctorChecks never reaches checkInsecure for it in practice. This
	// asserts checkInsecure's own defense in depth stays aligned with
	// Appendix C if that changes.
	cfg := config.DefaultFor(config.EnvironmentProduction)
	cfg.Auth.JWTSecret = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cfg.Auth.Mode = config.AuthModeCookie
	cfg.Auth.CookieSecure = false
	cfg.Database.Driver = config.DatabaseDriverPostgres

	check := checkInsecure(cfg)
	if check.Status != doctorStatusOK {
		t.Fatalf("checkInsecure status = %q, want %q (cookie security is enforced by config.Validate, not checkInsecure)", check.Status, doctorStatusOK)
	}
}

func TestConfigLoadRejectsInsecureCookieInProductionBeforeDoctorChecks(t *testing.T) {
	prev := LoadConfig
	t.Cleanup(func() { LoadConfig = prev })
	LoadConfig = func() (config.Config, error) {
		cfg := config.DefaultFor(config.EnvironmentProduction)
		cfg.Auth.JWTSecret = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		cfg.Auth.Mode = config.AuthModeCookie
		cfg.Auth.CookieSecure = false
		return cfg, cfg.Validate()
	}

	checks := runDoctorChecks(context.Background(), doctorOptions{})
	var configCheck *doctorCheck
	for i := range checks {
		if checks[i].Name == "config" {
			configCheck = &checks[i]
			break
		}
	}
	if configCheck == nil {
		t.Fatal("runDoctorChecks did not produce a config check")
	}
	if configCheck.Status != doctorStatusFail {
		t.Fatalf("config check status = %q, want %q; message: %s", configCheck.Status, doctorStatusFail, configCheck.Message)
	}
	if !strings.Contains(configCheck.Message, "CookieSecure") {
		t.Fatalf("config check message = %q, want mention of CookieSecure", configCheck.Message)
	}
}

func TestWriteConfigShowIncludesAuthCookieFields(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Mode = config.AuthModeCookie
	cfg.Auth.CookieSecure = true
	cfg.Auth.CookieSameSite = config.CookieSameSiteStrict

	var buf bytes.Buffer
	if err := writeConfigShow(&buf, cfg.Redacted()); err != nil {
		t.Fatalf("writeConfigShow() error = %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Auth.Mode", "cookie", "Auth.CookieSecure", "true", "Auth.CookieSameSite", "strict"} {
		if !strings.Contains(out, want) {
			t.Fatalf("config show output missing %q:\n%s", want, out)
		}
	}
}

func TestWriteConfigShowIncludesSecurityField(t *testing.T) {
	cfg := config.Default()
	cfg.Security.SanitizeInput = true

	var buf bytes.Buffer
	if err := writeConfigShow(&buf, cfg.Redacted()); err != nil {
		t.Fatalf("writeConfigShow() error = %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Security.SanitizeInput") || !strings.Contains(out, "true") {
		t.Fatalf("config show output missing Security.SanitizeInput=true:\n%s", out)
	}
}
