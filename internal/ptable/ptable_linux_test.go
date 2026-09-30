package ptable

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
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
	want := Proc{PID: 2749404, PPID: 2749221, Name: "a) b (c)", Pgrp: 2749404, Tpgid: 2749404, Start: boot.Add(620299850 * time.Millisecond)}
	if p != want {
		t.Fatalf("got %+v\nwant %+v", p, want)
	}
	if !p.Foreground() {
		t.Fatal("pgrp == tpgid is the foreground")
	}
}
