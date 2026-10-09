package install

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// With the profile's shell wrapper, the prompt moves the process's working
// directory to where Set-Location went (the GitHub screen reads it from
// the PEB), and the prompt the profile had still draws.
func TestPowerShellWrapperFollowsSetLocation(t *testing.T) {
	var bin string
	for _, name := range []string{"powershell", "pwsh"} {
		if p, err := exec.LookPath(name); err == nil {
			bin = p
			break
		}
	}
	if bin == "" {
		t.Skip("no PowerShell")
	}
	dir := t.TempDir()
	rc := filepath.Join(dir, "profile.ps1")
	writeFile(t, rc, "function global:prompt { 'MINE> ' }\n"+powershellBlock(psQuote(filepath.Join(dir, "stagent"))))
	target, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	script := ". " + psQuote(rc) + "\n" +
		"Set-Location -LiteralPath " + psQuote(target) + "\n" +
		"'before=' + [Environment]::CurrentDirectory\n" +
		"'prompt=' + (prompt)\n" +
		"'after=' + [Environment]::CurrentDirectory\n"
	out, err := exec.Command(bin, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := map[string]string{}
	for _, l := range strings.Split(strings.ReplaceAll(string(out), "\r", ""), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			got[k] = v
		}
	}
	same := func(a, b string) bool { return strings.EqualFold(filepath.Clean(a), filepath.Clean(b)) }
	if same(got["before"], target) {
		t.Fatalf("Set-Location alone moved the working directory; the test proves nothing:\n%s", out)
	}
	if got["prompt"] != "MINE> " || !same(got["after"], target) {
		t.Fatalf("prompt %q, working directory after it %q, want %q:\n%s", got["prompt"], got["after"], target, out)
	}
}
