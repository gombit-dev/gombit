package upgrade

import (
	"fmt"
	"strings"
)

// unifiedDiff is the unified diff (3 lines of context) turning before into
// after for the file at path, as `git apply` and `patch -p1` read it: the
// headers name a/path and b/path. Empty when the two are equal.
func unifiedDiff(path string, before, after []byte) string {
	a, b := diffLines(string(before)), diffLines(string(after))
	ops := myers(a, b)
	if len(ops) == 0 {
		return ""
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n", path, path)
	const context = 3
	// Group the edit script into hunks: runs of changes with up to 2*context
	// equal lines between them.
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := i - context
		if start < 0 {
			start = 0
		}
		end := i
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			run := end
			for run < len(ops) && ops[run].kind == ' ' {
				run++
			}
			if run == len(ops) || run-end > 2*context {
				break
			}
			end = run
		}
		stop := end + context
		if stop > len(ops) {
			stop = len(ops)
		}
		aStart, bStart, aCount, bCount := ops[start].a, ops[start].b, 0, 0
		var body strings.Builder
		for _, op := range ops[start:stop] {
			switch op.kind {
			case ' ':
				aCount++
				bCount++
				body.WriteString(" " + a[op.a])
			case '-':
				aCount++
				body.WriteString("-" + a[op.a])
			case '+':
				bCount++
				body.WriteString("+" + b[op.b])
			}
		}
		fmt.Fprintf(&out, "@@ -%s +%s @@\n%s", hunkRange(aStart, aCount), hunkRange(bStart, bCount), body.String())
		i = stop
	}
	return out.String()
}

// hunkRange is a hunk header's range for count lines from the 0-based
// start: "start,count" (1-based), and for an empty range the line before it.
func hunkRange(start, count int) string {
	if count == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	return fmt.Sprintf("%d,%d", start+1, count)
}

// diffLines splits s into lines, each keeping its "\n"; a last line without
// one gets the marker diff tools use.
func diffLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	} else {
		lines[len(lines)-1] += "\n\\ No newline at end of file\n"
	}
	return lines
}

// diffOp is one line of an edit script: kept (' '), removed ('-') or added
// ('+'), with its index in a and in b (the next one, for an insertion or a
// deletion).
type diffOp struct {
	kind byte
	a, b int
}

// maxEdits bounds the edit distance myers searches: past it, the changed
// middle is replaced as one block (a correct, not a minimal, diff), so a
// file rewritten throughout costs O(N+M) instead of O(D²).
const maxEdits = 2000

// myers is the edit script from a to b, or nil when they are equal: the
// common prefix and suffix kept, and between them the shortest script
// (Myers' O(ND) algorithm) when it has at most maxEdits edits.
func myers(a, b []string) []diffOp {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	if pre == len(a) && pre == len(b) {
		return nil
	}
	ops := make([]diffOp, 0, len(a)+len(b))
	for i := 0; i < pre; i++ {
		ops = append(ops, diffOp{kind: ' ', a: i, b: i})
	}
	mid := middle(a[pre:len(a)-suf], b[pre:len(b)-suf])
	for _, op := range mid {
		op.a += pre
		op.b += pre
		ops = append(ops, op)
	}
	for i := 0; i < suf; i++ {
		ops = append(ops, diffOp{kind: ' ', a: len(a) - suf + i, b: len(b) - suf + i})
	}
	return ops
}

// middle is the shortest edit script from a to b, which differ at both
// ends, or every line of a removed and every line of b added when that
// script is longer than maxEdits.
func middle(a, b []string) []diffOp {
	n, m := len(a), len(b)
	max := n + m
	limit := max
	if limit > maxEdits {
		limit = maxEdits
	}
	v := make([]int, 2*max+2)
	// trace[d] is v's diagonals -d-1..d+1 (offset[d] the first) before
	// step d: all that the walk back reads.
	var trace [][]int
	var offset []int
	found := false
	for d := 0; d <= limit && !found; d++ {
		lo, hi := max-d-1, max+d+2
		if lo < 0 {
			lo = 0
		}
		if hi > len(v) {
			hi = len(v)
		}
		trace = append(trace, append([]int(nil), v[lo:hi]...))
		offset = append(offset, lo)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[max+k-1] < v[max+k+1]) {
				x = v[max+k+1] // down: an insertion
			} else {
				x = v[max+k-1] + 1 // right: a deletion
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[max+k] = x
			if x >= n && y >= m {
				found = true
				break
			}
		}
	}
	var ops []diffOp
	if !found {
		for i := range a {
			ops = append(ops, diffOp{kind: '-', a: i, b: 0})
		}
		for j := range b {
			ops = append(ops, diffOp{kind: '+', a: n, b: j})
		}
		return ops
	}
	// Walk back through the trace.
	x, y := n, m
	for d := len(trace) - 1; d >= 0; d-- {
		vd, off := trace[d], offset[d]
		at := func(k int) int { return vd[max+k-off] }
		k := x - y
		var prevK int
		if k == -d || (k != d && at(k-1) < at(k+1)) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := at(prevK)
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x--
			y--
			ops = append(ops, diffOp{kind: ' ', a: x, b: y})
		}
		if d > 0 {
			if x == prevX {
				y--
				ops = append(ops, diffOp{kind: '+', a: x, b: y})
			} else {
				x--
				ops = append(ops, diffOp{kind: '-', a: x, b: y})
			}
		}
	}
	for i, j := 0, len(ops)-1; i < j; i, j = i+1, j-1 {
		ops[i], ops[j] = ops[j], ops[i]
	}
	return ops
}
