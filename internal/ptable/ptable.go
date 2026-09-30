// Package ptable takes snapshots of the host's process table: pid, parent,
// name, process group and terminal foreground group (unix) and start time of
// every process. Details that cost a system call or more per process — argv,
// executable path, environment, working directory, open files — are loaded
// on first use and cached for the snapshot's lifetime, so walking a few
// process trees stays cheap on hosts with thousands of processes.
package ptable

import (
	"runtime"
	"sort"
	"strings"
	"time"
)

// Proc is one process of a Snapshot.
type Proc struct {
	PID, PPID int
	// Name is the executable name the OS keeps per process: Linux comm (at
	// most 15 bytes, or what the program set as its title), macOS p_comm,
	// Windows image name including ".exe".
	Name string
	// Pgrp is the process group and Tpgid the foreground process group of
	// the controlling terminal (0 when unknown, and always on Windows).
	Pgrp, Tpgid int
	Start       time.Time // zero when unknown
}

// Foreground reports whether p belongs to the foreground process group of
// its terminal, i.e. is what the terminal's user is interacting with.
func (p *Proc) Foreground() bool { return p.Tpgid > 0 && p.Pgrp == p.Tpgid }

// Source loads the per-process details a Snapshot fetches lazily. Errors
// mean "unknown" (the process exited, belongs to another user, or the
// platform cannot tell); errors.ErrUnsupported marks the latter.
type Source interface {
	Argv(pid int) ([]string, error)
	Exe(pid int) (string, error)
	Env(pid int) ([]string, error)
	Cwd(pid int) (string, error)
	OpenFiles(pid int) ([]string, error)
}

// Snapshot is the process table at one moment. It is not safe for
// concurrent use.
type Snapshot struct {
	procs    map[int]*Proc
	children map[int][]int
	pids     []int
	src      Source

	argv map[int][]string
	exe  map[int]string
	env  map[int][]string
	cwd  map[int]string
}

// Take snapshots the processes running on this host.
func Take() (*Snapshot, error) {
	procs, err := list()
	if err != nil {
		return nil, err
	}
	return New(procs, osSource{}), nil
}

// New builds a snapshot of procs whose lazy details come from src.
//
// A parent that started after its child is a recycled pid — Windows keeps
// the parent pid of a process whose parent exited — so such a child counts
// as orphaned instead of being attached to an unrelated process.
func New(procs []Proc, src Source) *Snapshot {
	s := &Snapshot{
		procs:    make(map[int]*Proc, len(procs)),
		children: map[int][]int{},
		src:      src,
		argv:     map[int][]string{},
		exe:      map[int]string{},
		env:      map[int][]string{},
		cwd:      map[int]string{},
	}
	for i := range procs {
		p := procs[i]
		s.procs[p.PID] = &p
		s.pids = append(s.pids, p.PID)
	}
	sort.Ints(s.pids)
	for _, pid := range s.pids {
		p := s.procs[pid]
		parent := s.procs[p.PPID]
		if parent == nil || p.PPID == p.PID {
			continue
		}
		if !parent.Start.IsZero() && !p.Start.IsZero() && parent.Start.After(p.Start) {
			p.PPID = 0
			continue
		}
		s.children[p.PPID] = append(s.children[p.PPID], pid)
	}
	return s
}

// Get returns the process pid, or nil.
func (s *Snapshot) Get(pid int) *Proc { return s.procs[pid] }

// PIDs returns every pid in ascending order.
func (s *Snapshot) PIDs() []int { return s.pids }

// Children returns the child pids of pid in ascending order.
func (s *Snapshot) Children(pid int) []int { return s.children[pid] }

// Argv returns the command line of pid (nil when unreadable).
func (s *Snapshot) Argv(pid int) []string {
	if v, ok := s.argv[pid]; ok {
		return v
	}
	v, _ := s.src.Argv(pid)
	s.argv[pid] = v
	return v
}

// Exe returns the executable path of pid ("" when unreadable).
func (s *Snapshot) Exe(pid int) string {
	if v, ok := s.exe[pid]; ok {
		return v
	}
	v, _ := s.src.Exe(pid)
	s.exe[pid] = v
	return v
}

// Env returns the environment ("KEY=value" entries) of pid, nil when it
// cannot be read.
func (s *Snapshot) Env(pid int) []string {
	if v, ok := s.env[pid]; ok {
		return v
	}
	v, _ := s.src.Env(pid)
	s.env[pid] = v
	return v
}

// Getenv returns the value of key in the environment of pid ("" when unset
// or unreadable).
func (s *Snapshot) Getenv(pid int, key string) string {
	return Lookup(s.Env(pid), key)
}

// Cwd returns the working directory of pid ("" when unreadable).
func (s *Snapshot) Cwd(pid int) string {
	if v, ok := s.cwd[pid]; ok {
		return v
	}
	v, _ := s.src.Cwd(pid)
	s.cwd[pid] = v
	return v
}

// OpenFiles returns the paths of the files pid has open, nil when that is
// unknown: only Linux can tell, and only for the user's own processes.
func (s *Snapshot) OpenFiles(pid int) []string {
	v, _ := s.src.OpenFiles(pid)
	return v
}

// Lookup returns the value of key in env ("KEY=value" entries; keys compare
// case-insensitively on Windows, like the OS does). The first entry wins,
// as with getenv(3).
func Lookup(env []string, key string) string {
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok && (k == key || runtime.GOOS == "windows" && strings.EqualFold(k, key)) {
			return v
		}
	}
	return ""
}
