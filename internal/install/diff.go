package install

import (
	"fmt"
	"strings"
)

// diffContext is the number of unchanged lines around each hunk.
const diffContext = 3

// maxDiffD bounds the edit distance the Myers search explores; beyond it the
// differing middle section is shown as a whole replacement, which keeps
// memory at O(maxDiffD²) for pathological inputs (e.g. a reformatted file).
const maxDiffD = 1500

// unifiedDiff renders a unified diff from old to new for path. A nil old
// means the file is created, a nil new that it is deleted. Equal inputs give
// "".
func unifiedDiff(path string, old, new []byte) string {
	if old != nil && new != nil && string(old) == string(new) {
		return ""
	}
	a, b := splitLines(old), splitLines(new)
	edits := diffLines(a, b)
	var sb strings.Builder
	from, to := path, path
	if old == nil {
		from = "/dev/null"
	}
	if new == nil {
		to = "/dev/null"
	}
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", from, to)
	for _, h := range hunks(edits) {
		writeHunk(&sb, h, a, b)
	}
	return sb.String()
}

// splitLines splits after each '\n', keeping the terminator so that a final
// line without newline differs from the same line with one.
func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := string(b)
	var out []string
	for len(s) > 0 {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			out = append(out, s)
			break
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
	}
	return out
}

type edit struct {
	kind   byte // ' ', '-', '+'
	ai, bi int  // line index in a (for ' ' and '-') / b (for ' ' and '+')
}

func diffLines(a, b []string) []edit {
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	edits := make([]edit, 0, len(a)+len(b))
	for i := 0; i < p; i++ {
		edits = append(edits, edit{' ', i, i})
	}
	edits = append(edits, groupChanges(myers(a[p:len(a)-s], b[p:len(b)-s], p, p))...)
	for i := 0; i < s; i++ {
		edits = append(edits, edit{' ', len(a) - s + i, len(b) - s + i})
	}
	return edits
}

// myers computes a shortest edit script between x and y (Myers 1986). ao
// and bo offset the reported indices.
func myers(x, y []string, ao, bo int) []edit {
	n, m := len(x), len(y)
	if n == 0 && m == 0 {
		return nil
	}
	replaceAll := func() []edit {
		out := make([]edit, 0, n+m)
		for i := range n {
			out = append(out, edit{'-', ao + i, 0})
		}
		for j := range m {
			out = append(out, edit{'+', 0, bo + j})
		}
		return out
	}
	maxD := min(n+m, maxDiffD)
	off := maxD + 1
	v := make([]int, 2*maxD+3)
	// trace[d] holds v[-d-1 .. d+1] as it was at the start of round d.
	var trace [][]int
	found := -1
	for d := 0; d <= maxD && found < 0; d++ {
		trace = append(trace, append([]int(nil), v[off-d-1:off+d+2]...))
		for k := -d; k <= d; k += 2 {
			var xx int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				xx = v[off+k+1]
			} else {
				xx = v[off+k-1] + 1
			}
			yy := xx - k
			for xx < n && yy < m && x[xx] == y[yy] {
				xx++
				yy++
			}
			v[off+k] = xx
			if xx >= n && yy >= m {
				found = d
				break
			}
		}
	}
	if found < 0 {
		return replaceAll()
	}
	var rev []edit
	xx, yy := n, m
	for d := found; d >= 0; d-- {
		w := trace[d]
		at := func(k int) int { return w[k+d+1] }
		k := xx - yy
		var pk int
		if k == -d || (k != d && at(k-1) < at(k+1)) {
			pk = k + 1
		} else {
			pk = k - 1
		}
		px := at(pk)
		py := px - pk
		for xx > px && yy > py {
			xx--
			yy--
			rev = append(rev, edit{' ', ao + xx, bo + yy})
		}
		if d > 0 {
			if xx == px {
				yy--
				rev = append(rev, edit{'+', 0, bo + yy})
			} else {
				xx--
				rev = append(rev, edit{'-', ao + xx, 0})
			}
		}
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

// groupChanges orders each run of changed lines as deletions followed by
// insertions, the conventional (and equivalent) form.
func groupChanges(es []edit) []edit {
	for i := 0; i < len(es); {
		if es[i].kind == ' ' {
			i++
			continue
		}
		j := i
		for j < len(es) && es[j].kind != ' ' {
			j++
		}
		run := make([]edit, 0, j-i)
		for _, k := range []byte{'-', '+'} {
			for _, e := range es[i:j] {
				if e.kind == k {
					run = append(run, e)
				}
			}
		}
		copy(es[i:j], run)
		i = j
	}
	return es
}

// hunks groups edits into ranges [lo, hi) that contain changes plus up to
// diffContext lines of context on each side.
func hunks(edits []edit) [][]edit {
	var out [][]edit
	i := 0
	for i < len(edits) {
		for i < len(edits) && edits[i].kind == ' ' {
			i++
		}
		if i == len(edits) {
			break
		}
		lo := max(0, i-diffContext)
		// Extend while the gap of unchanged lines to the next change is
		// small enough to merge the hunks.
		j := i
		for {
			for j < len(edits) && edits[j].kind != ' ' {
				j++
			}
			k := j
			for k < len(edits) && edits[k].kind == ' ' {
				k++
			}
			if k < len(edits) && k-j <= 2*diffContext {
				j = k
				continue
			}
			break
		}
		hi := min(len(edits), j+diffContext)
		out = append(out, edits[lo:hi])
		i = hi
	}
	return out
}

func writeHunk(sb *strings.Builder, h []edit, a, b []string) {
	aStart, bStart, aLen, bLen := -1, -1, 0, 0
	for _, e := range h {
		switch e.kind {
		case ' ':
			if aStart < 0 {
				aStart = e.ai
			}
			if bStart < 0 {
				bStart = e.bi
			}
			aLen++
			bLen++
		case '-':
			if aStart < 0 {
				aStart = e.ai
			}
			aLen++
		case '+':
			if bStart < 0 {
				bStart = e.bi
			}
			bLen++
		}
	}
	// A hunk without lines on one side can only come from an empty file on
	// that side (otherwise it would carry context lines).
	if aStart < 0 {
		aStart = 0
	}
	if bStart < 0 {
		bStart = 0
	}
	fmt.Fprintf(sb, "@@ -%s +%s @@\n", hunkRange(aStart, aLen), hunkRange(bStart, bLen))
	for _, e := range h {
		var line string
		var last bool
		switch e.kind {
		case ' ', '-':
			line, last = a[e.ai], e.ai == len(a)-1
		case '+':
			line, last = b[e.bi], e.bi == len(b)-1
		}
		sb.WriteByte(e.kind)
		text := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		sb.WriteString(text)
		sb.WriteByte('\n')
		if last && !strings.HasSuffix(line, "\n") {
			sb.WriteString("\\ No newline at end of file\n")
		}
	}
}

func hunkRange(start, n int) string {
	if n == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	if n == 1 {
		return fmt.Sprintf("%d", start+1)
	}
	return fmt.Sprintf("%d,%d", start+1, n)
}
