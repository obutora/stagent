package install

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/creack/pty"
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

func TestShellWrapperFishFileAndPowerShellProfile(t *testing.T) {
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

	w := newTestEnv(t, "windows")
	w.bins["pwsh"] = `C:\Program Files\PowerShell\7\pwsh.exe`
	r := w.integrate(t, integrateOpts{apply: true, shellWrapper: true})
	if got := strings.Join(changeIDs(r.Changes), ","); got != "shell-powershell,shell-pwsh" {
		t.Fatalf("changes = %s", got)
	}
	p := filepath.Join(w.docs, "WindowsPowerShell", "Microsoft.PowerShell_profile.ps1")
	got := readFile(t, p)
	if !strings.HasPrefix(got, "\xef\xbb\xbf"+blockBegin+"\r\n") || strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
		t.Fatalf("new PowerShell profile must be UTF-8 with BOM and CRLF: %q", got[:40])
	}
}

// The wrapper really runs the program under stagent only in interactive
// shells with a terminal, outside a stagent session.
func TestPosixWrapperBehaviour(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "stagent")
	// The fake stagent and the fake claude print what ran.
	writeFile(t, fake, "#!/bin/sh\necho \"stagent $*\"\n")
	os.Chmod(fake, 0o755)
	writeFile(t, filepath.Join(dir, "claude"), "#!/bin/sh\necho \"real claude $*\"\n")
	os.Chmod(filepath.Join(dir, "claude"), 0o755)
	rc := filepath.Join(dir, "rc")
	writeFile(t, rc, posixBlock(fake))

	run := func(interactive, tty bool, env ...string) string {
		args := []string{"--norc", "--noprofile"}
		if interactive {
			args = append(args, "-i")
		}
		args = append(args, "-c", ". "+shellQuote(rc)+" && claude a 'b c'")
		cmd := exec.Command(bash, args...)
		cmd.Env = append([]string{"PATH=" + dir + ":/usr/bin:/bin", "HOME=" + dir, "TERM=dumb"}, env...)
		if !tty {
			out, _ := cmd.Output()
			return strings.TrimSpace(string(out))
		}
		f, err := pty.Start(cmd)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		out, _ := io.ReadAll(f) // EIO once the shell exits
		cmd.Wait()
		lines := strings.Split(strings.ReplaceAll(string(out), "\r", ""), "\n")
		for _, l := range lines {
			if strings.Contains(l, "claude a b c") {
				return strings.TrimSpace(l)
			}
		}
		return string(out)
	}
	if got := run(true, true); got != "stagent run -- claude a b c" {
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
	os.Remove(fake)
	if got := run(true, true); got != "real claude a b c" {
		t.Errorf("binary missing: %q", got)
	}
}
