// Package ptable takes snapshots of the host's process table: pid, parent,
// name, process group and terminal foreground group (unix) and start time of
// every process. Details that cost a system call or more per process — argv,
// executable path, environment, working directory, open files, owner — are
// loaded on first use and cached for the snapshot's lifetime, so walking a
// few process trees stays cheap on hosts with thousands of processes.
package ptable

import (
	"errors"
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
	// TTY is the device number of the controlling terminal (see TTYName),
	// 0 without one, and always on Windows.
	TTY   uint64
	Start time.Time // zero when unknown
}

// Foreground reports whether p belongs to the foreground process group of
// its terminal, i.e. is what the terminal's user is interacting with.
func (p *Proc) Foreground() bool { return p.Tpgid > 0 && p.Pgrp == p.Tpgid }

// Source loads the per-process details a Snapshot fetches lazily. Errors
// mean "unknown" (the process exited, belongs to another user, or the
// platform cannot tell); errors.ErrUnsupported marks the latter, and
// ErrEnvWithheld an environment the OS keeps back from a readable process.
type Source interface {
	Argv(pid int) ([]string, error)
	Exe(pid int) (string, error)
	Env(pid int) ([]string, error)
	Cwd(pid int) (string, error)
	OpenFiles(pid int) ([]string, error)
	// Owner returns the user the process runs as, in the form of
	// Snapshot.Owner (never "" without an error).
	Owner(pid int) (string, error)
}

// ErrEnvWithheld is Source.Env's error for a process whose command line
// can be read but whose environment the OS withholds: macOS omits it for
// restricted processes (CS_RESTRICT — /bin/zsh, /bin/sh, /usr/bin/*,
// programs signed with entitlements) while System Integrity Protection is
// on, whoever asks, root included.
var ErrEnvWithheld = errors.New("ptable: the OS withholds the environment")

// Snapshot is the process table at one moment. It is not safe for
// concurrent use.
type Snapshot struct {
	procs    map[int]*Proc
	children map[int][]int
	pids     []int
	src      Source
	// detached holds the parent pid New dropped, per child.
	detached map[int]int

	argv     map[int][]string
	exe      map[int]string
	env      map[int][]string
	withheld map[int]bool
	cwd      map[int]string
	owner    map[int]string
}

// Take snapshots the processes running on this host.
func Take() (*Snapshot, error) {
	procs, err := list()
	if err != nil {
		return nil, err
	}
	return New(procs, osSource{}), nil
}

// Argv reads the command line of one process without a snapshot.
func Argv(pid int) ([]string, error) { return osSource{}.Argv(pid) }

// Process reads one process without a snapshot: its pid, parent, name,
// process group and terminal (Start may be zero). ok is false when it
// cannot be read, and always on Windows.
func Process(pid int) (p Proc, ok bool) { return get(pid) }

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
		detached: map[int]int{},
		argv:     map[int][]string{},
		exe:      map[int]string{},
		env:      map[int][]string{},
		withheld: map[int]bool{},
		cwd:      map[int]string{},
		owner:    map[int]string{},
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
			s.detached[pid] = p.PPID
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

// Detached returns the parent pid New dropped for pid because that process
// started after pid (the pid was recycled), 0 when pid kept its parent.
func (s *Snapshot) Detached(pid int) int { return s.detached[pid] }

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
	v, err := s.src.Env(pid)
	s.env[pid] = v
	if errors.Is(err, ErrEnvWithheld) {
		s.withheld[pid] = true
	}
	return v
}

// EnvWithheld reports whether the OS withheld the environment of pid (see
// ErrEnvWithheld).
func (s *Snapshot) EnvWithheld(pid int) bool {
	s.Env(pid)
	return s.withheld[pid]
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

// Owner returns the user pid runs as: its effective uid in decimal on unix,
// the SID of its token's user on Windows. It is "" when that cannot be
// read, and on unix when the process's real and effective uids differ: a
// set-user-ID program has its owner's privileges but the arguments and
// environment of whoever started it, so it belongs to neither.
//
// It is loaded on demand rather than listed with every process: Linux only
// tells reliably in /proc/<pid>/status, which costs as much again as the
// whole listing, and Windows needs the process's token.
func (s *Snapshot) Owner(pid int) string {
	if v, ok := s.owner[pid]; ok {
		return v
	}
	v, err := s.src.Owner(pid)
	if err != nil {
		v = ""
	}
	s.owner[pid] = v
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
