// Package install implements stagent's ledger-based installer: `install`
// (layout + manifest), `integrate` (harness hooks, shell wrappers, login
// service; planned as unified diffs before anything is written), `doctor`
// (state report, orphans) and `uninstall` (stop / unhook / purge). The app
// runs these over plain SSH exec and parses their JSON (PROTOCOL.md).
package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf16"

	"github.com/obutora/stagent/internal/version"
)

// InstallResult is the output of `stagent install`.
type InstallResult struct {
	OK      bool       `json:"ok"`
	Version string     `json:"version"`
	Layout  LayoutInfo `json:"layout"`
	Notes   []string   `json:"notes"`
}

// Main runs one installer command and returns the exit code.
func Main(cmd string, args []string) int {
	return mainWith(cmd, args, os.Stdout, os.Stderr, nil)
}

// mainWith is Main with injectable output and environment (tests).
func mainWith(cmd string, args []string, stdout, stderr io.Writer, e *env) int {
	fs := flag.NewFlagSet("stagent "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "print compact JSON")
	var plan, apply, shellWrapper, service, uploads *bool
	var harness, remove, level *string
	switch cmd {
	case "integrate":
		plan = fs.Bool("plan", false, "show the changes without applying them (default)")
		apply = fs.Bool("apply", false, "apply the changes")
		harness = fs.String("harness", "", "comma-separated harnesses to integrate: claude,codex,omp")
		shellWrapper = fs.Bool("shell-wrapper", false, "wrap claude/codex/omp in shell rc files")
		service = fs.Bool("service", false, "run the daemon as a login service")
		remove = fs.String("remove", "", "comma-separated integrations to remove: claude,codex,omp,shell-wrapper,service")
	case "uninstall":
		level = fs.String("level", "", "stop | unhook | purge")
		uploads = fs.Bool("uploads", false, "also delete ~/.ssh-term/uploads (purge)")
	}
	emit := func(v any) { stdout.Write(asciiJSON(v, !*jsonOut)) }
	fail := func(code int, err error) int {
		emit(map[string]string{"error": err.Error()})
		return code
	}
	if err := fs.Parse(args); err != nil {
		return fail(2, err)
	}
	if fs.NArg() > 0 {
		return fail(2, fmt.Errorf("unexpected argument %q", fs.Arg(0)))
	}
	if e == nil {
		var err error
		if e, err = newEnv(); err != nil {
			return fail(1, err)
		}
	}

	switch cmd {
	case "install":
		r, err := e.install()
		if err != nil {
			return fail(1, err)
		}
		emit(r)
	case "doctor":
		emit(e.doctor())
	case "integrate":
		if *plan && *apply {
			return fail(2, errors.New("--plan and --apply are mutually exclusive"))
		}
		o := integrateOpts{apply: *apply, shellWrapper: *shellWrapper, service: *service}
		var err error
		if o.harness, err = parseList(*harness, harnessIDs); err != nil {
			return fail(2, err)
		}
		if o.remove, err = parseList(*remove, removeIDs); err != nil {
			return fail(2, err)
		}
		if len(o.harness) == 0 && len(o.remove) == 0 && !o.shellWrapper && !o.service {
			for _, h := range harnessIDs {
				if e.findHarness(h) != "" {
					o.harness = append(o.harness, h)
				}
			}
		}
		r, err := e.integrate(o)
		if err != nil {
			return fail(1, err)
		}
		emit(r)
	case "uninstall":
		if _, ok := levelNames[*level]; !ok {
			return fail(2, fmt.Errorf("--level must be stop, unhook or purge (got %q)", *level))
		}
		emit(e.uninstall(*level, *uploads))
	default:
		return fail(2, fmt.Errorf("unknown installer command %q", cmd))
	}
	return 0
}

// asciiJSON encodes v as one JSON document plus newline, without HTML
// escaping and with every non-ASCII character \u-escaped, so the app can
// read it whatever the remote locale (as on the bridge, see PROTOCOL.md).
func asciiJSON(v any, indent bool) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(v); err != nil {
		buf.Reset()
		enc.Encode(map[string]string{"error": err.Error()})
	}
	out := make([]byte, 0, buf.Len())
	for _, r := range buf.String() {
		switch {
		case r < 0x80:
			out = append(out, byte(r))
		case r > 0xffff:
			r1, r2 := utf16.EncodeRune(r)
			out = fmt.Appendf(out, `\u%04x\u%04x`, r1, r2)
		default:
			out = fmt.Appendf(out, `\u%04x`, r)
		}
	}
	return out
}

func parseList(s string, allowed []string) ([]string, error) {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !slices.Contains(allowed, p) {
			return nil, fmt.Errorf("unknown %q (expected one of %s)", p, strings.Join(allowed, ", "))
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out, nil
}

// install creates the layout and writes (or refreshes) the manifest.
func (e *env) install() (*InstallResult, error) {
	if e.mErr != nil {
		return nil, e.mErr
	}
	l := e.l
	candidates := []string{filepath.Dir(l.Root), l.Root, l.BinDir, l.StateDir, l.DataDir, l.LogDir, l.RunDir}
	if d := l.HolderSocketDir(); d != "" {
		candidates = append(candidates, d)
	}
	var created []string
	for _, d := range candidates {
		if !exists(d) {
			created = append(created, d)
		}
	}
	if err := l.EnsureDirs(); err != nil {
		return nil, err
	}
	for _, d := range created {
		e.m.addDir(d)
	}
	if exists(l.Bin) {
		e.m.setFile(FileEntry{Path: l.Bin, Kind: "binary", SHA256: fileSHA256(l.Bin)})
	}
	// Executables renamed aside by an update on Windows (in use at the
	// time); remove the ones no longer running.
	if old, _ := filepath.Glob(filepath.Join(l.BinDir, "stagent*.old*")); len(old) > 0 {
		for _, f := range old {
			os.Remove(f)
		}
	}
	e.stopLegacyDaemon()
	e.touchManifest()
	if err := e.saveManifest(); err != nil {
		return nil, err
	}
	return &InstallResult{OK: true, Version: version.Version, Layout: layoutInfo(l), Notes: nonNil(e.notes)}, nil
}

// stopLegacyDaemon shuts down (best effort) a daemon of a version before
// 0.3.0 that still listens in $XDG_RUNTIME_DIR, where Linux sockets lived
// until they moved out of logind's reach (paths). Clients now look for the
// daemon in the new place, so the old one would only keep running unseen.
func (e *env) stopLegacyDaemon() {
	x := e.getenv("XDG_RUNTIME_DIR")
	if e.goos != "linux" || e.l.Isolated || x == "" {
		return
	}
	addr := filepath.Join(x, "stagent", "stagent.sock")
	if addr == e.l.DaemonAddr || e.daemonAt(addr).Shutdown() != nil {
		return
	}
	e.note("Stopped the daemon of an earlier stagent version at %s. Sessions started before the update keep running, but the app no longer lists or reaches them.", addr)
}
