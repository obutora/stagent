package install

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/creack/pty"

	"github.com/obutora/stagent/internal/paths"
)

func TestShellWrapperBlockAddRemoveIdempotent(t *testing.T) {
	te := newTestEnv(t, "linux")
	te.vars["SHELL"] = "/bin/zsh"
	bashrc := te.home(".bashrc")
	const orig = "export PATH=$HOME/bin:$PATH\nalias ll='ls -l'\n"
	writeFile(t, bashrc, orig)

	r := te.integrate(t, integrateOpts{apply: true, shellWrapper: true})
	// bash: rc exists; zsh: login shell (rc created); fish: not used.
	if got := strings.Join(changeIDs(r.Changes), ","); got != "shell-bash,shell-zsh" {
		t.Fatalf("changes = %s", got)
	}
	b := readFile(t, bashrc)
	if !strings.HasPrefix(b, orig+"\n"+blockBegin+"\n") || !strings.HasSuffix(b, blockEnd+"\n") {
		t.Fatalf("bashrc block not appended after a blank line:\n%s", b)
	}
	if strings.Count(b, blockBegin) != 1 {
		t.Fatal("block duplicated")
	}
	if r := te.integrate(t, integrateOpts{apply: true, shellWrapper: true}); len(r.Changes) != 0 {
		t.Fatalf("second apply changed %v", changeIDs(r.Changes))
	}

	// A user line after our block survives removal; the block and its
	// separator go.
	writeFile(t, bashrc, b+"export EDITOR=vim\n")
	te.integrate(t, integrateOpts{apply: true, remove: []string{"shell-wrapper"}})
	if got := readFile(t, bashrc); got != orig+"export EDITOR=vim\n" {
		t.Fatalf("bashrc after remove = %q", got)
	}
	if exists(te.home(".zshrc")) {
		t.Fatal(".zshrc created by stagent was not deleted")
	}
	if r := te.integrate(t, integrateOpts{apply: true, remove: []string{"shell-wrapper"}}); len(r.Changes) != 0 {
		t.Fatalf("second remove changed %v", changeIDs(r.Changes))
	}
}

// A login bash (macOS Terminal) reads ~/.bash_profile, not ~/.bashrc: when
// the profile exists and bash is wrapped, it gets the block too, and
// doctor, removal and uninstall cover it.
func TestShellWrapperBashProfile(t *testing.T) {
	te := newTestEnv(t, "darwin")
	te.vars["SHELL"] = "/bin/zsh"
	profile := te.home(".bash_profile")
	const orig = "export PATH=/opt/homebrew/bin:$PATH\n"
	writeFile(t, profile, orig)

	// bash is not wrapped (no .bashrc, login shell zsh): the profile is
	// left alone.
	r := te.integrate(t, integrateOpts{shellWrapper: true})
	if got := strings.Join(changeIDs(r.Changes), ","); got != "shell-zsh" {
		t.Fatalf("without bash, changes = %s", got)
	}

	te.vars["SHELL"] = "/bin/bash"
	r = te.integrate(t, integrateOpts{apply: true, shellWrapper: true})
	if got := strings.Join(changeIDs(r.Changes), ","); got != "shell-bash,shell-bash-profile" || r.Result != resultOK {
		t.Fatalf("changes = %s, result %s", got, r.Result)
	}
	if got := readFile(t, profile); !strings.HasPrefix(got, orig+"\n"+blockBegin+"\n") || !strings.Contains(got, "run --handoff=auto --") {
		t.Fatalf(".bash_profile:\n%s", got)
	}
	te.reload()
	if d := te.doctor(); !slices.Contains(d.ShellWrapper.Files, profile) {
		t.Fatalf("doctor shell_wrapper.files = %v", d.ShellWrapper.Files)
	}
	te.integrate(t, integrateOpts{apply: true, remove: []string{"shell-wrapper"}})
	if got := readFile(t, profile); got != orig {
		t.Fatalf(".bash_profile after remove = %q", got)
	}

	te.integrate(t, integrateOpts{apply: true, shellWrapper: true})
	te.reload()
	te.uninstall("unhook", false, false)
	if got := readFile(t, profile); got != orig {
		t.Fatalf(".bash_profile after uninstall = %q", got)
	}
}

func TestShellWrapperFishFile(t *testing.T) {
	te := newTestEnv(t, "linux")
	te.vars["SHELL"] = "/usr/bin/fish"
	te.integrate(t, integrateOpts{apply: true, shellWrapper: true})
	fish := te.home(".config", "fish", "conf.d", "ssh-term-stagent.fish")
	if !strings.Contains(readFile(t, fish), "status is-interactive; and isatty stdin; and isatty stdout; and not set -q STAGENT_SESSION_ID") {
		t.Fatalf("fish wrapper:\n%s", readFile(t, fish))
	}
	te.integrate(t, integrateOpts{apply: true, remove: []string{"shell-wrapper"}})
	if exists(fish) {
		t.Fatal("fish file not deleted")
	}
}

// PowerShell profiles: the stagent path is built from $env:USERPROFILE so
// the block is ASCII; new profiles get no BOM and an existing BOM stays.
// Only a literal non-ASCII path (stagent outside %USERPROFILE%) in a file
// without BOM is warned about. UTF-16 profiles are skipped.
func TestShellWrapperPowerShellProfile(t *testing.T) {
	te := newTestEnv(t, "windows")
	t.Setenv(paths.EnvHome, filepath.Join(t.TempDir(), "ユーザー"))
	l, err := paths.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	te.l = l
	te.vars["USERPROFILE"] = l.Home
	te.bins["pwsh"] = `C:\Program Files\PowerShell\7\pwsh.exe`
	ps5 := filepath.Join(te.docs, "WindowsPowerShell", "Microsoft.PowerShell_profile.ps1")
	ps7 := filepath.Join(te.docs, "PowerShell", "Microsoft.PowerShell_profile.ps1")

	r := te.integrate(t, integrateOpts{apply: true, shellWrapper: true})
	if got := strings.Join(changeIDs(r.Changes), ","); got != "shell-powershell,shell-pwsh" {
		t.Fatalf("changes = %s", got)
	}
	got := readFile(t, ps5)
	if !strings.HasPrefix(got, blockBegin+"\r\n") || strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
		t.Fatalf("new PowerShell profile must be UTF-8 without BOM and CRLF: %q", got[:40])
	}
	if !isASCII(got) || !strings.Contains(got, `$bin = Join-Path $env:USERPROFILE '.ssh-term\agent\bin\`+filepath.Base(l.Bin)+`'`+"\r\n") {
		t.Fatalf("$bin is not built from USERPROFILE / profile not ASCII:\n%s", got)
	}
	if len(notesWith(r.Notes, notePowerShellNoBOM)) != 0 {
		t.Fatalf("unexpected encoding note: %v", r.Notes)
	}
	te.integrate(t, integrateOpts{apply: true, remove: []string{"shell-wrapper"}})

	// stagent outside %USERPROFILE%: the literal path. Its non-ASCII
	// characters are warned about for the profile without BOM only.
	te.vars["USERPROFILE"] = filepath.Join(t.TempDir(), "other")
	writeFile(t, ps7, "\xef\xbb\xbf# mine\r\n")
	r = te.integrate(t, integrateOpts{shellWrapper: true})
	bom := notesWith(r.Notes, notePowerShellNoBOM)
	if len(bom) != 1 || bom[0].Args["path"] != ps5 || bom[0].Args["bin"] != l.Bin || !strings.HasPrefix(bom[0].Text, ps5+" has no UTF-8 BOM") || !strings.Contains(bom[0].Text, "("+l.Bin+")") {
		t.Fatalf("notes = %+v", r.Notes)
	}
	for _, c := range r.Changes {
		if !strings.Contains(c.Diff, "$bin = "+psQuote(l.Bin)) {
			t.Fatalf("%s: literal $bin missing:\n%s", c.ID, c.Diff)
		}
	}
	te.integrate(t, integrateOpts{apply: true, shellWrapper: true})
	if got := readFile(t, ps7); !strings.HasPrefix(got, "\xef\xbb\xbf# mine\r\n\r\n"+blockBegin+"\r\n") {
		t.Fatalf("existing BOM profile = %q", got[:40])
	}

	// A UTF-16 profile is a skipped target with its own error code.
	writeFile(t, ps5, "\xff\xfe#\x00\r\x00\n\x00")
	te.reload()
	r, err = te.env.integrate(integrateOpts{shellWrapper: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Changes) != 1 || r.Changes[0].Action != "skip" || r.Changes[0].ErrorCode != codeUTF16Profile || r.Changes[0].Target != ps5 {
		t.Fatalf("UTF-16 profile: %+v", r.Changes)
	}
}

// wrapperCalls run in every shell after the wrapper is loaded; want is what
// the fake programs print. Non-interactive invocations reach the real
// command, interactive ones go through `stagent run --handoff=auto`.
var wrapperCalls = []struct{ call, want string }{
	{`claude a 'b c'`, "stagent run --handoff=auto -- claude a b c"},
	{`claude -p hi`, "real claude -p hi"},
	{`claude hi --print`, "real claude hi --print"},
	{`codex exec hi`, "real codex exec hi"},
	{`codex e hi`, "real codex e hi"},
	{`codex -p work`, "stagent run --handoff=auto -- codex -p work"},
	{`codex hi exec`, "stagent run --handoff=auto -- codex hi exec"},
	{`omp -p hi`, "real omp -p hi"},
	{`omp --print hi`, "real omp --print hi"},
	{`omp --mode json`, "real omp --mode json"},
	{`omp --mode=rpc hi`, "real omp --mode=rpc hi"},
	{`omp --mode text`, "stagent run --handoff=auto -- omp --mode text"},
	{`omp --mode=text hi`, "stagent run --handoff=auto -- omp --mode=text hi"},
	{`sh -c 'echo marker=$STAGENT_SHELL_WRAPPER'`, "marker=1"},
}

// fakePrograms puts a fake stagent and fake claude / codex / omp, which
// print how they were run, in a new directory.
func fakePrograms(t *testing.T) (dir, stagent string) {
	t.Helper()
	dir = t.TempDir()
	stagent = filepath.Join(dir, "stagent")
	writeFile(t, stagent, "#!/bin/sh\necho \"stagent $*\"\n")
	chmodX(t, stagent)
	for _, c := range wrappedCommands {
		p := filepath.Join(dir, c)
		writeFile(t, p, "#!/bin/sh\necho \"real "+c+" $*\"\n")
		chmodX(t, p)
	}
	return dir, stagent
}

// runOnTerminal runs argv on a new terminal and returns the lines the fake
// programs printed.
func runOnTerminal(t *testing.T, argv, env []string) []string {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	f, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out, _ := io.ReadAll(f) // EIO once the shell exits
	cmd.Wait()
	var lines []string
	for _, l := range strings.Split(strings.ReplaceAll(string(out), "\r", ""), "\n") {
		for _, p := range []string{"stagent run ", "real ", "marker="} {
			if i := strings.Index(l, p); i >= 0 {
				lines = append(lines, strings.TrimSpace(l[i:]))
				break
			}
		}
	}
	return lines
}

func TestWrapperBehaviourInEveryShell(t *testing.T) {
	var calls, want []string
	for _, c := range wrapperCalls {
		calls = append(calls, c.call)
		want = append(want, c.want)
	}
	script := strings.Join(calls, "\n")
	for _, sh := range []struct {
		name string
		rc   func(stagent, dir string) string // wrapper file content
		argv func(bin, rc string) []string
	}{
		{"bash", func(s, _ string) string { return posixBlock(s) }, func(bin, rc string) []string {
			return []string{bin, "--norc", "--noprofile", "-i", "-c", ". " + shellQuote(rc) + "\n" + script}
		}},
		{"zsh", func(s, _ string) string { return posixBlock(s) }, func(bin, rc string) []string {
			return []string{bin, "-f", "-i", "-c", ". " + shellQuote(rc) + "\n" + script}
		}},
		{"fish", func(s, _ string) string { return fishFile(s) }, func(bin, rc string) []string {
			return []string{bin, "--no-config", "-i", "-c", "source " + fishQuote(rc) + "\n" + script}
		}},
		{"pwsh", func(s, dir string) string { return powershellBlock(psBinExpr(s, dir)) }, func(bin, rc string) []string {
			return []string{bin, "-NoLogo", "-NoProfile", "-Command", ". " + psQuote(rc) + "\n" + script}
		}},
	} {
		t.Run(sh.name, func(t *testing.T) {
			bin, err := exec.LookPath(sh.name)
			if err != nil {
				t.Skip(sh.name + " not available")
			}
			dir, stagent := fakePrograms(t)
			block := sh.rc(stagent, dir)
			if !strings.Contains(block, "run --handoff=auto --") || !strings.Contains(block, "STAGENT_SHELL_WRAPPER") {
				t.Fatalf("block lacks --handoff=auto or the marker:\n%s", block)
			}
			rc := filepath.Join(dir, "rc")
			if sh.name == "pwsh" {
				rc += ".ps1" // PowerShell dot-sources scripts only
			}
			writeFile(t, rc, block)
			env := []string{"PATH=" + dir + ":/usr/bin:/bin", "HOME=" + dir, "USERPROFILE=" + dir, "TERM=dumb"}
			got := runOnTerminal(t, sh.argv(bin, rc), env)
			if !slices.Equal(got, want) {
				t.Errorf("printed\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
			}
		})
	}
}

// The wrapper runs the program under stagent only in interactive shells
// with a terminal, outside a stagent session, when the binary exists; the
// handoff choice is left to `stagent run --handoff=auto`.
func TestPosixWrapperBehaviour(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	dir, fake := fakePrograms(t)
	rc := filepath.Join(dir, "rc")
	writeFile(t, rc, posixBlock(fake))

	run := func(interactive, tty bool, env ...string) string {
		args := []string{"--norc", "--noprofile"}
		if interactive {
			args = append(args, "-i")
		}
		args = append(args, "-c", ". "+shellQuote(rc)+" && claude a 'b c'")
		env = append([]string{"PATH=" + dir + ":/usr/bin:/bin", "HOME=" + dir, "TERM=dumb"}, env...)
		if !tty {
			cmd := exec.Command(bash, args...)
			cmd.Env = env
			out, _ := cmd.Output()
			return strings.TrimSpace(string(out))
		}
		return strings.Join(runOnTerminal(t, append([]string{bash}, args...), env), "\n")
	}
	if got := run(true, true); got != "stagent run --handoff=auto -- claude a b c" {
		t.Errorf("interactive terminal: %q", got)
	}
	if got := run(true, false); got != "real claude a b c" {
		t.Errorf("interactive, output piped: %q", got)
	}
	if got := run(false, false); got != "real claude a b c" {
		t.Errorf("non-interactive: %q", got)
	}
	if got := run(true, true, "STAGENT_SESSION_ID=0123456789abcdef"); got != "real claude a b c" {
		t.Errorf("nested: %q", got)
	}
	if got := run(true, true, "STAGENT_HANDOFF=0"); got != "stagent run --handoff=auto -- claude a b c" {
		t.Errorf("STAGENT_HANDOFF=0 (decided by stagent run): %q", got)
	}
	os.Remove(fake)
	if got := run(true, true); got != "real claude a b c" {
		t.Errorf("binary missing: %q", got)
	}
}
