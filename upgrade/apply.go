package upgrade

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gombit-dev/gombit/internal/atomicfile"
)

// Applied is what ApplyDependencies did: the plan it carried out, the go
// commands it ran for it, and the lines of go.mod and go.sum they changed.
type Applied struct {
	// Plan is the plan the dependency changes came from. Its automatic and
	// manual changes are not applied: they are still to do.
	Plan Plan `json:"plan"`
	// Commands are the go commands run, in order, with their output.
	Commands []GoCommand `json:"commands"`
	// GoMod and GoSum are the lines the commands removed and added.
	GoMod FileChange `json:"go_mod"`
	GoSum FileChange `json:"go_sum"`
	// FollowUp are steps the dependency change leaves to the developer
	// before the app builds again, such as refreshing a vendor directory.
	FollowUp []string `json:"follow_up"`
}

// GoCommand is one go command run for an upgrade.
type GoCommand struct {
	// Args are its arguments, after "go".
	Args []string `json:"args"`
	// Output is what it printed (stdout and stderr).
	Output string `json:"output,omitempty"`
}

// FileChange is the lines a file lost and gained, each in file order.
type FileChange struct {
	Removed []string `json:"removed"`
	Added   []string `json:"added"`
}

// GoCommandError is a go command that failed while applying an upgrade:
// its arguments, its error, and everything it printed, unedited. The
// upgrade restores go.mod and go.sum before returning it.
type GoCommandError struct {
	Args   []string
	Output string
	Err    error
}

func (e *GoCommandError) Error() string {
	msg := fmt.Sprintf("go %s: %v", strings.Join(e.Args, " "), e.Err)
	if out := strings.TrimSpace(e.Output); out != "" {
		msg += "\n" + out
	}
	return msg
}

func (e *GoCommandError) Unwrap() error { return e.Err }

// ErrNoTarget: applying an upgrade needs its target named, never a
// default.
var ErrNoTarget = errors.New("upgrade: name the target release to apply an upgrade to (--to)")

// runGo runs the go command in dir. Indirected for tests.
var runGo = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "go", args...) // #nosec G204 -- the go command, with arguments built from the plan (module paths and versions the manifest validated)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

// ApplyDependencies upgrades the framework dependency of the application in
// workDir to the release to, as PlanUpgrade plans it: it changes go.mod and
// go.sum with the go command, and nothing else. Each replace the plan names
// is edited first (go mod edit -dropreplace, or -replace), so that the
// require then resolves to the target (go get MODULE@to, last). The go
// command does the resolution, so the result is Go's own, deterministic for
// the same module sources; Applied reports every command and changed line.
//
// It refuses what PlanUpgrade refuses, and to must be given. A go command
// that fails is reported with its full output (GoCommandError), never
// summarized, and so is a result whose build is not the target release;
// either way go.mod and go.sum are restored to the bytes they had. The
// plan's automatic and manual changes are left to do.
func ApplyDependencies(ctx context.Context, workDir string, m *Manifest, to string) (Applied, error) {
	if to == "" {
		return Applied{}, ErrNoTarget
	}
	plan, err := PlanUpgrade(workDir, m, to)
	if err != nil {
		return Applied{}, err
	}
	applied := Applied{Plan: plan, Commands: []GoCommand{}, FollowUp: []string{}}
	if plan.Current == plan.Target {
		return applied, nil
	}
	goMod := filepath.Join(workDir, "go.mod")
	goSum := filepath.Join(workDir, "go.sum")
	before, err := snapshotFiles(goMod, goSum)
	if err != nil {
		return Applied{}, err
	}
	fail := func(cause error) (Applied, error) {
		if restoreErr := before.restore(); restoreErr != nil {
			return applied, fmt.Errorf("%w\n(restoring go.mod and go.sum failed too: %v)", cause, restoreErr)
		}
		return applied, fmt.Errorf("%w\n(go.mod and go.sum are restored)", cause)
	}
	for _, args := range dependencyCommands(plan) {
		out, err := runGo(ctx, workDir, args...)
		applied.Commands = append(applied.Commands, GoCommand{Args: args, Output: string(out)})
		if err != nil {
			return fail(&GoCommandError{Args: args, Output: string(out), Err: err})
		}
	}
	after, err := Detect(workDir)
	if err != nil {
		return fail(fmt.Errorf("upgrade: after the go commands: %w", err))
	}
	if after.Framework.Version != plan.Target {
		return fail(fmt.Errorf("upgrade: after the go commands the app builds against %q, not %s; go.mod has another directive deciding it", after.Framework.Version, plan.Target))
	}
	now, err := snapshotFiles(goMod, goSum)
	if err != nil {
		// The upgrade is done, but cannot be reported: not done, then.
		return fail(err)
	}
	applied.GoMod = lineChange(before.data[0], now.data[0])
	applied.GoSum = lineChange(before.data[1], now.data[1])
	// go get does not touch a vendor directory, and the go command builds
	// from it (-mod=vendor is the default when a top-level vendor/ is there,
	// with modules.txt or not: a missing one is just as stale), so it must be
	// refreshed before the app builds again. That rewrites the whole tree,
	// which is the developer's call, not this command's.
	if info, err := os.Stat(filepath.Join(workDir, "vendor")); err == nil && info.IsDir() {
		applied.FollowUp = append(applied.FollowUp, "vendor/ still holds the old modules: run `go mod vendor` before building")
	}
	return applied, nil
}

// dependencyCommands are the go commands carrying out plan's dependency
// changes: the replace edits, then go get of the target, which moves the
// require and records the sums it needs. Each is pinned to the module's own
// go.mod (and so go.sum): an inherited -modfile (GOFLAGS, or go env -w)
// would make it write an alternate .mod and .sum that the snapshot, the
// verification and the rollback never see. go mod edit takes the file as
// its argument; go get takes -modfile, which the command line sets over
// GOFLAGS.
func dependencyCommands(plan Plan) [][]string {
	var cmds [][]string
	for _, d := range plan.Dependencies {
		if d.Directive != "replace" {
			continue
		}
		old := d.Module
		if d.ForVersion != "" {
			old += "@" + d.ForVersion
		}
		if d.To == "" {
			cmds = append(cmds, []string{"mod", "edit", "-dropreplace=" + old, "go.mod"})
		} else {
			cmds = append(cmds, []string{"mod", "edit", "-replace=" + old + "=" + d.Module + "@" + d.To, "go.mod"})
		}
	}
	return append(cmds, []string{"get", "-modfile=go.mod", FrameworkModulePath + "@" + plan.Target})
}

// fileSnapshot is the bytes of some files (nil for one that does not exist).
type fileSnapshot struct {
	paths []string
	data  [][]byte
	modes []os.FileMode
}

func snapshotFiles(paths ...string) (fileSnapshot, error) {
	f := fileSnapshot{paths: paths}
	for _, p := range paths {
		data, err := os.ReadFile(p) // #nosec G304 -- the app's go.mod and go.sum
		mode := os.FileMode(0o644)
		switch {
		case errors.Is(err, os.ErrNotExist):
			data = nil
		case err != nil:
			return fileSnapshot{}, fmt.Errorf("upgrade: read %s: %w", p, err)
		default:
			if info, err := os.Stat(p); err == nil {
				mode = info.Mode().Perm()
			}
		}
		f.data = append(f.data, data)
		f.modes = append(f.modes, mode)
	}
	return f, nil
}

// restore puts every file back as it was, removing one that did not exist.
func (f fileSnapshot) restore() error {
	var errs []error
	for i, p := range f.paths {
		if f.data[i] == nil {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		if err := atomicfile.Write(p, f.data[i], f.modes[i], atomicfile.AllowNonAtomic()); !atomicfile.Committed(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// lineChange is the lines of before that after lost and the lines it
// gained, counting repeats, each in file order.
func lineChange(before, after []byte) FileChange {
	count := func(data []byte) map[string]int {
		n := map[string]int{}
		for _, l := range splitLines(data) {
			n[l]++
		}
		return n
	}
	inBefore, inAfter := count(before), count(after)
	c := FileChange{Removed: []string{}, Added: []string{}}
	for _, l := range splitLines(before) {
		if inAfter[l] > 0 {
			inAfter[l]--
			continue
		}
		c.Removed = append(c.Removed, l)
	}
	for _, l := range splitLines(after) {
		if inBefore[l] > 0 {
			inBefore[l]--
			continue
		}
		c.Added = append(c.Added, l)
	}
	return c
}

func splitLines(data []byte) []string {
	var lines []string
	for _, l := range bytes.Split(bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n")), []byte("\n")) {
		if s := strings.TrimSpace(string(l)); s != "" {
			lines = append(lines, string(l))
		}
	}
	return lines
}

// Render writes what the upgrade did, and what is still to do.
func (a Applied) Render(w io.Writer) error {
	var b strings.Builder
	p := a.Plan
	if p.Current == p.Target {
		fmt.Fprintf(&b, "Already at %s: nothing to upgrade.\n", p.Target)
		_, err := io.WriteString(w, b.String())
		return err
	}
	fmt.Fprintf(&b, "Gombit upgrade %s -> %s\n\nDependency changes:\n", p.Current, p.Target)
	p.renderDependencies(&b)
	b.WriteString("\nGo commands run:\n")
	for _, c := range a.Commands {
		fmt.Fprintf(&b, "  go %s\n", strings.Join(c.Args, " "))
	}
	b.WriteString("\ngo.mod:\n")
	for _, l := range a.GoMod.Removed {
		fmt.Fprintf(&b, "  - %s\n", strings.TrimSpace(l))
	}
	for _, l := range a.GoMod.Added {
		fmt.Fprintf(&b, "  + %s\n", strings.TrimSpace(l))
	}
	fmt.Fprintf(&b, "go.sum: %d line(s) added, %d removed\n", len(a.GoSum.Added), len(a.GoSum.Removed))
	for _, f := range a.FollowUp {
		fmt.Fprintf(&b, "\nNext: %s.\n", f)
	}
	b.WriteString("\nReview them with `git diff go.mod go.sum`. Still to do (not applied):\n")
	p.renderToDo(&b)
	_, err := io.WriteString(w, b.String())
	return err
}
