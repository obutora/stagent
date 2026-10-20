//go:build !windows

package schedagent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

// fakeAgent puts an executable name on PATH that runs script (sh).
func fakeAgent(t *testing.T, name, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func runWrapper(t *testing.T, argv ...string) (int, Outcome, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "outcome.json")
	var screen, stderr bytes.Buffer
	code := run(argv, path, &screen, &stderr)
	o, err := ReadOutcome(path)
	if err != nil {
		t.Fatalf("outcome: %v", err)
	}
	return code, o, screen.String()
}

// Output that is not the agent's JSON: the exit code decides, and the run
// says it could not be judged.
func TestWrapperUnreadableOutput(t *testing.T) {
	fakeAgent(t, "claude", "echo 'Segmentation fault'\nexit 3\n")
	code, o, screen := runWrapper(t, "claude", "-p", "--output-format", "json", "--", "hi")
	if code != 3 || o.Result != wire.RunFailed || !o.Unjudged {
		t.Errorf("exit %d, %+v", code, o)
	}
	if !strings.Contains(screen, "Segmentation fault") || !strings.Contains(screen, "judged by the exit code") {
		t.Errorf("screen:\n%s", screen)
	}

	fakeAgent(t, "omp", "echo 'plain text answer'\n")
	if code, o, _ := runWrapper(t, "omp", "-p", "--mode", "json", "--", "hi"); code != 0 || o.Result != wire.RunSucceeded || !o.Unjudged {
		t.Errorf("exit %d, %+v", code, o)
	}
}

// The agent's JSON is shown as text and judged; its standard input is
// empty (it is not the terminal).
func TestWrapperJudgesTheAgentsOutput(t *testing.T) {
	fakeAgent(t, "claude", "cat >/dev/null\ncat <<'EOF'\n"+claudeDenied+"EOF\n")
	code, o, screen := runWrapper(t, "claude", "-p", "--output-format", "json", "--permission-prompts", "none", "--", "write hello.txt")
	if code != 0 || o.Result != wire.RunDenied || o.Denials != 2 || o.ConversationID == "" || o.Unjudged {
		t.Errorf("exit %d, %+v", code, o)
	}
	for _, want := range []string{"[stagent] claude -p --output-format json --permission-prompts none -- 'write hello.txt'", "refused: Write", "2 refused operation(s)"} {
		if !strings.Contains(screen, want) {
			t.Errorf("screen lacks %q:\n%s", want, screen)
		}
	}
}

func TestWrapperAgentMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	code, o, _ := runWrapper(t, "codex", "exec", "--json", "--", "hi")
	if code != exitNotStarted || o.Result != wire.RunFailed || !strings.Contains(o.Error, "codex not found") {
		t.Errorf("exit %d, %+v", code, o)
	}
}
