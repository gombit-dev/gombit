package cli

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"

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
	if info, ok := readBuildInfo(); ok && info != nil {
		// Modules built from a checkout report "(devel)", which is less useful
		// to a bug reporter than the honest "dev".
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return DevVersion
}

func resolveCommit() string {
	if Commit != "" {
		return Commit
	}
	return buildSetting("vcs.revision")
}

func resolveBuildDate() string {
	if BuildDate != "" {
		return BuildDate
	}
	return buildSetting("vcs.time")
}

func buildSetting(key string) string {
	info, ok := readBuildInfo()
	if !ok || info == nil {
		return ""
	}
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
