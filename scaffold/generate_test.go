package scaffold

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/migrations"
)

func TestGenerateWritesFeaturePackageLayout(t *testing.T) {
	workDir := t.TempDir()
	stdout := new(bytes.Buffer)
	err := Generate(context.Background(), Options{
		Name:     "demo",
		Database: "sqlite",
		WorkDir:  workDir,
		Stdout:   stdout,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	dest := filepath.Join(workDir, "demo")
	wantFiles := []string{
		"cmd/server/main.go",
		"cmd/gombit/main.go",
		"internal/platform/database.go",
		"internal/product/product.go",
		"internal/product/handler.go",
		"internal/product/routes.go",
		"internal/product/commands.go",
		"internal/web/embed.go",
		"internal/web/static/.keep",
		"database/migrations/.gitkeep",
		"database/seeds/.gitkeep",
		"config/README.md",
		"frontend/package.json",
		"frontend/vite.config.ts",
		"frontend/index.html",
		"frontend/src/main.tsx",
		"frontend/src/resources.tsx",
		"frontend/src/app/providers.tsx",
		"frontend/src/app/router.tsx",
		"frontend/src/api/client.ts",
		"frontend/src/api/apiPrefix.ts",
		"frontend/src/api/retry.ts",
		"frontend/src/api/formErrors.ts",
		"frontend/src/api/generated/client.ts",
		"frontend/src/api/generated/error.ts",
		"frontend/src/api/generated/schema.ts",
		"frontend/src/auth/session.ts",
		"frontend/src/auth/RequireAuth.tsx",
		"frontend/src/layouts/AppLayout.tsx",
		"frontend/src/pages/LoginPage.tsx",
		"frontend/src/pages/ProductListPage.tsx",
		"frontend/src/pages/ProductFormPage.tsx",
		"frontend/src/vite-env.d.ts",
		"frontend/tsconfig.json",
		"frontend/README.md",
		".air.toml",
		"gombit.yaml",
		".env.example",
		".env",
		"go.mod",
		"README.md",
		".gitignore",
	}
	for _, rel := range wantFiles {
		path := filepath.Join(dest, filepath.FromSlash(rel))
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing %s: %v", rel, err)
		}
	}
	for _, rel := range []string{"internal/product/service.go", "internal/product/repo.go", "frontend/src/theme.ts"} {
		path := filepath.Join(dest, filepath.FromSlash(rel))
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("unexpected generated file %s", rel)
		}
	}

	goMod := readFile(t, filepath.Join(dest, "go.mod"))
	if !strings.Contains(goMod, "module github.com/example/demo") {
		t.Fatalf("go.mod module = %q, want github.com/example/demo", goMod)
	}
	if !strings.Contains(goMod, "github.com/gombit-dev/gombit") {
		t.Fatalf("go.mod missing gombit require:\n%s", goMod)
	}
	if strings.Contains(goMod, "replace ") {
		t.Fatal("generated go.mod must not include a replace directive")
	}

	yaml := readFile(t, filepath.Join(dest, "gombit.yaml"))
	for _, want := range []string{"database: sqlite", "cache: memory", "auth: jwt", "ui: minimal"} {
		if !strings.Contains(yaml, want) {
			t.Fatalf("gombit.yaml missing %q:\n%s", want, yaml)
		}
	}

	envExample := readFile(t, filepath.Join(dest, ".env.example"))
	if !strings.Contains(envExample, "GOMBIT_DATABASE_DRIVER=sqlite") {
		t.Fatalf(".env.example missing sqlite driver:\n%s", envExample)
	}
	// The DSN value carries `&`/`?`; it must be quoted so `set -a; . ./.env`
	// does not background the line and truncate the value. See #225 item 3.
	if !strings.Contains(envExample, `GOMBIT_DATABASE_DSN="`) {
		t.Fatalf(".env.example DSN must be shell-quote-safe:\n%s", envExample)
	}
	if !strings.Contains(envExample, "VITE_API_URL=") {
		t.Fatalf(".env.example missing public VITE_API_URL:\n%s", envExample)
	}
	if strings.Contains(envExample, "VITE_API_URL=http://127.0.0.1:8080/api/v1") {
		t.Fatal(".env.example must not bake /api/v1 into VITE_API_URL; OpenAPI paths already include the prefix")
	}
	if !strings.Contains(envExample, "GOMBIT_JWT_SECRET=") {
		t.Fatal(".env.example missing GOMBIT_JWT_SECRET placeholder")
	}
	if !strings.Contains(envExample, "GOMBIT_JWT_SECRET="+config.DevelopmentJWTPlaceholder) {
		t.Fatalf(".env.example JWT placeholder = %q, want %s", envExample, config.DevelopmentJWTPlaceholder)
	}
	if strings.Contains(envExample, "change-me-in-development-use-a-long-random-value") {
		t.Fatal(".env.example still has the historical long JWT placeholder")
	}
	if strings.Contains(envExample, "VITE_JWT") {
		t.Fatal(".env.example must not put JWT material in VITE_*")
	}
	dotenv := readFile(t, filepath.Join(dest, ".env"))
	if strings.Contains(dotenv, "GOMBIT_JWT_SECRET="+config.DevelopmentJWTPlaceholder) {
		t.Fatal(".env must not reuse the .env.example JWT placeholder")
	}
	secret := jwtSecretFromDotEnv(t, dotenv)
	if len(secret) < config.MinProductionJWTSecretLength {
		t.Fatalf(".env JWT secret length = %d, want >= %d", len(secret), config.MinProductionJWTSecretLength)
	}
	if config.IsInsecureJWTSecret(secret) {
		t.Fatal(".env JWT secret is a known insecure placeholder")
	}
	if strings.Contains(dotenv, "shorter than 32") {
		t.Fatal(".env JWT comment must not claim the generated secret is shorter than 32 characters")
	}
	if !strings.Contains(dotenv, "gombit new generated this as a random") {
		t.Fatalf(".env JWT comment was not swapped for the per-project-secret version:\n%s", dotenv)
	}
	if !strings.Contains(stdout.String(), "create demo/go.mod") {
		t.Fatalf("stdout = %q, want created file list", stdout.String())
	}
	if !strings.Contains(stdout.String(), "create demo/.env") {
		t.Fatalf("stdout = %q, want generated .env", stdout.String())
	}

	cliMain := readFile(t, filepath.Join(dest, "cmd", "gombit", "main.go"))
	if !strings.Contains(cliMain, "product.RegisterCommands") {
		t.Fatal("cmd/gombit/main.go does not call product.RegisterCommands")
	}
	if !strings.Contains(cliMain, "cli.NewRoot") {
		t.Fatal("cmd/gombit/main.go does not use cli.NewRoot")
	}
	commands := readFile(t, filepath.Join(dest, "internal", "product", "commands.go"))
	if !strings.Contains(commands, "func RegisterCommands") {
		t.Fatal("internal/product/commands.go missing RegisterCommands")
	}

	embedGo := readFile(t, filepath.Join(dest, "internal", "web", "embed.go"))
	if !strings.Contains(embedGo, "//go:embed all:static") {
		t.Fatal("internal/web/embed.go missing //go:embed all:static")
	}
	if !strings.Contains(embedGo, "func FS()") {
		t.Fatal("internal/web/embed.go missing FS helper")
	}
	indexPath := filepath.Join(dest, "internal", "web", "static", "index.html")
	if _, err := os.Stat(indexPath); !os.IsNotExist(err) {
		t.Fatal("placeholder embed must not include index.html")
	}
	serverMain := readFile(t, filepath.Join(dest, "cmd", "server", "main.go"))
	if !strings.Contains(serverMain, "framework.WithEmbeddedFrontend(web.FS())") {
		t.Fatal("cmd/server/main.go does not pass WithEmbeddedFrontend")
	}
	gitignore := readFile(t, filepath.Join(dest, ".gitignore"))
	for _, want := range []string{"internal/web/static/*", "!internal/web/static/.keep", "bin/"} {
		if !strings.Contains(gitignore, want) {
			t.Fatalf(".gitignore missing %q:\n%s", want, gitignore)
		}
	}

	viteConfig := readFile(t, filepath.Join(dest, "frontend", "vite.config.ts"))
	for _, want := range []string{"/api", "/openapi.json", "/docs", "/admin", "GOMBIT_DEV_FRONTEND_HOST", "GOMBIT_API_PREFIX", "__GOMBIT_API_PREFIX__", "injectAPIPrefix"} {
		if !strings.Contains(viteConfig, want) {
			t.Fatalf("vite.config.ts missing %q:\n%s", want, viteConfig)
		}
	}
	appReadme := readFile(t, filepath.Join(dest, "README.md"))
	if !strings.Contains(appReadme, "Backend, Frontend, OpenAPI, API docs") {
		t.Fatal("README.md missing gombit dev service table description")
	}
	if strings.Contains(appReadme, "API docs, Admin") {
		t.Fatal("jwt README.md must not list Admin in the gombit dev service table")
	}
	if strings.Contains(appReadme, "cp .env.example .env") {
		t.Fatal("README.md must not copy .env.example over the generated .env JWT secret")
	}
	if !strings.Contains(appReadme, "go run ./cmd/server") {
		t.Fatal("README.md missing go run ./cmd/server")
	}
	indexHTML := readFile(t, filepath.Join(dest, "frontend", "index.html"))
	if !strings.Contains(indexHTML, `content="__GOMBIT_API_PREFIX__"`) {
		t.Fatal("frontend/index.html missing __GOMBIT_API_PREFIX__ placeholder")
	}
	pkg := readFile(t, filepath.Join(dest, "frontend", "package.json"))
	for _, want := range []string{`"vite"`, `"react"`, `"react-dom"`, `"react-router"`, `"react-hook-form"`, `"@vitejs/plugin-react"`, `"openapi-fetch": "0.17.0"`} {
		if !strings.Contains(pkg, want) {
			t.Fatalf("frontend/package.json missing %s:\n%s", want, pkg)
		}
	}
	if strings.Contains(pkg, `"@mui/`) {
		t.Fatal("minimal skeleton must not include MUI (M5-4)")
	}
	mainTSX := readFile(t, filepath.Join(dest, "frontend", "src", "main.tsx"))
	if strings.Contains(strings.ToLower(mainTSX), "localstorage") {
		t.Fatal("frontend/src/main.tsx uses localStorage")
	}
	if !strings.Contains(mainTSX, `from "./app/providers"`) || !strings.Contains(mainTSX, `from "./app/router"`) {
		t.Fatal("frontend/src/main.tsx does not mount providers and router")
	}
	formErrors := readFile(t, filepath.Join(dest, "frontend", "src", "api", "formErrors.ts"))
	for _, want := range []string{"setError", "fields", "ContractError", "isD10ErrorBody"} {
		if !strings.Contains(formErrors, want) {
			t.Fatalf("formErrors.ts missing %q:\n%s", want, formErrors)
		}
	}
	if strings.Contains(strings.ToLower(formErrors), "localstorage") {
		t.Fatal("formErrors.ts uses localStorage")
	}
	session := readFile(t, filepath.Join(dest, "frontend", "src", "auth", "session.ts"))
	if !strings.Contains(session, "getAccessToken") {
		t.Fatal("auth/session.ts missing getAccessToken")
	}
	if !strings.Contains(session, "getRefreshToken") || !strings.Contains(session, "clearSession") {
		t.Fatal("auth/session.ts missing refresh token helpers")
	}
	if strings.Contains(strings.ToLower(session), "localstorage") || strings.Contains(strings.ToLower(session), "sessionstorage") {
		t.Fatal("auth/session.ts uses web storage")
	}
	loginPage := readFile(t, filepath.Join(dest, "frontend", "src", "pages", "LoginPage.tsx"))
	platformDB := readFile(t, filepath.Join(dest, "internal", "platform", "database.go"))
	if !strings.Contains(platformDB, "&auth.User{}") || !strings.Contains(platformDB, "&auth.RefreshToken{}") {
		t.Fatal("internal/platform/database.go AutoMigrate must include auth models for Atlas")
	}
	if strings.Contains(platformDB, "auth.Migrate(") {
		t.Fatal("internal/platform/database.go should AutoMigrate auth models directly so CollectAutoMigrateModels sees them")
	}

	if !strings.Contains(loginPage, "/api/v1/auth/login") {
		t.Fatal("LoginPage.tsx missing login path")
	}
	if strings.Contains(loginPage, "bootstrapCSRF") {
		t.Fatal("jwt LoginPage.tsx must not import bootstrapCSRF")
	}
	if strings.Contains(strings.ToLower(loginPage), "localstorage") {
		t.Fatal("LoginPage.tsx uses localStorage")
	}
	if strings.Contains(loginPage, "required: true") {
		t.Fatal("LoginPage.tsx required rule has no message; submitting an empty field will show no error text")
	}
	if !strings.Contains(loginPage, `required: "Email is required"`) || !strings.Contains(loginPage, `required: "Password is required"`) {
		t.Fatalf("LoginPage.tsx missing required-field error messages:\n%s", loginPage)
	}
	productFormPage := readFile(t, filepath.Join(dest, "frontend", "src", "pages", "ProductFormPage.tsx"))
	if strings.Contains(productFormPage, "required: true") {
		t.Fatal("ProductFormPage.tsx required rule has no message")
	}
	if !strings.Contains(productFormPage, `required: "Name is required"`) {
		t.Fatalf("ProductFormPage.tsx missing required-field error message:\n%s", productFormPage)
	}
	assertNumberInputEmptyIsZero(t, productFormPage, "ProductFormPage.tsx")
	appClient := readFile(t, filepath.Join(dest, "frontend", "src", "api", "client.ts"))
	if !strings.Contains(appClient, "refreshInFlight") {
		t.Fatal("api/client.ts missing shared refresh promise")
	}
	if strings.Contains(appClient, "csrfInFlight") || strings.Contains(appClient, "bootstrapCSRF") {
		t.Fatal("jwt api/client.ts must not include cookie CSRF bootstrap")
	}
	providers := readFile(t, filepath.Join(dest, "frontend", "src", "app", "providers.tsx"))
	if strings.Contains(providers, "bootstrapCSRF") {
		t.Fatal("jwt AppProviders must not import bootstrapCSRF")
	}
	assertSPAHonorsRuntimeAPIPrefix(t, dest, "jwt")
	router := readFile(t, filepath.Join(dest, "frontend", "src", "app", "router.tsx"))
	if !strings.Contains(router, "RequireAuth") || !strings.Contains(router, "LoginPage") {
		t.Fatal("router.tsx missing RequireAuth / LoginPage")
	}
	schema := readFile(t, filepath.Join(dest, "frontend", "src", "api", "generated", "schema.ts"))
	if !strings.Contains(schema, "Code generated by gombit client generate") {
		t.Fatal("placeholder schema.ts missing generated banner")
	}
	if !strings.Contains(schema, `"/api/v1/products"`) {
		t.Fatal("placeholder schema.ts missing product path")
	}
	if !strings.Contains(schema, `"/api/v1/auth/login"`) || !strings.Contains(schema, `"/api/v1/me"`) {
		t.Fatal("placeholder schema.ts missing auth paths")
	}
	if !strings.Contains(schema, "per_page?: number") {
		t.Fatal("placeholder schema.ts missing list-products per_page query param")
	}
	handler := readFile(t, filepath.Join(dest, "internal", "product", "handler.go"))
	if !strings.Contains(handler, "contract.ClampPage") || !strings.Contains(handler, `query:"per_page"`) {
		t.Fatal("product handler missing page/per_page list pagination")
	}
}

func TestGenerateDryRunWritesNothing(t *testing.T) {
	workDir := t.TempDir()
	stdout := new(bytes.Buffer)
	err := Generate(context.Background(), Options{
		Name:     "demo",
		Database: "sqlite",
		WorkDir:  workDir,
		DryRun:   true,
		Stdout:   stdout,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "demo")); !os.IsNotExist(err) {
		t.Fatal("dry-run created destination directory")
	}
	if !strings.Contains(stdout.String(), "create demo/go.mod") {
		t.Fatalf("stdout = %q, want file list", stdout.String())
	}
}

func TestGenerateRefusesNonEmptyDestinationWithoutForce(t *testing.T) {
	workDir := t.TempDir()
	dest := filepath.Join(workDir, "demo")
	if err := os.MkdirAll(dest, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dest, "owned.txt"), []byte("user"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	err := Generate(context.Background(), Options{
		Name:     "demo",
		Database: "sqlite",
		WorkDir:  workDir,
	})
	if err == nil {
		t.Fatal("Generate() error = nil, want non-empty destination")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Fatalf("Generate() error = %q, want --force hint", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "go.mod")); !os.IsNotExist(err) {
		t.Fatal("refused generate still wrote go.mod")
	}

	err = Generate(context.Background(), Options{
		Name:     "demo",
		Database: "sqlite",
		WorkDir:  workDir,
		Force:    true,
	})
	if err != nil {
		t.Fatalf("Generate(--force) error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "go.mod")); err != nil {
		t.Fatalf("force did not write go.mod: %v", err)
	}
	owned := readFile(t, filepath.Join(dest, "owned.txt"))
	if owned != "user" {
		t.Fatalf("force overwrote user-owned file: %q", owned)
	}
}

func TestGenerateFlagValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts Options
		want string
	}{
		{
			name: "unknown database",
			opts: Options{Name: "demo", Database: "oracle"},
			want: "database",
		},
		{
			name: "unknown cache",
			opts: Options{Name: "demo", Cache: "memcached"},
			want: "cache",
		},
		{
			name: "unknown auth",
			opts: Options{Name: "demo", Auth: "oauth"},
			want: "auth",
		},
		{
			name: "auth none not implemented",
			opts: Options{Name: "demo", Auth: "none"},
			want: "auth",
		},
		{
			name: "unknown ui",
			opts: Options{Name: "demo", UI: "bootstrap"},
			want: "ui",
		},
		{
			name: "path name",
			opts: Options{Name: "../evil"},
			want: "project name",
		},
		{
			name: "missing name non-interactive",
			opts: Options{Database: "sqlite", IsTTY: func() bool { return false }},
			want: "project name is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.opts.WorkDir = t.TempDir()
			err := Generate(context.Background(), tt.opts)
			if err == nil {
				t.Fatal("Generate() error = nil, want validation error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Generate() error = %q, want %q", err, tt.want)
			}
		})
	}
}

func TestGenerateRecordsAuthAndUIChoices(t *testing.T) {
	workDir := t.TempDir()
	err := Generate(context.Background(), Options{
		Name:     "shop",
		Module:   "example.com/shop",
		Database: "postgres",
		Cache:    "redis",
		Auth:     "cookie",
		UI:       "mui",
		WorkDir:  workDir,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	yaml := readFile(t, filepath.Join(workDir, "shop", "gombit.yaml"))
	for _, want := range []string{
		"module: example.com/shop",
		"database: postgres",
		"cache: redis",
		"auth: cookie",
		"ui: mui",
	} {
		if !strings.Contains(yaml, want) {
			t.Fatalf("gombit.yaml missing %q:\n%s", want, yaml)
		}
	}
	appReadme := readFile(t, filepath.Join(workDir, "shop", "README.md"))
	if !strings.Contains(appReadme, "API docs, Admin") {
		t.Fatal("cookie README.md missing Admin in the gombit dev service table")
	}
	envExample := readFile(t, filepath.Join(workDir, "shop", ".env.example"))
	if !strings.Contains(envExample, "GOMBIT_DATABASE_DRIVER=postgres") {
		t.Fatalf(".env.example missing postgres driver:\n%s", envExample)
	}
	if !strings.Contains(envExample, "USER:PASSWORD") {
		t.Fatalf(".env.example missing DSN placeholders:\n%s", envExample)
	}

	pkg := readFile(t, filepath.Join(workDir, "shop", "frontend", "package.json"))
	if !strings.Contains(pkg, `"@mui/material"`) {
		t.Fatal("cookie + mui package.json missing @mui/material")
	}
	providers := readFile(t, filepath.Join(workDir, "shop", "frontend", "src", "app", "providers.tsx"))
	if !strings.Contains(providers, "ThemeProvider") || !strings.Contains(providers, "CssBaseline") {
		t.Fatal("cookie + mui providers.tsx missing ThemeProvider/CssBaseline")
	}
	layout := readFile(t, filepath.Join(workDir, "shop", "frontend", "src", "layouts", "AppLayout.tsx"))
	if !strings.Contains(layout, "AppBar") {
		t.Fatal("cookie + mui AppLayout.tsx missing AppBar")
	}
	login := readFile(t, filepath.Join(workDir, "shop", "frontend", "src", "pages", "LoginPage.tsx"))
	if !strings.Contains(login, "TextField") || !strings.Contains(login, "Paper") {
		t.Fatal("cookie + mui LoginPage.tsx missing MUI Paper/TextField")
	}
	if !strings.Contains(login, "bootstrapCSRF") {
		t.Fatal("cookie + mui LoginPage.tsx missing bootstrapCSRF")
	}
	if strings.Contains(strings.ToLower(login), "localstorage") {
		t.Fatal("cookie + mui LoginPage.tsx uses localStorage")
	}
	if strings.Contains(login, "required: true") {
		t.Fatal("cookie + mui LoginPage.tsx required rule has no message")
	}
	if !strings.Contains(login, `required: "Email is required"`) || !strings.Contains(login, `required: "Password is required"`) {
		t.Fatalf("cookie + mui LoginPage.tsx missing required-field error messages:\n%s", login)
	}
	productForm := readFile(t, filepath.Join(workDir, "shop", "frontend", "src", "pages", "ProductFormPage.tsx"))
	if strings.Contains(productForm, "required: true") {
		t.Fatal("cookie + mui ProductFormPage.tsx required rule has no message")
	}
	if !strings.Contains(productForm, `required: "Name is required"`) {
		t.Fatalf("cookie + mui ProductFormPage.tsx missing required-field error message:\n%s", productForm)
	}
	client := readFile(t, filepath.Join(workDir, "shop", "frontend", "src", "api", "client.ts"))
	if !strings.Contains(client, "X-CSRF-Token") {
		t.Fatal("cookie + mui client.ts missing X-CSRF-Token")
	}
	list := readFile(t, filepath.Join(workDir, "shop", "frontend", "src", "pages", "ProductListPage.tsx"))
	if !strings.Contains(list, "Table") || !strings.Contains(list, "@mui/material") {
		t.Fatal("cookie + mui ProductListPage.tsx missing MUI Table")
	}
}

// TestGenerate401RetryReusesBufferedBody is the #106 contract: after fetch
// consumes request.body, `new Request(request, ...)` throws in the browser
// (or retries POST/PATCH with an empty body). Both auth branches must buffer
// the body in onRequest and rebuild the retry from those bytes.
func TestGenerate401RetryReusesBufferedBody(t *testing.T) {
	for _, tt := range []struct {
		name string
		auth string
		want []string
	}{
		{
			name: "jwt",
			auth: "jwt",
			want: []string{`headers.set("Authorization", ` + "`Bearer ${access}`" + `)`},
		},
		{
			name: "cookie",
			auth: "cookie",
			want: []string{`headers.set("X-CSRF-Token", token)`},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			workDir := t.TempDir()
			err := Generate(context.Background(), Options{
				Name:     "demo",
				Database: "sqlite",
				Auth:     tt.auth,
				WorkDir:  workDir,
			})
			if err != nil {
				t.Fatalf("Generate() error = %v", err)
			}
			apiDir := filepath.Join(workDir, "demo", "frontend", "src", "api")
			assert401RetryReusesBufferedBody(t, readFile(t, filepath.Join(apiDir, "client.ts")), readFile(t, filepath.Join(apiDir, "retry.ts")), tt.want)
		})
	}
}

// TestGenerateSPAHonorsRuntimeAPIPrefix is the #109 contract: changing
// GOMBIT_API_PREFIX must not require regenerating frontend source. Plumbing
// goes through apiPrefix()/apiPath()/rewriteAPIRequest; index.html keeps the
// serve-time placeholder instead of baking /api/v1.
func TestGenerateSPAHonorsRuntimeAPIPrefix(t *testing.T) {
	for _, auth := range []string{"jwt", "cookie"} {
		t.Run(auth, func(t *testing.T) {
			workDir := t.TempDir()
			err := Generate(context.Background(), Options{
				Name:     "demo",
				Database: "sqlite",
				Auth:     auth,
				WorkDir:  workDir,
			})
			if err != nil {
				t.Fatalf("Generate() error = %v", err)
			}
			assertSPAHonorsRuntimeAPIPrefix(t, filepath.Join(workDir, "demo"), auth)
		})
	}
}

func TestAPIPrefixRewriteHonorsInjectedPrefix(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	harness := filepath.Join(filepath.Dir(thisFile), "testdata", "api_prefix_harness.mjs")
	workDir := t.TempDir()
	err := Generate(context.Background(), Options{
		Name:     "demo",
		Database: "sqlite",
		WorkDir:  workDir,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	prefixPath := filepath.Join(workDir, "demo", "frontend", "src", "api", "apiPrefix.ts")
	cmd := exec.Command("node", "--experimental-strip-types", "--no-warnings", harness, prefixPath) //nolint:gosec // fixed harness argv
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("api prefix harness: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ok") {
		t.Fatalf("api prefix harness stdout = %q, want ok", out)
	}
}

func TestRetryBodyResentWhenRequestBodyUndefined(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	harness := filepath.Join(filepath.Dir(thisFile), "testdata", "retry_body_harness.mjs")
	for _, auth := range []string{"jwt", "cookie"} {
		t.Run(auth, func(t *testing.T) {
			workDir := t.TempDir()
			err := Generate(context.Background(), Options{
				Name:     "demo",
				Database: "sqlite",
				Auth:     auth,
				WorkDir:  workDir,
			})
			if err != nil {
				t.Fatalf("Generate() error = %v", err)
			}
			retryPath := filepath.Join(workDir, "demo", "frontend", "src", "api", "retry.ts")
			cmd := exec.Command("node", "--experimental-strip-types", "--no-warnings", harness, retryPath) //nolint:gosec // fixed harness argv
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("retry body harness: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), "ok") {
				t.Fatalf("retry body harness stdout = %q, want ok", out)
			}
		})
	}
}

func assert401RetryReusesBufferedBody(t *testing.T, client, retry string, extra []string) {
	t.Helper()
	if strings.Contains(client, "new Request(request") {
		t.Error("api/client.ts 401 retry clones a consumed Request; rebuild from buffered body bytes instead")
	}
	for _, src := range []string{client, retry} {
		if strings.Contains(src, "request.body !=") || strings.Contains(src, "request.body !==") {
			t.Error("401 retry must not gate on Request.body (undefined in Firefox); gate on method")
		}
	}
	for _, want := range append([]string{
		"refreshInFlight",
		"isAuthURL",
		`from "./retry"`,
		"bufferRetryBody",
		"fetch(request.url",
		"retryInit(",
	}, extra...) {
		if !strings.Contains(client, want) {
			t.Errorf("api/client.ts missing %q", want)
		}
	}
	for _, want := range []string{
		`request.method !== "GET" && request.method !== "HEAD"`,
		"request.clone().arrayBuffer()",
		"init.body = body",
		"signal: request.signal",
		"mode: request.mode",
		"cache: request.cache",
		"redirect: request.redirect",
		"referrer: request.referrer",
		"referrerPolicy: request.referrerPolicy",
		"integrity: request.integrity",
		"keepalive: request.keepalive",
	} {
		if !strings.Contains(retry, want) {
			t.Errorf("api/retry.ts missing %q", want)
		}
	}
	lower := strings.ToLower(client + retry)
	if strings.Contains(lower, "localstorage") || strings.Contains(lower, "sessionstorage") {
		t.Error("api/client.ts uses web storage")
	}
}

func assertSPAHonorsRuntimeAPIPrefix(t *testing.T, dest, auth string) {
	t.Helper()
	index := readFile(t, filepath.Join(dest, "frontend", "index.html"))
	if !strings.Contains(index, `meta name="gombit-api-prefix"`) {
		t.Error("index.html missing gombit-api-prefix meta")
	}
	if !strings.Contains(index, "__GOMBIT_API_PREFIX__") {
		t.Error("index.html missing __GOMBIT_API_PREFIX__ placeholder")
	}

	viteEnv := readFile(t, filepath.Join(dest, "frontend", "src", "vite-env.d.ts"))
	if !strings.Contains(viteEnv, "__GOMBIT_API_PREFIX__") {
		t.Error("vite-env.d.ts missing Window.__GOMBIT_API_PREFIX__")
	}

	viteConfig := readFile(t, filepath.Join(dest, "frontend", "vite.config.ts"))
	for _, want := range []string{"GOMBIT_API_PREFIX", "transformIndexHtml", "injectAPIPrefix", `"/admin"`} {
		if !strings.Contains(viteConfig, want) {
			t.Errorf("vite.config.ts missing %q", want)
		}
	}

	prefix := readFile(t, filepath.Join(dest, "frontend", "src", "api", "apiPrefix.ts"))
	for _, want := range []string{
		"export function apiPrefix()",
		"export function apiPath(",
		"export function rewriteAPIRequest(",
		"DEFAULT_API_PREFIX",
		"__GOMBIT_API_PREFIX__",
		`meta[name="gombit-api-prefix"]`,
	} {
		if !strings.Contains(prefix, want) {
			t.Errorf("apiPrefix.ts missing %q", want)
		}
	}
	if strings.Contains(prefix, `"/api/v1/auth/`) || strings.Contains(prefix, `"/api/v1/products`) {
		t.Error("apiPrefix.ts hardcodes /api/v1 request paths; callers must compose via apiPath/rewrite")
	}

	client := readFile(t, filepath.Join(dest, "frontend", "src", "api", "client.ts"))
	if !strings.Contains(client, `from "./apiPrefix"`) {
		t.Error("client.ts must import the runtime prefix helper")
	}
	if !strings.Contains(client, "rewriteAPIRequest(") {
		t.Error("client.ts missing rewriteAPIRequest in createAppClient")
	}
	if strings.Contains(client, `const CSRF_PATH = "/api/v1/`) || strings.Contains(client, `const REFRESH_PATH = "/api/v1/`) {
		t.Error("client.ts hardcodes CSRF_PATH/REFRESH_PATH to /api/v1")
	}
	if auth == "cookie" {
		if !strings.Contains(client, `apiPath("/auth/csrf")`) {
			t.Error("cookie client.ts CSRF bootstrap must use apiPath(\"/auth/csrf\")")
		}
		if !strings.Contains(client, `apiPath("/auth/refresh")`) {
			t.Error("cookie client.ts refresh must use apiPath(\"/auth/refresh\")")
		}
	}

	readme := readFile(t, filepath.Join(dest, "frontend", "README.md"))
	if !strings.Contains(readme, "A CDN must replace `__GOMBIT_API_PREFIX__`") {
		t.Error("frontend/README.md must document split-deploy placeholder substitution")
	}
	if !strings.Contains(readme, "`/admin`") {
		t.Error("frontend/README.md must mention the Vite /admin proxy")
	}
	envExample := readFile(t, filepath.Join(dest, ".env.example"))
	if !strings.Contains(envExample, "A CDN must replace __GOMBIT_API_PREFIX__") {
		t.Error(".env.example must document split-deploy placeholder substitution")
	}
}

// TestGenerateCookieCSRFBootstrapIsSerialized is the #107 regression (bootstrap
// is serialized behind csrfInFlight and skips when a cookie already exists) plus
// the #250 fix (the no-op keys off the JS-readable gombit_csrf cookie, and a 403
// on an unsafe request re-bootstraps and retries once).
func TestGenerateCookieCSRFBootstrapIsSerialized(t *testing.T) {
	workDir := t.TempDir()
	err := Generate(context.Background(), Options{
		Name:     "shop",
		Database: "sqlite",
		Auth:     "cookie",
		WorkDir:  workDir,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	client := readFile(t, filepath.Join(workDir, "shop", "frontend", "src", "api", "client.ts"))
	if !strings.Contains(client, "csrfInFlight") {
		t.Fatal("cookie client.ts missing csrfInFlight lock")
	}
	if !strings.Contains(client, "if (!force && readCSRFCookie())") {
		t.Fatal("cookie client.ts must skip bootstrap when the gombit_csrf cookie is already present (#250)")
	}
	if !strings.Contains(client, "if (csrfInFlight)") {
		t.Fatal("cookie client.ts missing in-flight promise reuse")
	}
	if !strings.Contains(client, "response.status === 403") {
		t.Fatal("cookie client.ts missing 403 CSRF recovery retry (#250)")
	}

	login := readFile(t, filepath.Join(workDir, "shop", "frontend", "src", "pages", "LoginPage.tsx"))
	if strings.Count(login, "await bootstrapCSRF()") < 2 {
		t.Fatal("cookie LoginPage.tsx must await bootstrapCSRF() on both login and register")
	}
	if !strings.Contains(login, "void bootstrapCSRF()") {
		t.Fatal("cookie LoginPage.tsx missing eager CSRF bootstrap on mount")
	}

	providers := readFile(t, filepath.Join(workDir, "shop", "frontend", "src", "app", "providers.tsx"))
	if !strings.Contains(providers, "void bootstrapCSRF()") {
		t.Fatal("cookie AppProviders must bootstrap CSRF on mount so reload on a gated route still has X-CSRF-Token")
	}
	session := readFile(t, filepath.Join(workDir, "shop", "frontend", "src", "auth", "session.ts"))
	if !strings.Contains(session, "csrfToken = undefined") {
		t.Fatal("cookie clearSession must drop the in-memory CSRF token")
	}
	if !strings.Contains(client, "await bootstrapCSRF()") {
		t.Fatal("cookie client.ts must await bootstrapCSRF before unsafe requests / refresh")
	}
}

func TestGenerateMUIPreset(t *testing.T) {
	workDir := t.TempDir()
	err := Generate(context.Background(), Options{
		Name:     "demo",
		Database: "sqlite",
		UI:       "mui",
		WorkDir:  workDir,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	dest := filepath.Join(workDir, "demo")

	theme := readFile(t, filepath.Join(dest, "frontend", "src", "theme.ts"))
	for _, want := range []string{"createTheme", "#1976d2", "#dc004e", "textTransform"} {
		if !strings.Contains(theme, want) {
			t.Fatalf("theme.ts missing %q:\n%s", want, theme)
		}
	}

	pkg := readFile(t, filepath.Join(dest, "frontend", "package.json"))
	for _, want := range []string{`"@mui/material"`, `"@mui/icons-material"`, `"@emotion/react"`, `"@emotion/styled"`} {
		if !strings.Contains(pkg, want) {
			t.Fatalf("MUI package.json missing %s:\n%s", want, pkg)
		}
	}
	if strings.Contains(pkg, "axios") || strings.Contains(pkg, "react-router-dom") {
		t.Fatal("MUI package.json must not add axios or react-router-dom")
	}

	providers := readFile(t, filepath.Join(dest, "frontend", "src", "app", "providers.tsx"))
	if !strings.Contains(providers, "ThemeProvider") || !strings.Contains(providers, "CssBaseline") {
		t.Fatal("MUI providers.tsx missing ThemeProvider/CssBaseline")
	}
	if !strings.Contains(providers, `from "../theme"`) {
		t.Fatal("MUI providers.tsx missing theme import")
	}

	layout := readFile(t, filepath.Join(dest, "frontend", "src", "layouts", "AppLayout.tsx"))
	if !strings.Contains(layout, "AppBar") || !strings.Contains(layout, "Toolbar") {
		t.Fatal("MUI AppLayout.tsx missing AppBar/Toolbar")
	}
	if strings.Contains(layout, "ThemeToggle") || strings.Contains(strings.ToLower(layout), "localstorage") {
		t.Fatal("MUI AppLayout.tsx must not port ThemeToggle or localStorage")
	}

	login := readFile(t, filepath.Join(dest, "frontend", "src", "pages", "LoginPage.tsx"))
	for _, want := range []string{"Paper", "TextField", "Alert", "CircularProgress", "applyTokenPair"} {
		if !strings.Contains(login, want) {
			t.Fatalf("MUI LoginPage.tsx missing %q", want)
		}
	}
	if strings.Contains(strings.ToLower(login), "localstorage") {
		t.Fatal("MUI LoginPage.tsx uses localStorage")
	}

	list := readFile(t, filepath.Join(dest, "frontend", "src", "pages", "ProductListPage.tsx"))
	for _, want := range []string{"TableContainer", "TableHead", "CircularProgress", "Paper"} {
		if !strings.Contains(list, want) {
			t.Fatalf("MUI ProductListPage.tsx missing %q", want)
		}
	}
	if strings.Contains(list, "date-fns") || strings.Contains(list, "onDelete") {
		t.Fatal("MUI ProductListPage.tsx must not add date-fns or delete actions")
	}

	form := readFile(t, filepath.Join(dest, "frontend", "src", "pages", "ProductFormPage.tsx"))
	for _, want := range []string{"Paper", "TextField", "applyContractErrors", "setError"} {
		if !strings.Contains(form, want) {
			t.Fatalf("MUI ProductFormPage.tsx missing %q", want)
		}
	}

	index := readFile(t, filepath.Join(dest, "frontend", "index.html"))
	if !strings.Contains(index, "fonts.googleapis.com") || !strings.Contains(index, "Roboto") {
		t.Fatal("MUI index.html missing Roboto Google Fonts link")
	}

	yaml := readFile(t, filepath.Join(dest, "gombit.yaml"))
	if !strings.Contains(yaml, "ui: mui") {
		t.Fatalf("gombit.yaml missing ui: mui:\n%s", yaml)
	}
}

func TestGenerateSourceHasNoLocalStorageOrViteSecrets(t *testing.T) {
	workDir := t.TempDir()
	err := Generate(context.Background(), Options{
		Name:     "demo",
		Database: "sqlite",
		WorkDir:  workDir,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	err = filepath.Walk(filepath.Join(workDir, "demo"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !isGeneratedSource(path) {
			return nil
		}
		body := readFile(t, path)
		lower := strings.ToLower(body)
		if strings.Contains(lower, "localstorage") || strings.Contains(lower, "sessionstorage") {
			t.Errorf("%s contains localStorage/sessionStorage", path)
		}
		for _, line := range strings.Split(body, "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "VITE_") {
				continue
			}
			if strings.Contains(strings.ToLower(trimmed), "secret") || strings.Contains(strings.ToLower(trimmed), "password") {
				t.Errorf("%s puts a secret in VITE_*: %s", path, trimmed)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

func TestGenerateInteractivePromptsOnTTY(t *testing.T) {
	workDir := t.TempDir()
	stdin := strings.NewReader("demo\nsqlite\nmemory\njwt\nminimal\n\n")
	stdout := new(bytes.Buffer)
	err := Generate(context.Background(), Options{
		WorkDir: workDir,
		Stdin:   stdin,
		Stdout:  stdout,
		IsTTY:   func() bool { return true },
		DryRun:  true,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "Project name") {
		t.Fatalf("stdout = %q, want interactive prompts", stdout.String())
	}
	if !strings.Contains(stdout.String(), "create demo/go.mod") {
		t.Fatalf("stdout = %q, want dry-run file list for prompted name", stdout.String())
	}
}

func isGeneratedSource(path string) bool {
	base := filepath.Base(path)
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go", ".ts", ".tsx", ".js", ".jsx", ".json", ".html", ".mjs":
		return true
	}
	return base == ".env.example" || base == "gombit.yaml"
}

func jwtSecretFromDotEnv(t *testing.T, dotenv string) string {
	t.Helper()
	for _, line := range strings.Split(dotenv, "\n") {
		if strings.HasPrefix(line, "GOMBIT_JWT_SECRET=") {
			return strings.TrimPrefix(line, "GOMBIT_JWT_SECRET=")
		}
	}
	t.Fatal(".env missing GOMBIT_JWT_SECRET")
	return ""
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	// #nosec G304 -- test fixture path
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func assertNumberInputEmptyIsZero(t *testing.T, src, name string) {
	t.Helper()
	if strings.Contains(src, "valueAsNumber") {
		t.Fatalf("%s uses valueAsNumber; empty number inputs become NaN and JSON.stringify emits null", name)
	}
	if !strings.Contains(src, `setValueAs: (value) => (value === "" ? 0 : Number(value))`) {
		t.Fatalf("%s missing setValueAs empty→0 for number inputs:\n%s", name, src)
	}
}

func TestGeneratePinsResolvableFrameworkVersion(t *testing.T) {
	workDir := t.TempDir()
	stderr := new(bytes.Buffer)
	err := Generate(context.Background(), Options{
		Name:             "demo",
		Database:         "sqlite",
		FrameworkVersion: "v0.1.0",
		WorkDir:          workDir,
		Stdout:           new(bytes.Buffer),
		Stderr:           stderr,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	gomod := readFile(t, filepath.Join(workDir, "demo", "go.mod"))
	want := "require github.com/gombit-dev/gombit v0.1.0"
	if !strings.Contains(gomod, want) {
		t.Errorf("go.mod = %q, want it to contain %q", gomod, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("a resolvable version must not warn, got %q", stderr.String())
	}
}

func TestGenerateWarnsWhenFrameworkVersionIsUnresolvable(t *testing.T) {
	workDir := t.TempDir()
	stderr := new(bytes.Buffer)
	err := Generate(context.Background(), Options{
		Name:             "demo",
		Database:         "sqlite",
		FrameworkVersion: "dev",
		WorkDir:          workDir,
		Stdout:           new(bytes.Buffer),
		Stderr:           stderr,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	gomod := readFile(t, filepath.Join(workDir, "demo", "go.mod"))
	want := "require github.com/gombit-dev/gombit " + FallbackFrameworkVersion
	if !strings.Contains(gomod, want) {
		t.Errorf("go.mod = %q, want it to contain %q", gomod, want)
	}
	// The opaque failure this replaces is "missing go.sum entry", so the
	// warning has to name the fix.
	for _, fragment := range []string{"go mod edit -replace", "will not build", "@latest"} {
		if !strings.Contains(stderr.String(), fragment) {
			t.Errorf("warning = %q, want it to mention %q", stderr.String(), fragment)
		}
	}
}

// stubGoModTidy replaces the tidy shell-out for one test. Scaffold tests must
// never reach the network.
func stubGoModTidy(t *testing.T, fn func(ctx context.Context, dir string) ([]byte, error)) {
	t.Helper()
	prev := goModTidy
	goModTidy = fn
	t.Cleanup(func() { goModTidy = prev })
}

func TestGenerateRunsTidyForResolvableVersion(t *testing.T) {
	var gotDir string
	calls := 0
	stubGoModTidy(t, func(_ context.Context, dir string) ([]byte, error) {
		calls++
		gotDir = dir
		return nil, nil
	})

	workDir := t.TempDir()
	stdout := new(bytes.Buffer)
	err := Generate(context.Background(), Options{
		Name:             "demo",
		Database:         "sqlite",
		FrameworkVersion: "v0.1.0",
		Tidy:             true,
		WorkDir:          workDir,
		Stdout:           stdout,
		Stderr:           new(bytes.Buffer),
		skipAtlas:        true, // this test only cares about the tidy step
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("go mod tidy ran %d times, want 1", calls)
	}
	if want := filepath.Join(workDir, "demo"); gotDir != want {
		t.Errorf("tidy dir = %q, want %q", gotDir, want)
	}
	if !strings.Contains(stdout.String(), "go mod tidy") {
		t.Errorf("stdout = %q, want it to report the tidy step", stdout.String())
	}
}

func TestGenerateSkipsTidyWhenVersionIsUnresolvable(t *testing.T) {
	stubGoModTidy(t, func(context.Context, string) ([]byte, error) {
		t.Fatal("go mod tidy must not run for an unresolvable version")
		return nil, nil
	})

	err := Generate(context.Background(), Options{
		Name:             "demo",
		Database:         "sqlite",
		FrameworkVersion: "dev",
		Tidy:             true,
		WorkDir:          t.TempDir(),
		Stdout:           new(bytes.Buffer),
		Stderr:           new(bytes.Buffer),
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
}

func TestGenerateSkipsTidyOnDryRun(t *testing.T) {
	stubGoModTidy(t, func(context.Context, string) ([]byte, error) {
		t.Fatal("go mod tidy must not run for --dry-run")
		return nil, nil
	})

	err := Generate(context.Background(), Options{
		Name:             "demo",
		Database:         "sqlite",
		FrameworkVersion: "v0.1.0",
		Tidy:             true,
		DryRun:           true,
		WorkDir:          t.TempDir(),
		Stdout:           new(bytes.Buffer),
		Stderr:           new(bytes.Buffer),
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
}

func TestGenerateTidyFailureIsNotFatal(t *testing.T) {
	stubGoModTidy(t, func(context.Context, string) ([]byte, error) {
		return []byte("dial tcp: lookup proxy.golang.org: no such host"), errors.New("exit status 1")
	})

	workDir := t.TempDir()
	stderr := new(bytes.Buffer)
	err := Generate(context.Background(), Options{
		Name:             "demo",
		Database:         "sqlite",
		FrameworkVersion: "v0.1.0",
		Tidy:             true,
		WorkDir:          workDir,
		Stdout:           new(bytes.Buffer),
		Stderr:           stderr,
	})
	// The tree is already written and correct; an offline machine must still
	// get a usable scaffold.
	if err != nil {
		t.Fatalf("Generate() error = %v, want tidy failure to be non-fatal", err)
	}
	if _, statErr := os.Stat(filepath.Join(workDir, "demo", "go.mod")); statErr != nil {
		t.Fatalf("go.mod missing after tidy failure: %v", statErr)
	}
	for _, fragment := range []string{"go mod tidy failed", "no such host", "cd demo && go mod tidy"} {
		if !strings.Contains(stderr.String(), fragment) {
			t.Errorf("warning = %q, want it to mention %q", stderr.String(), fragment)
		}
	}
}

func stubScaffoldLookPath(t *testing.T, fn func(name string) (string, error)) {
	t.Helper()
	prev := scaffoldLookPath
	scaffoldLookPath = fn
	t.Cleanup(func() { scaffoldLookPath = prev })
}

func stubScaffoldMakeMigrations(t *testing.T, fn func(ctx context.Context, opts migrations.Options) error) {
	t.Helper()
	prev := scaffoldMakeMigrations
	scaffoldMakeMigrations = fn
	t.Cleanup(func() { scaffoldMakeMigrations = prev })
}

func stubScaffoldMigrate(t *testing.T, fn func(ctx context.Context, opts migrations.ApplyOptions) error) {
	t.Helper()
	prev := scaffoldMigrate
	scaffoldMigrate = fn
	t.Cleanup(func() { scaffoldMigrate = prev })
}

// TestGenerateSeedsBootstrapMigrationForResolvableVersion guards the fix for
// #96: gombit new must seed an initial migration covering every model
// internal/platform/database.go.tmpl registers for AutoMigrate, so Atlas's
// tracked history matches what AutoMigrate creates at every app startup
// from the very first run.
func TestGenerateSeedsBootstrapMigrationForResolvableVersion(t *testing.T) {
	stubGoModTidy(t, func(context.Context, string) ([]byte, error) { return nil, nil })
	stubScaffoldLookPath(t, func(string) (string, error) { return "/usr/bin/atlas", nil })

	var got migrations.Options
	calls := 0
	stubScaffoldMakeMigrations(t, func(_ context.Context, opts migrations.Options) error {
		calls++
		got = opts
		return nil
	})

	workDir := t.TempDir()
	err := Generate(context.Background(), Options{
		Name:             "demo",
		Module:           "github.com/example/demo",
		Database:         "postgres",
		FrameworkVersion: "v0.1.0",
		Tidy:             true,
		WorkDir:          workDir,
		Stdout:           new(bytes.Buffer),
		Stderr:           new(bytes.Buffer),
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("bootstrap migration ran %d times, want 1", calls)
	}
	if got.Name != "bootstrap" {
		t.Errorf("migration name = %q, want bootstrap", got.Name)
	}
	if got.Driver != config.DatabaseDriverPostgres {
		t.Errorf("migration driver = %q, want postgres", got.Driver)
	}
	if got.WorkDir != filepath.Join(workDir, "demo") {
		t.Errorf("migration workdir = %q, want %q", got.WorkDir, filepath.Join(workDir, "demo"))
	}
	wantModels := []migrations.Model{
		{ImportPath: "github.com/gombit-dev/gombit/auth", TypeName: "User"},
		{ImportPath: "github.com/gombit-dev/gombit/auth", TypeName: "RefreshToken"},
		{ImportPath: "github.com/gombit-dev/gombit/auth", TypeName: "Group"},
		{ImportPath: "github.com/gombit-dev/gombit/auth", TypeName: "Permission"},
		{ImportPath: "github.com/example/demo/internal/product", TypeName: "Product"},
	}
	if !reflect.DeepEqual(got.Models, wantModels) {
		t.Errorf("migration models = %#v, want %#v", got.Models, wantModels)
	}
}

func TestGenerateWithoutAtlasHintsBootstrapMigration(t *testing.T) {
	stubGoModTidy(t, func(context.Context, string) ([]byte, error) { return nil, nil })
	stubScaffoldLookPath(t, func(string) (string, error) { return "", errors.New("not found") })
	stubScaffoldMakeMigrations(t, func(context.Context, migrations.Options) error {
		t.Fatal("makemigrations must not run when atlas is missing")
		return nil
	})

	stdout := new(bytes.Buffer)
	err := Generate(context.Background(), Options{
		Name:             "demo",
		Database:         "sqlite",
		FrameworkVersion: "v0.1.0",
		Tidy:             true,
		WorkDir:          t.TempDir(),
		Stdout:           stdout,
		Stderr:           new(bytes.Buffer),
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "gombit db makemigrations bootstrap") {
		t.Fatalf("stdout = %q, want a makemigrations hint", stdout.String())
	}
	if !strings.Contains(stdout.String(), "--model github.com/gombit-dev/gombit/auth.User") {
		t.Fatalf("stdout = %q, want auth models in hint", stdout.String())
	}
}

// TestGenerateBootstrapMigrationFailureHintsInsteadOfFailing guards a real
// deployment risk: --database postgres/mysql need a running Docker daemon
// for Atlas's dev-database (see devURL in migrations/makemigrations.go),
// which gombit new never required before this feature existed. A user with
// atlas installed but no Docker running must still get a working scaffold
// out of `gombit new --database postgres`, not a fatal error.
func TestGenerateBootstrapMigrationFailureHintsInsteadOfFailing(t *testing.T) {
	stubGoModTidy(t, func(context.Context, string) ([]byte, error) { return nil, nil })
	stubScaffoldLookPath(t, func(string) (string, error) { return "/usr/bin/atlas", nil })
	stubScaffoldMakeMigrations(t, func(context.Context, migrations.Options) error {
		return errors.New("atlas migrate diff: docker: command not found")
	})

	stdout := new(bytes.Buffer)
	err := Generate(context.Background(), Options{
		Name:             "demo",
		Database:         "postgres",
		FrameworkVersion: "v0.1.0",
		Tidy:             true,
		WorkDir:          t.TempDir(),
		Stdout:           stdout,
		Stderr:           new(bytes.Buffer),
	})
	if err != nil {
		t.Fatalf("Generate() error = %v, want bootstrap migration failure to be non-fatal", err)
	}
	if !strings.Contains(stdout.String(), "gombit db makemigrations bootstrap") {
		t.Fatalf("stdout = %q, want a makemigrations hint", stdout.String())
	}
	if !strings.Contains(stdout.String(), "docker: command not found") {
		t.Fatalf("stdout = %q, want the underlying atlas error surfaced", stdout.String())
	}
}

// TestGenerateAppliesBootstrapMigrationForSQLite is the regression test for
// #102: gombit new must not just write the bootstrap migration, it must
// apply it — otherwise the AutoMigrate call in the very first `gombit dev`
// (chapter 2, before the reader ever reaches chapter 3's own
// `gombit db migrate`) creates the same tables live first, and applying the
// bootstrap migration afterward fails with "table already exists".
func TestGenerateAppliesBootstrapMigrationForSQLite(t *testing.T) {
	stubGoModTidy(t, func(context.Context, string) ([]byte, error) { return nil, nil })
	stubScaffoldLookPath(t, func(string) (string, error) { return "/usr/bin/atlas", nil })
	stubScaffoldMakeMigrations(t, func(context.Context, migrations.Options) error { return nil })

	var got migrations.ApplyOptions
	calls := 0
	stubScaffoldMigrate(t, func(_ context.Context, opts migrations.ApplyOptions) error {
		calls++
		got = opts
		return nil
	})

	workDir := t.TempDir()
	err := Generate(context.Background(), Options{
		Name:             "demo",
		Database:         "sqlite",
		FrameworkVersion: "v0.1.0",
		Tidy:             true,
		WorkDir:          workDir,
		Stdout:           new(bytes.Buffer),
		Stderr:           new(bytes.Buffer),
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("bootstrap migration apply ran %d times, want 1", calls)
	}
	dest := filepath.Join(workDir, "demo")
	if got.WorkDir != dest {
		t.Errorf("apply workdir = %q, want %q", got.WorkDir, dest)
	}
	if got.Database.Driver != config.DatabaseDriverSQLite {
		t.Errorf("apply driver = %q, want sqlite", got.Database.Driver)
	}
	wantDSN := "file:" + filepath.Join(dest, "gombit.db") + "?cache=shared&_fk=1"
	if got.Database.DSN != wantDSN {
		t.Errorf("apply DSN = %q, want absolute path %q (not the app's own relative DSN, which resolves against gombit's process cwd here, not the app dir)", got.Database.DSN, wantDSN)
	}
}

// TestGenerateSkipsApplyForNonSQLite guards --database postgres/mysql: their
// DSN from gombit new is a USER:PASSWORD placeholder until the reader edits
// .env, so there's nothing reachable to apply against yet. Attempting it
// anyway would just be a guaranteed-to-fail connection attempt on every
// postgres/mysql scaffold.
func TestGenerateSkipsApplyForNonSQLite(t *testing.T) {
	stubGoModTidy(t, func(context.Context, string) ([]byte, error) { return nil, nil })
	stubScaffoldLookPath(t, func(string) (string, error) { return "/usr/bin/atlas", nil })
	stubScaffoldMakeMigrations(t, func(context.Context, migrations.Options) error { return nil })
	stubScaffoldMigrate(t, func(context.Context, migrations.ApplyOptions) error {
		t.Fatal("migrate must not run for postgres/mysql; the DSN is a placeholder")
		return nil
	})

	stdout := new(bytes.Buffer)
	err := Generate(context.Background(), Options{
		Name:             "demo",
		Database:         "postgres",
		FrameworkVersion: "v0.1.0",
		Tidy:             true,
		WorkDir:          t.TempDir(),
		Stdout:           stdout,
		Stderr:           new(bytes.Buffer),
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "credentials in .env") {
		t.Fatalf("stdout = %q, want a hint about .env credentials", stdout.String())
	}
	if !strings.Contains(stdout.String(), "gombit db migrate") {
		t.Fatalf("stdout = %q, want a gombit db migrate hint", stdout.String())
	}
}

// TestGenerateBootstrapApplyFailureHintsInsteadOfFailing mirrors
// TestGenerateBootstrapMigrationFailureHintsInsteadOfFailing for the apply
// step: a real deployment risk (e.g. the SQLite file can't be created for
// some environment-specific reason) must not fail gombit new outright.
func TestGenerateBootstrapApplyFailureHintsInsteadOfFailing(t *testing.T) {
	stubGoModTidy(t, func(context.Context, string) ([]byte, error) { return nil, nil })
	stubScaffoldLookPath(t, func(string) (string, error) { return "/usr/bin/atlas", nil })
	stubScaffoldMakeMigrations(t, func(context.Context, migrations.Options) error { return nil })
	stubScaffoldMigrate(t, func(context.Context, migrations.ApplyOptions) error {
		return errors.New("migrations: open database: permission denied")
	})

	stdout := new(bytes.Buffer)
	err := Generate(context.Background(), Options{
		Name:             "demo",
		Database:         "sqlite",
		FrameworkVersion: "v0.1.0",
		Tidy:             true,
		WorkDir:          t.TempDir(),
		Stdout:           stdout,
		Stderr:           new(bytes.Buffer),
	})
	if err != nil {
		t.Fatalf("Generate() error = %v, want bootstrap apply failure to be non-fatal", err)
	}
	if !strings.Contains(stdout.String(), "gombit db migrate") {
		t.Fatalf("stdout = %q, want a gombit db migrate hint", stdout.String())
	}
	if !strings.Contains(stdout.String(), "permission denied") {
		t.Fatalf("stdout = %q, want the underlying error surfaced", stdout.String())
	}
}
