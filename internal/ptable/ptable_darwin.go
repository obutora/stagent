package ptable

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
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
		k := &kps[i]
		name := k.Proc.P_comm[:]
		if n := bytes.IndexByte(name, 0); n >= 0 {
			name = name[:n]
		}
		out = append(out, Proc{
			PID:   int(k.Proc.P_pid),
			PPID:  int(k.Eproc.Ppid),
			Name:  string(name),
			Pgrp:  int(k.Eproc.Pgid),
			Tpgid: int(k.Eproc.Tpgid),
			Start: time.Unix(k.Proc.P_starttime.Sec, int64(k.Proc.P_starttime.Usec)*1000),
		})
	}
	return out, nil
}

type osSource struct{}

// procArgs reads kern.procargs2: executable path, argv and environment.
// The kernel only returns it for processes of the same user.
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
	return a.env, err
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
