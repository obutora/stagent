package wire

import "testing"

func TestDetectHarness(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{"claude", "fix it"}, HarnessClaude},
		{[]string{`C:\Users\u\AppData\Roaming\npm\codex.cmd`}, HarnessCodex},
		{[]string{"/bin/bash", "-l"}, HarnessOther},
		{nil, HarnessOther},
		// A task's prepared session is told by what `stagent task run`
		// ends in.
		{[]string{"/home/u/.local/bin/stagent", "task", "run", "0123456789abcdef", "--", "claude", "Issue #3"}, HarnessClaude},
		{[]string{`C:\stagent\stagent.exe`, "task", "run", "--switch-branch", "0123456789abcdef", "--", "omp.cmd"}, HarnessOmp},
		{[]string{"/usr/bin/stagent", "task", "run", "0123456789abcdef", "--", "/bin/bash", "-l"}, HarnessOther},
		{[]string{"/usr/bin/stagent", "run", "--", "claude"}, HarnessOther},
	} {
		if got := DetectHarness(c.argv); got != c.want {
			t.Errorf("DetectHarness(%q) = %q, want %q", c.argv, got, c.want)
		}
	}
}
