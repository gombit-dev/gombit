package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// newCreateSuperuserCommand builds `gombit createsuperuser` (M4-6), the CLI
// admin seed path over the auth core: config.Load, database.Open,
// prepareAuthSchema, then auth.Service.CreateSuperuser (same hasher and unique
// email path as /auth/register).
func newCreateSuperuserCommand(stdout io.Writer) *cobra.Command {
	var (
		email    string
		password string
		noInput  bool
	)
	cmd := silence(&cobra.Command{
		Use:   "createsuperuser",
		Short: "Create a superuser (admin) account",
		Long: `Create a superuser account against the auth users table
(Django createsuperuser). Requires GOMBIT_JWT_SECRET to be set: without it
Bearer auth is unmounted and there is no way to log the account in.

Prompts for email and password when --email / --password are omitted and
stdin is a TTY. --no-input never prompts and requires both flags (use this
in scripts and tests).

The password is hashed with the same bcrypt hasher as /auth/register
(auth.Service), and duplicate emails are refused with the same unique
constraint.

Development and test AutoMigrate the auth tables so a fresh local DB
works. Production never AutoMigrates: run gombit db migrate first.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := LoadConfig()
			if err != nil {
				return fmt.Errorf("gombit createsuperuser: %w", err)
			}
			if !cfg.Auth.Enabled() {
				return errors.New(
					"gombit createsuperuser: GOMBIT_JWT_SECRET is not set; Bearer auth is disabled, so a superuser could never log in",
				)
			}

			resolvedEmail, resolvedPassword, err := resolveSuperuserCredentials(cmd, email, password, noInput)
			if err != nil {
				return fmt.Errorf("gombit createsuperuser: %w", err)
			}
			// The service's rules, checked before the database is opened
			// (and, outside production, the auth tables migrated) for input
			// that would be refused (#461).
			if err := auth.CheckNewAccount(resolvedEmail, resolvedPassword); err != nil {
				return superuserCredentialError(resolvedEmail, err)
			}

			db, err := database.Open(cfg.Database)
			if err != nil {
				return fmt.Errorf("gombit createsuperuser: open database: %w", err)
			}
			defer func() { _ = db.Close() }()

			if err := prepareAuthSchema(cfg.Environment, db); err != nil {
				return fmt.Errorf("gombit createsuperuser: %w", err)
			}

			svc, err := auth.NewService(db.DB, cfg)
			if err != nil {
				return fmt.Errorf("gombit createsuperuser: %w", err)
			}

			user, err := svc.CreateSuperuser(cmd.Context(), resolvedEmail, resolvedPassword)
			if err != nil {
				if errors.Is(err, auth.ErrInvalidEmail) || errors.As(err, new(auth.PasswordPolicyError)) {
					return superuserCredentialError(resolvedEmail, err)
				}
				if errors.Is(err, auth.ErrEmailTaken) {
					return fmt.Errorf("gombit createsuperuser: a user with email %q already exists", strings.ToLower(strings.TrimSpace(resolvedEmail)))
				}
				return fmt.Errorf("gombit createsuperuser: %w", err)
			}

			_, werr := fmt.Fprintf(stdout, "Superuser created: %s (id=%d)\n", user.Email, user.ID)
			return werr
		},
	})
	cmd.Flags().StringVar(&email, "email", "", "superuser account email")
	cmd.Flags().StringVar(&password, "password", "", "superuser account password (prefer the interactive prompt; --password is visible in shell history and process listings)")
	cmd.Flags().BoolVar(&noInput, "no-input", false, "never prompt; require --email and --password")
	return cmd
}

// superuserCredentialError words a refusal of the account's email or
// password (auth.CheckNewAccount) for the operator.
func superuserCredentialError(email string, err error) error {
	var policy auth.PasswordPolicyError
	if errors.As(err, &policy) {
		return fmt.Errorf("gombit createsuperuser: %w", policy.Err)
	}
	return fmt.Errorf("gombit createsuperuser: %q is not a bare email address of at most 255 characters (such as admin@example.com)", email)
}

func prepareAuthSchema(env config.Environment, db *database.DB) error {
	if env == config.EnvironmentProduction {
		if db.Migrator().HasTable(&auth.User{}) {
			return nil
		}
		return errors.New("users table is missing; run gombit db migrate first (production never AutoMigrates)")
	}
	if err := auth.Migrate(db.DB); err != nil {
		return fmt.Errorf("migrate auth tables: %w", err)
	}
	return nil
}

func resolveSuperuserCredentials(cmd *cobra.Command, email, password string, noInput bool) (string, string, error) {
	email = strings.TrimSpace(email)

	if noInput {
		if email == "" {
			return "", "", errors.New("--email is required with --no-input")
		}
		if password == "" {
			return "", "", errors.New("--password is required with --no-input")
		}
		return email, password, nil
	}

	stdin := cmd.InOrStdin()
	stdout := cmd.OutOrStdout()
	tty := isTerminalReader(stdin)

	if email == "" {
		if !tty {
			return "", "", errors.New("--email is required (stdin is not a TTY; pass flags or use --no-input)")
		}
		prompted, err := promptSuperuserEmail(stdout, stdin)
		if err != nil {
			return "", "", err
		}
		email = prompted
	}

	if password == "" {
		if !tty {
			return "", "", errors.New("--password is required (stdin is not a TTY; pass flags or use --no-input)")
		}
		prompted, err := promptSuperuserPassword(stdout, stdin)
		if err != nil {
			return "", "", err
		}
		password = prompted
	}

	return email, password, nil
}

func promptSuperuserEmail(stdout io.Writer, stdin io.Reader) (string, error) {
	if _, err := fmt.Fprint(stdout, "Email: "); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("read email: %w", err)
	}
	email := strings.TrimSpace(line)
	if email == "" {
		return "", errors.New("email is required")
	}
	return email, nil
}

func promptSuperuserPassword(stdout io.Writer, stdin io.Reader) (string, error) {
	file, ok := stdin.(*os.File)
	if !ok {
		return "", errors.New("password prompt requires a terminal")
	}

	first, err := readHiddenPassword(stdout, file, "Password: ")
	if err != nil {
		return "", err
	}
	second, err := readHiddenPassword(stdout, file, "Password (again): ")
	if err != nil {
		return "", err
	}
	if first != second {
		return "", errors.New("passwords did not match")
	}
	if first == "" {
		return "", errors.New("password is required")
	}
	return first, nil
}

func readHiddenPassword(stdout io.Writer, file *os.File, prompt string) (string, error) {
	if _, err := fmt.Fprint(stdout, prompt); err != nil {
		return "", err
	}
	raw, err := term.ReadPassword(int(file.Fd()))
	if _, nerr := fmt.Fprintln(stdout); nerr != nil {
		return "", nerr
	}
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	return string(raw), nil
}

// isTerminalReader reports whether r is an interactive terminal. It uses
// term.IsTerminal (an ioctl probe) rather than os.ModeCharDevice: /dev/null
// and other non-terminal devices also report as character devices, which
// would otherwise make createsuperuser think a non-interactive test/CI
// stdin is a TTY and block on a prompt.
func isTerminalReader(r io.Reader) bool {
	file, ok := r.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(file.Fd()))
}
