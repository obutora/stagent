package install

import (
	"errors"
	"slices"
	"strings"
)

// IntegrateResult is the output of `stagent integrate`.
type IntegrateResult struct {
	Applied bool      `json:"applied"`
	Changes []*Change `json:"changes"`
	Notes   []string  `json:"notes"`
}

type integrateOpts struct {
	apply        bool
	harness      []string
	shellWrapper bool
	service      bool
	remove       []string
}

// Removable integration names for --remove.
var removeIDs = []string{hClaude, hCodex, hOmp, "shell-wrapper", "service"}

func (e *env) integrate(o integrateOpts) (*IntegrateResult, error) {
	for _, h := range o.harness {
		if slices.Contains(o.remove, h) {
			return nil, errors.New("cannot both add and remove " + h)
		}
	}
	if o.shellWrapper && slices.Contains(o.remove, "shell-wrapper") || o.service && slices.Contains(o.remove, "service") {
		return nil, errors.New("cannot both add and remove the same integration")
	}
	var changes []*Change
	var planErrs []string
	// many / one collect planned changes; a planning error for one target
	// becomes a note and does not stop the others.
	many := func(id string) func([]*Change, error) {
		return func(cs []*Change, err error) {
			if err != nil {
				e.note("%s: %v", id, err)
				planErrs = append(planErrs, id)
			}
			for _, c := range cs {
				if c != nil {
					changes = append(changes, c)
				}
			}
		}
	}
	one := func(id string) func(*Change, error) {
		return func(c *Change, err error) { many(id)([]*Change{c}, err) }
	}

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
			e.note("No supported shell rc file was found (bash, zsh, fish or PowerShell); nothing to wrap.")
		}
		for _, t := range ts {
			one(t.id)(e.addChange(t))
		}
		if e.goos != "windows" && slices.ContainsFunc(ts, func(t *target) bool { return t.id == "shell-bash" }) && exists(e.home(".bash_profile")) {
			e.note("bash login shells read ~/.bash_profile; make sure it sources ~/.bashrc so the wrapper is defined there too.")
		}
	}
	if o.service {
		many("service")(e.serviceAddChanges())
	}
	var removed []*target
	for _, id := range o.remove {
		ts := e.targetsFor(id)
		removed = append(removed, ts...)
		for _, t := range ts {
			one(t.id)(e.removeChange(t))
		}
		if id == "service" {
			many(id)(e.serviceRemoveChanges())
		}
	}
	if len(o.harness)+len(o.remove) > 0 || o.shellWrapper || o.service {
		if !exists(e.l.Bin) {
			e.note("The stagent binary is not installed at %s yet; hooks and wrappers stay inactive until it is.", e.l.Bin)
		}
	}

	r := &IntegrateResult{Changes: changes, Notes: e.notes}
	if r.Changes == nil {
		r.Changes = []*Change{}
	}
	if !o.apply {
		r.Notes = nonNil(e.notes)
		return r, nil
	}
	if err := e.l.EnsureDirs(); err != nil {
		return nil, err
	}
	failed := planErrs
	for _, c := range changes {
		if err := c.apply(); err != nil {
			c.Error = err.Error()
			failed = append(failed, c.ID)
		}
	}
	e.dropStaleRecords(removed)
	e.touchManifest()
	if err := e.saveManifest(); err != nil {
		failed = append(failed, "manifest: "+err.Error())
	}
	// Partial failures are reported in the document itself (applied=false,
	// changes[].error, notes); the command still exits 0.
	r.Applied = len(failed) == 0
	if !r.Applied {
		e.note("Not applied: %s.", strings.Join(failed, ", "))
	}
	r.Notes = nonNil(e.notes)
	return r, nil
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
