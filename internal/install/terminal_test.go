package install

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// terminalHost serves Terminal's preferences through fake `defaults export`
// / `defaults import`; *prefs is the domain as cfprefsd holds it.
func terminalHost(t *testing.T, te *testEnv, prefs *string) {
	t.Helper()
	te.run.respond = func(name string, args []string) (string, error) {
		if name != "defaults" || len(args) < 3 || args[1] != terminalDomain {
			return "", nil
		}
		switch args[0] {
		case "export":
			return *prefs, nil
		case "import":
			b, err := os.ReadFile(args[2])
			if err != nil {
				t.Fatal(err)
			}
			*prefs = string(b)
		}
		return "", nil
	}
}

const terminalPrefsBefore = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Default Window Settings</key>
	<string>Basic</string>
	<key>Window Settings</key>
	<dict>
		<key>Basic</key>
		<dict>
			<key>Font</key>
			<data>
			YnBsaXN0MDDUAQIDBAUGBwpYJHZlcnNpb25ZJGFyY2hpdmVy
			</data>
			<key>FontWidthSpacing</key>
			<real>1.004032258064516</real>
			<key>name</key>
			<string>Basic</string>
		</dict>
		<key>Clear Dark</key>
		<dict>
			<key>name</key>
			<string>Clear Dark &amp; more</string>
			<key>noWarnProcesses</key>
			<array>
				<dict>
					<key>ProcessName</key>
					<string>screen</string>
				</dict>
				<dict>
					<key>ProcessName</key>
					<string>vim</string>
				</dict>
			</array>
			<key>shellExitAction</key>
			<integer>1</integer>
		</dict>
		<key>Pro</key>
		<dict>
			<key>noWarnProcesses</key>
			<array>
				<dict>
					<key>ProcessName</key>
					<string>stagent</string>
				</dict>
			</array>
		</dict>
	</dict>
	<key>SecureKeyboardEntry</key>
	<false/>
</dict>
</plist>
`

// noWarnOf returns the process names of a profile in prefs; nil when the
// key is absent.
func noWarnOf(t *testing.T, prefs, profile string) []string {
	t.Helper()
	root, err := parsePlist([]byte(prefs))
	if err != nil {
		t.Fatal(err)
	}
	p := root.get("Window Settings").get(profile)
	if p == nil {
		t.Fatalf("profile %s missing", profile)
	}
	names, ok := noWarnNames(p)
	if !ok {
		return nil
	}
	return names
}

// `integrate --terminal` adds stagent to every profile without it (with
// Terminal's default list where the key is absent), leaving everything
// else of the domain as it was; `--remove terminal` takes out only what it
// added: a list the user changed since keeps the change, a key stagent
// created goes again, and a stagent entry stagent did not add stays.
func TestIntegrateTerminal(t *testing.T) {
	te := newTestEnv(t, "darwin")
	prefs := terminalPrefsBefore
	terminalHost(t, te, &prefs)

	if r := te.doctor().Terminal; r == nil || r.Profiles != 3 || r.NoWarn || r.AddedByStagent {
		t.Fatalf("doctor before: %+v", r)
	}
	r := te.integrate(t, integrateOpts{terminal: true})
	if len(r.Changes) != 1 || r.Changes[0].ID != "terminal" || r.Changes[0].Target != terminalDomain || !strings.Contains(r.Changes[0].Summary, "Basic, Clear Dark (") {
		t.Fatalf("plan: %+v", r.Changes)
	}
	if !strings.Contains(r.Changes[0].Diff, "+Clear Dark: screen, vim, stagent") || te.run.ran("defaults import") {
		t.Fatalf("plan diff %q, calls %v", r.Changes[0].Diff, te.run.calls)
	}

	te.integrate(t, integrateOpts{apply: true, terminal: true})
	for profile, want := range map[string][]string{
		"Basic":      {"screen", "tmux", "stagent"},
		"Clear Dark": {"screen", "vim", "stagent"},
		"Pro":        {"stagent"},
	} {
		if got := noWarnOf(t, prefs, profile); !slices.Equal(got, want) {
			t.Errorf("%s after add: %v, want %v", profile, got, want)
		}
	}
	for _, keep := range []string{"YnBsaXN0MDDUAQIDBAUGBwpYJHZlcnNpb25ZJGFyY2hpdmVy", "<real>1.004032258064516</real>", "Clear Dark &amp; more", "<integer>1</integer>", "<key>SecureKeyboardEntry</key>\n\t<false/>", "<key>Default Window Settings</key>"} {
		if !strings.Contains(prefs, keep) {
			t.Errorf("lost %q:\n%s", keep, prefs)
		}
	}
	if d := te.doctor().Terminal; d.Profiles != 3 || !d.NoWarn || !d.AddedByStagent {
		t.Errorf("doctor after add: %+v", d)
	}
	if r := te.integrate(t, integrateOpts{terminal: true}); len(r.Changes) != 0 {
		t.Errorf("second plan: %+v", r.Changes)
	}

	// The user adds htop to Clear Dark in Terminal's settings.
	prefs = strings.Replace(prefs, "<string>vim</string>", "<string>vim</string>\n\t\t\t\t</dict>\n\t\t\t\t<dict>\n\t\t\t\t\t<key>ProcessName</key>\n\t\t\t\t\t<string>htop</string>", 1)

	te.integrate(t, integrateOpts{apply: true, remove: []string{"terminal"}})
	if got := noWarnOf(t, prefs, "Basic"); got != nil {
		t.Errorf("Basic after remove: %v, want the key gone", got)
	}
	if got := noWarnOf(t, prefs, "Clear Dark"); !slices.Equal(got, []string{"screen", "vim", "htop"}) {
		t.Errorf("Clear Dark after remove: %v", got)
	}
	if got := noWarnOf(t, prefs, "Pro"); !slices.Equal(got, []string{"stagent"}) {
		t.Errorf("Pro after remove: %v", got)
	}
	if d := te.doctor().Terminal; d.NoWarn || d.AddedByStagent {
		t.Errorf("doctor after remove: %+v", d)
	}
	if r := te.integrate(t, integrateOpts{remove: []string{"terminal"}}); len(r.Changes) != 0 {
		t.Errorf("second removal plan: %+v", r.Changes)
	}
}

// unhook takes stagent's entries out too and reports what it did; the
// entry counts as left over until then.
func TestUninstallUnhookRemovesTerminalEntry(t *testing.T) {
	te := newTestEnv(t, "darwin")
	prefs := terminalPrefsBefore
	terminalHost(t, te, &prefs)
	te.integrate(t, integrateOpts{apply: true, terminal: true})

	te.reload()
	if !slices.ContainsFunc(te.inventory(false), func(a artifact) bool { return a.Target == terminalDomain && a.level == levelUnhook }) {
		t.Errorf("inventory %+v", te.inventory(false))
	}
	r := te.uninstall("unhook", false, false)
	if !slices.ContainsFunc(r.Steps, func(s Step) bool { return s.Target == terminalDomain && s.OK }) {
		t.Errorf("unhook steps %+v", r.Steps)
	}
	if slices.ContainsFunc(r.Remaining, func(f Finding) bool { return f.Target == terminalDomain }) {
		t.Errorf("unhook: remaining %+v", r.Remaining)
	}
	if got := noWarnOf(t, prefs, "Clear Dark"); !slices.Equal(got, []string{"screen", "vim"}) {
		t.Errorf("Clear Dark after unhook: %v", got)
	}
}

// Off macOS --terminal is a skip and Terminal is not asked; a Mac without
// saved profiles has nothing to change.
func TestIntegrateTerminalNotApplicable(t *testing.T) {
	te := newTestEnv(t, "linux")
	r, err := te.env.integrate(integrateOpts{terminal: true})
	if err != nil || len(r.Changes) != 1 || r.Changes[0].Action != "skip" || te.run.ran("defaults") {
		t.Fatalf("linux: %+v %v", r, err)
	}
	if te.doctor().Terminal != nil {
		t.Error("linux doctor has a terminal item")
	}

	mac := newTestEnv(t, "darwin")
	prefs := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>SecureKeyboardEntry</key>
	<false/>
</dict>
</plist>
`
	terminalHost(t, mac, &prefs)
	if r := mac.integrate(t, integrateOpts{apply: true, terminal: true}); len(r.Changes) != 0 || r.Result != resultOK || mac.run.ran("defaults import") {
		t.Errorf("no profiles: %+v", r)
	}
	if d := mac.doctor().Terminal; d == nil || d.Profiles != 0 || d.NoWarn {
		t.Errorf("no profiles doctor: %+v", d)
	}
}
