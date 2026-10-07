package install

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// wslDistro makes te a WSL distribution with cmd.exe on its PATH.
func wslDistro(te *testEnv) {
	te.kernelRelease = func() string { return "6.6.87.2-microsoft-standard-WSL2\n" }
	te.vars["WSL_DISTRO_NAME"] = "Ubuntu"
	te.bins["cmd.exe"] = "/mnt/c/WINDOWS/system32/cmd.exe"
}

// wslHost makes te a WSL distribution whose Windows profile is a temporary
// directory (the drive mount); it returns the .wslconfig path. cmd.exe
// warns about the UNC working directory, as it does from a Linux path.
//
// stagent takes what wslpath prints for a Linux path ("/mnt/c/Users/…")
// and rejects anything else; a Windows test host has no such path to the
// temporary directory, so the tests that use the file skip there.
func wslHost(t *testing.T, te *testEnv) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("emulating WSL's .wslconfig needs a Linux path to the profile (wslpath -u), which a Windows host cannot give")
	}
	profile := filepath.Join(t.TempDir(), "Users", "obuto")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	wslDistro(te)
	te.run.respond = func(name string, args []string) (string, error) {
		switch {
		case name == "/mnt/c/WINDOWS/system32/cmd.exe":
			return "'\\\\wsl.localhost\\Ubuntu\\home\\obuto'\r\nCMD.EXE was started with the above path as the current directory.\r\nUNC paths are not supported.  Defaulting to Windows directory.\r\nC:\\Users\\obuto\r\n", nil
		case name == "wslpath" && strings.Join(args, " ") == `-u C:\Users\obuto`:
			return profile + "\n", nil
		}
		return "", errors.New("unexpected command " + name)
	}
	return filepath.Join(profile, ".wslconfig")
}

func wslChange(r *IntegrateResult) *Change {
	for _, c := range r.Changes {
		if c.ID == wslKeepRunningID {
			return c
		}
	}
	return nil
}

func wslReportOf(t *testing.T, te *testEnv) *WSLReport {
	t.Helper()
	te.reload()
	te.wslConfig = nil
	w := te.doctor().Persistence.WSL
	if w == nil {
		t.Fatal("persistence.wsl is null on WSL")
	}
	return w
}

// --wsl-keep-running adds one line and keeps every other byte; removing
// it gives back the original file (deleted when stagent created it).
func TestWSLKeepRunningAddRemove(t *testing.T) {
	cases := []struct {
		name, before, after string
	}{
		{"no file", "", "[general]\r\ninstanceIdleTimeout=-1\r\n"},
		{"wsl2 only", "[wsl2]\r\nmemory=8GB\r\nnetworkingMode=mirrored\r\n",
			"[wsl2]\r\nmemory=8GB\r\nnetworkingMode=mirrored\r\n\r\n[general]\r\ninstanceIdleTimeout=-1\r\n"},
		{"general present", "[wsl2]\nmemory=8GB\n\n[general]\nautoProxy=false\n",
			"[wsl2]\nmemory=8GB\n\n[general]\ninstanceIdleTimeout=-1\nautoProxy=false\n"},
		{"no trailing newline", "[wsl2]\r\nmemory=8GB",
			"[wsl2]\r\nmemory=8GB\r\n\r\n[general]\r\ninstanceIdleTimeout=-1\r\n"},
		{"utf-8 bom", "\ufeff[general]\r\n", "\ufeff[general]\r\ninstanceIdleTimeout=-1\r\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t, "linux")
			cfg := wslHost(t, te)
			if tc.before != "" {
				writeFile(t, cfg, tc.before)
			}
			if w := wslReportOf(t, te); !*w.KeepRunningNeeded || *w.InstanceIdleTimeout != wslDefaultIdleTimeout || w.KeepRunningByStagent {
				t.Fatalf("before: %+v", w)
			}

			r := te.integrate(t, integrateOpts{apply: true, wslKeepRunning: true})
			c := wslChange(r)
			if c == nil || c.Target != cfg || !r.Applied || len(notesWith(r.Notes, noteWSLRestart)) != 1 {
				t.Fatalf("apply: %+v", r)
			}
			if got := readFile(t, cfg); got != tc.after {
				t.Fatalf("after apply:\n%q\nwant\n%q", got, tc.after)
			}
			w := wslReportOf(t, te)
			if *w.KeepRunningNeeded || *w.InstanceIdleTimeout != -1 || !w.KeepRunningByStagent || *w.ConfigPath != cfg || w.Distro != "Ubuntu" {
				t.Fatalf("after apply: %+v", w)
			}
			if r := te.integrate(t, integrateOpts{wslKeepRunning: true}); wslChange(r) != nil {
				t.Fatalf("set already: %+v", r.Changes)
			}

			te.integrate(t, integrateOpts{apply: true, remove: []string{wslKeepRunningID}})
			if tc.before == "" {
				if exists(cfg) {
					t.Fatalf("created .wslconfig left: %q", readFile(t, cfg))
				}
			} else if got := readFile(t, cfg); got != tc.before {
				t.Fatalf("after remove:\n%q\nwant\n%q", got, tc.before)
			}
			te.reload()
			if te.wslRecord() != nil || len(te.m.Backups) != 0 {
				t.Fatalf("record or backup left: %+v %v", te.wslRecord(), te.m.Backups)
			}
			if left, _ := filepath.Glob(cfg + ".sshterm-bak-*"); len(left) != 0 {
				t.Fatalf("backup files left: %v", left)
			}
		})
	}
}

// Edited by the user after stagent wrote it: only stagent's line (and the
// [general] it added, now empty) goes; a value stagent changed gets its
// previous line back.
func TestWSLKeepRunningRemoveAfterUserEdit(t *testing.T) {
	te := newTestEnv(t, "linux")
	cfg := wslHost(t, te)
	writeFile(t, cfg, "[wsl2]\r\nmemory=8GB\r\n")
	te.integrate(t, integrateOpts{apply: true, wslKeepRunning: true})
	writeFile(t, cfg, readFile(t, cfg)+"\r\n[experimental]\r\nsparseVhd=true\r\n")
	te.integrate(t, integrateOpts{apply: true, remove: []string{wslKeepRunningID}})
	if got, want := readFile(t, cfg), "[wsl2]\r\nmemory=8GB\r\n\r\n[experimental]\r\nsparseVhd=true\r\n"; got != want {
		t.Fatalf("added: got\n%q\nwant\n%q", got, want)
	}

	writeFile(t, cfg, "[general]\r\ninstanceIdleTimeout = 60000\r\n")
	te.integrate(t, integrateOpts{apply: true, wslKeepRunning: true})
	if got := readFile(t, cfg); got != "[general]\r\ninstanceIdleTimeout=-1\r\n" {
		t.Fatalf("changed: %q", got)
	}
	writeFile(t, cfg, "# mine\r\n"+readFile(t, cfg))
	te.integrate(t, integrateOpts{apply: true, remove: []string{wslKeepRunningID}})
	if got := readFile(t, cfg); got != "# mine\r\n[general]\r\ninstanceIdleTimeout = 60000\r\n" {
		t.Fatalf("changed back: %q", got)
	}

	// The user set another value since: nothing of stagent's is left.
	te.integrate(t, integrateOpts{apply: true, wslKeepRunning: true})
	writeFile(t, cfg, "[general]\r\ninstanceIdleTimeout=30000\r\n")
	if w := wslReportOf(t, te); w.KeepRunningByStagent || !*w.KeepRunningNeeded {
		t.Fatalf("user's value: %+v", w)
	}
	if r := te.integrate(t, integrateOpts{apply: true, remove: []string{wslKeepRunningID}}); wslChange(r) != nil || readFile(t, cfg) != "[general]\r\ninstanceIdleTimeout=30000\r\n" {
		t.Fatalf("user's value touched: %+v %q", r.Changes, readFile(t, cfg))
	}
}

// A line that was there before stagent: no change, no record, and neither
// --remove nor purge touches the file.
func TestWSLKeepRunningUsersLine(t *testing.T) {
	te := newTestEnv(t, "linux")
	cfg := wslHost(t, te)
	const mine = "[general]\ninstanceIdleTimeout=-1\n"
	writeFile(t, cfg, mine)
	if r := te.integrate(t, integrateOpts{apply: true, wslKeepRunning: true}); wslChange(r) != nil || len(notesWith(r.Notes, noteWSLRestart)) != 0 {
		t.Fatalf("set before: %+v", r)
	}
	if w := wslReportOf(t, te); w.KeepRunningByStagent || *w.KeepRunningNeeded {
		t.Fatalf("set before: %+v", w)
	}
	if r := te.integrate(t, integrateOpts{apply: true, remove: []string{wslKeepRunningID}}); wslChange(r) != nil {
		t.Fatalf("remove: %+v", r.Changes)
	}
	te.reload()
	te.uninstall("purge", false, false)
	if got := readFile(t, cfg); got != mine {
		t.Fatalf("user's line touched: %q", got)
	}
}

// uninstall --level purge undoes stagent's edit without being asked;
// unhook leaves it.
func TestUninstallPurgeRevertsWSL(t *testing.T) {
	te := newTestEnv(t, "linux")
	cfg := wslHost(t, te)
	const before = "[wsl2]\r\nmemory=8GB\r\n"
	writeFile(t, cfg, before)
	te.integrate(t, integrateOpts{apply: true, wslKeepRunning: true})

	te.reload()
	te.uninstall("unhook", false, false)
	if readFile(t, cfg) == before {
		t.Fatal("unhook undid WSL を動かし続ける")
	}
	te.reload()
	r := te.uninstall("purge", false, false)
	if got := readFile(t, cfg); got != before {
		t.Fatalf("after purge: %q (steps %+v)", got, r.Steps)
	}
	for _, f := range r.Remaining {
		if f.Target == cfg {
			t.Fatalf("still listed: %+v", r.Remaining)
		}
	}
	if left, _ := filepath.Glob(cfg + ".sshterm-bak-*"); len(left) != 0 {
		t.Fatalf("backup left: %v", left)
	}
}

// doctor on WSL: the file's values with WSL's defaults, a problem while WSL
// stops when idle, and no lingering needed.
func TestDoctorWSL(t *testing.T) {
	te := newTestEnv(t, "linux")
	cfg := wslHost(t, te)
	te.run.respond = func(next func(string, []string) (string, error)) func(string, []string) (string, error) {
		return func(name string, args []string) (string, error) {
			if name == "busctl" || name == "loginctl" {
				return "", errors.New("no logind")
			}
			return next(name, args)
		}
	}(te.run.respond)

	d := te.doctor()
	w := d.Persistence.WSL
	if w == nil || *w.InstanceIdleTimeout != wslDefaultIdleTimeout || *w.NetworkingMode != "nat" || !*w.KeepRunningNeeded {
		t.Fatalf("no file: %+v", w)
	}
	if !slicesHasPrefix(d.Problems, "WSL shuts this distribution down 15000 ms") {
		t.Fatalf("problems: %v", d.Problems)
	}
	if p := d.Persistence; p.LingerNeeded == nil || *p.LingerNeeded || p.LingerReason != nil {
		t.Fatalf("linger on WSL: %+v", p)
	}

	writeFile(t, cfg, "[wsl2]\nnetworkingMode=Mirrored\n[general]\ninstanceIdleTimeout=-1\n")
	te.wslConfig = nil
	d = te.doctor()
	if w := d.Persistence.WSL; *w.InstanceIdleTimeout != -1 || *w.NetworkingMode != "mirrored" || *w.KeepRunningNeeded {
		t.Fatalf("set: %+v", w)
	}
	if slicesHasPrefix(d.Problems, "WSL shuts") {
		t.Fatalf("problems: %v", d.Problems)
	}

	// UTF-16 cannot be read: unknown, and integrate leaves it alone.
	writeFile(t, cfg, "\xff\xfe[\x00")
	te.wslConfig = nil
	if w := te.doctor().Persistence.WSL; w.InstanceIdleTimeout != nil || w.KeepRunningNeeded != nil || w.ConfigPath == nil {
		t.Fatalf("utf-16: %+v", w)
	}
	te.reload()
	r, _ := te.env.integrate(integrateOpts{wslKeepRunning: true})
	if c := wslChange(r); c == nil || c.Action != "skip" || c.ErrorCode != codeUnmanagedFile {
		t.Fatalf("utf-16 plan: %+v", r.Changes)
	}
}

// Without interop the profile cannot be found: wslconfig_unreachable, and
// doctor says nothing about the file. Off WSL there is no WSL report and
// the change is a skip.
func TestWSLKeepRunningUnreachable(t *testing.T) {
	te := newTestEnv(t, "linux")
	wslDistro(te)
	te.run.respond = func(string, []string) (string, error) {
		return "", errors.New("exec format error")
	}
	r, err := te.env.integrate(integrateOpts{wslKeepRunning: true})
	if err != nil {
		t.Fatal(err)
	}
	if c := wslChange(r); c == nil || c.Action != "skip" || c.ErrorCode != codeWSLConfigUnreachable || r.Result != resultFailed {
		t.Fatalf("plan: %+v", r)
	}
	if w := wslReportOf(t, te); w.ConfigPath != nil || w.KeepRunningNeeded != nil || w.Distro != "Ubuntu" {
		t.Fatalf("doctor: %+v", w)
	}

	plain := newTestEnv(t, "linux")
	plain.kernelRelease = func() string { return "6.8.0-45-generic\n" }
	if w := plain.doctor().Persistence.WSL; w != nil {
		t.Fatalf("not WSL: %+v", w)
	}
	r, _ = plain.env.integrate(integrateOpts{wslKeepRunning: true})
	if c := wslChange(r); c == nil || c.Action != "skip" {
		t.Fatalf("not WSL: %+v", r.Changes)
	}
}

// wsl-shutdown runs wsl.exe --shutdown, reporting its (UTF-16) message
// when it fails; off WSL it refuses.
func TestWSLShutdown(t *testing.T) {
	te := newTestEnv(t, "linux")
	wslDistro(te)
	te.bins["wsl.exe"] = "/mnt/c/WINDOWS/system32/wsl.exe"
	te.run.respond = func(name string, args []string) (string, error) {
		return "", nil
	}
	if err := te.wslShutdown(); err != nil || !te.run.ran("/mnt/c/WINDOWS/system32/wsl.exe --shutdown") {
		t.Fatalf("err %v, calls %v", err, te.run.calls)
	}
	te.run.respond = func(name string, args []string) (string, error) {
		return "a\x00c\x00c\x00e\x00s\x00s\x00 \x00d\x00e\x00n\x00i\x00e\x00d\x00\r\x00\n\x00", errors.New("exit status 1")
	}
	if err := te.wslShutdown(); err == nil || err.Error() != "wsl.exe --shutdown: access denied" {
		t.Fatalf("failure: %v", err)
	}
	if err := newTestEnv(t, "linux").wslShutdown(); err == nil {
		t.Fatal("not WSL: no error")
	}
}

func slicesHasPrefix(list []string, prefix string) bool {
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}
