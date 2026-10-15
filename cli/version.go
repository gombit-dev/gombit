package cli

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"

	"github.com/gombit-dev/gombit/internal/frameworkmod"
	"github.com/spf13/cobra"
)

// Build metadata. Release binaries stamp these with -ldflags, e.g.
//
//	-X github.com/gombit-dev/gombit/cli.Version=v0.1.0
//
// When they are empty the values fall back to the module build info recorded
// by `go install`, so `go install ...@v0.1.0` still self-reports v0.1.0.
var (
	Version   = ""
	Commit    = ""
	BuildDate = ""
)

// DevVersion is reported when the binary carries neither ldflags nor usable
// module build info (a plain `go run ./cmd/gombit` or `go build` from source).
const DevVersion = "dev"

// readBuildInfo is indirected so tests can supply build info instead of
// depending on how the test binary itself was linked.
var readBuildInfo = debug.ReadBuildInfo

func resolveVersion() string {
	if Version != "" {
		return Version
	}
	if v := readFrameworkBuild().version; v != "" {
		return v
	}
	return DevVersion
}

// frameworkBuild is what this binary's module build info says about Gombit.
type frameworkBuild struct {
	version string
	commit  string
	built   string
}

// readFrameworkBuild resolves the framework release this binary was built with,
// alongside the build metadata that can honestly be attributed to it.
func readFrameworkBuild() frameworkBuild {
	info, ok := readBuildInfo()
	if !ok || info == nil {
		return frameworkBuild{}
	}
	mod := &info.Main
	// A scaffolded app ships its own `cmd/gombit`, where the main module is the
	// application and Gombit is a dependency, so the main module's version —
	// "(devel)", or an app tag or pseudo-version — says nothing about Gombit.
	fromApp := mod.Path != frameworkmod.ModulePath
	if fromApp {
		mod = frameworkDependency(info)
	}
	build := frameworkBuild{version: moduleRelease(mod)}
	// The `vcs.*` settings are recorded for the main module: in an app's binary
	// they describe the app's repository, and pairing a Gombit release with a
	// commit no one can find in the Gombit repository is the same misleading
	// claim as printing the app's version.
	if !fromApp {
		build.commit = buildSetting(info, "vcs.revision")
		build.built = buildSetting(info, "vcs.time")
	}
	return build
}

// frameworkDependency is the framework entry an app binary was built with, or
// nil. Build info lists every module once, so the first match is the only one.
func frameworkDependency(info *debug.BuildInfo) *debug.Module {
	for _, dep := range info.Deps {
		if dep.Path == frameworkmod.ModulePath {
			return dep
		}
	}
	return nil
}

// moduleRelease is the framework release a module entry names. A replace ships
// someone else's code, and only a replace by the framework module itself at
// another version names a framework release: a local checkout has no version
// and a fork's is not Gombit's, which is how internal/frameworkmod reads the
// same directives out of a go.mod. Neither is resolvable from the module proxy,
// so they report no version and the command falls back to "dev", which sends
// the user to a replace directive or to --framework-version. A module built
// from a checkout reports "(devel)", less useful to a bug reporter than the
// honest "dev".
func moduleRelease(mod *debug.Module) string {
	if mod == nil {
		return ""
	}
	if replace := mod.Replace; replace != nil {
		if replace.Path != frameworkmod.ModulePath {
			return ""
		}
		mod = replace
	}
	if mod.Version == "" || mod.Version == "(devel)" {
		return ""
	}
	return mod.Version
}

func resolveCommit() string {
	if Commit != "" {
		return Commit
	}
	return readFrameworkBuild().commit
}

func resolveBuildDate() string {
	if BuildDate != "" {
		return BuildDate
	}
	return readFrameworkBuild().built
}

func buildSetting(info *debug.BuildInfo, key string) string {
	for _, setting := range info.Settings {
		if setting.Key == key {
			return setting.Value
		}
	}
	return ""
}

func newVersionCommand(stdout io.Writer) *Command {
	var short bool
	cmd := &Command{
		Use:   "version",
		Short: "Print the gombit version",
		Long: "Print the gombit version, commit, build date, Go toolchain, and platform.\n" +
			"Include this output when filing a bug report.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if short {
				_, err := fmt.Fprintln(stdout, resolveVersion())
				return err
			}
			return writeVersion(stdout)
		},
	}
	cmd.Flags().BoolVar(&short, "short", false, "print only the version string")
	return silence(cmd)
}

func writeVersion(w io.Writer) error {
	lines := [][2]string{
		{"gombit", resolveVersion()},
		{"commit", resolveCommit()},
		{"built", resolveBuildDate()},
		{"go", runtime.Version()},
		{"platform", runtime.GOOS + "/" + runtime.GOARCH},
	}
	for _, line := range lines {
		value := line[1]
		if value == "" {
			value = "unknown"
		}
		if _, err := fmt.Fprintf(w, "%-9s %s\n", line[0]+":", value); err != nil {
			return err
		}
	}
	return nil
}
