package follow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/ptable"
)

// invocation says how to run a multiplexer's CLI the way its client runs:
// the client's own binary (a different version may not speak the server's
// protocol, and the exec channel's PATH often lacks Homebrew or ~/.local)
// with the client's environment (TMUX_TMPDIR, XDG_RUNTIME_DIR,
// ZELLIJ_SOCKET_DIR, … locate the server's socket). The client is always a
// process of stagent's own user (locator.owns), and run drops the dynamic
// loader's variables (muxEnv).
type invocation struct {
	bin string   // absolute path, or a name looked up in env's PATH
	env []string // nil: stagent's own environment
}

type tmuxClient struct {
	clientPID, panePID int
	paneID, session    string
}

type zellijClient struct {
	id, pane string // pane: "terminal_<n>" or "plugin_<n>"
}

type herdrPane struct {
	ID      string `json:"pane_id"`
	Focused bool   `json:"focused"`
}

type herdrProcessInfo struct {
	ShellPID   int `json:"shell_pid"`
	Foreground []struct {
		PID int `json:"pid"`
	} `json:"foreground_processes"`
}

// muxSystem queries multiplexer servers. The locator only depends on this
// interface so its decisions are tested with canned answers.
type muxSystem interface {
	tmuxClients(inv invocation, socket []string) ([]tmuxClient, error)
	zellijClients(inv invocation, session string) ([]zellijClient, error)
	// screenWindow is the number of the window the session shows.
	screenWindow(inv invocation, sty string) (int, error)
	herdrPanes(inv invocation, session string) ([]herdrPane, error)
	herdrProcessInfo(inv invocation, session, pane string) (herdrProcessInfo, error)
}

// muxTimeout bounds one multiplexer CLI call.
const muxTimeout = 3 * time.Second

// execMux runs the real CLIs.
type execMux struct{}

func (execMux) run(inv invocation, args ...string) ([]byte, error) {
	bin := inv.bin
	if !filepath.IsAbs(bin) {
		bin = lookPath(bin, inv.env)
	}
	ctx, cancel := context.WithTimeout(context.Background(), muxTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = muxEnv(inv.env)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, fmt.Errorf("%s %s: %v %s", filepath.Base(bin), strings.Join(args, " "), err, msg)
	}
	return out, nil
}

// muxEnv is the environment a multiplexer CLI runs with: env (stagent's own
// when nil) without the dynamic loader's variables (LD_PRELOAD,
// LD_LIBRARY_PATH, LD_AUDIT, DYLD_INSERT_LIBRARIES, …). They are dropped
// whoever's environment it is: they load code into the CLI that was meant
// for the program they were set for, and the CLI must run as the client's
// binary, not as whatever a variable injects into it.
func muxEnv(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, "LD_") && !strings.HasPrefix(kv, "DYLD_") {
			out = append(out, kv)
		}
	}
	return out
}

// lookPath finds name in the PATH of env (falling back to stagent's own),
// returning name unchanged when it is nowhere, so exec reports the error.
func lookPath(name string, env []string) string {
	path := ptable.Lookup(env, "PATH")
	if path == "" {
		path = os.Getenv("PATH")
	}
	for _, dir := range filepath.SplitList(path) {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	return name
}

// tmuxFormat has literal tabs between the fields.
const tmuxFormat = "#{client_pid}\t#{pane_pid}\t#{pane_id}\t#{session_name}"

func (m execMux) tmuxClients(inv invocation, socket []string) ([]tmuxClient, error) {
	out, err := m.run(inv, append(slices.Clip(socket), "list-clients", "-F", tmuxFormat)...)
	if err != nil {
		return nil, err
	}
	return parseTmuxClients(out), nil
}

func parseTmuxClients(out []byte) []tmuxClient {
	var cs []tmuxClient
	for line := range strings.SplitSeq(string(out), "\n") {
		f := strings.SplitN(strings.TrimRight(line, "\r"), "\t", 4)
		if len(f) != 4 {
			continue
		}
		client, err1 := strconv.Atoi(f[0])
		pane, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		cs = append(cs, tmuxClient{clientPID: client, panePID: pane, paneID: f[2], session: f[3]})
	}
	return cs
}

func (m execMux) zellijClients(inv invocation, session string) ([]zellijClient, error) {
	out, err := m.run(inv, "--session", session, "action", "list-clients")
	if err != nil {
		return nil, err
	}
	return parseZellijClients(out), nil
}

// parseZellijClients reads the table `zellij action list-clients` prints:
//
//	CLIENT_ID ZELLIJ_PANE_ID RUNNING_COMMAND
//	1         terminal_1     vim file
func parseZellijClients(out []byte) []zellijClient {
	var cs []zellijClient
	for line := range strings.SplitSeq(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] == "CLIENT_ID" {
			continue
		}
		cs = append(cs, zellijClient{id: f[0], pane: f[1]})
	}
	return cs
}

func (m execMux) screenWindow(inv invocation, sty string) (int, error) {
	out, err := m.run(inv, "-S", sty, "-Q", "number")
	if err != nil {
		return 0, err
	}
	return parseScreenNumber(out)
}

var firstNumber = regexp.MustCompile(`\d+`)

// parseScreenNumber reads the answer to `screen -Q number`, "<n> (<title>)"
// (the non-query form is "This is window <n> (<title>).").
func parseScreenNumber(out []byte) (int, error) {
	m := firstNumber.Find(out)
	if m == nil {
		return 0, fmt.Errorf("screen: no window number in %q", bytes.TrimSpace(out))
	}
	return strconv.Atoi(string(m))
}

// herdrArgs prefixes a herdr command with the session flag (none for the
// default session).
func herdrArgs(session string, args ...string) []string {
	if session == "" {
		return args
	}
	return append([]string{"--session", session}, args...)
}

func (m execMux) herdrPanes(inv invocation, session string) ([]herdrPane, error) {
	out, err := m.run(inv, herdrArgs(session, "pane", "list")...)
	if err != nil {
		return nil, err
	}
	var r struct {
		Panes []herdrPane `json:"panes"`
	}
	if err := decodeHerdr(out, &r); err != nil {
		return nil, err
	}
	return r.Panes, nil
}

func (m execMux) herdrProcessInfo(inv invocation, session, pane string) (herdrProcessInfo, error) {
	out, err := m.run(inv, herdrArgs(session, "pane", "process-info", "--pane", pane)...)
	if err != nil {
		return herdrProcessInfo{}, err
	}
	var r struct {
		ProcessInfo herdrProcessInfo `json:"process_info"`
	}
	if err := decodeHerdr(out, &r); err != nil {
		return herdrProcessInfo{}, err
	}
	return r.ProcessInfo, nil
}

// decodeHerdr unwraps herdr's CLI envelope, {"id", "result"} or
// {"id", "error": {code, message}} (the exit status is 0 either way).
func decodeHerdr(out []byte, result any) error {
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &env); err != nil {
		return fmt.Errorf("herdr: %w", err)
	}
	if env.Error != nil {
		return fmt.Errorf("herdr: %s: %s", env.Error.Code, env.Error.Message)
	}
	if env.Result == nil {
		return errors.New("herdr: empty result")
	}
	return json.Unmarshal(env.Result, result)
}
