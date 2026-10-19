package install

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/wire"
)

// harnessIntegration is how stagent integrates with one harness of the
// registry (harness.All), the id accepted by --harness / --remove. Each
// lives in the harness's own file (<id>.go).
type harnessIntegration interface {
	// plan plans adding the integration, noting what the user should know.
	plan(e *env) ([]*Change, error)
	// targets are the harness's file targets, for --remove, uninstall and
	// doctor's orphans.
	targets(e *env) []*target
	// report is the harness's item of doctor.
	report(e *env) HarnessReport
}

// integrations holds the integration of every harness of the registry, by
// harness id.
var integrations = map[string]harnessIntegration{
	wire.HarnessClaude:  claudeIntegration{},
	wire.HarnessCodex:   codexIntegration{},
	wire.HarnessOmp:     ompIntegration{},
	wire.HarnessPi:      piIntegration{},
	wire.HarnessCopilot: copilotIntegration{},
}

// hookFile is how a harness reads our hooks from its JSON hooks file.
type hookFile struct {
	// harness is the harness id, the argument of `stagent hook`.
	harness string
	events  []string
	// timeouts are the hook timeouts (seconds) of events whose hook takes
	// longer than hookTimeoutSec.
	timeouts map[string]int
	// sessionShell: the harness runs the hook command with the session's
	// shell, PowerShell on Windows.
	sessionShell bool
}

const (
	hookTimeoutSec = 10
	identifyHooks  = `hooks.<event>[].hooks[].command contains "` + binMarker + `"`
)

// --- paths ---------------------------------------------------------------

func (e *env) home(parts ...string) string {
	return filepath.Join(append([]string{e.l.Home}, parts...)...)
}

// --- hook commands -------------------------------------------------------

// hookCommand is the command the harness of f runs for our hooks. On unix
// it is guarded so that a deleted binary is a silent no-op. On Windows the
// absolute path (forward slashes) is invoked directly; a missing binary
// then shows up as a non-blocking hook error in the harness, and doctor
// reports the entries as orphans. A harness running the command through
// Git Bash or cmd gets the path quoted. One that runs it with the
// session's shell (hookFile.sessionShell), PowerShell on Windows, where a
// quoted path followed by arguments is a syntax error, gets the path bare,
// valid in every shell, unless it has a character PowerShell treats
// specially; then PowerShell's call operator is used.
func (e *env) hookCommand(f hookFile) string {
	id := f.harness
	if e.goos == "windows" {
		bin := strings.ReplaceAll(e.l.Bin, `\`, "/")
		if !f.sessionShell {
			return `"` + bin + `" hook ` + id
		}
		if strings.ContainsAny(bin, " \t'\"`$&|;,(){}[]@#<>%^") {
			return "& '" + strings.ReplaceAll(bin, "'", "''") + "' hook " + id
		}
		return bin + " hook " + id
	}
	q := shellQuote(e.l.Bin)
	return "[ -x " + q + " ] && " + q + " hook " + id + " || true"
}

// shellQuote single-quotes s for POSIX shells.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func hookSpecs(f hookFile) []hookSpec {
	out := make([]hookSpec, len(f.events))
	for i, ev := range f.events {
		t, ok := f.timeouts[ev]
		if !ok {
			t = hookTimeoutSec
		}
		out[i] = hookSpec{ev, t}
	}
	return out
}

// --- JSON hooks files ----------------------------------------------------

// hooksTarget is target id: the JSON hooks file at path, read as f.
func (e *env) hooksTarget(id, path string, f hookFile) *target {
	return &target{
		id: id, path: path, identify: identifyHooks, mode: 0o600,
		add: func(cur []byte) (addResult, error) {
			d, err := parseJSONDoc(cur)
			if err != nil {
				return addResult{}, unmanagedFile(path + " is not valid JSON; left unchanged")
			}
			created, changed, err := mergeHooks(d.root, e.hookCommand(f), hookSpecs(f))
			if err != nil {
				return addResult{}, unmanagedFile(path + ": " + err.Error() + "; left unchanged")
			}
			if !changed {
				return addResult{after: cur}, nil
			}
			return addResult{
				after:      d.encode(),
				containers: created,
				summary:    "add `stagent hook " + f.harness + "` for " + strings.Join(f.events, ", "),
			}, nil
		},
		remove: func(cur []byte, rec *ConfigEntry) ([]byte, error) {
			d, err := parseJSONDoc(cur)
			if err != nil {
				return nil, unmanagedFile(path + " is not valid JSON; left unchanged")
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

// --- discovery -------------------------------------------------------------

// findHarness locates the executable of harness id: PATH first, then the
// usual per-user install locations (ssh exec shells often have a minimal
// PATH).
func (e *env) findHarness(id string) string {
	h, ok := harness.Lookup(id)
	if !ok {
		return ""
	}
	if p, err := e.lookPath(h.Command); err == nil {
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
			p := filepath.Join(dir, h.Command+ext)
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
