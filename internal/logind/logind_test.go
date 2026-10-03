package logind

import (
	"errors"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestSystemdCgroup(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"0::/user.slice/user-1000.slice/session-3.scope\n", "/user.slice/user-1000.slice/session-3.scope"},
		// hybrid: systemd tracks units in name=systemd
		{"12:cpu,cpuacct:/\n1:name=systemd:/user.slice/user-1000.slice/user@1000.service/app.slice/x.scope\n0::/other\n", "/user.slice/user-1000.slice/user@1000.service/app.slice/x.scope"},
		{"3:memory:/foo\n", ""},
		{"", ""},
	} {
		if got := SystemdCgroup(c.in); got != c.want {
			t.Errorf("SystemdCgroup(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Whether a process outlives logout follows from where it runs and the
// logind setting that governs that place.
func TestSurvivesByPlacement(t *testing.T) {
	session := "/user.slice/user-1000.slice/session-3.scope"
	manager := "/user.slice/user-1000.slice/user@1000.service/app.slice/stagent-run-1-ab.scope"
	gnome := "/user.slice/user-1000.slice/user@1000.service/app.slice/app-org.gnome.Terminal.slice/vte-spawn-1.scope"
	system := "/system.slice/cron.service"
	type want struct{ survives, known bool }
	for _, c := range []struct {
		cgroup string
		f      Facts
		want   want
	}{
		{session, Facts{Kill: true, KillKnown: true}, want{false, true}},
		{session, Facts{KillKnown: true, Linger: false, LingerKnown: true}, want{true, true}},
		{session, Facts{Linger: true, LingerKnown: true}, want{false, false}},
		{manager, Facts{Kill: true, KillKnown: true, LingerKnown: true}, want{false, true}},
		{manager, Facts{Kill: true, KillKnown: true, Linger: true, LingerKnown: true}, want{true, true}},
		{gnome, Facts{KillKnown: true, LingerKnown: true}, want{false, true}},
		{gnome, Facts{KillKnown: true}, want{false, false}},
		{system, Facts{Kill: true, KillKnown: true, LingerKnown: true}, want{true, true}},
		{"/", Facts{}, want{true, true}},
		{"/user.slice/user-1000.slice", Facts{KillKnown: true, LingerKnown: true}, want{false, false}},
		{"", Facts{KillKnown: true, LingerKnown: true}, want{false, false}},
	} {
		s, k := c.f.Survives(Place(c.cgroup))
		if !k {
			s = false // meaningless when unknown
		}
		if (want{s, k}) != c.want {
			t.Errorf("%s with %+v: survives %v known %v, want %+v", c.cgroup, c.f, s, k, c.want)
		}
	}
}

// fakeLogind answers busctl like logind and the user manager would, and
// moves the process when the scope is started.
type fakeLogind struct {
	kill, linger string // busctl get-property output ("" fails)
	startFails   bool
	cgroup       string
	starts       [][]string
}

func (f *fakeLogind) install(t *testing.T) {
	oldRun, oldRead := run, readCgroup
	t.Cleanup(func() { run, readCgroup = oldRun, oldRead })
	run = func(argv []string) (string, error) {
		switch {
		case slices.Contains(argv, "KillUserProcesses"):
			if f.kill == "" {
				return "", errors.New("no logind")
			}
			return f.kill, nil
		case slices.Contains(argv, "Linger"):
			if f.linger == "" {
				return "", errors.New("no user")
			}
			return f.linger, nil
		case slices.Contains(argv, "StartTransientUnit"):
			f.starts = append(f.starts, argv)
			if f.startFails {
				return "", errors.New("no user manager")
			}
			f.cgroup = "/user.slice/user-1000.slice/user@1000.service/app.slice/" + argv[slices.Index(argv, "ssa(sv)a(sa(sv))")+1]
			return "o \"/org/freedesktop/systemd1/job/1\"\n", nil
		}
		t.Fatalf("unexpected command %q", argv)
		return "", nil
	}
	readCgroup = func() string { return f.cgroup }
}

func TestEscape(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("logind is Linux only")
	}
	session := "/user.slice/user-1000.slice/session-3.scope"
	t.Run("kill user processes moves into a scope", func(t *testing.T) {
		f := &fakeLogind{kill: "b true", linger: "b false", cgroup: session}
		f.install(t)
		r := Escape("stagent-run")
		if !r.Moved || len(f.starts) != 1 || Place(r.Cgroup) != PlaceManager {
			t.Fatalf("result %+v, starts %q", r, f.starts)
		}
		argv := f.starts[0]
		unit := argv[slices.Index(argv, "ssa(sv)a(sa(sv))")+1]
		pid := argv[slices.Index(argv, "au")+2]
		if !strings.HasPrefix(unit, "stagent-run-") || !strings.HasSuffix(unit, ".scope") || argv[1] != "--user" || pid != strconv.Itoa(os.Getpid()) {
			t.Fatalf("start argv %q", argv)
		}
		if s, k := r.SurvivesLogout(); s || !k {
			t.Fatalf("survives %v known %v: the user manager ends at the last logout without lingering", s, k)
		}
	})
	t.Run("lingering moves too and survives", func(t *testing.T) {
		f := &fakeLogind{kill: "b false", linger: "b true", cgroup: session}
		f.install(t)
		r := Escape("stagent-daemon")
		if s, k := r.SurvivesLogout(); !r.Moved || !s || !k {
			t.Fatalf("result %+v", r)
		}
	})
	t.Run("neither: stays in the session, which survives", func(t *testing.T) {
		f := &fakeLogind{kill: "b false", linger: "b false", cgroup: session}
		f.install(t)
		r := Escape("stagent-run")
		if s, k := r.SurvivesLogout(); r.Moved || len(f.starts) != 0 || !s || !k {
			t.Fatalf("result %+v, starts %q", r, f.starts)
		}
	})
	t.Run("a service unit stays", func(t *testing.T) {
		f := &fakeLogind{kill: "b true", linger: "b true", cgroup: "/user.slice/user-1000.slice/user@1000.service/app.slice/stagent.service"}
		f.install(t)
		if r := Escape("stagent-daemon"); r.Moved || len(f.starts) != 0 {
			t.Fatalf("result %+v, starts %q", r, f.starts)
		}
	})
	t.Run("failed start runs on in the session", func(t *testing.T) {
		f := &fakeLogind{kill: "b true", linger: "b false", cgroup: session, startFails: true}
		f.install(t)
		r := Escape("stagent-run")
		if s, k := r.SurvivesLogout(); r.Moved || r.Cgroup != session || s || !k {
			t.Fatalf("result %+v", r)
		}
	})
	t.Run("logind unreachable: no move, unknown", func(t *testing.T) {
		f := &fakeLogind{cgroup: session}
		f.install(t)
		r := Escape("stagent-run")
		if _, k := r.SurvivesLogout(); r.Moved || len(f.starts) != 0 || k {
			t.Fatalf("result %+v", r)
		}
	})
}
