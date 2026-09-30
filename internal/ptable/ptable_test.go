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
