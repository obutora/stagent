package install

import (
	"errors"
	"strings"
)

// PowerShell's execution policy decides whether it runs the profile that
// holds the shell wrapper: under Restricted (the default of Windows client
// editions) it runs no script, under AllSigned only signed ones, and the
// wrapper block is not signed. `doctor` reports the effective policy of
// each installed PowerShell; `integrate --execution-policy` sets
// RemoteSigned for the current user in those that would not load it. A
// policy fixed by Group Policy wins over that scope: the change then fails
// with PowerShell's own message.

// PowerShellReport is one installed PowerShell in `doctor` (Windows).
type PowerShellReport struct {
	ID   string `json:"id"`   // powershell | pwsh
	Name string `json:"name"` // Windows PowerShell 5.1 | PowerShell 7
	Path string `json:"path"`
	// ExecutionPolicy is the effective policy (Get-ExecutionPolicy); null
	// when it could not be read.
	ExecutionPolicy *string `json:"execution_policy"`
	// LoadsProfile is false when that policy keeps the profile's wrapper
	// from running (Restricted, AllSigned); null when unknown.
	LoadsProfile *bool `json:"loads_profile"`
}

// psShell is an installed PowerShell.
type psShell struct{ id, name, exe string }

var psShells = []psShell{
	{"powershell", "Windows PowerShell 5.1", "powershell"},
	{"pwsh", "PowerShell 7", "pwsh"},
}

// executionPolicies are the values Get-ExecutionPolicy prints.
var executionPolicies = []string{"Restricted", "AllSigned", "RemoteSigned", "Unrestricted", "Bypass", "Undefined", "Default"}

// policyLoadsProfile reports whether a profile with the unsigned wrapper
// block runs under policy.
func policyLoadsProfile(policy string) bool {
	return policy != "Restricted" && policy != "AllSigned"
}

// powerShells reports every installed PowerShell with its effective
// execution policy; nil off Windows. Cached for the command run.
func (e *env) powerShells() []PowerShellReport {
	if e.goos != "windows" {
		return nil
	}
	if e.ps != nil {
		return *e.ps
	}
	out := []PowerShellReport{}
	for _, s := range psShells {
		path, err := e.lookPath(s.exe)
		if err != nil {
			continue
		}
		r := PowerShellReport{ID: s.id, Name: s.name, Path: path}
		if p, _, ok := e.executionPolicy(path); ok {
			loads := policyLoadsProfile(p)
			r.ExecutionPolicy, r.LoadsProfile = &p, &loads
		}
		out = append(out, r)
	}
	e.ps = &out
	return out
}

// policyScript prints the effective execution policy and the scope that
// sets it, as Get-ExecutionPolicy decides without a process scope: the
// first defined of MachinePolicy, UserPolicy (Group Policy), CurrentUser,
// LocalMachine, else the OS default (Restricted on Windows client
// editions). It runs with -ExecutionPolicy Bypass because PowerShell 7
// under Restricted cannot even load the module of Get-ExecutionPolicy (its
// .ps1xml type file counts as a script).
const policyScript = `$l = Get-ExecutionPolicy -List
foreach ($s in 'MachinePolicy','UserPolicy','CurrentUser','LocalMachine') { $v = "$(($l | Where-Object Scope -eq $s).ExecutionPolicy)"; if ($v -and $v -ne 'Undefined') { "$v $s"; exit } }
if ((Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\ProductOptions').ProductType -eq 'WinNT') { 'Restricted Default' } else { 'RemoteSigned Default' }`

// executionPolicy reads the effective execution policy of the PowerShell
// at exe and the scope that sets it (policyScript).
func (e *env) executionPolicy(exe string) (policy, scope string, ok bool) {
	out, err := e.run.Run(cmdTimeout, exe, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", policyScript)
	if err != nil {
		return "", "", false
	}
	return parsePolicy(out)
}

// parsePolicy reads policyScript's output: the policy and its scope on the
// last non-empty line.
func parsePolicy(out string) (policy, scope string, ok bool) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	f := strings.Fields(lines[len(lines)-1])
	if len(f) != 2 {
		return "", "", false
	}
	for _, p := range executionPolicies {
		if strings.EqualFold(f[0], p) {
			return p, f[1], true
		}
	}
	return "", "", false
}

// executionPolicyChanges plans RemoteSigned for the current user in every
// PowerShell whose effective policy does not run the profile.
func (e *env) executionPolicyChanges() ([]*Change, error) {
	if e.goos != "windows" {
		return nil, errors.New("the execution policy is a setting of PowerShell on Windows")
	}
	var out []*Change
	for _, r := range e.powerShells() {
		if r.LoadsProfile == nil || *r.LoadsProfile {
			continue
		}
		exe, before := r.Path, *r.ExecutionPolicy
		c := &Change{ID: "execution-policy-" + r.ID, Target: r.ID + ":ExecutionPolicy", Action: "modify",
			Summary: "run `Set-ExecutionPolicy -Scope CurrentUser RemoteSigned` in " + r.Name + " (effective policy now " + before + ") so that it runs the profile with the shell wrapper"}
		c.apply = func() error { return e.setRemoteSigned(exe) }
		out = append(out, c)
	}
	return out, nil
}

// overriddenError is an execution policy that Set-ExecutionPolicy could not
// change because a more specific scope (Group Policy) defines it; the
// message is PowerShell's.
type overriddenError struct{ msg string }

func (e *overriddenError) Error() string { return e.msg }

// setRemoteSigned sets RemoteSigned for the current user in the PowerShell
// at exe and checks that it took effect. The process scope Bypass it runs
// with (see policyScript) makes Set-ExecutionPolicy report an override,
// which is left out; a policy Group Policy keeps is the error.
func (e *env) setRemoteSigned(exe string) error {
	out, err := e.run.Run(cmdTimeout, exe, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command",
		`Set-ExecutionPolicy -Scope CurrentUser -ExecutionPolicy RemoteSigned -Force -ErrorAction SilentlyContinue -ErrorVariable failed; $failed | Where-Object { $_.FullyQualifiedErrorId -notlike 'ExecutionPolicyOverride*' } | ForEach-Object { $_.ToString() }`)
	msg := strings.TrimSpace(out)
	p, scope, ok := e.executionPolicy(exe)
	switch {
	case ok && policyLoadsProfile(p):
		return nil
	case ok:
		why := "the effective execution policy stays " + p + ", set by the " + scope + " scope"
		if scope == "MachinePolicy" || scope == "UserPolicy" {
			why += " (Group Policy), which overrides CurrentUser"
		}
		if msg != "" {
			why = msg + "\n" + why
		}
		return &overriddenError{why}
	case msg != "":
		return errors.New(msg)
	case err != nil:
		return errors.New("Set-ExecutionPolicy: " + err.Error())
	}
	return errors.New("the execution policy could not be read after Set-ExecutionPolicy")
}
