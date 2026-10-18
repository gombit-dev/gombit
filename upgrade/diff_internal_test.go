package upgrade

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnifiedDiffEqual(t *testing.T) {
	if d := unifiedDiff("a.go", []byte("x\ny\n"), []byte("x\ny\n")); d != "" {
		t.Fatalf("diff of equal content = %q", d)
	}
}

func TestUnifiedDiffShape(t *testing.T) {
	before := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n"
	after := "1\n2\n3\nfour\n5\n6\n7\n8\n9\n10\n11\n12\n13\n"
	want := "--- a/f.go\n+++ b/f.go\n@@ -1,7 +1,7 @@\n 1\n 2\n 3\n-4\n+four\n 5\n 6\n 7\n@@ -10,3 +10,4 @@\n 10\n 11\n 12\n+13\n"
	if got := unifiedDiff("f.go", []byte(before), []byte(after)); got != want {
		t.Fatalf("diff:\n%s\nwant:\n%s", got, want)
	}
}

// TestUnifiedDiffAppliesWithGit: for random edits, `git apply` turns the
// before file into the after file with the patch, which is what
// gombit upgrade fix --patch hands the developer.
func TestUnifiedDiffAppliesWithGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	rng := rand.New(rand.NewSource(7)) // #nosec G404 -- a seeded, reproducible test input
	words := []string{"a", "b", "c", "d", "e"}
	for i := 0; i < 200; i++ {
		var before []string
		for j := rng.Intn(60); j > 0; j-- {
			before = append(before, words[rng.Intn(len(words))])
		}
		after := append([]string(nil), before...)
		for e := rng.Intn(5); e > 0; e-- {
			switch pos := rng.Intn(len(after) + 1); rng.Intn(3) {
			case 0: // insert
				after = append(after[:pos], append([]string{words[rng.Intn(len(words))]}, after[pos:]...)...)
			case 1: // delete
				if pos < len(after) {
					after = append(after[:pos], after[pos+1:]...)
				}
			default: // change
				if pos < len(after) {
					after[pos] = "changed"
				}
			}
		}
		join := func(l []string) string {
			if len(l) == 0 {
				return ""
			}
			return strings.Join(l, "\n") + "\n"
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "f.go"), []byte(join(before)), 0o600); err != nil {
			t.Fatal(err)
		}
		patch := unifiedDiff("f.go", []byte(join(before)), []byte(join(after)))
		if patch == "" {
			if join(before) != join(after) {
				t.Fatalf("case %d: no diff for different content", i)
			}
			continue
		}
		cmd := exec.Command("git", "-c", "core.autocrlf=false", "apply", "--unidiff-zero", "-") // not the host's line-ending conversion
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(patch)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("case %d: git apply: %v\n%s\npatch:\n%s", i, err, out, patch)
		}
		got, _ := os.ReadFile(filepath.Join(dir, "f.go")) // #nosec G304 -- under t.TempDir()
		if string(got) != join(after) {
			t.Fatalf("case %d: applied = %q, want %q\npatch:\n%s", i, got, join(after), patch)
		}
	}
}

// TestUnifiedDiffLargeRewrite: a file changed throughout (past maxEdits)
// still gets a patch git applies, as one replaced block, in bounded work.
func TestUnifiedDiffLargeRewrite(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	var before, after strings.Builder
	before.WriteString("package p\n")
	after.WriteString("package p\n")
	for i := 0; i < 3*maxEdits; i++ {
		fmt.Fprintf(&before, "    line %d\n", i)
		fmt.Fprintf(&after, "\tline %d\n", i)
	}
	before.WriteString("// end\n")
	after.WriteString("// end\n")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.go"), []byte(before.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	patch := unifiedDiff("f.go", []byte(before.String()), []byte(after.String()))
	if !strings.Contains(patch, fmt.Sprintf("@@ -1,%d +1,%d @@\n package p\n", 3*maxEdits+2, 3*maxEdits+2)) {
		t.Fatalf("patch head:\n%.300s", patch)
	}
	cmd := exec.Command("git", "-c", "core.autocrlf=false", "apply", "-")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(patch)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git apply: %v\n%s", err, out)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "f.go")) // #nosec G304 -- under t.TempDir()
	if string(got) != after.String() {
		t.Fatal("applied content differs")
	}
}
