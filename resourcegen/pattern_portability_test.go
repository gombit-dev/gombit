//go:build !integration

package resourcegen

import (
	"os/exec"
	"regexp"
	"testing"
)

// TestPatternEnginesAgree runs the request pattern in Go and the form pattern
// in node. Patterns the two engines match differently are rejected. A pattern
// the gate accepts matches the same samples on both sides, including a
// non-BMP `.`.
func TestPatternEnginesAgree(t *testing.T) {
	t.Parallel()
	disagree := []struct {
		pattern string
		sample  string
	}{
		{`\a`, "a"},
		{`\a`, "\a"},
		{`[]a]`, "a"},
		{`[]a]`, "]"},
		{`a]`, "a"},
		{`a{01}`, "a"},
		{`a{00}`, "a"},
		{`^+`, ""},
		{`[\d-9]`, "-"},
		{`\B`, "a😀b"},
	}
	for _, c := range disagree {
		if err := portablePattern(c.pattern); err == nil {
			t.Fatalf("portablePattern(%q) accepted a split match set", c.pattern)
		}
		goYes := regexp.MustCompile(c.pattern).MatchString(c.sample)
		js := nodeRegExp(t, c.pattern, c.sample, "u")
		if js == yesNo(goYes) {
			t.Fatalf("engines agree on %q %q (%s); rejection would hide a shared pattern", c.pattern, c.sample, js)
		}
	}

	agree := []struct {
		pattern string
		sample  string
		want    bool
	}{
		{`^.$`, "😀", true},
		{`^.{2}$`, "😀", false},
		{`^.$`, "\r", true},
		{`^.$`, "\u2028", true},
		{`^.$`, "\u2029", true},
		{`^.$`, "\n", false},
		{`\x41`, "A", true},
		{`a{0}`, "hello", true},
		{`a{10}`, "a", false},
		{`a{0,10}`, "aa", true},
		{`[-\d]`, "-", true},
		{`[\d-]`, "-", true},
		{`[a-z]`, "m", true},
		{`(?:^)+`, "", true},
	}
	for _, c := range agree {
		if err := portablePattern(c.pattern); err != nil {
			t.Fatalf("portablePattern(%q): %v", c.pattern, err)
		}
		if got := regexp.MustCompile(c.pattern).MatchString(c.sample); got != c.want {
			t.Fatalf("re2 %q %q = %v, want %v", c.pattern, c.sample, got, c.want)
		}
		form := jsFormPattern(c.pattern)
		if js := nodeRegExp(t, form, c.sample, "u"); js != yesNo(c.want) {
			t.Fatalf("js %q %q = %s, want %s", form, c.sample, js, yesNo(c.want))
		}
	}
	if got := jsFormPattern(`a.b[.]`); got != `a[^\n]b[.]` {
		t.Fatalf("jsFormPattern = %q", got)
	}
}

func yesNo(ok bool) string {
	if ok {
		return "yes"
	}
	return "no"
}

func nodeRegExp(t *testing.T, pattern, sample, flags string) string {
	t.Helper()
	cmd := exec.Command("node", "-e", nodeRegExpScript, pattern, sample, flags) // #nosec G204 -- node is the form's regex engine; arguments are samples, not a shell.
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	return string(out)
}

const nodeRegExpScript = `
const pattern = process.argv[1];
const sample = process.argv[2];
const flags = process.argv[3];
try {
  const re = new RegExp(pattern, flags);
  process.stdout.write(re.test(sample) ? "yes" : "no");
} catch (e) {
  process.stdout.write("syntax");
}
`
