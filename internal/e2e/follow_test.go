//go:build linux

package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/obutora/stagent/internal/wire"
)

// A test binary started as "sshd" with these variables plays the SSH
// server process of one connection: it runs the terminal command on a pty
// and `stagent follow` on its own stdin/stdout, as sshd serves a terminal
// and an exec channel of one connection.
const (
	envFakeSSHDTerminal = "STAGENT_E2E_FAKE_SSHD_TERMINAL"
	envFakeSSHDStagent  = "STAGENT_E2E_FAKE_SSHD_STAGENT"
)

func TestMain(m *testing.M) {
	if term := os.Getenv(envFakeSSHDTerminal); term != "" {
		os.Exit(fakeSSHD(term, os.Getenv(envFakeSSHDStagent)))
	}
	os.Exit(m.Run())
}

func fakeSSHD(terminal, stagent string) int {
	env := withoutEnv(os.Environ(), envFakeSSHDTerminal, envFakeSSHDStagent)
	shell := exec.Command("/bin/sh", "-c", terminal)
	shell.Env = env
	ptmx, err := pty.StartWithSize(shell, &pty.Winsize{Rows: 40, Cols: 120})
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake sshd:", err)
		return 1
	}
	go io.Copy(io.Discard, ptmx)
	follow := exec.Command(stagent, "follow")
	follow.Env = env
	follow.Stdin, follow.Stdout, follow.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = follow.Run()
	syscall.Kill(-shell.Process.Pid, syscall.SIGKILL) // the terminal's session
	shell.Wait()
	ptmx.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake sshd: follow:", err)
		return 1
	}
	return 0
}

// withoutEnv drops the named variables and those of an enclosing
// multiplexer session (the tests may themselves run in one).
func withoutEnv(env []string, drop ...string) []string {
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(k, "TMUX"), strings.HasPrefix(k, "ZELLIJ"), strings.HasPrefix(k, "HERDR"),
			k == "STY", k == "WINDOW", k == "SSH_CONNECTION":
			continue
		}
		skip := false
		for _, d := range drop {
			skip = skip || k == d
		}
		if !skip {
			out = append(out, kv)
		}
	}
	return out
}

// fakeClaude is a stand-in for Claude Code: it registers its session the
// way the real one does, writes a two-message transcript and keeps running.
const fakeClaude = `#!/bin/sh
cfg=${CLAUDE_CONFIG_DIR:-$HOME/.claude}
id=$(printf '%08d-0000-4000-8000-000000000000' $$)
mkdir -p "$cfg/sessions" "$cfg/projects/-e2e"
t="$cfg/projects/-e2e/$id.jsonl"
printf '{"type":"user","message":{"role":"user","content":"hello from %s"},"timestamp":"2026-09-30T00:00:00Z"}\n' $$ >> "$t"
printf '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]},"timestamp":"2026-09-30T00:00:01Z"}\n' >> "$t"
printf '{"pid":%s,"sessionId":"%s","cwd":"%s","startedAt":%s000}\n' $$ "$id" "$PWD" "$(date +%s)" > "$cfg/sessions/$$.json"
while :; do sleep 1; done
`

// agentEnv returns an environment whose PATH starts with a directory
// holding the fake claude, and that directory.
func agentEnv(t *testing.T) (env []string, bin string) {
	t.Helper()
	bin = t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	env = withoutEnv(os.Environ(), "PATH", "CLAUDE_CONFIG_DIR", "TERM")
	env = append(env,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"CLAUDE_CONFIG_DIR="+filepath.Join(t.TempDir(), "claude"),
		"TERM=xterm-256color",
	)
	return env, bin
}

type followFrame struct {
	T         string `json:"t"`
	Version   string `json:"version"`
	Mux       string `json:"mux"`
	MuxDetail string `json:"mux_detail"`
	Reason    string `json:"reason"`
	Agents    []struct {
		Key            string `json:"key"`
		Harness        string `json:"harness"`
		PID            int    `json:"pid"`
		Pane           string `json:"pane"`
		TranscriptPath string `json:"transcript_path"`
		Foreground     bool   `json:"foreground"`
	} `json:"agents"`
	Selected *string        `json:"selected"`
	Key      string         `json:"key"`
	Path     string         `json:"path"`
	Reset    bool           `json:"reset"`
	Messages []wire.Message `json:"messages"`
	Message  string         `json:"message"`
}

type follower struct {
	in     io.WriteCloser
	frames chan followFrame
	seen   []string
}

// startFollow runs `stagent follow` as the exec channel of a fake SSH
// connection whose terminal runs terminal.
func startFollow(t *testing.T, stagent, terminal string, env []string) *follower {
	t.Helper()
	sshd := filepath.Join(t.TempDir(), "sshd")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, sshd); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sshd)
	cmd.Env = append(env, envFakeSSHDTerminal+"="+terminal, envFakeSSHDStagent+"="+stagent)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	f := &follower{in: in, frames: make(chan followFrame, 100)}
	go func() {
		defer close(f.frames)
		sc := bufio.NewScanner(out)
		sc.Buffer(nil, wire.MaxLine)
		for sc.Scan() {
			var fr followFrame
			if json.Unmarshal(sc.Bytes(), &fr) == nil {
				f.frames <- fr
			}
		}
	}()
	t.Cleanup(func() {
		in.Close() // stdin EOF ends follow, then the fake sshd
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("fake sshd: %v", err)
			}
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
			t.Error("stagent follow did not exit on stdin EOF")
		}
	})
	return f
}

// waitFor returns the first frame from now on matching pred.
func (f *follower) waitFor(t *testing.T, what string, pred func(followFrame) bool) followFrame {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case fr, ok := <-f.frames:
			if !ok {
				t.Fatalf("stream ended waiting for %s; saw:%s", what, strings.Join(f.seen, ""))
			}
			b, _ := json.Marshal(fr)
			f.seen = append(f.seen, "\n  "+string(b))
			if pred(fr) {
				return fr
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s; saw:%s", what, strings.Join(f.seen, ""))
		}
	}
}

func (f *follower) send(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(f.in, line+"\n"); err != nil {
		t.Fatal(err)
	}
}

// targetWith waits for a target whose selected agent is a foreground claude
// with a transcript in pane (and the given multiplexer), returning the
// frame and the agent's transcript path.
func (f *follower) targetWith(t *testing.T, mux, pane string) (followFrame, string) {
	t.Helper()
	fr := f.waitFor(t, fmt.Sprintf("a %s target with claude in pane %q", mux, pane), func(fr followFrame) bool {
		if fr.T != "target" || fr.Mux != mux || fr.Selected == nil || len(fr.Agents) == 0 {
			return false
		}
		a := fr.Agents[0]
		return *fr.Selected == a.Key && a.Harness == "claude" && a.Pane == pane && a.Foreground && a.TranscriptPath != ""
	})
	return fr, fr.Agents[0].TranscriptPath
}

func (f *follower) messagesOf(t *testing.T, path string, reset bool) followFrame {
	t.Helper()
	return f.waitFor(t, "messages of "+path, func(fr followFrame) bool {
		return fr.T == "messages" && fr.Path == path && fr.Reset == reset
	})
}

func lookTool(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not installed", name)
	}
	return p
}

func TestFollowAgentInTerminalShell(t *testing.T) {
	stagent := buildStagent(t)
	env, _ := agentEnv(t)
	f := startFollow(t, stagent, "exec claude", env)
	hello := f.waitFor(t, "hello", func(fr followFrame) bool { return fr.T == "hello" })
	if hello.Version == "" {
		t.Fatalf("hello %+v", hello)
	}
	tg, path := f.targetWith(t, "none", "")
	pid := tg.Agents[0].PID
	m := f.messagesOf(t, path, true)
	if len(m.Messages) != 2 || m.Messages[0].Text != fmt.Sprintf("hello from %d", pid) || m.Messages[1].Text != "hi" {
		t.Fatalf("messages %+v", m.Messages)
	}

	fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(fh, `{"type":"user","message":{"role":"user","content":"appended"},"timestamp":"2026-09-30T00:00:02Z"}`)
	fh.Close()
	if m := f.messagesOf(t, path, false); len(m.Messages) != 1 || m.Messages[0].Text != "appended" {
		t.Fatalf("tail %+v", m.Messages)
	}

	f.send(t, `{"op":"older","before":1}`)
	f.waitFor(t, "page", func(fr followFrame) bool { return fr.T == "page" && fr.Key == tg.Agents[0].Key })
}

func TestFollowTmuxPane(t *testing.T) {
	tmux := lookTool(t, "tmux")
	stagent := buildStagent(t)
	env, _ := agentEnv(t)
	sock := filepath.Join(t.TempDir(), "tmux.sock")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(tmux, append([]string{"-S", sock, "-f", "/dev/null"}, args...)...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("tmux %v: %v\n%s", args, err, out)
		}
	}
	run("new-session", "-d", "-s", "e2e", "-x", "120", "-y", "40", "claude")
	t.Cleanup(func() { exec.Command(tmux, "-S", sock, "kill-server").Run() })

	f := startFollow(t, stagent, "exec tmux -S "+sock+" attach -t e2e", env)
	tg, path := f.targetWith(t, "tmux", "%0")
	if tg.MuxDetail != "e2e %0" {
		t.Fatalf("mux_detail %q", tg.MuxDetail)
	}
	f.messagesOf(t, path, true)

	// A new window becomes the one the client shows.
	run("new-window", "-t", "e2e", "claude")
	tg, next := f.targetWith(t, "tmux", "%1")
	if next == path || tg.MuxDetail != "e2e %1" {
		t.Fatalf("after new-window %+v", tg)
	}
	f.messagesOf(t, next, true)
}

func TestFollowZellijPane(t *testing.T) {
	zellij := lookTool(t, "zellij")
	stagent := buildStagent(t)
	env, bin := agentEnv(t)
	env = append(env, "SHELL="+filepath.Join(bin, "claude")) // the first pane runs the agent
	session := fmt.Sprintf("stagent-e2e-%d", os.Getpid())
	t.Cleanup(func() {
		for _, args := range [][]string{{"kill-session", session}, {"delete-session", "--force", session}} {
			cmd := exec.Command(zellij, args...)
			cmd.Env = env
			cmd.Run()
		}
	})

	f := startFollow(t, stagent, "exec zellij -s "+session, env)
	tg, path := f.targetWith(t, "zellij", "terminal_0")
	if tg.MuxDetail != session+" terminal_0" {
		t.Fatalf("mux_detail %q", tg.MuxDetail)
	}
	f.messagesOf(t, path, true)
}

func TestFollowHerdrPane(t *testing.T) {
	herdr := lookTool(t, "herdr")
	stagent := buildStagent(t)
	env, bin := agentEnv(t)
	env = append(env, "SHELL="+filepath.Join(bin, "claude"))
	session := fmt.Sprintf("stagent-e2e-%d", os.Getpid())
	t.Cleanup(func() {
		for _, args := range [][]string{{"session", "stop", session}, {"session", "delete", session}} {
			cmd := exec.Command(herdr, args...)
			cmd.Env = env
			cmd.Run()
		}
	})

	f := startFollow(t, stagent, "exec herdr --session "+session, env)
	tg, path := f.targetWith(t, "herdr", "w1:p1")
	if tg.MuxDetail != session+" w1:p1" {
		t.Fatalf("mux_detail %q", tg.MuxDetail)
	}
	f.messagesOf(t, path, true)
}
