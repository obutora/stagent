package install

import (
	"strconv"
	"strings"
	"testing"
)

func TestUnifiedDiff(t *testing.T) {
	cases := []struct {
		name     string
		old, new []byte
		want     string
	}{
		{"equal", []byte("a\n"), []byte("a\n"), ""},
		{"create", nil, []byte("x\ny\n"), "--- /dev/null\n+++ f\n@@ -0,0 +1,2 @@\n+x\n+y\n"},
		{"delete", []byte("x\n"), nil, "--- f\n+++ /dev/null\n@@ -1 +0,0 @@\n-x\n"},
		{"modify with context", []byte("1\n2\n3\n4\n5\n6\n7\n8\n9\n"), []byte("1\n2\n3\n4\nFIVE\n6\n7\n8\n9\n10\n"),
			"--- f\n+++ f\n@@ -2,8 +2,9 @@\n 2\n 3\n 4\n-5\n+FIVE\n 6\n 7\n 8\n 9\n+10\n"},
		{"separate hunks", []byte("a\n1\n2\n3\n4\n5\n6\n7\n8\nb\n"), []byte("A\n1\n2\n3\n4\n5\n6\n7\n8\nB\n"),
			"--- f\n+++ f\n@@ -1,4 +1,4 @@\n-a\n+A\n 1\n 2\n 3\n@@ -7,4 +7,4 @@\n 6\n 7\n 8\n-b\n+B\n"},
		{"missing final newline", []byte("a\nb"), []byte("a\nb\n"),
			"--- f\n+++ f\n@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+b\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := unifiedDiff("f", c.old, c.new); got != c.want {
				t.Fatalf("got:\n%s\nwant:\n%s", got, c.want)
			}
		})
	}
}

// Beyond maxDiffD the middle is replaced wholesale, still a valid diff.
func TestUnifiedDiffLargeRewrite(t *testing.T) {
	var a, b strings.Builder
	for i := range 2000 {
		a.WriteString("old " + strconv.Itoa(i) + "\n")
		b.WriteString("new " + strconv.Itoa(i) + "\n")
	}
	d := unifiedDiff("f", []byte(a.String()), []byte(b.String()))
	if !strings.HasPrefix(d, "--- f\n+++ f\n@@ -1,2000 +1,2000 @@\n-old 0\n") || strings.Count(d, "\n+new ") != 2000 {
		t.Fatalf("unexpected diff head: %.80q", d)
	}
}

func TestPlanShowsDiffAndWritesNothing(t *testing.T) {
	te := newTestEnv(t, "linux")
	const orig = "{\n  \"model\": \"opus\"\n}\n"
	writeFile(t, te.claudeSettings(), orig)
	r := te.integrate(t, integrateOpts{harness: []string{hClaude, hOmp}})
	if r.Applied {
		t.Fatal("plan reported applied")
	}
	if got := strings.Join(changeIDs(r.Changes), ","); got != "claude,omp" {
		t.Fatalf("changes = %s", got)
	}
	c := r.Changes[0]
	if c.Action != "modify" || !strings.HasPrefix(c.Diff, "--- "+te.claudeSettings()+"\n+++ ") ||
		!strings.Contains(c.Diff, "\n-  \"model\": \"opus\"\n+  \"model\": \"opus\",\n+  \"hooks\": {\n") ||
		!strings.Contains(c.Diff, "hook claude || true") {
		t.Fatalf("claude diff:\n%s", c.Diff)
	}
	if o := r.Changes[1]; o.Action != "create" || !strings.Contains(o.Diff, "+"+ompMarker) {
		t.Fatalf("omp change: %+v", o)
	}
	if readFile(t, te.claudeSettings()) != orig || exists(te.ompExtension()) || exists(te.l.Manifest) {
		t.Fatal("plan wrote to disk")
	}
}
