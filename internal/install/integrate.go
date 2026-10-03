package install

import (
	"errors"
	"slices"
	"strings"
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
	Notes      []string  `json:"notes"`
}

// Values of IntegrateResult.Result.
const (
	resultOK          = "ok"
	resultPartial     = "partial"
	resultNothingToDo = "nothing_to_do"
	resultFailed      = "failed"
)

type integrateOpts struct {
	apply        bool
	harness      []string
	shellWrapper bool
	service      bool
	linger       bool
	terminal     bool
	remove       []string
}

// Removable integration names for --remove.
var removeIDs = []string{hClaude, hCodex, hOmp, "shell-wrapper", "service", "linger", "terminal"}

func (e *env) integrate(o integrateOpts) (*IntegrateResult, error) {
	for _, h := range o.harness {
		if slices.Contains(o.remove, h) {
			return nil, errors.New("cannot both add and remove " + h)
		}
	}
	if o.shellWrapper && slices.Contains(o.remove, "shell-wrapper") || o.service && slices.Contains(o.remove, "service") || o.linger && slices.Contains(o.remove, "linger") || o.terminal && slices.Contains(o.remove, "terminal") {
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
		switch h {
		case hClaude:
			c, err := e.addChange(e.claudeTarget())
			one(h)(c, err)
			if c != nil {
				e.note("Claude Code reads hooks when a session starts; restart running Claude sessions to activate them.")
				if e.goos == "windows" {
					e.note("On Windows the hooks run stagent.exe directly (valid in Git Bash and cmd). If the binary is deleted without `stagent uninstall --level unhook`, Claude reports a non-blocking hook error until the entries are removed; `stagent doctor` lists them as orphans.")
				}
			}
		case hCodex:
			many(h)(e.codexAddChanges())
		case hOmp:
			one(h)(e.addChange(e.ompTarget()))
			if e.findHarness(hOmp) != "" {
				e.note("omp loads extensions at startup; restart running omp sessions to activate the extension.")
			}
		}
	}
	if o.shellWrapper {
		ts := e.shellAddTargets()
		if len(ts) == 0 {
			noShell = true
			e.note("No supported shell rc file was found (bash, zsh, fish or PowerShell); nothing to wrap.")
		}
		for _, t := range ts {
			one(t.id)(e.addChange(t))
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
		}
	}
	if len(o.harness)+len(o.remove) > 0 || o.shellWrapper || o.service {
		if !exists(e.l.Bin) {
			e.note("The stagent binary is not installed at %s yet; hooks and wrappers stay inactive until it is.", e.l.Bin)
		}
	}

	r := &IntegrateResult{LoginShell: e.loginShell(), Changes: changes}
	if r.Changes == nil {
		r.Changes = []*Change{}
	}
	if !o.apply {
		r.Result = summarize(r.Changes, settled, false, noShell)
		r.Notes = nonNil(e.notes)
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
		e.note("Not applied: %s.", strings.Join(failed, ", "))
	}
	r.Result = summarize(r.Changes, settled, manifestErr != nil, noShell)
	r.Notes = nonNil(e.notes)
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

// codexAddChanges plans the Codex integration: hooks.json plus the features
// flag, or the notify fallback.
func (e *env) codexAddChanges() ([]*Change, error) {
	mode, why := e.codexMode()
	var out []*Change
	if mode == "hooks" {
		c, err := e.addChange(e.codexHooksTarget())
		if err != nil {
			return nil, err
		}
		if c != nil {
			out = append(out, c)
			e.note("Codex asks you to review and trust the new hooks on its next start; stagent does not write trust entries.")
		}
	} else {
		e.note("Codex: %s; using the notify program instead (turn-complete events only, no approvals).", why)
	}
	c, err := e.addChange(e.codexConfigTarget(mode))
	if err != nil {
		return out, err
	}
	if c != nil {
		out = append(out, c)
	}
	return out, nil
}

// targetsFor maps a --remove name to its file targets.
func (e *env) targetsFor(id string) []*target {
	switch id {
	case hClaude:
		return []*target{e.claudeTarget()}
	case hCodex:
		return []*target{e.codexHooksTarget(), e.codexConfigTarget("")}
	case hOmp:
		return []*target{e.ompTarget()}
	case "shell-wrapper":
		return e.shellTargets()
	}
	return nil
}

// allTargets lists every file target of this OS (service files are handled
// by serviceRemoveChanges).
func (e *env) allTargets() []*target {
	var ts []*target
	for _, id := range []string{hClaude, hCodex, hOmp, "shell-wrapper"} {
		ts = append(ts, e.targetsFor(id)...)
	}
	return ts
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
