package ptable

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// clockTicks is the unit of /proc/<pid>/stat starttime: USER_HZ, which the
// kernel keeps at 100 for userspace on every architecture Go supports.
const clockTicks = 100

func list() ([]Proc, error) {
	boot, err := bootTime()
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	out := make([]Proc, 0, len(ents))
	buf := make([]byte, 2048) // a stat line is well under 1 KiB
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		n, err := readStat("/proc/"+e.Name()+"/stat", buf)
		if err != nil {
			continue // exited meanwhile
		}
		if p, ok := parseStat(pid, buf[:n], boot); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// readStat reads a /proc stat file with bare system calls: os.ReadFile's
// extra fstat and allocations make a scan of every process half again as
// slow, and it runs every second.
func readStat(path string, buf []byte) (int, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	defer syscall.Close(fd)
	return syscall.Read(fd, buf)
}

// parseStat parses /proc/<pid>/stat. comm is parenthesized and may itself
// contain spaces and parentheses, so the fields are split after the last
// ')'.
func parseStat(pid int, b []byte, boot time.Time) (Proc, bool) {
	open := bytes.IndexByte(b, '(')
	end := bytes.LastIndexByte(b, ')')
	if open < 0 || end < open {
		return Proc{}, false
	}
	// Fields from 3 on: state ppid pgrp session tty_nr tpgid flags minflt
	// cminflt majflt cmajflt utime stime cutime cstime priority nice
	// num_threads itrealvalue starttime …
	f := strings.Fields(string(b[end+1:]))
	if len(f) < 20 {
		return Proc{}, false
	}
	num := func(i int) int {
		n, _ := strconv.Atoi(f[i])
		return n
	}
	p := Proc{
		PID:   pid,
		PPID:  num(1),
		Name:  string(b[open+1 : end]),
		Pgrp:  num(2),
		Tpgid: num(5),
	}
	if ticks, err := strconv.ParseInt(f[19], 10, 64); err == nil {
		p.Start = boot.Add(time.Duration(ticks) * time.Second / clockTicks)
	}
	return p, true
}

// bootTime reads the boot time from /proc/stat (whole seconds).
func bootTime() (time.Time, error) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, err
	}
	for line := range bytes.SplitSeq(b, []byte("\n")) {
		if v, ok := bytes.CutPrefix(line, []byte("btime ")); ok {
			sec, err := strconv.ParseInt(string(bytes.TrimSpace(v)), 10, 64)
			if err != nil {
				return time.Time{}, err
			}
			return time.Unix(sec, 0), nil
		}
	}
	return time.Time{}, errors.New("ptable: no btime in /proc/stat")
}

type osSource struct{}

func procPath(pid int, name string) string {
	return "/proc/" + strconv.Itoa(pid) + "/" + name
}

func (osSource) Argv(pid int) ([]string, error) {
	b, err := os.ReadFile(procPath(pid, "cmdline"))
	if err != nil {
		return nil, err
	}
	return splitNul(b), nil
}

func (osSource) Exe(pid int) (string, error) {
	p, err := os.Readlink(procPath(pid, "exe"))
	// A binary replaced since the process started (an upgrade) reads as
	// "<path> (deleted)"; the path usually holds the new version.
	return strings.TrimSuffix(p, " (deleted)"), err
}

func (osSource) Env(pid int) ([]string, error) {
	b, err := os.ReadFile(procPath(pid, "environ"))
	if err != nil {
		return nil, err
	}
	return splitNul(b), nil
}

func (osSource) Cwd(pid int) (string, error) {
	return os.Readlink(procPath(pid, "cwd"))
}

func (osSource) OpenFiles(pid int) ([]string, error) {
	dir := procPath(pid, "fd")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := []string{} // readable: known, even when empty
	for _, e := range ents {
		// Sockets, pipes and anonymous inodes read as "type:[inode]".
		if p, err := os.Readlink(filepath.Join(dir, e.Name())); err == nil && filepath.IsAbs(p) {
			out = append(out, p)
		}
	}
	return out, nil
}

// splitNul splits NUL-separated strings (cmdline, environ), ignoring the
// terminating NULs.
func splitNul(b []byte) []string {
	s := strings.TrimRight(string(b), "\x00")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x00")
}
