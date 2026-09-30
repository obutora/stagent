package install

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/wire"
)

// Harness ids accepted by --harness / --remove.
const (
	hClaude = wire.HarnessClaude
	hCodex  = wire.HarnessCodex
	hOmp    = wire.HarnessOmp
)

var harnessIDs = []string{hClaude, hCodex, hOmp}

var claudeEvents = []string{"SessionStart", "SessionEnd", "UserPromptSubmit", "Stop", "Notification", "PermissionRequest"}

var codexEvents = []string{"SessionStart", "UserPromptSubmit", "Stop", "PermissionRequest"}

const (
	hookTimeoutSec = 10
	identifyHooks  = `hooks.<event>[].hooks[].command contains "` + binMarker + `"`
)

// --- paths ---------------------------------------------------------------

func (e *env) home(parts ...string) string {
	return filepath.Join(append([]string{e.l.Home}, parts...)...)
}

func (e *env) claudeSettings() string { return e.home(".claude", "settings.json") }
func (e *env) codexHooks() string     { return e.home(".codex", "hooks.json") }
func (e *env) codexConfig() string    { return e.home(".codex", "config.toml") }
func (e *env) ompExtension() string {
	return e.home(".omp", "agent", "extensions", "ssh-term-stagent.ts")
}

// --- hook commands -------------------------------------------------------

// hookCommand is the command a harness runs for our hooks. On unix it is
// guarded so that a deleted binary is a silent no-op. On Windows the harness
// may run it through Git Bash or cmd, which share no guard syntax, so the
// quoted absolute path (forward slashes, valid in both) is invoked directly;
// a missing binary then shows up as a non-blocking hook error in the
// harness, and doctor reports the entries as orphans.
func (e *env) hookCommand(harness string) string {
	if e.goos == "windows" {
		return `"` + strings.ReplaceAll(e.l.Bin, `\`, "/") + `" hook ` + harness
	}
	q := shellQuote(e.l.Bin)
	return "[ -x " + q + " ] && " + q + " hook " + harness + " || true"
}

// shellQuote single-quotes s for POSIX shells.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// approvalTimeout reads approval_timeout_sec from config.json (with the
// documented default).
func (e *env) approvalTimeout() int {
	var c wire.Config
	if b, err := os.ReadFile(e.l.Config); err == nil {
		json.Unmarshal(b, &c)
	}
	return c.WithDefaults().ApprovalTimeoutSec
}

func (e *env) hookSpecs(events []string) []hookSpec {
	out := make([]hookSpec, len(events))
	for i, ev := range events {
		t := hookTimeoutSec
		if ev == "PermissionRequest" {
			t = e.approvalTimeout() + 10
		}
		out[i] = hookSpec{ev, t}
	}
	return out
}

// --- Claude Code / Codex hooks files -------------------------------------

func (e *env) hooksTarget(id, path, harness string, events []string) *target {
	return &target{
		id: id, path: path, identify: identifyHooks, mode: 0o600,
		add: func(cur []byte) (addResult, error) {
			d, err := parseJSONDoc(cur)
			if err != nil {
				return addResult{}, errors.New(path + " is not valid JSON; left unchanged")
			}
			created, changed, err := mergeHooks(d.root, e.hookCommand(harness), e.hookSpecs(events))
			if err != nil {
				return addResult{}, errors.New(path + ": " + err.Error() + "; left unchanged")
			}
			if !changed {
				return addResult{after: cur}, nil
			}
			return addResult{
				after:      d.encode(),
				containers: created,
				summary:    "add `stagent hook " + harness + "` for " + strings.Join(events, ", "),
			}, nil
		},
		remove: func(cur []byte, rec *ConfigEntry) ([]byte, error) {
			d, err := parseJSONDoc(cur)
			if err != nil {
				return nil, errors.New(path + " is not valid JSON; left unchanged")
			}
			var created []string
			if rec != nil {
				created = rec.CreatedContainers
			}
			if unmergeHooks(d.root, created, rec != nil) == 0 {
				return cur, nil
			}
			if rec != nil && rec.Created && len(d.root.keys) == 0 {
				return nil, nil
			}
			return d.encode(), nil
		},
		present: func(cur []byte) string {
			d, err := parseJSONDoc(cur)
			if err != nil {
				return ""
			}
			if ev := ourHookEvents(d.root); len(ev) > 0 {
				return "stagent hook entries for " + strings.Join(ev, ", ")
			}
			return ""
		},
	}
}

func (e *env) claudeTarget() *target {
	return e.hooksTarget(hClaude, e.claudeSettings(), hClaude, claudeEvents)
}

func (e *env) codexHooksTarget() *target {
	return e.hooksTarget("codex-hooks", e.codexHooks(), hCodex, codexEvents)
}

// --- Codex config.toml ---------------------------------------------------

// codexMode decides how Codex is integrated: "hooks" (hooks.json plus the
// features flag) or "notify" (the legacy notify program, turn-complete only).
func (e *env) codexMode() (mode string, why string) {
	if e.goos == "windows" {
		return "notify", "Codex hooks are not supported on Windows"
	}
	if cur, _ := readOptional(e.codexConfig()); cur != nil {
		d := parseTOMLLines(cur)
		if d.findKey("features", 0, d.firstHeader()) >= 0 {
			return "notify", "config.toml defines features as an inline table, which stagent does not edit"
		}
	}
	if bin := e.findHarness(hCodex); bin != "" {
		out, err := e.run.Run(5*time.Second, bin, "features", "list")
		if err == nil {
			for _, line := range strings.Split(out, "\n") {
				f := strings.Fields(line)
				if len(f) >= 2 && f[0] == "hooks" {
					if f[1] == "removed" {
						return "notify", "this Codex version removed the hooks feature"
					}
					return "hooks", ""
				}
			}
			return "notify", "this Codex version has no hooks feature"
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
				ed, err := ensureFeatureHooks(d)
				if err != nil {
					return addResult{}, err
				}
				if ed == nil {
					return addResult{after: cur}, nil
				}
				return addResult{after: d.bytes(), edits: []TOMLEdit{*ed}, summary: "enable Codex hooks ([features] hooks = true)"}, nil
			}
			added, existing := addNotify(d, e.l.Bin)
			if !added {
				if !isOurCommand(existing) {
					e.note("Codex already has a notify program (%s); stagent did not replace it, so Codex turn-complete events are not reported.", strings.TrimSpace(existing))
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
						e.note("Left Codex [features] hooks as it is: it was changed after stagent enabled it.")
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

// --- omp -------------------------------------------------------------------

func (e *env) ompTarget() *target {
	return &target{
		id: hOmp, path: e.ompExtension(), owned: true, mode: 0o644,
		identify: "whole file (first line " + ompMarker + ")",
		add: func(cur []byte) (addResult, error) {
			if cur != nil && !isOmpExtension(cur) {
				return addResult{}, errors.New(e.ompExtension() + " exists and is not managed by stagent; left unchanged")
			}
			return addResult{after: []byte(ompExtensionSource), summary: "install the omp extension that reports session events to stagent"}, nil
		},
		remove: func(cur []byte, _ *ConfigEntry) ([]byte, error) {
			if !isOmpExtension(cur) {
				return cur, nil
			}
			return nil, nil
		},
		present: func(cur []byte) string {
			if isOmpExtension(cur) {
				return "stagent omp extension"
			}
			return ""
		},
	}
}

// --- discovery -------------------------------------------------------------

// findHarness locates a harness executable: PATH first, then the usual
// per-user install locations (ssh exec shells often have a minimal PATH).
func (e *env) findHarness(name string) string {
	if p, err := e.lookPath(name); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	exts := []string{""}
	if e.goos == "windows" {
		exts = []string{".exe", ".cmd", ".bat", ".ps1"}
	}
	for _, dir := range e.harnessDirs() {
		for _, ext := range exts {
			p := filepath.Join(dir, name+ext)
			if st, err := os.Stat(p); err == nil && !st.IsDir() && (e.goos == "windows" || st.Mode()&0o111 != 0) {
				return p
			}
		}
	}
	return ""
}

func (e *env) harnessDirs() []string {
	return paths.ToolDirs(e.goos, e.l.Home, e.getenv)
}

var versionRe = regexp.MustCompile(`\d+\.\d+(?:\.\d+)?(?:[-+][0-9A-Za-z.\-]+)?`)

// harnessVersion runs `<bin> --version` (3 s) and extracts the version.
func (e *env) harnessVersion(bin string) string {
	out, err := e.run.Run(3*time.Second, bin, "--version")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		if v := versionRe.FindString(line); v != "" {
			return v
		}
	}
	return ""
}
