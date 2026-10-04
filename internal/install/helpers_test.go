package install

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/version"
	"github.com/obutora/stagent/internal/wire"
)

// fakeRunner records system commands instead of running them.
type fakeRunner struct {
	calls   []string
	respond func(name string, args []string) (string, error)
}

func (f *fakeRunner) Run(_ time.Duration, name string, args ...string) (string, error) {
	f.calls = append(f.calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))
	if f.respond != nil {
		return f.respond(name, args)
	}
	return "", nil
}

func (f *fakeRunner) ran(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// fakeDaemon stands in for the daemon's IPC.
type fakeDaemon struct {
	running        bool
	pid            int
	version        string // "" = this binary's
	exitOnShutdown bool
	shutdowns      int
	sessions       []wire.Session
	// bootstrap / bootstrapErr are daemon.status's bootstrap_swapped /
	// bootstrap_error.
	bootstrap    *bool
	bootstrapErr string
}

func (d *fakeDaemon) Status() (*wire.DaemonStatus, error) {
	if !d.running {
		return nil, errors.New("daemon unreachable")
	}
	v := d.version
	if v == "" {
		v = version.Version
	}
	return &wire.DaemonStatus{PID: d.pid, Version: v, BootstrapSwapped: d.bootstrap, BootstrapError: d.bootstrapErr}, nil
}

func (d *fakeDaemon) Shutdown() error {
	if !d.running {
		return errors.New("daemon unreachable")
	}
	d.shutdowns++
	if d.exitOnShutdown {
		d.running = false
	}
	return nil
}

func (d *fakeDaemon) Sessions() ([]wire.Session, error) {
	if !d.running {
		return nil, errors.New("daemon unreachable")
	}
	return d.sessions, nil
}

type testEnv struct {
	*env
	run    *fakeRunner
	daemon *fakeDaemon
	// others are daemons listening at other addresses (daemonAt).
	others map[string]*fakeDaemon
	vars   map[string]string
	bins   map[string]string // lookPath results
	killed []int
	// spawns counts detached daemon starts; spawnFails makes them fail.
	spawns     int
	spawnFails bool
}

// newTestEnv builds an env on a fresh STAGENT_HOME with fake system access.
func newTestEnv(t *testing.T, goos string) *testEnv {
	t.Helper()
	home := t.TempDir()
	t.Setenv(paths.EnvHome, home)
	l, err := paths.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	te := &testEnv{run: &fakeRunner{}, daemon: &fakeDaemon{}, others: map[string]*fakeDaemon{}, vars: map[string]string{}, bins: map[string]string{}}
	clock := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	te.env = &env{
		l: l, goos: goos, run: te.run, daemon: te.daemon,
		daemonAt: func(addr string) daemonAPI {
			if d, ok := te.others[addr]; ok {
				return d
			}
			return &fakeDaemon{}
		},
		lookPath: func(name string) (string, error) {
			if p, ok := te.bins[name]; ok {
				return p, nil
			}
			return "", exec.ErrNotFound
		},
		getenv: func(k string) string { return te.vars[k] },
		now: func() time.Time {
			clock = clock.Add(time.Second)
			return clock
		},
		uid:    1000,
		alive:  func(int) bool { return te.daemon.running },
		settle: 50 * time.Millisecond,
		sid:    "S-1-5-21-1-2-3-1001",
		docs:   filepath.Join(home, "Documents"),
	}
	te.kill = func(pid int) error {
		te.killed = append(te.killed, pid)
		te.daemon.running = false
		return nil
	}
	te.spawnDaemon = func() error {
		te.spawns++
		if te.spawnFails {
			return errors.New("spawn failed")
		}
		te.daemon.running, te.daemon.version = true, ""
		return nil
	}
	te.loadManifest()
	return te
}

// reload starts a fresh command run on the same home (new env state,
// manifest re-read), as the CLI would.
func (te *testEnv) reload() {
	te.notes = nil
	te.dirty = false
	te.ps = nil
	te.loadManifest()
}

// notesWith returns the notes of code.
func notesWith(notes []Note, code string) []Note {
	var out []Note
	for _, n := range notes {
		if n.Code == code {
			out = append(out, n)
		}
	}
	return out
}

func (te *testEnv) integrate(t *testing.T, o integrateOpts) *IntegrateResult {
	t.Helper()
	te.reload()
	r, err := te.env.integrate(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range r.Changes {
		if c.Error != "" {
			t.Fatalf("change %s %s failed: %s", c.ID, c.Target, c.Error)
		}
	}
	return r
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func changeIDs(cs []*Change) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.ID)
	}
	return out
}

func removeAll(t *testing.T, p string) {
	t.Helper()
	if err := os.RemoveAll(p); err != nil {
		t.Fatal(err)
	}
}

func chmodX(t *testing.T, p string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no executable bit and no #! scripts")
	}
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
}
