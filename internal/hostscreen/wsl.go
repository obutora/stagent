package hostscreen

import (
	"io/fs"
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/obutora/stagent/internal/wire"
)

// DetectWSL tells whether fsys (the root file system) is that of a Linux
// running inside Windows' WSL: the kernel's release names Microsoft, or
// WSL's interop with Windows binaries is registered.
func DetectWSL(fsys fs.FS) bool {
	if b, err := fs.ReadFile(fsys, "proc/sys/kernel/osrelease"); err == nil &&
		strings.Contains(strings.ToLower(string(b)), "microsoft") {
		return true
	}
	_, err := fs.Stat(fsys, "proc/sys/fs/binfmt_misc/WSLInterop")
	return err == nil
}

// IsWSL is DetectWSL of this host, looked at once; false off Linux.
var IsWSL = sync.OnceValue(func() bool {
	return runtime.GOOS == "linux" && DetectWSL(os.DirFS("/"))
})

// WSLNote is the note of a kind whose targets a WSL host may run on the
// Windows side, where it does not look (browsers, Android emulators): nil
// unless wsl.
func WSLNote(wsl bool) *wire.HostScreenNote {
	if !wsl {
		return nil
	}
	return &wire.HostScreenNote{
		Code:    wire.NoteWSL,
		Message: "this host is Windows' WSL: only WSL is searched, so browsers and emulators on the Windows side are not listed",
	}
}
