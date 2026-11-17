package holder

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/wire"
)

// OpenCode V2's terminal UI talks to a background service shared by every
// terminal (`opencode serve --service`), whose plugins keep the
// environment of the opencode that started it: the hooks of stagent's
// plugin would carry another session's STAGENT_SESSION_ID. With
// --standalone the UI runs a server of its own as its child, with its own
// environment, so the plugin's hooks are the session's (#1589).
const (
	openCodeStandalone     = "--standalone"
	openCodeVersionTimeout = 5 * time.Second
)

// openCodeCommand returns argv with --standalone after the program when
// argv runs the terminal UI of an OpenCode V2 — directly or in the end,
// after `stagent task run … --` (wire.AgentArgv). Its commands (the
// harness's batch runs), a UI given --standalone or connecting to a
// server (--server) and an opencode whose version is not 2 or later (V1
// has no such flag) are left as they are. The version is asked the way
// pty.Start runs the program: same lookup, dir and env.
func openCodeCommand(argv []string, dir string, env []string) []string {
	agent := wire.AgentArgv(argv)
	if harness.Command(agent) != wire.HarnessOpenCode || harness.BatchArgs(wire.HarnessOpenCode, agent) {
		return argv
	}
	for _, a := range agent[1:] {
		if a == openCodeStandalone || a == "--server" || strings.HasPrefix(a, "--server=") {
			return argv
		}
	}
	if !openCodeVersions.v2(agent[0], dir, env) {
		return argv
	}
	pre := len(argv) - len(agent)
	out := make([]string, 0, len(argv)+1)
	out = append(out, argv[:pre+1]...)
	out = append(out, openCodeStandalone)
	return append(out, agent[1:]...)
}

// openCodeVersions remembers what the opencode binaries the holder ran
// answered, so that launching the same binary again does not wait for
// `opencode --version` (up to openCodeVersionTimeout) each time.
var openCodeVersions = &openCodeVersionCache{probe: openCodeV2}

// openCodeVersionCache asks probe whether a binary is an OpenCode V2 once
// per binary file: an entry holds for the resolved path as long as the
// file there keeps its modification time and size, so an upgrade in
// place (or a link pointed at another release) is asked again. Answers
// probe could not give (the program failed, timed out or printed no
// version) are not kept: the next launch asks again.
type openCodeVersionCache struct {
	probe func(bin, dir string, env []string) (v2, ok bool)

	mu      sync.Mutex
	entries map[string]openCodeVersionEntry // by resolved path
}

type openCodeVersionEntry struct {
	mod  time.Time
	size int64
	v2   bool
}

func (c *openCodeVersionCache) v2(bin, dir string, env []string) bool {
	path, fi := openCodeBinary(bin, dir)
	if fi != nil {
		c.mu.Lock()
		e, hit := c.entries[path]
		c.mu.Unlock()
		if hit && e.mod.Equal(fi.ModTime()) && e.size == fi.Size() {
			return e.v2
		}
	}
	// Not under the lock: two launches racing on a new binary both ask,
	// rather than one waiting out the other's probe.
	v2, ok := c.probe(bin, dir, env)
	if ok && fi != nil {
		c.mu.Lock()
		if c.entries == nil {
			c.entries = map[string]openCodeVersionEntry{}
		}
		c.entries[path] = openCodeVersionEntry{mod: fi.ModTime(), size: fi.Size(), v2: v2}
		c.mu.Unlock()
	}
	return v2
}

// openCodeBinary resolves bin the way exec.Command does (a bare name on
// PATH, a relative path from dir) through its links to the file that
// runs; a nil FileInfo when it cannot, and the answer is then not kept.
func openCodeBinary(bin, dir string) (string, os.FileInfo) {
	path := exec.Command(bin).Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return "", nil
	}
	return path, fi
}

// openCodeVersion finds the version `opencode --version` prints: "opencode
// v2.3.0" (V2), "1.19.5" (V1).
var openCodeVersion = regexp.MustCompile(`(?m)^(?:opencode\s+)?v?(\d+)\.\d+`)

// openCodeV2 runs `bin --version`; ok is false when it gave no version.
func openCodeV2(bin, dir string, env []string) (v2, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), openCodeVersionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Dir, cmd.Env = dir, env
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return false, false
	}
	m := openCodeVersion.FindSubmatch(out)
	if m == nil {
		return false, false
	}
	major, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return false, false
	}
	return major >= 2, true
}
