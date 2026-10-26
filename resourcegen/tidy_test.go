package resourcegen

import (
	"os/exec"
)

// tidyThrowawayModule runs `go mod tidy -e` in dir, a throwaway module that
// requires this framework through a local replace, and returns its output.
//
// -e: tidy also resolves the tests of the packages the module imports (huma's
// tests import testify), which building and testing the module never needs.
// Such a module's own graph can select versions of those test-only modules
// that this module's graph never does (an upgraded dependency moves this
// module's testify while huma still pins its own), so CI, which runs with
// GOPROXY=off over a cache warmed from this module, does not have them. With
// -e tidy records what it can resolve and goes on; a module the generated
// code really needs and the cache lacks still fails the `go test` that
// follows, by name.
func tidyThrowawayModule(dir string) ([]byte, error) {
	tidy := exec.Command("go", "mod", "tidy", "-e")
	tidy.Dir = dir
	return tidy.CombinedOutput()
}
