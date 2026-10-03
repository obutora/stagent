package ptable

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

func list() ([]Proc, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	out := make([]Proc, 0, len(kps))
	for i := range kps {
		out = append(out, kinfoProc(&kps[i]))
	}
	return out, nil
}

func get(pid int) (Proc, bool) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(k.Proc.P_pid) != pid {
		return Proc{}, false
	}
	return kinfoProc(k), true
}

func kinfoProc(k *unix.KinfoProc) Proc {
	name := k.Proc.P_comm[:]
	if n := bytes.IndexByte(name, 0); n >= 0 {
		name = name[:n]
	}
	p := Proc{
		PID:   int(k.Proc.P_pid),
		PPID:  int(k.Eproc.Ppid),
		Name:  string(name),
		Pgrp:  int(k.Eproc.Pgid),
		Tpgid: int(k.Eproc.Tpgid),
		Start: time.Unix(k.Proc.P_starttime.Sec, int64(k.Proc.P_starttime.Usec)*1000),
	}
	// NODEV (-1) without a controlling terminal.
	if k.Eproc.Tdev != -1 {
		p.TTY = uint64(uint32(k.Eproc.Tdev))
	}
	return p
}

type osSource struct{}

// procArgs reads kern.procargs2: executable path, argv and environment.
// The kernel only returns it for processes of the same user (any for
// root), and leaves the environment out for restricted processes.
func procArgs(pid int) (procArgs2, error) {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return procArgs2{}, err
	}
	return parseProcArgs2(b)
}

func (osSource) Argv(pid int) ([]string, error) {
	a, err := procArgs(pid)
	return a.argv, err
}

func (osSource) Exe(pid int) (string, error) {
	a, err := procArgs(pid)
	return a.exe, err
}

func (osSource) Env(pid int) ([]string, error) {
	a, err := procArgs(pid)
	if err != nil {
		return nil, err
	}
	return a.environ()
}

// lsofTimeout bounds one working-directory lookup.
const lsofTimeout = 2 * time.Second

// Cwd asks lsof: the kernel call behind it (proc_pidinfo) is only reachable
// through libproc, which needs cgo.
func (osSource) Cwd(pid int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), lsofTimeout)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "/usr/sbin/lsof", "-a", "-d", "cwd", "-p", strconv.Itoa(pid), "-Fn").Output()
	// -F output: one field per line, "p<pid>", "f<fd>", "n<name>".
	for line := range strings.SplitSeq(string(out), "\n") {
		if name, ok := strings.CutPrefix(line, "n"); ok && name != "" {
			return name, nil
		}
	}
	return "", errors.New("ptable: lsof found no cwd")
}

func (osSource) OpenFiles(int) ([]string, error) { return nil, errors.ErrUnsupported }

// Owner reads the process's credentials from its kinfo_proc: the real uid
// (p_ruid) and the effective one (cr_uid).
func (osSource) Owner(pid int) (string, error) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	e := &k.Eproc
	if e.Pcred.P_ruid != e.Ucred.Uid {
		return "", errMixedUIDs
	}
	return strconv.FormatUint(uint64(e.Ucred.Uid), 10), nil
}

// ReadsTerminal reports whether pid has a controlling terminal, what `ps
// -o tty=` shows: another process's stdin is only reachable through
// libproc, which needs cgo. known is false when pid cannot be looked up.
func ReadsTerminal(pid int) (tty, known bool) {
	p, ok := get(pid)
	return p.TTY != 0, ok
}

// ttyGuess names a pseudo-terminal from its device number: /dev/ttys<nnn>
// has minor nnn.
func ttyGuess(dev uint64) string {
	return fmt.Sprintf("ttys%03d", unix.Minor(dev))
}

// rdev returns the device number of the device file at path, 0 when it
// cannot be read.
func rdev(path string) uint64 {
	var st unix.Stat_t
	if unix.Stat(path, &st) != nil {
		return 0
	}
	return uint64(uint32(st.Rdev))
}

// utmpxPath holds macOS's current logins (see parseUtmpx).
const utmpxPath = "/var/run/utmpx"

// ttyHosts caches TTYHosts until utmpx changes.
var ttyHosts struct {
	sync.Mutex
	mod   time.Time
	size  int64
	hosts map[uint64]string
}

// TTYHosts returns the remote host of each terminal's login by the
// terminal's device number: `login -h HOST` records it in utmpx, which
// everyone may read. nil when utmpx cannot be read.
func TTYHosts() map[uint64]string {
	fi, err := os.Stat(utmpxPath)
	if err != nil {
		return nil
	}
	ttyHosts.Lock()
	defer ttyHosts.Unlock()
	if ttyHosts.hosts != nil && fi.ModTime().Equal(ttyHosts.mod) && fi.Size() == ttyHosts.size {
		return ttyHosts.hosts
	}
	b, err := os.ReadFile(utmpxPath)
	if err != nil {
		return nil
	}
	hosts := map[uint64]string{}
	for _, l := range parseUtmpx(b) {
		if l.line == "" || l.host == "" || strings.Contains(l.line, "..") {
			continue
		}
		if dev := rdev("/dev/" + l.line); dev != 0 {
			hosts[dev] = l.host
		}
	}
	ttyHosts.mod, ttyHosts.size, ttyHosts.hosts = fi.ModTime(), fi.Size(), hosts
	return hosts
}
