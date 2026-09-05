package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gombit-dev/gombit/config"
	"github.com/spf13/cobra"
)

// Command is the Cobra command type. App-registered management commands
// attach with AddCommand (D13 / ADR-014).
type Command = cobra.Command

// LoadConfig is the typed config loader used by doctor, config show, db, and
// dev. Tests may replace it.
var LoadConfig = config.Load

// LoadConfigFromDir loads configuration for a specific project directory
// without mutating the process (see config.LoadFromDir). Used by
// gombit contract app --dir. A test var so commands can stub it.
var LoadConfigFromDir = config.LoadFromDir

// AddCommand attaches cmds to root via Cobra AddCommand. This is the only
// supported registration path for app-owned management commands.
func AddCommand(root *Command, cmds ...*Command) {
	if root == nil {
		panic("cli: AddCommand requires a non-nil root")
	}
	root.AddCommand(cmds...)
}

// NewRoot returns the framework Cobra tree (new, dev, build, make, db, openapi,
// client, routes, doctor, config, createsuperuser, version). Generated apps call NewRoot, then
// feature-package RegisterCommands, then ExecuteRoot.
func NewRoot(stdout io.Writer, stderr io.Writer) *Command {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	root := &Command{
		Use:           "gombit",
		Short:         "Gombit is a Django-for-Go full-stack framework",
		Long:          rootLongHelp(),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			usage(stderr)
			if len(args) == 0 {
				return errors.New("gombit: command is required")
			}
			return fmt.Errorf("gombit: unknown command %q", args[0])
		},
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.CompletionOptions.DisableDefaultCmd = true
	root.Version = resolveVersion()
	root.SetVersionTemplate("gombit {{.Version}}\n")
	root.AddCommand(newNewCommand(stdout, stderr))
	root.AddCommand(newDevCommand(stdout, stderr))
	root.AddCommand(newBuildCommand(stdout, stderr))
	root.AddCommand(newMakeCommand(stdout, stderr))
	root.AddCommand(newDBCommand(stdout, stderr))
	root.AddCommand(newOpenAPICommand(stdout, stderr))
	root.AddCommand(newContractCommand(stdout, stderr))
	root.AddCommand(newClientCommand(stdout, stderr))
	root.AddCommand(newRoutesCommand(stdout, stderr))
	root.AddCommand(newDoctorCommand(stdout))
	root.AddCommand(newConfigCommand(stdout, stderr))
	root.AddCommand(newCreateSuperuserCommand(stdout))
	root.AddCommand(newVersionCommand(stdout))
	return root
}

// Execute runs a fresh framework root. The framework binary uses this.
func Execute(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) error {
	return ExecuteRoot(ctx, NewRoot(stdout, stderr), args)
}

// ExecuteRoot runs an already-configured root (framework tree plus any
// app-registered commands). Generated cmd/gombit uses this after
// product.RegisterCommands(root).
func ExecuteRoot(ctx context.Context, root *Command, args []string) error {
	if root == nil {
		return errors.New("cli: nil root command")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	root.SetArgs(args)
	return root.ExecuteContext(ctx)
}

func rootLongHelp() string {
	return strings.Join([]string{
		"Gombit is a Django-for-Go full-stack framework.",
		"",
		"Command families:",
		"  new       Scaffold a new application",
		"  dev       Run the API and Vite frontend together",
		"  build     Production build (embed is opt-in via --embed)",
		"  make      Generate application code (resource, command)",
		"  db        Database migrations (makemigrations, migrate, rollback, status, seed, reset)",
		"  openapi   Write the live OpenAPI 3.1 document",
		"  client    Generate and check the TypeScript client",
		"  routes    Print HTTP routes",
		"  doctor    Check Go, Node, config, database, Redis, migrations, and ports",
		"  config    Show typed configuration (config show)",
		"  createsuperuser  Create a superuser (admin) account",
		"  version   Print the gombit version and build metadata",
	}, "\n")
}

func usage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: gombit <command>")
	_, _ = fmt.Fprintln(w, "available commands:")
	_, _ = fmt.Fprintln(w, "  new [name]        scaffold a new Gombit application")
	_, _ = fmt.Fprintln(w, "  dev               run the API and Vite frontend together")
	_, _ = fmt.Fprintln(w, "  build --embed     collectstatic + compile a single binary (opt-in)")
	_, _ = fmt.Fprintln(w, "  make resource     generate a feature-package resource")
	_, _ = fmt.Fprintln(w, "  make command      generate a management command")
	_, _ = fmt.Fprintln(w, "  db <subcommand>   see gombit db")
	_, _ = fmt.Fprintln(w, "  openapi generate [--out openapi.json] [--url http://127.0.0.1:8080/openapi.json]")
	_, _ = fmt.Fprintln(w, "  client generate [--spec openapi.json] [--out frontend/src/api/generated] [--dry-run] [--force]")
	_, _ = fmt.Fprintln(w, "  client check [--write] [--spec openapi.json] [--out frontend/src/api/generated] [--url http://127.0.0.1:8080/openapi.json]")
	_, _ = fmt.Fprintln(w, "  routes [--url http://127.0.0.1:8080]")
	_, _ = fmt.Fprintln(w, "  doctor [--dir database/migrations]")
	_, _ = fmt.Fprintln(w, "  config show")
	_, _ = fmt.Fprintln(w, "  createsuperuser [--email you@example.com] [--password ...] [--no-input]")
	_, _ = fmt.Fprintln(w, "  version [--short]")
}

func dbUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "available db subcommands:")
	_, _ = fmt.Fprintln(w, "  makemigrations <name> --model <import.Type> [--driver sqlite|postgres|mysql]")
	_, _ = fmt.Fprintln(w, "  migrate [--dir database/migrations] [--atlas-bin atlas]")
	_, _ = fmt.Fprintln(w, "  rollback [--dir database/migrations]")
	_, _ = fmt.Fprintln(w, "  status [--dir database/migrations] [--atlas-bin atlas]")
	_, _ = fmt.Fprintln(w, "  seed [--seeds database/seeds]")
	_, _ = fmt.Fprintln(w, "  reset [--dir database/migrations] [--seeds database/seeds] [--atlas-bin atlas] [--force]")
}

func openapiUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "available openapi subcommands:")
	_, _ = fmt.Fprintln(w, "  generate [--out openapi.json] [--url http://127.0.0.1:8080/openapi.json]")
}

func clientUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "available client subcommands:")
	_, _ = fmt.Fprintln(w, "  generate [--spec openapi.json] [--out frontend/src/api/generated] [--dry-run] [--force] [--npx npx]")
	_, _ = fmt.Fprintln(w, "  check [--write] [--spec openapi.json] [--out frontend/src/api/generated] [--url http://127.0.0.1:8080/openapi.json] [--npx npx]")
}

func configUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "available config subcommands:")
	_, _ = fmt.Fprintln(w, "  show    print typed configuration with secrets redacted")
}

func silence(cmd *cobra.Command) *cobra.Command {
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	return cmd
}
