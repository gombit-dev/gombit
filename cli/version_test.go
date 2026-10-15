package cli

import (
	"bytes"
	"context"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/internal/frameworkmod"
)

// stubBuild swaps the build-info source and the ldflags vars for one test, so
// the assertions don't depend on how the test binary itself was linked.
func stubBuild(t *testing.T, version, commit, buildDate string, info *debug.BuildInfo) {
	t.Helper()
	prevVersion, prevCommit, prevDate, prevRead := Version, Commit, BuildDate, readBuildInfo
	Version, Commit, BuildDate = version, commit, buildDate
	readBuildInfo = func() (*debug.BuildInfo, bool) { return info, info != nil }
	t.Cleanup(func() {
		Version, Commit, BuildDate, readBuildInfo = prevVersion, prevCommit, prevDate, prevRead
	})
}

func TestResolveVersionPrefersLdflags(t *testing.T) {
	stubBuild(t, "v1.2.3", "", "", &debug.BuildInfo{Main: debug.Module{Version: "v9.9.9"}})
	if got := resolveVersion(); got != "v1.2.3" {
		t.Fatalf("resolveVersion() = %q, want v1.2.3", got)
	}
}

func TestResolveVersionFallsBackToBuildInfo(t *testing.T) {
	stubBuild(t, "", "", "", &debug.BuildInfo{
		Main: debug.Module{Path: frameworkmod.ModulePath, Version: "v0.4.0"},
	})
	if got := resolveVersion(); got != "v0.4.0" {
		t.Fatalf("resolveVersion() = %q, want v0.4.0", got)
	}
}

func TestResolveVersionDevWhenUnstamped(t *testing.T) {
	for name, info := range map[string]*debug.BuildInfo{
		"no build info": nil,
		"devel module":  {Main: debug.Module{Path: frameworkmod.ModulePath, Version: "(devel)"}},
		"empty module":  {Main: debug.Module{Path: frameworkmod.ModulePath, Version: ""}},
	} {
		t.Run(name, func(t *testing.T) {
			stubBuild(t, "", "", "", info)
			if got := resolveVersion(); got != DevVersion {
				t.Fatalf("resolveVersion() = %q, want %q", got, DevVersion)
			}
		})
	}
}

// An app's own `cmd/gombit` builds the framework in as a dependency, so its
// main module is the application: neither "(devel)" nor an app tag is a
// framework version, and the dependency entry has to be read instead.
func TestResolveVersionOfAnAppBinary(t *testing.T) {
	appFrameworkDep := func(mainVersion string, dep debug.Module) *debug.BuildInfo {
		return &debug.BuildInfo{
			Main: debug.Module{Path: "github.com/example/rel", Version: mainVersion},
			Deps: []*debug.Module{&dep},
		}
	}

	tests := []struct {
		name string
		info *debug.BuildInfo
		want string
	}{
		{
			name: "app from a checkout, framework released",
			info: appFrameworkDep("(devel)", debug.Module{Path: frameworkmod.ModulePath, Version: "v0.6.0"}),
			want: "v0.6.0",
		},
		{
			name: "a tagged app does not report its own version",
			info: appFrameworkDep("v3.4.5", debug.Module{Path: frameworkmod.ModulePath, Version: "v0.6.0"}),
			want: "v0.6.0",
		},
		{
			// `go version -m` on an app's cmd/gombit built against the
			// released framework, captured from a real build.
			name: "an app's own pseudo-version is not the framework's",
			info: appFrameworkDep("v0.0.0-20261015013337-ba8fc61f2a66", debug.Module{Path: frameworkmod.ModulePath, Version: "v0.7.1"}),
			want: "v0.7.1",
		},
		{
			name: "framework pseudo-version",
			info: appFrameworkDep("(devel)", debug.Module{
				Path: frameworkmod.ModulePath, Version: "v0.0.0-20260818193315-9abb3c6ecc8c",
			}),
			want: "v0.0.0-20260818193315-9abb3c6ecc8c",
		},
		{
			name: "framework replaced by a local checkout is a source build",
			info: appFrameworkDep("(devel)", debug.Module{
				Path: frameworkmod.ModulePath, Version: "v0.6.0",
				Replace: &debug.Module{Path: "../gombit"},
			}),
			want: DevVersion,
		},
		{
			name: "framework replaced by a fork is not a framework release",
			info: appFrameworkDep("(devel)", debug.Module{
				Path: frameworkmod.ModulePath, Version: "v0.6.0",
				Replace: &debug.Module{Path: "github.com/acme/gombit", Version: "v1.2.3"},
			}),
			want: DevVersion,
		},
		{
			name: "framework replaced by itself at another version",
			info: appFrameworkDep("(devel)", debug.Module{
				Path: frameworkmod.ModulePath, Version: "v0.6.0",
				Replace: &debug.Module{Path: frameworkmod.ModulePath, Version: "v0.5.0"},
			}),
			want: "v0.5.0",
		},
		{
			name: "framework dependency built from a checkout",
			info: appFrameworkDep("(devel)", debug.Module{Path: frameworkmod.ModulePath, Version: "(devel)"}),
			want: DevVersion,
		},
		{
			name: "app binary with no framework dependency",
			info: &debug.BuildInfo{
				Main: debug.Module{Path: "github.com/example/rel", Version: "v1.0.0"},
				Deps: []*debug.Module{{Path: "github.com/go-gorm/gorm", Version: "v1.25.5"}},
			},
			want: DevVersion,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubBuild(t, "", "", "", tt.info)
			if got := resolveVersion(); got != tt.want {
				t.Fatalf("resolveVersion() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The bug report users paste is `gombit version` from the app's own CLI, so
// the app's dependency is what that output has to name.
func TestVersionCommandOfAnAppBinaryNamesTheFramework(t *testing.T) {
	stubBuild(t, "", "", "", &debug.BuildInfo{
		Main: debug.Module{Path: "github.com/example/rel", Version: "(devel)"},
		Deps: []*debug.Module{{Path: frameworkmod.ModulePath, Version: "v0.6.0"}},
	})

	stdout := new(bytes.Buffer)
	if err := ExecuteRoot(context.Background(), NewRoot(stdout, new(bytes.Buffer)), []string{"version"}); err != nil {
		t.Fatalf("version: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "gombit:   v0.6.0") {
		t.Fatalf("version output does not name the framework version:\n%s", out)
	}
	if strings.Contains(out, "devel") {
		t.Fatalf("version output leaks the app module's build info:\n%s", out)
	}
}

func TestResolveCommitAndDateFromVCSSettings(t *testing.T) {
	stubBuild(t, "", "", "", &debug.BuildInfo{
		Main: debug.Module{Version: "v0.4.0"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abc1234"},
			{Key: "vcs.time", Value: "2026-08-18T00:00:00Z"},
		},
	})
	if got := resolveCommit(); got != "abc1234" {
		t.Fatalf("resolveCommit() = %q, want abc1234", got)
	}
	if got := resolveBuildDate(); got != "2026-08-18T00:00:00Z" {
		t.Fatalf("resolveBuildDate() = %q, want 2026-08-18T00:00:00Z", got)
	}
}

func TestVersionCommandPrintsBuildMetadata(t *testing.T) {
	stubBuild(t, "v0.1.0", "deadbeef", "2026-08-18T12:00:00Z", nil)

	stdout := new(bytes.Buffer)
	if err := ExecuteRoot(context.Background(), NewRoot(stdout, new(bytes.Buffer)), []string{"version"}); err != nil {
		t.Fatalf("version: %v", err)
	}
	out := stdout.String()
	for _, want := range []string{
		"gombit:   v0.1.0",
		"deadbeef",
		"2026-08-18T12:00:00Z",
		runtime.Version(),
		runtime.GOOS + "/" + runtime.GOARCH,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("version output missing %q:\n%s", want, out)
		}
	}
}

func TestVersionCommandUnknownFieldsRenderPlaceholder(t *testing.T) {
	stubBuild(t, "v0.1.0", "", "", nil)

	stdout := new(bytes.Buffer)
	if err := ExecuteRoot(context.Background(), NewRoot(stdout, new(bytes.Buffer)), []string{"version"}); err != nil {
		t.Fatalf("version: %v", err)
	}
	if got := strings.Count(stdout.String(), "unknown"); got != 2 {
		t.Fatalf("want commit and built to render as unknown, got %d:\n%s", got, stdout.String())
	}
}

func TestVersionCommandShort(t *testing.T) {
	stubBuild(t, "v0.1.0", "deadbeef", "2026-08-18T12:00:00Z", nil)

	stdout := new(bytes.Buffer)
	if err := ExecuteRoot(context.Background(), NewRoot(stdout, new(bytes.Buffer)), []string{"version", "--short"}); err != nil {
		t.Fatalf("version --short: %v", err)
	}
	if got := stdout.String(); got != "v0.1.0\n" {
		t.Fatalf("version --short = %q, want %q", got, "v0.1.0\n")
	}
}

func TestRootVersionFlag(t *testing.T) {
	stubBuild(t, "v0.1.0", "", "", nil)

	stdout := new(bytes.Buffer)
	if err := ExecuteRoot(context.Background(), NewRoot(stdout, new(bytes.Buffer)), []string{"--version"}); err != nil {
		t.Fatalf("--version: %v", err)
	}
	if got := stdout.String(); got != "gombit v0.1.0\n" {
		t.Fatalf("--version = %q, want %q", got, "gombit v0.1.0\n")
	}
}

func TestVersionCommandRejectsArgs(t *testing.T) {
	stubBuild(t, "v0.1.0", "", "", nil)

	err := ExecuteRoot(context.Background(), NewRoot(new(bytes.Buffer), new(bytes.Buffer)), []string{"version", "extra"})
	if err == nil {
		t.Fatal("expected an error for unexpected args")
	}
}
