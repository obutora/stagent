//go:build !windows

package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestRunDirResolution(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	home := t.TempDir()
	uid := strconv.Itoa(os.Getuid())

	def, err := resolve(home, false)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".ssh-term", "agent", "run")
	if runtime.GOOS == "linux" {
		// Not $XDG_RUNTIME_DIR: logind deletes it at the last logout.
		want = filepath.Join(tmp, "stagent-"+uid)
	}
	if def.RunDir != want || def.DaemonAddr != filepath.Join(want, "stagent.sock") {
		t.Errorf("real installation: RunDir %q DaemonAddr %q, want %q", def.RunDir, def.DaemonAddr, want)
	}

	iso, err := resolve(home, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".ssh-term", "agent", "run"); iso.RunDir != want || iso.DaemonAddr != filepath.Join(want, "stagent.sock") {
		t.Errorf("isolated: RunDir %q DaemonAddr %q, want %q", iso.RunDir, iso.DaemonAddr, want)
	}

	// A home whose socket paths would not fit sockaddr_un moves to TMPDIR,
	// namespaced by the home.
	long := filepath.Join(home, strings.Repeat("h", maxSunPath))
	iso, err = resolve(long, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(tmp, "stagent-"+uid+"-"+homeTag(long)); iso.RunDir != want {
		t.Errorf("isolated, long home: RunDir %q, want %q", iso.RunDir, want)
	}
}

// sharedLayout returns a layout whose RunDir is in a fresh TMPDIR.
func sharedLayout(t *testing.T) *Layout {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	l, err := resolve(filepath.Join(t.TempDir(), strings.Repeat("h", maxSunPath)), true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(l.RunDir, os.TempDir()) {
		t.Fatalf("RunDir %q is not in TMPDIR", l.RunDir)
	}
	return l
}

func TestEnsureDirsCreatesPrivateSharedRunDir(t *testing.T) {
	l := sharedLayout(t)
	if err := l.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(l.RunDir)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Fatalf("RunDir %v (%v), want a 0700 directory", st.Mode(), err)
	}
	if _, err := os.Stat(l.HolderSocketDir()); err != nil {
		t.Fatal(err)
	}
	// Existing and still private: accepted again.
	if err := l.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureDirsRefusesUnsafeSharedRunDir(t *testing.T) {
	t.Run("open to others", func(t *testing.T) {
		l := sharedLayout(t)
		if err := os.Mkdir(l.RunDir, 0o755); err != nil {
			t.Fatal(err)
		}
		os.Chmod(l.RunDir, 0o755)
		if err := l.EnsureDirs(); err == nil {
			t.Fatal("accepted a 0755 run directory")
		}
		st, _ := os.Stat(l.RunDir)
		if st.Mode().Perm() != 0o755 {
			t.Errorf("mode changed to %v", st.Mode())
		}
		if _, err := os.Stat(l.HolderSocketDir()); err == nil {
			t.Error("holder socket directory created inside the refused run directory")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		l := sharedLayout(t)
		target := t.TempDir()
		os.Chmod(target, 0o700)
		if err := os.Symlink(target, l.RunDir); err != nil {
			t.Fatal(err)
		}
		if err := l.EnsureDirs(); err == nil {
			t.Fatal("accepted a symlink as run directory")
		}
		if _, err := os.Stat(filepath.Join(target, "s")); err == nil {
			t.Error("holder socket directory created through the symlink")
		}
	})
}
