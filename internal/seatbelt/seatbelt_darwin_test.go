package seatbelt

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// TestLoad: sandbox_check answers on this macOS; a process in a sandbox
// and its double-forked child (launchd's child, out of its parent's tree)
// count as sandboxed, a process outside does not, nor does a gone one.
func TestLoad(t *testing.T) {
	sandboxed := Load()
	if sandboxed == nil {
		t.Fatal("sandbox_check is missing or does not answer 0 for launchd")
	}
	if sandboxed(os.Getpid()) {
		t.Skip("the test runs in a sandbox, where sandbox-exec cannot nest")
	}

	plain := exec.Command("sleep", "30")
	if err := plain.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { plain.Process.Kill(); plain.Wait() })
	if sandboxed(plain.Process.Pid) {
		t.Error("an unsandboxed child reads as sandboxed")
	}

	// The inner sh exits at once, leaving the orphan sleep to launchd.
	sb := exec.Command("sandbox-exec", "-p", "(version 1)(allow default)",
		"/bin/sh", "-c", `/bin/sh -c 'sleep 30 >/dev/null & echo $!'; exec sleep 30`)
	out, err := sb.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sb.Process.Kill(); sb.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("reading the orphan's pid: %v", err)
	}
	orphan, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Kill(orphan, syscall.SIGKILL) })
	if !sandboxed(sb.Process.Pid) {
		t.Error("a process under sandbox-exec reads as unsandboxed")
	}
	if !sandboxed(orphan) {
		t.Error("a double-forked sandboxed process reads as unsandboxed")
	}

	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	if !sandboxed(gone.Process.Pid) {
		t.Error("a gone process reads as unsandboxed")
	}
}
