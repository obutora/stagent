package hostscreen

import (
	"testing"
	"testing/fstest"

	"github.com/obutora/stagent/internal/wire"
)

func TestDetectWSL(t *testing.T) {
	for name, tc := range map[string]struct {
		fs   fstest.MapFS
		want bool
	}{
		"wsl2 osrelease": {fstest.MapFS{"proc/sys/kernel/osrelease": {Data: []byte("5.15.153.1-microsoft-standard-WSL2\n")}}, true},
		"wsl1 osrelease": {fstest.MapFS{"proc/sys/kernel/osrelease": {Data: []byte("4.4.0-19041-Microsoft\n")}}, true},
		"WSLInterop":     {fstest.MapFS{"proc/sys/kernel/osrelease": {Data: []byte("6.1.0\n")}, "proc/sys/fs/binfmt_misc/WSLInterop": {}}, true},
		"plain linux":    {fstest.MapFS{"proc/sys/kernel/osrelease": {Data: []byte("6.8.0-45-generic\n")}}, false},
		"no proc":        {fstest.MapFS{}, false},
	} {
		if got := DetectWSL(tc.fs); got != tc.want {
			t.Errorf("%s: DetectWSL = %v, want %v", name, got, tc.want)
		}
	}
}

func TestWSLNote(t *testing.T) {
	if n := WSLNote(false); n != nil {
		t.Errorf("WSLNote(false) = %+v, want nil", n)
	}
	if n := WSLNote(true); n == nil || n.Code != wire.NoteWSL || n.Message == "" {
		t.Errorf("WSLNote(true) = %+v, want the wsl note", n)
	}
}
