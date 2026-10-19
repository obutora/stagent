package install

import (
	"errors"
	"slices"
	"strings"

	"github.com/obutora/stagent/internal/harness"
)

// IntegrateResult is the output of `stagent integrate`.
type IntegrateResult struct {
	Applied bool `json:"applied"`
	// Result sums up the outcome: ok | partial | nothing_to_do | failed
	// (summarize).
	Result string `json:"result"`
	// LoginShell is the base name of $SHELL ("" when unknown).
	LoginShell string    `json:"login_shell"`
	Changes    []*Change `json:"changes"`
	Notes      []Note    `json:"notes"`
}

// Values of IntegrateResult.Result.
const (
	resultOK          = "ok"
	resultPartial     = "partial"
	resultNothingToDo = "nothing_to_do"
	resultFailed      = "failed"
)

type integrateOpts struct {
	apply           bool
	harness         []string
	shellWrapper    bool
	service         bool
	linger          bool
	terminal        bool
	executionPolicy bool
	wslKeepRunning  bool
	remove          []string
}

// Removable integration names for --remove: the harnesses of the registry,
// then stagent's own integrations.
var removeIDs = append(harness.IDs(), "shell-wrapper", "service", "linger", "terminal", wslKeepRunningID)

func (e *env) integrate(o integrateOpts) (*IntegrateResult, error) {
	for _, h := range o.harness {
		if slices.Contains(o.remove, h) {
			return nil, errors.New("cannot both add and remove " + h)
		}
	}
	if o.shellWrapper && slices.Contains(o.remove, "shell-wrapper") || o.service && slices.Contains(o.remove, "service") || o.linger && slices.Contains(o.remove, "linger") || o.terminal && slices.Contains(o.remove, "terminal") || o.wslKeepRunning && slices.Contains(o.remove, wslKeepRunningID) {
		return nil, errors.New("cannot both add and remove the same integration")
	}
	var changes []*Change
	// settled counts targets planned without error that need no change.
	settled := 0
	// many / one collect planned changes; a target that cannot be planned
	// becomes a skip change carrying the error and does not stop the
	// others.
	many := func(id string) func([]*Change, error) {
		return func(cs []*Change, err error) {
			n := 0
			for _, c := range cs {
				if c != nil {
					changes = append(changes, c)
					n++
				}
			}
			if err != nil {
				changes = append(changes, skipChange(id, err))
			} else if n == 0 {
				settled++
			}
		}
	}
	one := func(id string) func(*Change, error) {
		return func(c *Change, err error) { many(id)([]*Change{c}, err) }
	}
	noShell := false

	for _, h := range o.harness {
		many(h)(integrations[h].plan(e))
	}
	if o.shellWrapper {
		ts := e.shellAddTargets()
		if len(ts) == 0 {
			noShell = true
			e.note(noteNoShell, "No supported shell rc file was found (bash, zsh, fish or PowerShell); nothing to wrap.")
		}
		for _, t := range ts {
			one(t.id)(e.addChange(t))
		}
		if login := e.loginShell(); e.goos == "windows" && strings.TrimSuffix(strings.ToLower(login), ".exe") == "cmd" {
			e.note(noteCmdNotWrapped, "The default shell ({shell}) is cmd, which has no profile for the wrapper; it goes into the PowerShell profiles only.", "shell", login)
		}
	}
	if o.service {
		many("service")(e.serviceAddChanges())
	}
	if o.linger {
		one("linger")(e.lingerAddChange())
	}
	if o.terminal {
		one("terminal")(e.terminalAddChange())
	}
	if o.executionPolicy {
		many("execution-policy")(e.executionPolicyChanges())
	}
	if o.wslKeepRunning {
		one(wslKeepRunningID)(e.wslAddChange())
	}
	var removed []*target
	for _, id := range o.remove {
		ts := e.targetsFor(id)
		removed = append(removed, ts...)
		for _, t := range ts {
			one(t.id)(e.removeChange(t))
		}
		switch id {
		case "service":
			many(id)(e.serviceRemoveChanges())
		case "linger":
			one(id)(e.lingerRemoveChange())
		case "terminal":
			one(id)(e.terminalRemoveChange())
		case wslKeepRunningID:
			one(id)(e.wslRemoveChange())
		}
	}
	if len(o.harness)+len(o.remove) > 0 || o.shellWrapper || o.service {
		if !exists(e.l.Bin) {
			e.note(noteBinaryMissing, "The stagent binary is not installed at {path} yet; hooks and wrappers stay inactive until it is.", "path", e.l.Bin)
		}
	}

	r := &IntegrateResult{LoginShell: e.loginShell(), Changes: changes}
	if r.Changes == nil {
		r.Changes = []*Change{}
	}
	if !o.apply {
		r.Result = summarize(r.Changes, settled, false, noShell)
		r.Notes = e.notesOut()
		return r, nil
	}
	if err := e.l.EnsureDirs(); err != nil {
		return nil, err
	}
	var failed []string
	for _, c := range changes {
		if c.apply == nil {
			failed = append(failed, c.ID) // skip
		} else if err := c.apply(); err != nil {
			c.Error, c.ErrorCode = err.Error(), errorCode(err)
			failed = append(failed, c.ID)
		}
	}
	e.dropStaleRecords(removed)
	e.touchManifest()
	manifestErr := e.saveManifest()
	if manifestErr != nil {
		failed = append(failed, "manifest: "+manifestErr.Error())
	}
	// Partial failures are reported in the document itself (applied=false,
	// result, changes[].error, notes); the command still exits 0.
	r.Applied = len(failed) == 0
	if !r.Applied {
		e.note(noteNotApplied, "Not applied: {failed}.", "failed", strings.Join(failed, ", "))
	}
	r.Result = summarize(r.Changes, settled, manifestErr != nil, noShell)
	r.Notes = e.notesOut()
	return r, nil
}

// summarize is IntegrateResult.Result: nothing_to_do when a shell wrapper
// was asked for and no shell target exists; otherwise ok without errors,
// failed when nothing went right (no clean change and no target already
// in place), partial in between.
func summarize(changes []*Change, settled int, otherErr, noShell bool) string {
	if noShell {
		return resultNothingToDo
	}
	bad, good := 0, settled
	if otherErr {
		bad++
	}
	for _, c := range changes {
		if c.Error != "" {
			bad++
		} else {
			good++
		}
	}
	switch {
	case bad == 0:
		return resultOK
	case good == 0:
		return resultFailed
	}
	return resultPartial
}

// targetsFor maps a --remove name to its file targets.
func (e *env) targetsFor(id string) []*target {
	if hi, ok := integrations[id]; ok {
		return hi.targets(e)
	}
	if id == "shell-wrapper" {
		return e.shellTargets()
	}
	return nil
}

// allTargets lists every file target of this OS (service files are handled
// by serviceRemoveChanges).
func (e *env) allTargets() []*target {
	var ts []*target
	for _, h := range harness.All {
		ts = append(ts, integrations[h.ID].targets(e)...)
	}
	return append(ts, e.shellTargets()...)
}

// touchManifest refreshes the layout facts recorded in the manifest.
func (e *env) touchManifest() {
	e.m.Layout = layoutInfo(e.l)
	e.m.DaemonAddr = e.l.DaemonAddr
	e.dirty = true
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
