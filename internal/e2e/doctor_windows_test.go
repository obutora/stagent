package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/obutora/stagent/internal/paths"
)

// doctorPolicy runs `stagent doctor --json` with env and returns the
// execution policy it reports for Windows PowerShell 5.1.
func doctorPolicy(t *testing.T, bin string, env []string) string {
	t.Helper()
	cmd := exec.Command(bin, "doctor", "--json")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, out)
	}
	var r struct {
		PowerShell []struct {
			ID              string  `json:"id"`
			ExecutionPolicy *string `json:"execution_policy"`
		} `json:"powershell"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("doctor output: %v\n%s", err, out)
	}
	for _, p := range r.PowerShell {
		if p.ID == "powershell" && p.ExecutionPolicy != nil {
			return *p.ExecutionPolicy
		}
	}
	t.Fatalf("no Windows PowerShell policy in %s", out)
	return ""
}

// A stagent started by PowerShell 7 (sshd's default shell pwsh) inherits
// its PSModulePath, where Windows PowerShell finds PowerShell 7's modules
// first and fails to load Get-ExecutionPolicy. doctor reads the same policy
// as without that path. The decoy module stands in for PowerShell 7's.
func TestWindowsDoctorPolicyUnderPwshModulePath(t *testing.T) {
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		t.Skip("Windows PowerShell not installed")
	}
	bin := compileStagent(t)
	env := []string{paths.EnvHome + "=" + t.TempDir()}
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !strings.EqualFold(k, "PSModulePath") && !strings.EqualFold(k, paths.EnvHome) {
			env = append(env, kv)
		}
	}
	want := doctorPolicy(t, bin, env)

	mods := t.TempDir()
	decoy := filepath.Join(mods, "Microsoft.PowerShell.Security")
	if err := os.MkdirAll(decoy, 0o755); err != nil {
		t.Fatal(err)
	}
	// Like PowerShell 7's own manifest, it exports Get-ExecutionPolicy and
	// names a types file Windows PowerShell cannot load (PowerShell 7's
	// needs its DLL, which sits next to pwsh.exe).
	manifest := "@{ ModuleVersion = '7.0.0.0'; CmdletsToExport = 'Get-ExecutionPolicy', 'Set-ExecutionPolicy'; TypesToProcess = 'Security.types.ps1xml' }\r\n"
	if err := os.WriteFile(filepath.Join(decoy, "Microsoft.PowerShell.Security.psd1"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	polluted := append(env[:len(env):len(env)], "PSModulePath="+mods+`;C:\WINDOWS\system32\WindowsPowerShell\v1.0\Modules`)
	if got := doctorPolicy(t, bin, polluted); got != want {
		t.Fatalf("doctor under PowerShell 7's module path: policy %q, want %q", got, want)
	}
}
