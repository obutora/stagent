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

func get(pid int) (Proc, bool) {
	buf := make([]byte, 2048)
	n, err := readStat(procPath(pid, "stat"), buf)
	if err != nil {
		return Proc{}, false
	}
	p, ok := parseStat(pid, buf[:n], time.Time{})
	p.Start = time.Time{} // needs the boot time, which Process does not read
	return p, ok
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
	if tty := num(4); tty > 0 {
		p.TTY = uint64(tty)
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

// Owner reads the Uid line of /proc/<pid>/status. The owner of /proc/<pid>
// would be cheaper but reads as root for every non-dumpable process — a
// set-user-ID program, and any process that asks for it with
// prctl(PR_SET_DUMPABLE, 0) — so another user's process could pass for
// root's.
func (osSource) Owner(pid int) (string, error) {
	b, err := os.ReadFile(procPath(pid, "status"))
	if err != nil {
		return "", err
	}
	return statusOwner(b)
}

// CurrentOwner returns the user this process runs as, in the form of
// Snapshot.Owner, read the way Owner reads every other process:
// /proc/self/status. geteuid would disagree with it under proot's fake
// root (-0), which fakes the uid system calls but leaves /proc untouched,
// and every process would look like another user's.
func CurrentOwner() (string, error) {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return "", err
	}
	return statusOwner(b)
}

// statusOwner returns the uid of a /proc/<pid>/status whose real and
// effective uids ("Uid:\t<real>\t<effective>\t<saved>\t<fs>") are one.
func statusOwner(b []byte) (string, error) {
	for line := range bytes.SplitSeq(b, []byte("\n")) {
		v, ok := bytes.CutPrefix(line, []byte("Uid:"))
		if !ok {
			continue
		}
		f := strings.Fields(string(v))
		if len(f) < 2 {
			break
		}
		if _, err := strconv.ParseUint(f[0], 10, 32); err != nil {
			break
		}
		if f[1] != f[0] {
			return "", errMixedUIDs
		}
		return f[0], nil
	}
	return "", errors.New("ptable: no uids in /proc status")
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

// ttyGuess names a pseudo-terminal from its device number: /dev/pts/<n> is
// major 136, minor n.
func ttyGuess(dev uint64) string {
	major, minor := (dev>>8)&0xfff, (dev&0xff)|((dev>>12)&0xfff00)
	if major != 136 {
		return ""
	}
	return "pts/" + strconv.FormatUint(minor, 10)
}

// rdev returns the device number of the device file at path, 0 when it
// cannot be read.
func rdev(path string) uint64 {
	var st syscall.Stat_t
	if syscall.Stat(path, &st) != nil {
		return 0
	}
	return uint64(st.Rdev)
}

// TTYHosts returns nil: Linux does not need it, the command lines of the
// login processes that name the remote host are readable to everyone.
func TTYHosts() map[uint64]string { return nil }

// ReadsTerminal reports whether pid's stdin is a terminal, by where
// /proc/<pid>/fd/0 points; known is false when that cannot be read
// (another user's process, the process exited).
func ReadsTerminal(pid int) (tty, known bool) {
	target, err := os.Readlink(procPath(pid, "fd/0"))
	if err != nil {
		return false, false
	}
	return strings.HasPrefix(target, "/dev/pts/") || strings.HasPrefix(target, "/dev/tty"), true
}
