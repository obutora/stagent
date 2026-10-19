package install

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// Codex: stagent's hooks in ~/.codex/hooks.json with the hooks feature
// enabled in ~/.codex/config.toml, or the legacy notify program where this
// Codex has no hooks.

var codexEvents = []string{"SessionStart", "UserPromptSubmit", "Stop", "PermissionRequest"}

// codexHooks: Codex runs the hook command with the session's shell,
// PowerShell on Windows.
var codexHooks = hookFile{harness: wire.HarnessCodex, events: codexEvents, sessionShell: true}

type codexIntegration struct{}

// plan plans the Codex integration: hooks.json plus the features flag, or
// the notify fallback.
func (codexIntegration) plan(e *env) ([]*Change, error) {
	mode, reason := e.codexMode()
	var out []*Change
	if mode == "hooks" {
		c, err := e.addChange(e.codexHooksTarget())
		if err != nil {
			return nil, err
		}
		if c != nil {
			out = append(out, c)
			e.note(noteCodexTrust, "Codex asks you to review and trust the new hooks on its next start; stagent does not write trust entries.")
		}
	} else {
		e.note(noteCodexNotifyFallback, "Codex: "+codexNotifyWhy[reason]+"; using the notify program instead (turn-complete events only, no approvals).", "reason", reason)
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

func (codexIntegration) targets(e *env) []*target {
	return []*target{e.codexHooksTarget(), e.codexConfigTarget("")}
}

func (codexIntegration) report(e *env) HarnessReport {
	h, _ := e.harnessReport(wire.HarnessCodex, e.codexHooksTarget())
	cfg := e.codexConfigTarget("")
	mode, reason := e.codexMode()
	h.HooksSupported = mode == "hooks"
	if !h.HooksSupported {
		h.ConfigPath = cfg.path
		h.Notes = append(h.Notes, codexNotifyWhy[reason]+"; integration uses the notify program")
	}
	if cur, _ := readOptional(cfg.path); cur != nil {
		d := parseTOMLLines(cur)
		if i := topLevelNotify(d); i >= 0 && isOurCommand(d.lines[i].text()) {
			if !h.HooksSupported {
				h.Integrated = true
			} else if removeNotify(parseTOMLLines(cur)) {
				// A leftover of the notify fallback: integrate replaces it
				// with hooks, so the app offers to run it.
				h.Integrated = false
				h.Notes = append(h.Notes, "config.toml still runs stagent as the notify program (set up while this Codex had no hooks); integrate replaces it with hooks")
			}
		}
		if on, set := featureHooksEnabled(d); set && !on && h.HooksSupported {
			h.Notes = append(h.Notes, "hooks are disabled in config.toml ([features] hooks = false); integrate enables them")
		}
	}
	return h
}

func (e *env) codexHooks() string  { return e.home(".codex", "hooks.json") }
func (e *env) codexConfig() string { return e.home(".codex", "config.toml") }

func (e *env) codexHooksTarget() *target {
	return e.hooksTarget("codex-hooks", e.codexHooks(), codexHooks)
}

// --- Codex config.toml ---------------------------------------------------

// Why Codex falls back to the notify program: the reason of codexMode
// (noteCodexNotifyFallback's arg) and its English wording.
const (
	codexReasonInlineFeatures = "inline_features"
	codexReasonHooksRemoved   = "hooks_removed"
	codexReasonNoHooksFeature = "no_hooks_feature"
)

var codexNotifyWhy = map[string]string{
	codexReasonInlineFeatures: "config.toml defines features as an inline table, which stagent does not edit",
	codexReasonHooksRemoved:   "this Codex version removed the hooks feature",
	codexReasonNoHooksFeature: "this Codex version has no hooks feature",
}

// codexMode decides how Codex is integrated: "hooks" (hooks.json plus the
// features flag) or "notify" (the legacy notify program, turn-complete
// only), with the reason (codexReason*) for "notify".
func (e *env) codexMode() (mode string, reason string) {
	if cur, _ := readOptional(e.codexConfig()); cur != nil {
		d := parseTOMLLines(cur)
		if d.findKey("features", 0, d.firstHeader()) >= 0 {
			return "notify", codexReasonInlineFeatures
		}
	}
	if bin := e.findHarness(wire.HarnessCodex); bin != "" {
		out, err := e.run.Run(5*time.Second, bin, "features", "list")
		if err == nil {
			for _, line := range strings.Split(out, "\n") {
				f := strings.Fields(line)
				if len(f) >= 2 && f[0] == "hooks" {
					if f[1] == "removed" {
						return "notify", codexReasonHooksRemoved
					}
					return "hooks", ""
				}
			}
			return "notify", codexReasonNoHooksFeature
		}
	}
	return "hooks", ""
}

func (e *env) codexConfigTarget(mode string) *target {
	path := e.codexConfig()
	return &target{
		id: "codex-config", path: path, mode: 0o600,
		identify: `top-level notify containing "` + binMarker + `"; [features] hooks as recorded in toml_edits`,
		add: func(cur []byte) (addResult, error) {
			d := parseTOMLLines(cur)
			if mode == "hooks" {
				// A notify program of ours is left from a time this Codex
				// had no hooks; with hooks.json it would report each turn
				// twice. It goes for good: removal does not bring it back.
				dropped := removeNotify(d)
				ed, err := ensureFeatureHooks(d)
				if err != nil {
					return addResult{}, err
				}
				var parts []string
				var edits []TOMLEdit
				if dropped {
					parts = append(parts, "remove the notify program that ran stagent")
				}
				if ed != nil {
					parts = append(parts, "enable Codex hooks ([features] hooks = true)")
					edits = append(edits, *ed)
				}
				if len(parts) == 0 {
					return addResult{after: cur}, nil
				}
				return addResult{after: d.bytes(), edits: edits, summary: strings.Join(parts, "; ")}, nil
			}
			added, existing := addNotify(d, e.l.Bin)
			if !added {
				if !isOurCommand(existing) {
					e.note(noteCodexNotifyKept, "Codex already has a notify program ({command}); stagent did not replace it, so Codex turn-complete events are not reported.", "command", strings.TrimSpace(existing))
				}
				return addResult{after: cur}, nil
			}
			return addResult{after: d.bytes(), edits: []TOMLEdit{{Kind: "notify", Action: "added"}}, summary: "set Codex notify to `stagent hook codex notify` (turn-complete only)"}, nil
		},
		remove: func(cur []byte, rec *ConfigEntry) ([]byte, error) {
			d := parseTOMLLines(cur)
			removeNotify(d)
			if rec != nil {
				for _, ed := range rec.TOMLEdits {
					if ed.Kind != "features_hooks" {
						continue
					}
					if _, ok := revertFeatureHooks(d, ed); !ok {
						e.note(noteCodexFeaturesKept, "Left Codex [features] hooks as it is: it was changed after stagent enabled it.")
					}
				}
			}
			removeTables(d, e.codexTrustTables())
			return d.bytes(), nil
		},
		present: func(cur []byte) string {
			d := parseTOMLLines(cur)
			var parts []string
			if i := topLevelNotify(d); i >= 0 && isOurCommand(d.lines[i].text()) {
				parts = append(parts, "notify runs stagent")
			}
			if rec := e.m.config("codex-config", path); rec != nil {
				for _, ed := range rec.TOMLEdits {
					if on, _ := featureHooksEnabled(d); ed.Kind == "features_hooks" && on {
						parts = append(parts, "[features] hooks enabled by stagent")
					}
				}
			}
			if countTables(d, e.codexTrustTables()) > 0 {
				parts = append(parts, "trust entries for stagent hooks")
			}
			return strings.Join(parts, "; ")
		},
	}
}

// codexTrustTables are the [hooks.state."<hooks.json>:<event>:<i>:<j>"]
// tables Codex writes once the user trusts our hooks, keyed by the current
// positions of our entries in hooks.json.
func (e *env) codexTrustTables() map[string]bool {
	out := map[string]bool{}
	cur, _ := readOptional(e.codexHooks())
	if cur == nil {
		return out
	}
	d, err := parseJSONDoc(cur)
	if err != nil {
		return out
	}
	for event, pos := range ourHookPositions(d.root) {
		for _, p := range pos {
			key := e.codexHooks() + ":" + snakeCase(event) + ":" + strconv.Itoa(p[0]) + ":" + strconv.Itoa(p[1])
			out["hooks.state."+key] = true
		}
	}
	return out
}

func countTables(d *tomlDoc, names map[string]bool) int {
	n := 0
	for _, l := range d.lines {
		if !l.array && names[l.header] {
			n++
		}
	}
	return n
}

var upperRun = regexp.MustCompile(`([a-z0-9])([A-Z])`)

func snakeCase(s string) string {
	return strings.ToLower(upperRun.ReplaceAllString(s, "${1}_${2}"))
}
