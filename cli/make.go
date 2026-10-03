package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/gombit-dev/gombit/commandgen"
	logical "github.com/gombit-dev/gombit/field"
	"github.com/gombit-dev/gombit/generate"
	"github.com/gombit-dev/gombit/migrations/schemaplan"
	"github.com/gombit-dev/gombit/resourcegen"
	"github.com/spf13/cobra"
)

func newMakeCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	cmd := silence(&cobra.Command{
		Use:   "make",
		Short: "Generate application code",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			makeUsage(stderr)
			if len(args) == 0 {
				return fmt.Errorf("gombit make: subcommand is required")
			}
			return fmt.Errorf("gombit make: unknown subcommand %q", args[0])
		},
	})
	cmd.AddCommand(newMakeResourceCommand(stdout, stderr))
	cmd.AddCommand(newMakeCommandCommand(stdout, stderr))
	return cmd
}

func newMakeResourceCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	var (
		service        bool
		repo           bool
		dryRun         bool
		force          bool
		skipMigrations bool
		idStrategy     string
	)
	cmd := silence(&cobra.Command{
		Use:   "resource <Name> [field:type[:modifiers]...]",
		Short: "Generate a feature-package resource (AST-safe)",
		Long: `Generate a model-first feature-package (ADR-016) under internal/<name>/,
React list/table + React Hook Form create pages, and its Atlas migration
(unless --skip-migrations; see the end of this help).

  internal/<name>/<name>.go         GORM model plus its gombit:"..." field
                                    policy. Yours: seeded once, kept on
                                    re-runs unless --force.
  internal/<name>/hooks.go          no-op BeforeCreate hook for server-managed
                                    columns. Yours: seeded once.
  internal/<name>/dto.gen.go        request/response DTOs and model mappers.
  internal/<name>/handler.gen.go    thin Huma list/get/create handler over GORM
                                    plus Register(app).
  internal/<name>/.gombit-resource  marker: gombit generate owns this package.
  frontend/src/<name>/list.tsx, form.tsx, and frontend/src/resources.tsx.

The *.gen.go files are generator-owned (DO NOT EDIT): make resource derives
them from the model with gombit generate, and re-running gombit generate
after you edit the model rewrites them. Customize the model, its field
policy, and hooks.go instead.

Wiring uses go/ast (never regex): <name>.Register(app) is added to
cmd/server/main.go next to product.Register(app), and the model is added to
the AutoMigrate call in internal/platform/database.go, which cmd/server runs
on start.

Field grammar:

  name:type[:modifier,...]                          scalar field
  name:relation:Target[,nullable][,on_delete=...]   relation (see Relations)

Scalar modifiers: required, unique, index, nullable, filterable, sortable,
searchable, aggregatable, default=<value>, min=<n> and max=<n> (int, int64,
uint, decimal), max_length=<n> (string, email, url, slug, ip), and
regex=<pattern> (string, text).

` + resourceTypeHelp() + `

  float              float64 (sortable, not aggregatable).
  decimal            money/exact numeric (types.Decimal; decimal(19,4) column).
  decimal(p,s)       pin precision/scale, e.g. decimal(10,2).
  date               calendar date (types.Date; YYYY-MM-DD).
  time               time.Time (RFC3339 in JSON). This is a datetime.
  time_of_day        clock time (types.TimeOfDay; HH:MM:SS). Not the time token.
  duration           Go duration (types.Duration; 1h30m). Stored as nanoseconds.
  uuid               uuid.UUID, stored as char(36).
  json               a JSON object or array (types.JSON; types.NullJSON when
                     optional).
  email, url, ip     strings validated as OpenAPI format email, uri, and ip.
  slug               string matching ^[-a-zA-Z0-9_]+$.
  enum(a,b)          stored values. enum(draft=Draft) separates the stored
                     value from the admin and form label.

Enum values are kept on the model's validate tag. gombit generate emits
that list as a Huma enum of the stored values.

List-query modifiers opt a field into the generated list handler's declared
query surface (safe, indexable subset). The query spelling matches Gombit's
admin data plane so the two contracts stay in sync:

  filterable         exact-match ?<field>=<value> query param. A belongs_to
                     foreign key is filterable by default (GET /children?
                     <parent>_id=<id>) with no modifier needed.
` + resourceCapabilityTypes(logical.AllowsFilter) + `
  sortable           ?ordering=<field> (prefix with - for DESC, e.g.
                     ?ordering=-title). Replaces the fixed id order; id
                     stays the default when ?ordering= is absent.
` + resourceCapabilityTypes(logical.AllowsSort) + `
  searchable         case-insensitive ?search=<term> LIKE across searchable
                     text fields.
` + resourceCapabilityTypes(logical.AllowsSearch) + `

The generated list handler (not the admin data plane) also supports numeric
aggregates:

  aggregatable       server-side ?aggregate=<func>:<field> (func: sum, avg,
                     min, max), computed over the filtered set before
                     pagination and returned in meta.aggregates. Integer
                     aggregates and decimal sum are exact on Postgres/MySQL;
                     SQLite computes avg and fractional decimal sum in float.
                     avg is an approximation on any driver.
` + resourceCapabilityTypes(logical.AllowsAggregate) + `

Relations use name:kind:Target, where Target is a model in internal/<target>/:

  engine:belongs_to:Engine        FK (EngineID) + Engine association; the API
                                  DTO exposes engine_id. nullable makes the FK
                                  a pointer. on_delete is restrict (default),
                                  cascade, or set_null.
  profile:one_to_one:Profile      same as belongs_to, with a unique foreign key.
  parts:has_many:Part             Parts []part.Part; read via the admin. The
                                  child (Part) must carry the RentalID FK.
  warehouses:many_to_many:Warehouse  join table + Warehouses association.

The generated CRUD handler stays thin: belongs_to and one_to_one are exposed
as the foreign key; many_to_many / has_many are generated on the model, not in
the REST handler. The admin picks them up — many_to_many is editable through a
relation widget, has_many is shown read-only. A nullable self-referential
belongs_to or one_to_one is allowed
(parent:belongs_to:Category,nullable,on_delete=set_null). The association is
a pointer so a tree root stores NULL. has_many and many_to_many onto the same
model are rejected: they need explicit join keys. nullable and on_delete on
those two kinds are rejected, because the foreign key lives on the other model.

--id chooses the primary key: uint (the default, an auto-increment ID) or uuid
(an application-assigned uuid.UUID). Both come with CreatedAt and UpdatedAt and
no soft-delete DeletedAt: rows are deleted physically, so the database's ON
DELETE policy is what deletion does (ADR-019).
Composite primary keys are rejected. A belongs_to or one_to_one foreign key
uses the target model's primary key when that model is already on disk, and
uint when it is not. A self relation uses this resource's --id. A model on
disk whose key is not uint or uuid.UUID is rejected. The foreign key stays
filterable (GET /children?<fk>=<id>) for both.

Examples:

  gombit make resource Widget name:string:required price:int
  gombit make resource Article title:string:required,searchable,sortable \
    status:string:filterable author:belongs_to:Author
  gombit make resource Invoice total:decimal:required,aggregatable \
    quantity:int:aggregatable,filterable customer:belongs_to:Customer
  gombit make resource Rental price:decimal:required starts_at:time \
    status:string engine:belongs_to:Engine warehouses:many_to_many:Warehouse
  gombit make resource Invoice --service --repo --dry-run
  gombit make resource Widget --force
  gombit make resource Session --id uuid

--service / --repo are opt-in pass-through files (C6). Default is a thin
handler over GORM. --dry-run prints the file list without writing.
Re-running refuses to clobber user edits unless --force is set.

Frontend pages import types from frontend/src/api/generated and map D10
error.fields into React Hook Form. Run gombit client generate or gombit
dev after the API is up so those types exist. Atlas SQL is generated from
every AutoMigrate model in internal/platform (not only the new resource), so
the atlas binary must be on PATH. Without it, make resource fails before
writing anything so the committed tree never depends on whether Atlas was
installed; pass --skip-migrations to scaffold the resource and registry now
and generate the SQL later with gombit db makemigrations.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// make resource is atomic. It plans both phases before touching the tree
			// and applies the result as one unit, so it can never leave a
			// half-migration (a resource marker with no generated handler, or a
			// registration edit pointing at a Register that was never generated).
			// A missing Atlas fails closed inside resourcegen.Plan (unless
			// --skip-migrations or --dry-run), before anything is written (#300).
			resOpts := resourcegen.Options{
				WorkDir:        ".",
				Name:           args[0],
				Fields:         args[1:],
				ID:             idStrategy,
				Service:        service,
				Repo:           repo,
				DryRun:         dryRun,
				Force:          force,
				SkipMigrations: skipMigrations,
				// The migration make resource writes gets the same plan gate as
				// db makemigrations (#309).
				MigrationGate: schemaplan.Gate(nil, stderr),
				Stdout:        stdout,
				Stderr:        stderr,
			}
			// Phase 1 (no writes): plan the human-owned scaffold — model, marker, AST
			// registration/AutoMigrate, frontend. This runs every static check (app
			// layout, name, HTTP-path collision, the enum gap, the legacy-layout
			// guard), so the command fails closed here, before mutating anything.
			plan, err := resourcegen.Plan(cmd.Context(), resOpts)
			if err != nil {
				return fmt.Errorf("gombit make resource: %w", err)
			}
			// Phase 2 preflight (no writes): render the generator-owned *.gen.go +
			// seed hooks from the pending model, compiled through a Go build overlay
			// so Program Mode validates the scaffold as if it were on disk while the
			// real tree stays untouched. A model that would not compile fails here —
			// still before any write.
			genOpts := generate.Options{WorkDir: ".", Stdout: stdout, Stderr: stderr}
			artifacts, err := generate.PlanResource(cmd.Context(), genOpts, plan.Pending)
			if err != nil {
				return fmt.Errorf("gombit make resource: %w", err)
			}
			// Phase-2 preflight, second step: compile the whole package as it will be
			// committed — the new model + rendered *.gen.go + every preserved
			// human-owned file (hooks, hand-written code) — so customization that no
			// longer matches the regenerated model fails here, not after commit.
			finalOverlay, err := plan.FinalOverlay(artifacts)
			if err != nil {
				return fmt.Errorf("gombit make resource: %w", err)
			}
			if err := generate.ValidateResource(cmd.Context(), genOpts, plan.Pending, finalOverlay); err != nil {
				return fmt.Errorf("gombit make resource: %w", err)
			}
			if dryRun {
				// Print the exact merged plan (scaffold + generated files) the command
				// would apply. Nothing has been written.
				if err := plan.PrintPlan(artifacts); err != nil {
					return fmt.Errorf("gombit make resource: %w", err)
				}
				return nil
			}
			// Commit: apply the whole validated plan as one transaction (rolls back on
			// a filesystem write error), then generate the migration as a post-commit
			// step (a separate artifact, not part of the source-tree transaction).
			if err := plan.Apply(artifacts); err != nil {
				return fmt.Errorf("gombit make resource: %w", err)
			}
			if _, err := fmt.Fprintf(stdout, "GORM model is Atlas-loader ready: %s\n", plan.ModelSpec()); err != nil {
				return fmt.Errorf("gombit make resource: %w", err)
			}
			if err := plan.Migrate(cmd.Context()); err != nil {
				return fmt.Errorf("gombit make resource: %w", err)
			}
			return nil
		},
	})
	cmd.Flags().BoolVar(&service, "service", false, "also write a pass-through service.go")
	cmd.Flags().BoolVar(&repo, "repo", false, "also write a pass-through repo.go")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print files that would be written without writing")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite files that differ from this run")
	cmd.Flags().BoolVar(&skipMigrations, "skip-migrations", false, "scaffold the resource and registry without generating migration SQL (does not require Atlas)")
	cmd.Flags().StringVar(&idStrategy, "id", "uint", "primary key strategy: uint (default, auto-increment) or uuid")
	return cmd
}

func newMakeCommandCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	var (
		pkg    string
		dryRun bool
		force  bool
	)
	cmd := silence(&cobra.Command{
		Use:   "command <name>",
		Short: "Generate a management command (AST-safe)",
		Long: `Generate a Cobra management command in a feature-package and register
it on the app-owned cmd/gombit tree via go/ast (never regex).

The default package is internal/commands. Override with --package to
write internal/<package>/ instead. Registration is explicit:
cmd/gombit calls product.RegisterCommands(root) and <package>.RegisterCommands(root).
There is no reflection discovery and no second command router (D13).

Examples:

  gombit make command greet
  gombit make command hello --package hello --dry-run
  gombit make command greet --force

--dry-run prints the file list without writing. Re-running is
idempotent and will not duplicate RegisterCommands / AddCommand calls.
User-owned command files are refused unless --force is set.

Run the generated command from the application module:

  go run ./cmd/gombit greet
  go run ./cmd/gombit --help`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			err := commandgen.Generate(cmd.Context(), commandgen.Options{
				WorkDir: ".",
				Name:    args[0],
				Package: pkg,
				DryRun:  dryRun,
				Force:   force,
				Stdout:  stdout,
				Stderr:  stderr,
			})
			if err != nil {
				return fmt.Errorf("gombit make command: %w", err)
			}
			return nil
		},
	})
	cmd.Flags().StringVar(&pkg, "package", "commands", "feature-package under internal/ (default commands)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print files that would be written without writing")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite files that differ from this run")
	return cmd
}

// helpWidth is the column the generated parts of `make resource --help` wrap at.
const helpWidth = 78

// resourceScalarSpecs is every scalar kind `gombit make resource` emits, in
// field-catalog order: the resource grammar's scalar types.
func resourceScalarSpecs() []logical.Spec {
	var out []logical.Spec
	for _, kind := range logical.Kinds() {
		spec, ok := logical.Lookup(kind)
		if !ok || !spec.GeneratorReady || kind == logical.Relation || len(spec.CLITokens) == 0 {
			continue
		}
		out = append(out, spec)
	}
	return out
}

// resourceRelationTokens is every relation cardinality the grammar accepts.
func resourceRelationTokens() []string {
	spec, ok := logical.Lookup(logical.Relation)
	if !ok || !spec.GeneratorReady {
		return nil
	}
	return spec.CLITokens
}

// resourceTypeHelp renders the type list of `gombit make resource --help` from
// the field vocabulary (package field), the same catalog the grammar parses
// against, so the help cannot drift from what is accepted.
func resourceTypeHelp() string {
	var scalars []string
	for _, spec := range resourceScalarSpecs() {
		scalars = append(scalars, strings.Join(spec.CLITokens, "/"))
	}
	return wrapHelpList("Scalar types (aliases after /): ", "  ", scalars) + "\n" +
		wrapHelpList("Relation types: ", "  ", resourceRelationTokens())
}

// resourceCapabilityTypes is the "Types:" line under a list-query modifier:
// the scalar types whose kind allows it, per the field vocabulary.
func resourceCapabilityTypes(allows func(logical.Kind, logical.RelationKind) bool) string {
	var tokens []string
	for _, spec := range resourceScalarSpecs() {
		if allows(spec.Kind, "") {
			tokens = append(tokens, spec.CLITokens[0])
		}
	}
	const indent = "                     "
	return wrapHelpList(indent+"Types: ", indent, tokens) + "."
}

// wrapHelpList joins items with ", " after first, wrapping at helpWidth with
// continuation lines starting with indent.
func wrapHelpList(first, indent string, items []string) string {
	var b strings.Builder
	line := first
	for i, item := range items {
		word := item
		if i < len(items)-1 {
			word += ","
		}
		switch {
		case line == first || line == indent:
			line += word
		case len(line)+1+len(word) > helpWidth:
			b.WriteString(line)
			b.WriteByte('\n')
			line = indent + word
		default:
			line += " " + word
		}
	}
	b.WriteString(line)
	return b.String()
}

func makeUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "available make subcommands:")
	_, _ = fmt.Fprintln(w, "  resource <Name> [field:type[:modifiers]...] [--service] [--repo] [--dry-run] [--force]")
	_, _ = fmt.Fprintln(w, "  command <name> [--package commands] [--dry-run] [--force]")
}
