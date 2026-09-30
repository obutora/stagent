package ptable

import (
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
	"time"
)

type noSource struct{}

func (noSource) Argv(int) ([]string, error)      { return nil, errors.ErrUnsupported }
func (noSource) Exe(int) (string, error)         { return "", errors.ErrUnsupported }
func (noSource) Env(int) ([]string, error)       { return nil, errors.ErrUnsupported }
func (noSource) Cwd(int) (string, error)         { return "", errors.ErrUnsupported }
func (noSource) OpenFiles(int) ([]string, error) { return nil, errors.ErrUnsupported }
func (noSource) Owner(int) (string, error)       { return "", errors.ErrUnsupported }

func TestNewOrphansChildOfRecycledParentPID(t *testing.T) {
	t0 := time.Now()
	s := New([]Proc{
		{PID: 4, PPID: 0, Start: t0},
		{PID: 8, PPID: 4, Start: t0.Add(time.Second)},
		// Its parent 4 exited and the pid went to a later process.
		{PID: 9, PPID: 12, Start: t0.Add(2 * time.Second)},
		{PID: 12, PPID: 4, Start: t0.Add(3 * time.Second)},
	}, noSource{})
	if got := s.Children(4); !reflect.DeepEqual(got, []int{8, 12}) {
		t.Fatalf("children of 4: %v", got)
	}
	if got := s.Children(12); got != nil || s.Get(9).PPID != 0 {
		t.Fatalf("recycled parent adopted its predecessor's child: %v, ppid %d", got, s.Get(9).PPID)
	}
	if s.Detached(9) != 12 || s.Detached(8) != 0 {
		t.Fatalf("detached: 9 from %d, 8 from %d", s.Detached(9), s.Detached(8))
	}
}

// withholdingSource withholds the environment of pid 2 and cannot read
// that of pid 3.
type withholdingSource struct{ noSource }

func (withholdingSource) Env(pid int) ([]string, error) {
	switch pid {
	case 2:
		return nil, ErrEnvWithheld
	case 3:
		return nil, errors.New("permission denied")
	}
	return []string{"A=1"}, nil
}

func TestSnapshotEnvWithheld(t *testing.T) {
	s := New([]Proc{{PID: 1}, {PID: 2, PPID: 1}, {PID: 3, PPID: 1}}, withholdingSource{})
	for pid, want := range map[int]bool{1: false, 2: true, 3: false} {
		if got := s.EnvWithheld(pid); got != want || s.Env(pid) != nil == (pid != 1) {
			t.Errorf("pid %d: withheld %v, env %q", pid, got, s.Env(pid))
		}
	}
}

func TestParseProcArgs2(t *testing.T) {
	b := binary.LittleEndian.AppendUint32(nil, 3)
	b = append(b, "/opt/homebrew/bin/tmux\x00\x00\x00\x00"...)
	b = append(b, "tmux\x00-L\x00work\x00"...)
	b = append(b, "HOME=/Users/u\x00TMUX_TMPDIR=/tmp/t\x00\x00"...)
	b = append(b, "executable_path=/opt/homebrew/bin/tmux\x00ptr_munge=\x00"...)
	got, err := parseProcArgs2(b)
	if err != nil {
		t.Fatal(err)
	}
	want := procArgs2{
		exe:  "/opt/homebrew/bin/tmux",
		argv: []string{"tmux", "-L", "work"},
		env:  []string{"HOME=/Users/u", "TMUX_TMPDIR=/tmp/t"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	if _, err := parseProcArgs2([]byte{1, 0}); err == nil {
		t.Fatal("short buffer parsed")
	}
}

// With System Integrity Protection on, macOS cuts kern.procargs2 after argv
// for restricted processes.
func TestProcArgs2EnvWithheld(t *testing.T) {
	b := binary.LittleEndian.AppendUint32(nil, 1)
	b = append(b, "/bin/zsh\x00\x00\x00\x00-zsh\x00"...)
	a, err := parseProcArgs2(b)
	if err != nil || !reflect.DeepEqual(a.argv, []string{"-zsh"}) {
		t.Fatalf("got %+v, %v", a, err)
	}
	if env, err := a.environ(); env != nil || !errors.Is(err, ErrEnvWithheld) {
		t.Fatalf("environ %q, %v", env, err)
	}
	b = append(b, "HOME=/Users/u\x00\x00executable_path=/bin/zsh\x00"...)
	if a, _ = parseProcArgs2(b); a.env == nil {
		t.Fatal("environment lost")
	}
	if env, err := a.environ(); err != nil || !reflect.DeepEqual(env, []string{"HOME=/Users/u"}) {
		t.Fatalf("environ %q, %v", env, err)
	}
}

// utmpxRecord builds a macOS utmpx record (struct utmpx32).
func utmpxRecord(user, line, host string, typ uint16) []byte {
	r := make([]byte, utmpxSize)
	copy(r, user)
	copy(r[utmpxLineOff:], line)
	binary.LittleEndian.PutUint16(r[utmpxTypeOff:], typ)
	copy(r[utmpxHostOff:], host)
	return r
}

func TestParseUtmpx(t *testing.T) {
	var b []byte
	b = append(b, utmpxRecord(utmpxMagic, "", "", utmpxSignature)...)
	b = append(b, utmpxRecord("u", "console", "", utmpxUserProcess)...)
	b = append(b, utmpxRecord("u", "ttys003", "100.1.2.3", utmpxUserProcess)...)
	b = append(b, utmpxRecord("u", "ttys004", "100.9.9.9", 8)...) // DEAD_PROCESS: logged out
	b = append(b, utmpxRecord("u", "ttys005", "fd7a:115c:a1e0::1", utmpxUserProcess)...)
	b = append(b, 1, 2, 3) // a record being written
	want := []utmpxLogin{{"console", ""}, {"ttys003", "100.1.2.3"}, {"ttys005", "fd7a:115c:a1e0::1"}}
	if got := parseUtmpx(b); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
	// Another layout: no signature where this one has it.
	if got := parseUtmpx(append(make([]byte, 4), b...)); got != nil {
		t.Fatalf("misaligned file parsed: %q", got)
	}
}
