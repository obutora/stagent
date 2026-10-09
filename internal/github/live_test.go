package github_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/obutora/stagent/internal/github"
	"github.com/obutora/stagent/internal/ptable"
)

// procSource serves the cwd and owner of fake processes.
type procSource struct {
	cwd   map[int]string
	owner map[int]string
}

func (s procSource) Argv(int) ([]string, error)      { return nil, errors.ErrUnsupported }
func (s procSource) Exe(int) (string, error)         { return "", errors.ErrUnsupported }
func (s procSource) Env(int) ([]string, error)       { return nil, errors.ErrUnsupported }
func (s procSource) OpenFiles(int) ([]string, error) { return nil, errors.ErrUnsupported }
func (s procSource) Cwd(pid int) (string, error)     { return s.cwd[pid], nil }
func (s procSource) Owner(pid int) (string, error) {
	if o, ok := s.owner[pid]; ok {
		return o, nil
	}
	return "", errors.New("unknown")
}

func TestLiveDirsTakesProcessesUnderSSHSessionsAndHolders(t *testing.T) {
	const me, other = "S-1-5-21-me", "S-1-5-21-other"
	procs := []ptable.Proc{
		{PID: 4, PPID: 0, Name: "sshd.exe"},          // the service (SYSTEM)
		{PID: 10, PPID: 4, Name: "sshd-session.exe"}, // my connection
		{PID: 11, PPID: 10, Name: "pwsh.exe"},
		{PID: 12, PPID: 11, Name: "claude.exe"},
		{PID: 13, PPID: 10, Name: "cmd.exe"},     // same directory as 11
		{PID: 20, PPID: 10, Name: "stagent.exe"}, // the bridge asking
		{PID: 21, PPID: 20, Name: "git.exe"},
		{PID: 30, PPID: 1, Name: "stagent.exe"}, // a holder started by Task Scheduler
		{PID: 31, PPID: 30, Name: "cmd.exe"},
		{PID: 40, PPID: 1, Name: "explorer.exe"}, // not in a session or holder
		{PID: 50, PPID: 4, Name: "sshd-session.exe"},
		{PID: 51, PPID: 50, Name: "pwsh.exe"}, // another user's
	}
	src := procSource{
		cwd: map[int]string{4: `C:\Windows\System32`, 10: `C:\Windows\System32`, 11: `C:\work\a`, 12: `C:\work\b`,
			13: `C:\work\a`, 20: `C:\Users\me`, 21: `C:\work\self`, 30: `C:\Users\me`, 31: `C:\work\c`,
			40: `C:\work\d`, 50: `C:\Windows\System32`, 51: `C:\work\e`},
		owner: map[int]string{10: me, 11: me, 12: me, 13: me, 20: me, 21: me, 30: me, 31: me, 40: me, 50: other, 51: other},
	}
	got := github.LiveDirs(ptable.New(procs, src), me, []int{30}, 20)
	want := []string{`C:\work\a`, `C:\work\b`, `C:\work\c`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LiveDirs = %q, want %q", got, want)
	}
}
