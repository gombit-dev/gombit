package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"golang.org/x/crypto/bcrypt"
)

func testCreateSuperuserConfig(t *testing.T, dbPath string) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Database.Driver = config.DatabaseDriverSQLite
	cfg.Database.DSN = "file:" + dbPath + "?cache=shared&_fk=1"
	cfg.Auth.JWTSecret = "test-jwt-secret-32-bytes-minimum!"
	cfg.Auth.BcryptCost = bcrypt.MinCost
	return cfg
}

// TestRunCreateSuperuserOnFreshDB is the M4-6 acceptance criterion: on a
// fresh database, gombit createsuperuser --no-input --email --password
// creates an admin account whose password is bcrypt-hashed.
func TestRunCreateSuperuserOnFreshDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "app.db")
	stubConfig(t, testCreateSuperuserConfig(t, dbPath))

	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	err := run(context.Background(), []string{
		"createsuperuser",
		"--no-input",
		"--email", "Admin@Example.com",
		"--password", "correct-horse",
	}, stdout, stderr)
	if err != nil {
		t.Fatalf("run() error = %v; stderr=%q", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Superuser created: admin@example.com") {
		t.Fatalf("stdout = %q, want superuser-created message with normalized email", stdout.String())
	}

	db, err := database.Open(config.DatabaseConfig{
		Driver: config.DatabaseDriverSQLite,
		DSN:    "file:" + dbPath + "?cache=shared&_fk=1",
	})
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var user auth.User
	if err := db.Where("email = ?", "admin@example.com").First(&user).Error; err != nil {
		t.Fatalf("load created user: %v", err)
	}
	if !user.IsSuperuser {
		t.Fatal("created user IsSuperuser = false, want true")
	}
	if user.PasswordHash == "correct-horse" {
		t.Fatal("password stored in plaintext")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte("correct-horse")); err != nil {
		t.Fatalf("password hash does not verify: %v", err)
	}
}

// TestRunCreateSuperuserRefusesDuplicate is the M4-6 duplicate-refusal
// acceptance criterion.
func TestRunCreateSuperuserRefusesDuplicate(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "app.db")
	cfg := testCreateSuperuserConfig(t, dbPath)
	stubConfig(t, cfg)

	args := []string{
		"createsuperuser",
		"--no-input",
		"--email", "admin@example.com",
		"--password", "correct-horse",
	}
	if err := run(context.Background(), args, new(bytes.Buffer), new(bytes.Buffer)); err != nil {
		t.Fatalf("first run() error = %v", err)
	}

	err := run(context.Background(), args, new(bytes.Buffer), new(bytes.Buffer))
	if err == nil {
		t.Fatal("second run() error = nil, want duplicate-email refusal")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second run() error = %v, want already-exists message", err)
	}
}

// TestRunCreateSuperuserRefusesAnAccountThatCannotSignIn: an email the login
// endpoint refuses, or a password shorter than a user may set, is refused
// with a message that says why, and nothing is created (issue #461).
func TestRunCreateSuperuserRefusesAnAccountThatCannotSignIn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "app.db")
	cfg := testCreateSuperuserConfig(t, dbPath)
	stubConfig(t, cfg)

	for _, tc := range []struct{ email, password, want string }{
		{"admin", "correct-horse", "not an email address"},
		{"Root Admin <root@example.com>", "correct-horse", "not an email address"},
		{"a b c", "correct-horse", "not an email address"},
		{"admin@example.com", "x", "at least 8 characters"},
	} {
		var stdout bytes.Buffer
		err := run(context.Background(), []string{"createsuperuser", "--no-input", "--email", tc.email, "--password", tc.password}, &stdout, new(bytes.Buffer))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("createsuperuser --email %q --password %q = %v, want an error mentioning %q", tc.email, tc.password, err, tc.want)
		}
		if strings.Contains(stdout.String(), "Superuser created") {
			t.Errorf("createsuperuser --email %q printed %q", tc.email, stdout.String())
		}
	}
	// The same email with a valid password is created: nothing was before.
	if err := run(context.Background(), []string{"createsuperuser", "--no-input", "--email", "admin@example.com", "--password", "correct-horse"}, new(bytes.Buffer), new(bytes.Buffer)); err != nil {
		t.Fatalf("createsuperuser with valid credentials = %v", err)
	}
}

func TestRunCreateSuperuserRequiresJWTSecret(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "app.db")
	cfg := config.Default()
	cfg.Database.Driver = config.DatabaseDriverSQLite
	cfg.Database.DSN = "file:" + dbPath + "?cache=shared&_fk=1"
	stubConfig(t, cfg)

	err := run(context.Background(), []string{
		"createsuperuser",
		"--no-input",
		"--email", "admin@example.com",
		"--password", "correct-horse",
	}, new(bytes.Buffer), new(bytes.Buffer))
	if err == nil {
		t.Fatal("run() error = nil, want JWT-secret-required refusal")
	}
	if !strings.Contains(err.Error(), "GOMBIT_JWT_SECRET") {
		t.Fatalf("run() error = %v, want GOMBIT_JWT_SECRET message", err)
	}
}

func TestRunCreateSuperuserNoInputTable(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "missing email",
			args: []string{"createsuperuser", "--no-input", "--password", "correct-horse"},
			want: "--email is required",
		},
		{
			name: "missing password",
			args: []string{"createsuperuser", "--no-input", "--email", "admin@example.com"},
			want: "--password is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "app.db")
			stubConfig(t, testCreateSuperuserConfig(t, dbPath))

			err := run(context.Background(), tt.args, new(bytes.Buffer), new(bytes.Buffer))
			if err == nil {
				t.Fatal("run() error = nil, want a validation error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("run() error = %v, want message containing %q", err, tt.want)
			}
		})
	}
}

// TestRunCreateSuperuserNotATTYWithoutFlags asserts createsuperuser never
// hangs waiting on stdin in CI: without --no-input, a non-TTY stdin (the
// default in tests) still requires --email/--password.
func TestRunCreateSuperuserNotATTYWithoutFlags(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "app.db")
	stubConfig(t, testCreateSuperuserConfig(t, dbPath))

	err := run(context.Background(), []string{"createsuperuser"}, new(bytes.Buffer), new(bytes.Buffer))
	if err == nil {
		t.Fatal("run() error = nil, want TTY-required refusal")
	}
	if !strings.Contains(err.Error(), "--email is required") {
		t.Fatalf("run() error = %v, want --email is required", err)
	}
}

func TestRunCreateSuperuserProductionRequiresMigratedSchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "app.db")
	cfg := testCreateSuperuserConfig(t, dbPath)
	cfg.Environment = config.EnvironmentProduction
	stubConfig(t, cfg)

	err := run(context.Background(), []string{
		"createsuperuser",
		"--no-input",
		"--email", "admin@example.com",
		"--password", "correct-horse",
	}, new(bytes.Buffer), new(bytes.Buffer))
	if err == nil {
		t.Fatal("run() error = nil, want missing-schema refusal in production")
	}
	if !strings.Contains(err.Error(), "gombit db migrate") {
		t.Fatalf("run() error = %v, want migrate-first message", err)
	}

	db, err := database.Open(config.DatabaseConfig{
		Driver: config.DatabaseDriverSQLite,
		DSN:    cfg.Database.DSN,
	})
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if db.Migrator().HasTable(&auth.User{}) {
		t.Fatal("production createsuperuser AutoMigrated the users table, want no schema change")
	}
}
