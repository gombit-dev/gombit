package cli

import (
	"bytes"
	"context"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
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
	stubBuild(t, "", "", "", &debug.BuildInfo{Main: debug.Module{Version: "v0.4.0"}})
	if got := resolveVersion(); got != "v0.4.0" {
		t.Fatalf("resolveVersion() = %q, want v0.4.0", got)
	}
}

func TestResolveVersionDevWhenUnstamped(t *testing.T) {
	for name, info := range map[string]*debug.BuildInfo{
		"no build info": nil,
		"devel module":  {Main: debug.Module{Version: "(devel)"}},
		"empty module":  {Main: debug.Module{Version: ""}},
	} {
		t.Run(name, func(t *testing.T) {
			stubBuild(t, "", "", "", info)
			if got := resolveVersion(); got != DevVersion {
				t.Fatalf("resolveVersion() = %q, want %q", got, DevVersion)
			}
		})
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
