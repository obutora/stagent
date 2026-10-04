package install

import (
	"errors"
	"strings"
	"testing"
)

// policyHost fakes the installed PowerShells: policy[exe] is the effective
// policy, set for the current user, or by Group Policy when gpo[exe] is
// set; Set-ExecutionPolicy makes the user's RemoteSigned, which Group
// Policy overrides.
func policyHost(te *testEnv, policy map[string]string, gpo map[string]bool) {
	te.bins["powershell"] = `C:\WINDOWS\System32\WindowsPowerShell\v1.0\powershell.exe`
	te.run.respond = func(name string, args []string) (string, error) {
		cmd := args[len(args)-1]
		switch {
		case cmd == policyScript:
			p, ok := policy[name]
			if !ok {
				return "", errors.New("exit status 1")
			}
			if gpo[name] {
				return p + " MachinePolicy\r\n", nil
			}
			return p + " CurrentUser\r\n", nil
		case strings.HasPrefix(cmd, "Set-ExecutionPolicy -Scope CurrentUser"):
			if !gpo[name] {
				policy[name] = "RemoteSigned"
			}
			return "", nil
		}
		return "", nil
	}
}

// doctor lists each installed PowerShell with its effective policy; one
// that does not run its profile is a problem.
func TestDoctorPowerShellPolicies(t *testing.T) {
	te := newTestEnv(t, "windows")
	ps5 := `C:\WINDOWS\System32\WindowsPowerShell\v1.0\powershell.exe`
	policyHost(te, map[string]string{ps5: "Restricted"}, nil)
	guard := true
	te.redirectionGuard = func() *bool { return &guard }

	r := te.doctor()
	if len(r.PowerShell) != 1 {
		t.Fatalf("powershell = %+v", r.PowerShell)
	}
	p := r.PowerShell[0]
	if p.ID != "powershell" || p.Path != ps5 || p.ExecutionPolicy == nil || *p.ExecutionPolicy != "Restricted" || p.LoadsProfile == nil || *p.LoadsProfile {
		t.Fatalf("powershell 5.1 = %+v", p)
	}
	if r.RedirectionGuard == nil || !*r.RedirectionGuard {
		t.Fatalf("redirection_guard = %v", r.RedirectionGuard)
	}
	if !strings.Contains(strings.Join(r.Problems, "\n"), "Windows PowerShell 5.1 does not run its profile (execution policy Restricted)") {
		t.Fatalf("problems = %v", r.Problems)
	}

	// pwsh too; an unreadable policy is unknown, not a problem.
	te.bins["pwsh"] = `C:\Program Files\PowerShell\7\pwsh.exe`
	te.reload()
	r = te.doctor()
	if len(r.PowerShell) != 2 || r.PowerShell[1].ID != "pwsh" || r.PowerShell[1].ExecutionPolicy != nil || r.PowerShell[1].LoadsProfile != nil {
		t.Fatalf("powershell = %+v", r.PowerShell)
	}

	// Elsewhere: null.
	if r := newTestEnv(t, "linux").doctor(); r.PowerShell != nil || r.RedirectionGuard != nil {
		t.Fatalf("linux: powershell %v, redirection_guard %v", r.PowerShell, r.RedirectionGuard)
	}
}

// integrate --execution-policy sets RemoteSigned for the user only in the
// PowerShells that do not run the profile; Group Policy keeping the policy
// is execution_policy_overridden, naming the scope.
func TestIntegrateExecutionPolicy(t *testing.T) {
	te := newTestEnv(t, "windows")
	ps5 := `C:\WINDOWS\System32\WindowsPowerShell\v1.0\powershell.exe`
	ps7 := `C:\Program Files\PowerShell\7\pwsh.exe`
	te.bins["pwsh"] = ps7
	policy := map[string]string{ps5: "AllSigned", ps7: "RemoteSigned"}
	gpo := map[string]bool{}
	policyHost(te, policy, gpo)

	plan := te.integrate(t, integrateOpts{executionPolicy: true})
	if len(plan.Changes) != 1 || plan.Changes[0].ID != "execution-policy-powershell" || plan.Changes[0].Target != "powershell:ExecutionPolicy" || plan.Changes[0].Action != "modify" || policy[ps5] != "AllSigned" {
		t.Fatalf("plan: %+v", plan.Changes)
	}
	te.reload()
	r := te.integrate(t, integrateOpts{apply: true, executionPolicy: true})
	if !r.Applied || r.Result != resultOK || policy[ps5] != "RemoteSigned" || policy[ps7] != "RemoteSigned" || te.run.ran(ps7+" -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command Set-ExecutionPolicy") {
		t.Fatalf("apply: %+v, calls %v", r, te.run.calls)
	}
	te.reload()
	if r := te.integrate(t, integrateOpts{executionPolicy: true}); len(r.Changes) != 0 || r.Result != resultOK {
		t.Fatalf("nothing left to do: %+v", r)
	}

	// Group Policy defines Restricted: the change fails with its words.
	policy[ps5], gpo[ps5] = "Restricted", true
	te.reload()
	r, err := te.env.integrate(integrateOpts{apply: true, executionPolicy: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Changes) != 1 || r.Result != resultFailed || r.Changes[0].ErrorCode != codeExecutionPolicyOverridden ||
		!strings.Contains(r.Changes[0].Error, "stays Restricted, set by the MachinePolicy scope (Group Policy)") {
		t.Fatalf("group policy: %+v", r.Changes)
	}

	if r, _ = newTestEnv(t, "linux").env.integrate(integrateOpts{executionPolicy: true}); len(r.Changes) != 1 || r.Changes[0].Action != "skip" || r.Result != resultFailed {
		t.Fatalf("linux: %+v", r.Changes)
	}
}

// With cmd as the SSH default shell the wrapper goes into the PowerShell
// profiles only, and integrate says so.
func TestShellWrapperCmdDefaultShell(t *testing.T) {
	te := newTestEnv(t, "windows")
	te.vars["SHELL"] = `c:\windows\system32\cmd.exe`
	te.vars["USERPROFILE"] = te.l.Home
	r := te.integrate(t, integrateOpts{shellWrapper: true})
	if got := strings.Join(changeIDs(r.Changes), ","); got != "shell-powershell" {
		t.Fatalf("changes = %s", got)
	}
	if n := notesWith(r.Notes, noteCmdNotWrapped); len(n) != 1 || n[0].Args["shell"] != "cmd.exe" {
		t.Fatalf("notes = %+v (login shell %q)", r.Notes, r.LoginShell)
	}
	te.vars["SHELL"] = `c:\windows\system32\windowspowershell\v1.0\powershell.exe`
	te.reload()
	if r := te.integrate(t, integrateOpts{shellWrapper: true}); len(notesWith(r.Notes, noteCmdNotWrapped)) != 0 {
		t.Fatalf("PowerShell default shell: %+v", r.Notes)
	}
}
