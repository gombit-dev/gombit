package commandgen_test

import (
	"testing"

	"github.com/gombit-dev/gombit/cli"
	"github.com/gombit-dev/gombit/commandgen"
)

// TestReservesEveryFrameworkCommand keeps the reserved command names in step
// with the framework tree: `gombit make command <name>` must refuse every
// command cli.NewRoot registers (and its aliases), plus Cobra's built-in help
// and completion, or the generated command would collide with it. It lives in
// the external test package because cli imports commandgen.
func TestReservesEveryFrameworkCommand(t *testing.T) {
	names := []string{"help", "completion"}
	cmds := cli.NewRoot(nil, nil).Commands()
	if len(cmds) == 0 {
		t.Fatal("cli.NewRoot registered no commands")
	}
	for _, cmd := range cmds {
		names = append(names, cmd.Name())
		names = append(names, cmd.Aliases...)
	}
	for _, name := range names {
		if !commandgen.IsReservedCommandName(name) {
			t.Errorf("framework command %q is not reserved; add it to reservedCommands in names.go", name)
		}
	}
}
