package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gombit-dev/gombit/cache"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/migrations"
	"github.com/spf13/cobra"
)

const (
	doctorStatusOK   = "ok"
	doctorStatusWarn = "warn"
	doctorStatusFail = "FAIL"
	doctorStatusSkip = "skip"
)

var doctorCheckTimeout = 3 * time.Second

type doctorCheck struct {
	Name    string
	Status  string
	Message string
}

func newDoctorCommand(stdout io.Writer) *cobra.Command {
	cmd := silence(&cobra.Command{
		Use:   "doctor",
		Short: "Check Go, Node, config, database, Redis, migrations, and ports",
		Long: `Run local environment checks and print a status table.

Checks:
  go           go is on PATH (go version)
  node         node is on PATH (warn if missing; required for gombit dev)
  config       config.Load() validates
  database     database.Open + ping when a driver/DSN is set
  redis        ping when GOMBIT_CACHE_DRIVER=redis
  migrations   pending Atlas SQL files in --dir (warn)
  http         GOMBIT_HTTP_ADDR parses; warn if the port is in use
  insecure     existing production/docs and unwritable SQLite path issues

Network checks use a short timeout so CI cannot hang. Appendix C flags a
production JWT secret shorter than 32 characters or equal to the
generated-app development placeholder, and a cookie-mode auth without
CookieSecure=true (config.Load rejects both; the "config" check below
fails loud when either is set).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			migrationDir, err := cmd.Flags().GetString("dir")
			if err != nil {
				return err
			}
			checks := runDoctorChecks(cmd.Context(), doctorOptions{MigrationDir: migrationDir})
			if err := writeDoctorTable(stdout, checks); err != nil {
				return err
			}
			if doctorFailed(checks) {
				return errors.New("gombit doctor: one or more checks failed")
			}
			return nil
		},
	})
	cmd.Flags().String("dir", "database/migrations", "migration directory to inspect for pending SQL")
	return cmd
}

type doctorOptions struct {
	MigrationDir string
}

func runDoctorChecks(ctx context.Context, opts doctorOptions) []doctorCheck {
	checks := []doctorCheck{
		checkGo(ctx),
		checkNode(ctx),
	}

	cfg, err := LoadConfig()
	if err != nil {
		checks = append(checks, doctorCheck{
			Name:    "config",
			Status:  doctorStatusFail,
			Message: config.SanitizeSecretText(err.Error(), config.Config{}),
		})
		checks = append(checks,
			skippedCheck("database", "skipped: config is invalid"),
			skippedCheck("redis", "skipped: config is invalid"),
			skippedCheck("migrations", "skipped: config is invalid"),
			skippedCheck("http", "skipped: config is invalid"),
			skippedCheck("insecure", "skipped: config is invalid"),
		)
		return checks
	}

	checks = append(checks, doctorCheck{
		Name:    "config",
		Status:  doctorStatusOK,
		Message: fmt.Sprintf("valid (%s)", cfg.Environment),
	})
	checks = append(checks, checkDatabase(ctx, cfg))
	checks = append(checks, checkRedis(ctx, cfg))
	checks = append(checks, checkMigrations(ctx, cfg, opts.MigrationDir))
	checks = append(checks, checkHTTPAddr(cfg))
	checks = append(checks, checkInsecure(cfg))
	return checks
}

func checkGo(ctx context.Context) doctorCheck {
	path, err := exec.LookPath("go")
	if err != nil {
		return doctorCheck{Name: "go", Status: doctorStatusFail, Message: "go not found on PATH"}
	}
	// #nosec G204 -- path is exec.LookPath("go"); arguments are the fixed "version" flag.
	cmd := exec.CommandContext(ctx, path, "version")
	out, err := cmd.Output()
	if err != nil {
		return doctorCheck{Name: "go", Status: doctorStatusFail, Message: err.Error()}
	}
	return doctorCheck{Name: "go", Status: doctorStatusOK, Message: strings.TrimSpace(string(out))}
}

func checkNode(ctx context.Context) doctorCheck {
	path, err := exec.LookPath("node")
	if err != nil {
		return doctorCheck{
			Name:    "node",
			Status:  doctorStatusWarn,
			Message: "node not found on PATH (required for gombit dev)",
		}
	}
	// #nosec G204 -- path is exec.LookPath("node"); arguments are the fixed "--version" flag.
	cmd := exec.CommandContext(ctx, path, "--version")
	out, err := cmd.Output()
	if err != nil {
		return doctorCheck{
			Name:    "node",
			Status:  doctorStatusWarn,
			Message: "node --version failed: " + err.Error(),
		}
	}
	return doctorCheck{Name: "node", Status: doctorStatusOK, Message: strings.TrimSpace(string(out))}
}

func checkDatabase(ctx context.Context, cfg config.Config) doctorCheck {
	if strings.TrimSpace(string(cfg.Database.Driver)) == "" && strings.TrimSpace(cfg.Database.DSN) == "" {
		return skippedCheck("database", "no database driver or DSN configured")
	}

	err := runWithTimeout(ctx, doctorCheckTimeout, func(ctx context.Context) error {
		db, err := database.Open(cfg.Database)
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		sqlDB, err := db.SQLDB()
		if err != nil {
			return err
		}
		return sqlDB.PingContext(ctx)
	})
	if err != nil {
		return doctorCheck{
			Name:    "database",
			Status:  doctorStatusFail,
			Message: config.SanitizeError(err, cfg),
		}
	}
	return doctorCheck{
		Name:    "database",
		Status:  doctorStatusOK,
		Message: fmt.Sprintf("%s reachable", cfg.Database.Driver),
	}
}

func checkRedis(ctx context.Context, cfg config.Config) doctorCheck {
	if cfg.Cache.Driver != config.CacheDriverRedis {
		return skippedCheck("redis", fmt.Sprintf("cache driver is %s", cfg.Cache.Driver))
	}

	redisCfg := cfg.Cache.Redis
	if redisCfg.DialTimeout <= 0 || redisCfg.DialTimeout > doctorCheckTimeout {
		redisCfg.DialTimeout = doctorCheckTimeout
	}

	err := runWithTimeout(ctx, doctorCheckTimeout, func(ctx context.Context) error {
		client, err := cache.NewRedisClient(redisCfg)
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		return client.Ping(ctx).Err()
	})
	if err != nil {
		return doctorCheck{
			Name:    "redis",
			Status:  doctorStatusFail,
			Message: config.SanitizeError(err, cfg),
		}
	}
	return doctorCheck{
		Name:    "redis",
		Status:  doctorStatusOK,
		Message: fmt.Sprintf("reachable at %s", cfg.Cache.Redis.Addr),
	}
}

func checkMigrations(ctx context.Context, cfg config.Config, migrationDir string) doctorCheck {
	if strings.TrimSpace(migrationDir) == "" {
		migrationDir = "database/migrations"
	}
	info, err := os.Stat(migrationDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return skippedCheck("migrations", fmt.Sprintf("%s not present", migrationDir))
		}
		return doctorCheck{Name: "migrations", Status: doctorStatusFail, Message: err.Error()}
	}
	if !info.IsDir() {
		return doctorCheck{Name: "migrations", Status: doctorStatusFail, Message: migrationDir + " is not a directory"}
	}

	files, err := migrations.ListMigrationFiles(migrationDir)
	if err != nil {
		return doctorCheck{Name: "migrations", Status: doctorStatusFail, Message: err.Error()}
	}
	if len(files) == 0 {
		return doctorCheck{Name: "migrations", Status: doctorStatusOK, Message: "no migration files"}
	}

	pending, err := pendingMigrationFiles(ctx, cfg, files)
	if err != nil {
		return doctorCheck{
			Name:    "migrations",
			Status:  doctorStatusWarn,
			Message: fmt.Sprintf("%d file(s) on disk; could not compare revisions: %s", len(files), config.SanitizeError(err, cfg)),
		}
	}
	if len(pending) == 0 {
		return doctorCheck{
			Name:    "migrations",
			Status:  doctorStatusOK,
			Message: fmt.Sprintf("%d applied, none pending", len(files)),
		}
	}
	names := make([]string, 0, len(pending))
	for _, file := range pending {
		names = append(names, file.Version+"_"+file.Name)
	}
	return doctorCheck{
		Name:    "migrations",
		Status:  doctorStatusWarn,
		Message: fmt.Sprintf("%d pending: %s", len(pending), strings.Join(names, ", ")),
	}
}

func pendingMigrationFiles(ctx context.Context, cfg config.Config, files []migrations.MigrationFile) ([]migrations.MigrationFile, error) {
	var pending []migrations.MigrationFile
	err := runWithTimeout(ctx, doctorCheckTimeout, func(ctx context.Context) error {
		db, err := database.Open(cfg.Database)
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		if err := db.WithContext(ctx).AutoMigrate(&migrations.Revision{}); err != nil {
			return err
		}
		var revisions []migrations.Revision
		if err := db.WithContext(ctx).Order("version asc").Find(&revisions).Error; err != nil {
			return err
		}
		applied := make(map[string]struct{}, len(revisions))
		for _, rev := range revisions {
			applied[rev.Version] = struct{}{}
		}
		found := make([]migrations.MigrationFile, 0)
		for _, file := range files {
			if _, ok := applied[file.Version]; !ok {
				found = append(found, file)
			}
		}
		pending = found
		return nil
	})
	if err != nil {
		return nil, err
	}
	return pending, nil
}

func checkHTTPAddr(cfg config.Config) doctorCheck {
	addr := strings.TrimSpace(cfg.HTTP.Addr)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return doctorCheck{
			Name:    "http",
			Status:  doctorStatusFail,
			Message: fmt.Sprintf("invalid listen address %q: %s", addr, err.Error()),
		}
	}
	if port == "0" {
		return doctorCheck{Name: "http", Status: doctorStatusOK, Message: addr + " (ephemeral)"}
	}
	dialHost := host
	if dialHost == "" || dialHost == "0.0.0.0" || dialHost == "::" {
		dialHost = "127.0.0.1"
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(dialHost, port), 200*time.Millisecond)
	if err != nil {
		return doctorCheck{Name: "http", Status: doctorStatusOK, Message: addr + " parses, not in use"}
	}
	_ = conn.Close()
	return doctorCheck{
		Name:    "http",
		Status:  doctorStatusWarn,
		Message: addr + " is in use",
	}
}

func checkInsecure(cfg config.Config) doctorCheck {
	var issues []string
	if cfg.Environment == config.EnvironmentProduction && cfg.API.DocsEnabled {
		issues = append(issues, "production has API docs enabled (/docs)")
	}
	if cfg.Environment == config.EnvironmentProduction && cfg.Auth.JWTSecret != "" {
		switch {
		case config.IsInsecureJWTSecret(cfg.Auth.JWTSecret):
			issues = append(issues, "production JWT secret is a publicly known example or development placeholder")
		case len(cfg.Auth.JWTSecret) < config.MinProductionJWTSecretLength:
			issues = append(issues, fmt.Sprintf("production JWT secret is shorter than %d characters", config.MinProductionJWTSecretLength))
		}
	}
	if cfg.Environment == config.EnvironmentProduction && config.IsInsecureStorageURLSecret(cfg.Storage.URLSecret) {
		issues = append(issues, "production storage URL secret is a publicly known example key")
	}
	if cfg.Database.Driver == config.DatabaseDriverSQLite {
		if err := sqliteDSNWritable(cfg.Database.DSN); err != nil {
			issues = append(issues, err.Error())
		}
	}
	if len(issues) == 0 {
		return doctorCheck{Name: "insecure", Status: doctorStatusOK, Message: "no issues in current config fields"}
	}
	return doctorCheck{
		Name:    "insecure",
		Status:  doctorStatusFail,
		Message: strings.Join(issues, "; "),
	}
}

func sqliteDSNWritable(dsn string) error {
	path, memory := sqlitePathFromDSN(dsn)
	if memory {
		return nil
	}
	if strings.TrimSpace(path) == "" {
		return errors.New("sqlite DSN path is empty")
	}
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		dir = "."
	}
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("sqlite path %s: parent directory does not exist", path)
		}
		return fmt.Errorf("sqlite path %s: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("sqlite path %s is not writable: parent is not a directory", path)
	}
	file, err := os.CreateTemp(dir, ".gombit-doctor-*")
	if err != nil {
		return fmt.Errorf("sqlite path %s is not writable", path)
	}
	name := file.Name()
	_ = file.Close()
	_ = os.Remove(name)
	return nil
}

func sqlitePathFromDSN(dsn string) (path string, memory bool) {
	trimmed := strings.TrimSpace(dsn)
	base, query, _ := strings.Cut(trimmed, "?")
	if base == ":memory:" || strings.Contains(base, ":memory:") || strings.Contains(query, "mode=memory") {
		return "", true
	}
	if strings.HasPrefix(base, "file://") {
		parsed, err := url.Parse(base)
		if err == nil && parsed.Path != "" {
			return parsed.Path, false
		}
	}
	return strings.TrimPrefix(base, "file:"), false
}

func skippedCheck(name, message string) doctorCheck {
	return doctorCheck{Name: name, Status: doctorStatusSkip, Message: message}
}

func doctorFailed(checks []doctorCheck) bool {
	for _, check := range checks {
		if check.Status == doctorStatusFail {
			return true
		}
	}
	return false
}

func writeDoctorTable(w io.Writer, checks []doctorCheck) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "STATUS\tCHECK\tDETAIL"); err != nil {
		return err
	}
	for _, check := range checks {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\n", check.Status, check.Name, check.Message); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func runWithTimeout(ctx context.Context, timeout time.Duration, fn func(context.Context) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- fn(ctx)
	}()
	select {
	case err := <-done:
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && err == nil {
			return context.DeadlineExceeded
		}
		return err
	case <-ctx.Done():
		return fmt.Errorf("%w after %s", context.DeadlineExceeded, timeout)
	}
}
