package install

import (
	"errors"
	"strconv"
	"strings"
)

// Lingering (`loginctl enable-linger`) keeps the user's systemd service
// manager, and the scopes `stagent run` moves into, running after the last
// logout (logind.Escape). `integrate --linger` turns it on and records that
// in the manifest; only then do `integrate --remove linger` and `uninstall
// --level purge --linger` turn it back off. Lingering an administrator
// enabled is never touched.

// lingerTarget is Change.Target / Step.Target of lingering.
const lingerTarget = "loginctl:linger"

// deniedError is loginctl refused by polkit (set-self-linger), carrying
// loginctl's own message.
type deniedError struct{ msg string }

func (e *deniedError) Error() string { return e.msg }

// loginctlLinger runs `loginctl enable-linger` / `disable-linger` for the
// current user without asking for a password (the app has no terminal).
// The error is loginctl's message; a polkit refusal is a *deniedError.
func (e *env) loginctlLinger(verb string) error {
	out, err := e.run.Run(cmdTimeout, "loginctl", "--no-ask-password", verb, strconv.Itoa(e.uid))
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(out)
	if msg == "" {
		msg = "loginctl " + verb + ": " + err.Error()
	}
	low := strings.ToLower(msg)
	for _, s := range []string{"access denied", "authentication required", "not authorized", "permission denied"} {
		if strings.Contains(low, s) {
			return &deniedError{msg}
		}
	}
	return errors.New(msg)
}

// lingerAddChange plans turning lingering on; nil when it is on already.
func (e *env) lingerAddChange() (*Change, error) {
	if e.goos != "linux" {
		return nil, errors.New("keeping agents running after logout (lingering) is a setting of systemd-logind on Linux")
	}
	if on, known := e.linger(); known && on {
		return nil, nil
	}
	c := &Change{ID: "linger", Target: lingerTarget, Action: "create",
		Summary: "run `loginctl enable-linger` so that agents started on this host keep running after you log out"}
	c.apply = func() error {
		if err := e.loginctlLinger("enable-linger"); err != nil {
			return err
		}
		e.m.LingerEnabled = true
		e.dirty = true
		return nil
	}
	return c, nil
}

// lingerRemoveChange plans turning lingering off when stagent turned it
// on; nil otherwise. A record of lingering someone else turned off since
// is dropped.
func (e *env) lingerRemoveChange() (*Change, error) {
	if !e.m.LingerEnabled {
		return nil, nil
	}
	if on, known := e.linger(); known && !on {
		e.m.LingerEnabled = false
		e.dirty = true
		return nil, nil
	}
	c := &Change{ID: "linger", Target: lingerTarget, Action: "delete",
		Summary: "run `loginctl disable-linger` (stagent turned lingering on)"}
	c.apply = e.disableLinger
	return c, nil
}

// disableLinger turns lingering off and drops the manifest record.
func (e *env) disableLinger() error {
	if err := e.loginctlLinger("disable-linger"); err != nil {
		return err
	}
	e.m.LingerEnabled = false
	e.dirty = true
	return nil
}
