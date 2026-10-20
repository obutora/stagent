package proc

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A holder goes through the GUI login session when it can (macOS: the
// launchd job in gui/<uid>); when it cannot, it is started directly, is
// outside the GUI login session, and its log says why.
func TestStartHolderOutsideGUI(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "h.log")
	direct := 0
	start := func() (int, error) { direct++; return 4242, nil }

	pid, outside, err := startHolder(func() error { return nil }, start, logPath)
	if err != nil || pid != 0 || outside || direct != 0 {
		t.Errorf("via the GUI: pid %d outside %v err %v direct %d", pid, outside, err, direct)
	}

	refused := errors.New("launchctl bootstrap gui/501: exit status 125: Domain does not support specified action")
	pid, outside, err = startHolder(func() error { return refused }, start, logPath)
	if err != nil || pid != 4242 || !outside || direct != 1 {
		t.Errorf("no GUI login: pid %d outside %v err %v direct %d", pid, outside, err, direct)
	}
	b, _ := os.ReadFile(logPath)
	if !strings.Contains(string(b), "outside the GUI login session") || !strings.Contains(string(b), "Domain does not support") {
		t.Errorf("log %q", b)
	}

	failed := errors.New("fork/exec: no such file")
	if _, outside, err = startHolder(func() error { return refused }, func() (int, error) { return 0, failed }, ""); !errors.Is(err, failed) || outside {
		t.Errorf("direct start failed: outside %v err %v", outside, err)
	}
}
