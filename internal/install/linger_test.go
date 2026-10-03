package install

import (
	"slices"
	"strings"
	"testing"
)

// lingerHost fakes loginctl: lingering is *on, enable/disable change it
// unless denied, and every call is recorded on te.run.
func lingerHost(te *testEnv, on *bool, denied *string) {
	te.run.respond = func(name string, args []string) (string, error) {
		if name != "loginctl" {
			return "", nil
		}
		switch {
		case slices.Contains(args, "--property=Linger"):
			if *on {
				return "yes\n", nil
			}
			return "no\n", nil
		case slices.Contains(args, "enable-linger"), slices.Contains(args, "disable-linger"):
			if *denied != "" {
				return *denied + "\n", errString("exit status 1")
			}
			*on = slices.Contains(args, "enable-linger")
		}
		return "", nil
	}
}

func lingerChange(r *IntegrateResult) *Change {
	for _, c := range r.Changes {
		if c.ID == "linger" {
			return c
		}
	}
	return nil
}

// integrate --linger turns lingering on and records it; only a recorded
// one is turned off again by --remove linger, and lingering that was on
// before is left alone either way.
func TestIntegrateLinger(t *testing.T) {
	te := newTestEnv(t, "linux")
	on, denied := false, ""
	lingerHost(te, &on, &denied)

	plan := te.integrate(t, integrateOpts{linger: true})
	c := lingerChange(plan)
	if c == nil || c.Action != "create" || c.Target != lingerTarget || on || te.run.ran("loginctl --no-ask-password enable-linger") {
		t.Fatalf("plan: %+v, calls %v", plan.Changes, te.run.calls)
	}
	r := te.integrate(t, integrateOpts{apply: true, linger: true})
	if !r.Applied || r.Result != resultOK || !on || !te.run.ran("loginctl --no-ask-password enable-linger 1000") {
		t.Fatalf("apply: %+v, calls %v", r, te.run.calls)
	}
	te.reload()
	if !te.m.LingerEnabled || !te.doctor().Persistence.LingerEnabledByStagent {
		t.Fatal("enabling lingering not recorded in the manifest")
	}
	if r := te.integrate(t, integrateOpts{linger: true}); lingerChange(r) != nil || r.Result != resultOK {
		t.Fatalf("already on: %+v", r.Changes)
	}

	r = te.integrate(t, integrateOpts{remove: []string{"linger"}})
	if c := lingerChange(r); c == nil || c.Action != "delete" || !on {
		t.Fatalf("remove plan: %+v", r.Changes)
	}
	te.integrate(t, integrateOpts{apply: true, remove: []string{"shell-wrapper", "linger"}})
	te.reload()
	if on || te.m.LingerEnabled {
		t.Fatalf("after --remove linger: on %v, recorded %v", on, te.m.LingerEnabled)
	}

	// Lingering an administrator enabled: no change, no record, never
	// turned off.
	on = true
	te.run.calls = nil
	if r := te.integrate(t, integrateOpts{apply: true, linger: true}); lingerChange(r) != nil {
		t.Fatalf("on before: %+v", r.Changes)
	}
	te.integrate(t, integrateOpts{apply: true, remove: []string{"linger"}})
	te.reload()
	if !on || te.m.LingerEnabled || te.run.ran("loginctl --no-ask-password") {
		t.Fatalf("lingering on before was touched: on %v, recorded %v, calls %v", on, te.m.LingerEnabled, te.run.calls)
	}
}

// polkit refusing set-self-linger is linger_denied with loginctl's own
// message; nothing is recorded.
func TestIntegrateLingerDenied(t *testing.T) {
	te := newTestEnv(t, "linux")
	on, denied := false, "Could not enable linger: Access denied"
	lingerHost(te, &on, &denied)
	te.reload()
	r, err := te.env.integrate(integrateOpts{apply: true, linger: true})
	if err != nil {
		t.Fatal(err)
	}
	c := lingerChange(r)
	if c == nil || c.ErrorCode != codeLingerDenied || c.Error != denied || r.Applied || r.Result != resultFailed {
		t.Fatalf("denied: %+v, change %+v", r, c)
	}
	te.reload()
	if te.m.LingerEnabled {
		t.Fatal("refused lingering recorded")
	}

	denied = "Failed to connect to bus: No such file or directory"
	te.reload()
	r, _ = te.env.integrate(integrateOpts{apply: true, linger: true})
	if c := lingerChange(r); c == nil || c.ErrorCode != codeIOError {
		t.Fatalf("other failure: %+v", c)
	}

	mac := newTestEnv(t, "darwin")
	r, _ = mac.env.integrate(integrateOpts{linger: true})
	if c := lingerChange(r); c == nil || c.Action != "skip" || mac.run.ran("loginctl") {
		t.Fatalf("darwin: %+v", r.Changes)
	}
}

// uninstall --level purge --linger turns lingering off only when stagent
// turned it on; without --linger it stays on.
func TestUninstallPurgeLinger(t *testing.T) {
	for _, revert := range []bool{false, true} {
		te := newTestEnv(t, "linux")
		on, denied := false, ""
		lingerHost(te, &on, &denied)
		if _, err := te.install(); err != nil {
			t.Fatal(err)
		}
		te.integrate(t, integrateOpts{apply: true, linger: true})
		te.reload()
		r := te.uninstall("purge", false, revert)
		var steps []string
		for _, s := range r.Steps {
			if s.Action == "disable-linger" {
				steps = append(steps, s.Target)
			}
		}
		if on == revert || (len(steps) == 1) != revert {
			t.Errorf("--linger=%v: lingering on %v, steps %v", revert, on, r.Steps)
		}
	}

	// Lingering stagent did not turn on is never turned off.
	te := newTestEnv(t, "linux")
	on, denied := true, ""
	lingerHost(te, &on, &denied)
	if _, err := te.install(); err != nil {
		t.Fatal(err)
	}
	te.reload()
	te.uninstall("purge", false, true)
	if !on || strings.Contains(strings.Join(te.run.calls, "\n"), "disable-linger") {
		t.Fatalf("calls %v", te.run.calls)
	}
}
