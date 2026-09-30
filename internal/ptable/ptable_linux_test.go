package ptable

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestTakeReadsRealProcesses(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "open.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	child := exec.Command("sleep", "30")
	child.Dir = dir
	child.Env = []string{"PATH=" + os.Getenv("PATH"), "SSH_CONNECTION=1.2.3.4 5 6.7.8.9 22"}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { child.Process.Kill(); child.Wait() }()

	s, err := Take()
	if err != nil {
		t.Fatal(err)
	}
	self := s.Get(os.Getpid())
	if self == nil || self.PPID != os.Getppid() {
		t.Fatalf("self %+v, want ppid %d", self, os.Getppid())
	}
	if age := time.Since(self.Start); age < 0 || age > time.Hour {
		t.Fatalf("start time %v is %v ago", self.Start, age)
	}
	if exe, _ := os.Executable(); s.Exe(os.Getpid()) != exe {
		t.Fatalf("exe %q, want %q", s.Exe(os.Getpid()), exe)
	}
	if !slices.Equal(s.Argv(os.Getpid()), os.Args) {
		t.Fatalf("argv %q, want %q", s.Argv(os.Getpid()), os.Args)
	}
	if !slices.Contains(s.OpenFiles(os.Getpid()), f.Name()) {
		t.Fatalf("open files miss %s", f.Name())
	}
	if want, _ := CurrentOwner(); s.Owner(os.Getpid()) != want || want == "" {
		t.Fatalf("owner %q, want %q", s.Owner(os.Getpid()), want)
	}

	pid := child.Process.Pid
	if !slices.Contains(s.Children(os.Getpid()), pid) {
		t.Fatalf("children %v miss %d", s.Children(os.Getpid()), pid)
	}
	c := s.Get(pid)
	if c.Name != "sleep" || c.Start.Before(self.Start.Add(-time.Second)) {
		t.Fatalf("child %+v", c)
	}
	if got := s.Getenv(pid, "SSH_CONNECTION"); got != "1.2.3.4 5 6.7.8.9 22" {
		t.Fatalf("SSH_CONNECTION %q", got)
	}
	if got := s.Cwd(pid); got != dir {
		t.Fatalf("cwd %q, want %q", got, dir)
	}
}

func TestParseStatOddComm(t *testing.T) {
	boot := time.Unix(1790103135, 0)
	line := []byte("2749404 (a) b (c)) S 2749221 2749404 2749221 34831 2749404 4194304 216271 9189 1 0 954 112 97 24 20 0 46 0 62029985 10075992064 152044\n")
	p, ok := parseStat(2749404, line, boot)
	if !ok {
		t.Fatal("not parsed")
	}
	want := Proc{PID: 2749404, PPID: 2749221, Name: "a) b (c)", Pgrp: 2749404, Tpgid: 2749404, TTY: 34831, Start: boot.Add(620299850 * time.Millisecond)}
	if p != want {
		t.Fatalf("got %+v\nwant %+v", p, want)
	}
	if !p.Foreground() {
		t.Fatal("pgrp == tpgid is the foreground")
	}
}

func TestTTYName(t *testing.T) {
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skip(err)
	}
	defer ptmx.Close()
	n, err := unix.IoctlGetInt(int(ptmx.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	name := "pts/" + strconv.Itoa(n)
	dev := rdev("/dev/" + name)
	if dev == 0 {
		t.Fatalf("no /dev/%s", name)
	}
	if got := TTYName(dev); got != name {
		t.Fatalf("got %q, want %q", got, name)
	}
	if got := TTYName(1<<40 | 3); got != "" {
		t.Fatalf("unknown device named %q", got)
	}
}

func TestStatusOwner(t *testing.T) {
	status := func(uids string) []byte {
		return []byte("Name:\tbash\nUmask:\t0022\nState:\tS (sleeping)\nPid:\t4242\nPPid:\t1\nTracerPid:\t0\n" +
			uids + "\nGid:\t1000\t1000\t1000\t1000\nFDSize:\t256\n")
	}
	for _, c := range []struct{ uids, want string }{
		{"Uid:\t1000\t1000\t1000\t1000", "1000"},
		{"Uid:\t0\t0\t0\t0", "0"},
		// A set-user-ID root program started by uid 1000: root's
		// privileges, 1000's arguments and environment.
		{"Uid:\t1000\t0\t0\t0", ""},
		// Root's process running with another effective uid for now.
		{"Uid:\t0\t1000\t0\t1000", ""},
		// The saved and filesystem uids give no one else control.
		{"Uid:\t1000\t1000\t0\t1000", "1000"},
		{"Uid:\t1000", ""},
		{"", ""},
	} {
		got, err := statusOwner(status(c.uids))
		if got != c.want || (err == nil) != (c.want != "") {
			t.Errorf("%q: got %q, %v; want %q", c.uids, got, err, c.want)
		}
	}
}
