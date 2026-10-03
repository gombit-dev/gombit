package cli

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestAddCommandAppearsInHelpAndRuns(t *testing.T) {
	attach := func(stdout, stderr *bytes.Buffer) *Command {
		root := NewRoot(stdout, stderr)
		AddCommand(root, &Command{
			Use:   "greet",
			Short: "test greet",
			RunE: func(cmd *Command, args []string) error {
				_, err := cmd.OutOrStdout().Write([]byte("greet: ok\n"))
				return err
			},
		})
		return root
	}

	helpOut := new(bytes.Buffer)
	if err := ExecuteRoot(context.Background(), attach(helpOut, new(bytes.Buffer)), []string{"--help"}); err != nil {
		t.Fatalf("help: %v", err)
	}
	if !strings.Contains(helpOut.String(), "greet") {
		t.Fatalf("help missing greet:\n%s", helpOut.String())
	}

	runOut := new(bytes.Buffer)
	if err := ExecuteRoot(context.Background(), attach(runOut, new(bytes.Buffer)), []string{"greet"}); err != nil {
		t.Fatalf("greet: %v", err)
	}
	if !strings.Contains(runOut.String(), "greet: ok") {
		t.Fatalf("greet output = %q", runOut.String())
	}
}

func TestAddCommandNilRootPanics(t *testing.T) {
	defer func() {
		got := recover()
		if got == nil {
			t.Fatal("expected panic")
		}
		msg, ok := got.(string)
		if !ok || !strings.Contains(msg, "nil root") {
			t.Fatalf("panic = %#v, want nil root", got)
		}
	}()
	AddCommand(nil, &Command{Use: "x"})
}

// frameworkCommandNames returns the name of every command NewRoot registers.
func frameworkCommandNames(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, cmd := range NewRoot(nil, nil).Commands() {
		names = append(names, cmd.Name())
	}
	if len(names) == 0 {
		t.Fatal("NewRoot registered no commands")
	}
	return names
}

// TestRootHelpListsEveryCommand is the drift guard for the hand-written
// command lists: every command NewRoot registers must appear in the root
// Long help, in the bare-gombit usage text, and in NewRoot's doc comment.
func TestRootHelpListsEveryCommand(t *testing.T) {
	long := rootLongHelp()
	var usageOut bytes.Buffer
	usage(&usageOut)

	src, err := os.ReadFile("root.go")
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "root.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	var newRootDoc string
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "NewRoot" && fn.Doc != nil {
			newRootDoc = fn.Doc.Text()
		}
	}
	if newRootDoc == "" {
		t.Fatal("NewRoot has no doc comment")
	}

	for _, name := range frameworkCommandNames(t) {
		line := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(name) + `(\s|$)`)
		if !line.MatchString(long) {
			t.Errorf("root --help (rootLongHelp) does not list %q:\n%s", name, long)
		}
		if !line.MatchString(usageOut.String()) {
			t.Errorf("bare gombit usage does not list %q:\n%s", name, usageOut.String())
		}
		word := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
		if !word.MatchString(newRootDoc) {
			t.Errorf("NewRoot doc comment does not list %q:\n%s", name, newRootDoc)
		}
	}
}

// TestRootHelpListsEveryDBSubcommand keeps the db summary in root --help in
// step with the db subcommands actually registered.
func TestRootHelpListsEveryDBSubcommand(t *testing.T) {
	long := rootLongHelp()
	start := strings.Index(long, "\n  db ")
	if start < 0 {
		t.Fatalf("root help has no db line:\n%s", long)
	}
	// The db entry runs until the next family line ("  <name>" at column 2);
	// its continuation lines are indented further.
	entry := long[start+1:]
	if loc := regexp.MustCompile(`(?m)^  \S`).FindAllStringIndex(entry, 2); len(loc) == 2 {
		entry = entry[:loc[1][0]]
	}

	var db *Command
	for _, cmd := range NewRoot(nil, nil).Commands() {
		if cmd.Name() == "db" {
			db = cmd
		}
	}
	if db == nil {
		t.Fatal("NewRoot registers no db command")
	}
	for _, sub := range db.Commands() {
		if !regexp.MustCompile(`\b` + regexp.QuoteMeta(sub.Name()) + `\b`).MatchString(entry) {
			t.Errorf("root help db entry does not list db %s:\n%s", sub.Name(), entry)
		}
	}
}
