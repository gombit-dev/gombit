package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gombit-dev/gombit/config"
	"github.com/spf13/cobra"
)

func newConfigCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	cmd := silence(&cobra.Command{
		Use:   "config",
		Short: "Show typed configuration",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			configUsage(stderr)
			if len(args) == 0 {
				return fmt.Errorf("gombit config: subcommand is required")
			}
			return fmt.Errorf("gombit config: unknown subcommand %q", args[0])
		},
	})
	cmd.AddCommand(newConfigShowCommand(stdout))
	return cmd
}

func newConfigShowCommand(stdout io.Writer) *cobra.Command {
	return silence(&cobra.Command{
		Use:   "show",
		Short: "Print the typed configuration with secrets redacted",
		Long: `Print the configuration returned by config.Load() as aligned
key=value lines. Database DSN userinfo/passwords, the Redis password, and
the JWT secret are replaced with ***** and never printed.

Appendix C rejects a production JWT secret shorter than 32 characters,
the generated-app development placeholder, and a cookie-mode auth
without CookieSecure=true, at config.Load / gombit doctor.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := LoadConfig()
			if err != nil {
				return fmt.Errorf("gombit config show: %w", err)
			}
			return writeConfigShow(stdout, cfg.Redacted())
		},
	})
}

func writeConfigShow(w io.Writer, cfg config.Config) error {
	rows := [][2]string{
		{"AppName", cfg.AppName},
		{"Environment", string(cfg.Environment)},
		{"HTTP.Addr", cfg.HTTP.Addr},
		{"HTTP.TrustedProxies", formatStringList(cfg.HTTP.TrustedProxies)},
		{"HTTP.RequestTimeout", cfg.HTTP.RequestTimeout.String()},
		{"API.Prefix", cfg.API.Prefix},
		{"API.DocsEnabled", strconv.FormatBool(cfg.API.DocsEnabled)},
		{"Database.Driver", string(cfg.Database.Driver)},
		{"Database.Required", strconv.FormatBool(cfg.Database.Required)},
		{"Database.DSN", cfg.Database.DSN},
		{"Database.MaxOpenConns", strconv.Itoa(cfg.Database.MaxOpenConns)},
		{"Database.MaxIdleConns", strconv.Itoa(cfg.Database.MaxIdleConns)},
		{"Database.ConnMaxLifetime", formatDuration(cfg.Database.ConnMaxLifetime)},
		{"Cache.Driver", string(cfg.Cache.Driver)},
		{"Cache.Namespace", cfg.Cache.Namespace},
		{"Cache.Redis.Addr", cfg.Cache.Redis.Addr},
		{"Cache.Redis.Username", cfg.Cache.Redis.Username},
		{"Cache.Redis.Password", cfg.Cache.Redis.Password},
		{"Cache.Redis.DB", strconv.Itoa(cfg.Cache.Redis.DB)},
		{"Cache.Redis.DialTimeout", cfg.Cache.Redis.DialTimeout.String()},
		{"Cache.Redis.ReadTimeout", cfg.Cache.Redis.ReadTimeout.String()},
		{"Cache.Redis.WriteTimeout", cfg.Cache.Redis.WriteTimeout.String()},
		{"Cache.Redis.TLS", strconv.FormatBool(cfg.Cache.Redis.TLS)},
		{"Cache.Redis.TLSInsecure", strconv.FormatBool(cfg.Cache.Redis.TLSInsecure)},
		{"Logging.Level", string(cfg.Logging.Level)},
		{"Logging.Sink", string(cfg.Logging.Sink)},
		{"Auth.JWTSecret", cfg.Auth.JWTSecret},
		{"Auth.AccessTokenTTL", cfg.Auth.AccessTokenTTL.String()},
		{"Auth.RefreshTokenTTL", cfg.Auth.RefreshTokenTTL.String()},
		{"Auth.BcryptCost", strconv.Itoa(cfg.Auth.BcryptCost)},
		{"Auth.Mode", string(cfg.Auth.EffectiveMode())},
		{"Auth.CookieSecure", strconv.FormatBool(cfg.Auth.CookieSecure)},
		{"Auth.CookieSameSite", string(cfg.Auth.EffectiveCookieSameSite())},
	}

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, row := range rows {
		if _, err := fmt.Fprintf(tw, "%s\t%s\n", row[0], row[1]); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func formatStringList(values []string) string {
	if len(values) == 0 {
		return "[]"
	}
	return strings.Join(values, ",")
}

func formatDuration(value time.Duration) string {
	if value == 0 {
		return "0s"
	}
	return value.String()
}
