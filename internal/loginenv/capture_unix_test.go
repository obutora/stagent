//go:build !windows

package loginenv

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// script writes an executable shell script named name into dir.
func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// dumper is a stand-in for `stagent __env`: it prints what Dump would.
func dumper(t *testing.T, dir string) string {
	return script(t, dir, "dump", `printf '\000__STAGENT_ENV__\000FROM_LOGIN=1\000'`)
}

// A login shell whose rc files hang takes the capture no longer than its
// time in all, both tries included; a child of the shell holding its
// output open does not make it wait longer.
func TestCaptureTakesAtMostItsTime(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SHELL", script(t, dir, "slowsh", "sleep 30; :"))
	start := time.Now()
	_, err := Capture(dumper(t, dir), 600*time.Millisecond)
	if took := time.Since(start); err == nil || took > 800*time.Millisecond {
		t.Errorf("capture took %v (%v), want at most 600ms and an error", took, err)
	}
}

// The login shell is tried interactive first, then not: one whose
// interactive start fails gives its environment the second time.
func TestCaptureTriesNonInteractive(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SHELL", script(t, dir, "sh", `[ "$2" = -i ] && exit 1; exec /bin/sh -c "$3"`))
	env, err := Capture(dumper(t, dir), time.Second)
	if err != nil || !slices.Contains(env, "FROM_LOGIN=1") {
		t.Errorf("env %v, err %v", env, err)
	}
}
