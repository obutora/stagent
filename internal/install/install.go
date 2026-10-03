// Package install implements stagent's ledger-based installer: `install`
// (layout + manifest; after an update, the running daemon and the shell
// wrapper blocks already in place), `integrate` (harness hooks, shell
// wrappers, login service; planned as unified diffs before anything is
// written), `doctor` (state report, orphans) and `uninstall` (stop / unhook
// / purge). The app runs these over plain SSH exec and parses their JSON
// (PROTOCOL.md).
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
	"time"
	"unicode/utf16"

	"github.com/obutora/stagent/internal/hostid"
	"github.com/obutora/stagent/internal/version"
)

// InstallResult is the output of `stagent install`.
type InstallResult struct {
	OK      bool       `json:"ok"`
	Version string     `json:"version"`
	Layout  LayoutInfo `json:"layout"`
	// ReplacedDaemon is the version of the running daemon this install
	// replaced with its own (an update); empty when none was replaced.
	ReplacedDaemon string `json:"replaced_daemon,omitempty"`
	// Changes are the shell wrapper files rewritten to this version's block
	// (refreshShellWrapper), in integrate's form.
	Changes []*Change `json:"changes"`
	Notes   []string  `json:"notes"`
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
	var plan, apply, shellWrapper, service, linger, terminal, uploads *bool
	var harness, remove, level *string
	switch cmd {
	case "integrate":
		plan = fs.Bool("plan", false, "show the changes without applying them (default)")
		apply = fs.Bool("apply", false, "apply the changes")
		harness = fs.String("harness", "", "comma-separated harnesses to integrate: claude,codex,omp")
		shellWrapper = fs.Bool("shell-wrapper", false, "wrap claude/codex/omp in shell rc files")
		service = fs.Bool("service", false, "run the daemon as a login service")
		linger = fs.Bool("linger", false, "turn on lingering (`loginctl enable-linger`, Linux) so agents keep running after logout")
		terminal = fs.Bool("terminal", false, "add stagent to the processes Terminal.app does not ask about before closing a window (macOS)")
		remove = fs.String("remove", "", "comma-separated integrations to remove: claude,codex,omp,shell-wrapper,service,linger,terminal (linger, terminal: only what stagent turned on or added)")
	case "uninstall":
		level = fs.String("level", "", "stop | unhook | purge")
		uploads = fs.Bool("uploads", false, "also delete ~/.ssh-term/uploads (purge)")
		linger = fs.Bool("linger", false, "also turn lingering off when stagent turned it on (purge)")
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
		o := integrateOpts{apply: *apply, shellWrapper: *shellWrapper, service: *service, linger: *linger, terminal: *terminal}
		var err error
		if o.harness, err = parseList(*harness, harnessIDs); err != nil {
			return fail(2, err)
		}
		if o.remove, err = parseList(*remove, removeIDs); err != nil {
			return fail(2, err)
		}
		if len(o.harness) == 0 && len(o.remove) == 0 && !o.shellWrapper && !o.service && !o.linger && !o.terminal {
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
		emit(e.uninstall(*level, *uploads, *linger))
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

// install creates the layout and writes (or refreshes) the manifest. Run
// after the binary was replaced, it completes the update: the shell
// wrapper blocks already in place get this version's block and a running
// daemon of another version is replaced.
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
	if _, err := hostid.Ensure(l.HostID); err != nil {
		return nil, err
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
	changes := e.refreshShellWrapper()
	replaced := e.replaceDaemon()
	e.touchManifest()
	if err := e.saveManifest(); err != nil {
		return nil, err
	}
	return &InstallResult{OK: true, Version: version.Version, Layout: layoutInfo(l), ReplacedDaemon: replaced, Changes: changes, Notes: nonNil(e.notes)}, nil
}

// refreshShellWrapper rewrites the shell wrapper blocks already on the host
// to this version's block, so an update changes what they run; a host
// without the wrapper gets none. Where bash has the block, an existing
// ~/.bash_profile gets it too, as `integrate --shell-wrapper` does. A file
// that cannot be read or written is reported as a change with its error,
// like integrate's, and does not stop the others.
func (e *env) refreshShellWrapper() []*Change {
	changes := []*Change{}
	bash := false
	for _, t := range e.shellTargets() {
		cur, err := readOptional(t.path)
		if err != nil {
			changes = append(changes, skipChange(t.id, &targetError{t.id, t.path, err}))
			continue
		}
		has := cur != nil && t.present(cur) != ""
		if t.id == "shell-bash" {
			bash = has
		}
		if !has && !(t.id == "shell-bash-profile" && bash && cur != nil) {
			continue
		}
		c, err := e.addChange(t)
		switch {
		case err != nil:
			changes = append(changes, skipChange(t.id, err))
		case c != nil:
			if err := c.apply(); err != nil {
				c.Error, c.ErrorCode = err.Error(), errorCode(err)
			}
			changes = append(changes, c)
		}
	}
	return changes
}

// replaceDaemon restarts a running daemon of another version (the binary
// was just updated) as this version: it is stopped like `uninstall --level
// stop` stops it, then started again through the login service when one
// is installed, otherwise detached. The holders keep their sessions and
// register them with the new daemon. Returns the replaced version, ""
// when nothing was replaced; a failure is reported in the notes.
func (e *env) replaceDaemon() string {
	st, err := e.daemon.Status()
	if err != nil || st.Version == version.Version {
		return ""
	}
	fail := func(err error) string {
		e.note("The running daemon is version %s, this binary %s, and replacing it failed: %v. Restart it with `stagent uninstall --level stop`.", st.Version, version.Version, err)
		return ""
	}
	if _, _, err := e.shutdownDaemon(st); err != nil {
		return fail(err)
	}
	if err := e.startDaemon(); err != nil {
		return fail(err)
	}
	deadline := time.Now().Add(e.settle)
	for {
		now, err := e.daemon.Status()
		if err == nil && now.Version == version.Version {
			return st.Version
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = fmt.Errorf("version %s answered instead", now.Version)
			}
			return fail(fmt.Errorf("the new daemon did not come up: %w", err))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// startDaemon starts the daemon through the installed login service, or
// detached when there is none or the service manager refuses.
func (e *env) startDaemon() error {
	if installed, _ := e.serviceState(); installed {
		var err error
		switch e.serviceKind() {
		case "systemd":
			err = e.runCmd("systemctl", "--user", "restart", systemdUnit)
		case "launchd":
			for _, d := range e.launchdDomains() {
				if err = e.runCmd("launchctl", "kickstart", "-k", d+"/"+launchdLabel); err == nil {
					break
				}
			}
		case "schtasks":
			err = e.runCmd("schtasks", "/Run", "/TN", schtasksDaemon)
		}
		if err == nil {
			return nil
		}
	}
	return e.spawnDaemon()
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
