package winfile

import (
	"strings"
	"testing"
)

// TestLongPathForm: a path the raw Win32 calls get is in the extended form
// once it is long enough to need it, as os.fixLongPath makes it for the os
// package's calls, and is left alone otherwise.
func TestLongPathForm(t *testing.T) {
	long := strings.Repeat("d", 250)
	for in, want := range map[string]string{
		`C:\short\path`:                  `C:\short\path`,
		`C:\` + long:                     `\\?\C:\` + long,
		`\\server\share\` + long:         `\\?\UNC\server\share\` + long,
		`\\?\C:\` + long:                 `\\?\C:\` + long,
		`\\?\UNC\server\share\` + long:   `\\?\UNC\server\share\` + long,
		`\\.\pipe\` + long:               `\\.\pipe\` + long,
		`C:\` + strings.Repeat("d", 244): `C:\` + strings.Repeat("d", 244),
		`C:\` + strings.Repeat("d", 245): `\\?\C:\` + strings.Repeat("d", 245),
	} {
		if got := LongPath(in); got != want {
			t.Errorf("LongPath(%.40q...) = %.48q..., want %.48q...", in, got, want)
		}
	}
}
