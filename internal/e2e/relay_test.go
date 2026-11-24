//go:build linux

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/pathstest"
	"github.com/obutora/stagent/internal/relay"
	"github.com/obutora/stagent/internal/wire"
)

// fakeAgent stands in for the target's harness: it records its arguments
// and where it started, then waits.
const fakeAgent = `#!/bin/sh
printf '%s\n' "$@" > "$HOME/agent-args"
pwd -P > "$HOME/agent-cwd"
echo "$STAGENT_RELAY_DOCUMENT" > "$HOME/agent-doc"
exec sleep 60
`

// relayHost is one host of a relay test: its own home, daemon and bridge.
type relayHost struct {
	home string
	l    *paths.Layout
	a    *app
}

// startRelayHost starts a host whose login shell puts the fake agent
// (as claude) first on PATH.
func startRelayHost(t *testing.T, bin string) *relayHost {
	t.Helper()
	home := t.TempDir()
	stub := filepath.Join(home, "stub-bin")
	if err := os.Mkdir(stub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stub, "claude"), []byte(fakeAgent), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := fmt.Sprintf("PS1='$ '\nexport PATH='%s':\"$PATH\"\n", stub)
	if err := os.WriteFile(filepath.Join(home, ".bash_profile"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	l := pathstest.SetHome(t, home)
	t.Cleanup(func() {
		shutdownDaemon(l)
		if t.Failed() {
			logs, _ := filepath.Glob(filepath.Join(l.LogDir, "*.log"))
			for _, f := range logs {
				b, _ := os.ReadFile(f)
				t.Logf("%s: %s:\n%s", home, filepath.Base(f), b)
			}
		}
	})
	h := &relayHost{home: home, l: l, a: startBridgeFor(t, bin, home)}
	// Checkouts in the home are the host's (a watch folder).
	h.a.call(t, wire.MethodWorktreeWatch, wire.WorktreeWatchParams{Add: home}, nil)
	return h
}

func gitAt(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// relayRepos makes a bare remote and the source's checkout of it, on a
// branch with one commit no remote has, a staged and an unstaged change,
// an executable untracked file and an untracked link; the conversation
// worked in its sub directory. setup is the orca.yaml committed ("": none).
func relayRepos(t *testing.T, root, setup string) (origin, src string) {
	t.Helper()
	seed := filepath.Join(root, "seed")
	gitAt(t, root, "init", "-q", "-b", "main", seed)
	os.WriteFile(filepath.Join(seed, "a.txt"), []byte("one\n"), 0o644)
	os.MkdirAll(filepath.Join(seed, "sub"), 0o755)
	os.WriteFile(filepath.Join(seed, "sub", "b.txt"), []byte("b\n"), 0o644)
	if setup != "" {
		os.WriteFile(filepath.Join(seed, "orca.yaml"), []byte("scripts:\n  setup: |\n    "+setup+"\n"), 0o644)
	}
	gitAt(t, seed, "add", ".")
	gitAt(t, seed, "commit", "-q", "-m", "init")
	gitAt(t, root, "clone", "-q", "--bare", seed, "origin.git")
	origin = filepath.Join(root, "origin.git")
	src = filepath.Join(root, "src")
	gitAt(t, root, "clone", "-q", origin, src)
	gitAt(t, src, "switch", "-q", "-c", "feat/relay")
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("two\n"), 0o644)
	gitAt(t, src, "commit", "-q", "-am", "local only")
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("three\n"), 0o644)
	gitAt(t, src, "add", "a.txt")
	os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("b changed\n"), 0o644)
	os.WriteFile(filepath.Join(src, "sub", "new.sh"), []byte("#!/bin/sh\necho new\n"), 0o755)
	os.Symlink("new.sh", filepath.Join(src, "sub", "link"))
	return origin, src
}

const relayConv = "11111111-2222-3333-4444-555555555555"

// writeConversation puts a Claude Code conversation that worked in cwd
// under home: a request, a tool call with its output, thinking and an
// answer.
func writeConversation(t *testing.T, home, cwd string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", "-src")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := func(v map[string]any) string {
		v["cwd"], v["sessionId"], v["timestamp"] = cwd, relayConv, "2026-11-24T10:00:00.000Z"
		b, _ := json.Marshal(v)
		return string(b) + "\n"
	}
	lines := rec(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": "Make b.txt say changed"}}) +
		rec(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "model": "m", "content": []any{
			map[string]any{"type": "thinking", "thinking": "SECRET-THOUGHT"},
			map[string]any{"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]any{"command": "cat sub/b.txt"}},
		}}}) +
		rec(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "TOOL-OUTPUT-B"},
		}}}) +
		rec(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "model": "m", "content": []any{
			map[string]any{"type": "text", "text": "Changed it."},
		}}})
	if err := os.WriteFile(filepath.Join(dir, relayConv+".jsonl"), []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
}

func prepare(t *testing.T, h *relayHost, target, sourceAgent string) wire.RelayPrepareResult {
	t.Helper()
	var res wire.RelayPrepareResult
	h.a.call(t, wire.MethodRelayPrepare, wire.RelayPrepareParams{
		Harness: wire.HarnessClaude, ConversationID: relayConv, Target: target, SourceAgent: sourceAgent,
		SourceHost: wire.RelayHost{HostID: "aaaa", Label: "source-host"}, Instruction: "Finish the change", Lang: wire.LangJa,
		Memo: true,
	}, &res)
	return res
}

// carry reads the package from s and writes it to t in small chunks.
func carry(t *testing.T, s, d *relayHost, res wire.RelayPrepareResult) {
	t.Helper()
	var rr wire.RelayReceiveResult
	d.a.call(t, wire.MethodRelayReceive, wire.RelayReceiveParams{RelayID: res.RelayID, Size: res.Size, SHA256: res.SHA256}, &rr)
	off := rr.Received
	for {
		var chunk wire.RelayReadResult
		s.a.call(t, wire.MethodRelayRead, wire.RelayReadParams{RelayID: res.RelayID, Offset: off, MaxBytes: 4096}, &chunk)
		var wr wire.RelayWriteResult
		d.a.call(t, wire.MethodRelayWrite, wire.RelayWriteParams{RelayID: res.RelayID, Offset: off, Data: chunk.Data}, &wr)
		off = wr.Received
		if chunk.EOF {
			break
		}
	}
	if off != res.Size {
		t.Fatalf("carried %d of %d bytes", off, res.Size)
	}
}

func startRelay(h *relayHost, id string, clone *wire.RelayClone) (wire.RelayStartResult, error) {
	var res wire.RelayStartResult
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	err := h.a.Call(ctx, wire.MethodRelayStart, wire.RelayStartParams{
		RelayID: id, Harness: wire.HarnessClaude, Model: "sonnet", Clone: clone,
		TargetHost: wire.RelayHost{HostID: "bbbb", Label: "target-host"}, Cols: 80, Rows: 24,
	}, &res)
	return res, err
}

func waitTask(t *testing.T, h *relayHost, id, state string) wire.Task {
	t.Helper()
	m := h.a.waitFor(t, "task "+state, func(m *wire.Msg) bool {
		return taskUpdated(m, func(tk wire.Task) bool { return tk.ID == id && tk.State == state })
	})
	var tk wire.Task
	json.Unmarshal(m.Params, &tk)
	return tk
}

// sameTree checks that the target's worktree has the source's branch tip,
// changes (staged and unstaged) and untracked files.
func sameTree(t *testing.T, src, dst string) {
	t.Helper()
	if a, b := gitAt(t, src, "rev-parse", "HEAD"), gitAt(t, dst, "rev-parse", "HEAD"); a != b {
		t.Errorf("HEAD %s, source %s", b, a)
	}
	for _, args := range [][]string{{"diff", "--cached"}, {"diff"}, {"status", "--porcelain=v1"}} {
		if a, b := gitAt(t, src, args...), gitAt(t, dst, args...); a != b {
			t.Errorf("git %v differs:\nsource:\n%s\ntarget:\n%s", args, a, b)
		}
	}
	st, err := os.Stat(filepath.Join(dst, "sub", "new.sh"))
	if err != nil || st.Mode()&0o100 == 0 {
		t.Errorf("untracked script %v, %v", st, err)
	}
	if target, err := os.Readlink(filepath.Join(dst, "sub", "link")); err != nil || target != "new.sh" {
		t.Errorf("untracked link %q, %v", target, err)
	}
}

func killSession(h *relayHost, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h.a.Call(ctx, wire.MethodSessionSignal, wire.SignalParams{ID: id, Signal: wire.SignalKill}, nil)
}

// A relay to another host end to end: the source builds the package, the
// client carries it, the target finds its own checkout of the same remote
// and starts the agent in a new task on the source's branch, with the
// same changes and untracked files, in the source's sub directory. The
// document holds the tool output and no thinking; the trace shows on
// both sides; discarding leaves the document.
func TestRelayToAnotherHost(t *testing.T) {
	bin := buildStagent(t)
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	s := startRelayHost(t, bin)
	d := startRelayHost(t, bin)
	origin, src := relayRepos(t, s.home, "")
	gitAt(t, d.home, "clone", "-q", origin, "mine")
	writeConversation(t, s.home, filepath.Join(src, "sub"))

	res := prepare(t, s, wire.RelayTargetOtherHost, wire.RelaySourceKeep)
	if res.Size == 0 || res.SHA256 == "" || res.Source.Branch != "feat/relay" || res.Source.CwdRel != "sub" ||
		res.Parts.BundleCommits != 1 || res.Parts.UntrackedFiles != 2 || res.Parts.PatchBytes == 0 ||
		res.Memo != (wire.RelayMemo{Status: wire.RelayMemoSkipped, Reason: wire.RelayMemoNotLive}) {
		t.Fatalf("relay.prepare = %+v", res)
	}
	for _, stage := range []string{wire.RelayStageTranscript, wire.RelayStageBundle, wire.RelayStageDiff, wire.RelayStageUntracked, wire.RelayStagePack} {
		s.a.waitFor(t, "relay.progress "+stage, func(m *wire.Msg) bool {
			var p wire.RelayProgressParams
			return m.Method == wire.NotifyRelayProgress && json.Unmarshal(m.Params, &p) == nil && p.Stage == stage && p.RelayID == res.RelayID
		})
	}
	carry(t, s, d, res)
	started, err := startRelay(d, res.RelayID, nil)
	if err != nil {
		t.Fatalf("relay.start: %v", err)
	}
	t.Cleanup(func() { killSession(d, started.Session.ID) })
	if started.Task == nil || started.Task.Branch != "feat/relay" || started.Task.RelayID != res.RelayID {
		t.Fatalf("relay.start = %+v", started)
	}
	tk := waitTask(t, d, started.Task.ID, wire.TaskReady)
	if !strings.HasPrefix(tk.Repo, d.home) || !strings.HasSuffix(tk.Repo, "mine") {
		t.Fatalf("task of checkout %s", tk.Repo)
	}
	sameTree(t, src, tk.Worktree)

	// The agent: the model, the fixed instruction, in cwd_rel.
	var args []byte
	deadline := time.Now().Add(waitLong)
	for time.Now().Before(deadline) {
		if args, err = os.ReadFile(filepath.Join(d.home, "agent-args")); err == nil && len(args) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.HasPrefix(string(args), "--model\nsonnet\nRead the relay document at "+started.Document) {
		t.Fatalf("agent args %q", args)
	}
	cwd, _ := os.ReadFile(filepath.Join(d.home, "agent-cwd"))
	if want, _ := filepath.EvalSymlinks(filepath.Join(tk.Worktree, "sub")); strings.TrimSpace(string(cwd)) != want {
		t.Errorf("agent started in %q, want %q", cwd, want)
	}
	doc, err := os.ReadFile(started.Document)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "TOOL-OUTPUT-B") || strings.Contains(string(doc), "SECRET-THOUGHT") || !strings.Contains(string(doc), "Finish the change") {
		t.Errorf("document:\n%s", doc)
	}

	// The trace: the target end recorded by relay.start, the source end by
	// the client.
	var sl wire.SessionsListResult
	d.a.call(t, wire.MethodSessionsList, struct{}{}, &sl)
	i := slices.IndexFunc(sl.Sessions, func(x wire.Session) bool { return x.ID == started.Session.ID })
	if i < 0 || sl.Sessions[i].Relay == nil || sl.Sessions[i].Relay.Source == nil ||
		sl.Sessions[i].Relay.Source.Host.Label != "source-host" || sl.Sessions[i].Relay.Source.ConversationID != relayConv {
		t.Fatalf("target session's trace: %+v", sl.Sessions)
	}
	host := wire.RelayHost{HostID: "bbbb", Label: "target-host"}
	s.a.call(t, wire.MethodRelayRecord, wire.RelayRecordParams{
		RelayID: res.RelayID, Role: wire.RelayRoleSource,
		Local: wire.RelayEnd{Harness: wire.HarnessClaude, ConversationID: relayConv},
		Peer:  wire.RelayEnd{Host: &host, Harness: wire.HarnessClaude, SessionID: started.Session.ID},
	}, nil)
	var cl wire.ConversationsListResult
	s.a.call(t, wire.MethodConversationsList, wire.ConversationsListParams{Harness: []string{wire.HarnessClaude}}, &cl)
	j := slices.IndexFunc(cl.Conversations, func(c wire.Conversation) bool { return c.ID == relayConv })
	if j < 0 || cl.Conversations[j].Relay == nil || cl.Conversations[j].Relay.Target == nil || cl.Conversations[j].Relay.Target.Host.Label != "target-host" {
		t.Fatalf("source conversation's trace: %+v", cl.Conversations)
	}

	// Discarding: the packages go, the document stays.
	s.a.call(t, wire.MethodRelayDiscard, wire.RelayRef{RelayID: res.RelayID}, nil)
	d.a.call(t, wire.MethodRelayDiscard, wire.RelayRef{RelayID: res.RelayID}, nil)
	if _, err := os.Stat(filepath.Join(s.l.StateDir, "relay-out", res.RelayID)); !os.IsNotExist(err) {
		t.Errorf("source package left (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(d.l.StateDir, "relay", res.RelayID, "package.tar.gz")); !os.IsNotExist(err) {
		t.Errorf("received package left (%v)", err)
	}
	if _, err := os.Stat(started.Document); err != nil {
		t.Errorf("document gone: %v", err)
	}
}

// A target with no checkout of the repository answers repo_missing,
// having made nothing; with clone it clones the remote and goes on.
func TestRelayRepoMissingThenClone(t *testing.T) {
	bin := buildStagent(t)
	s := startRelayHost(t, bin)
	d := startRelayHost(t, bin)
	_, src := relayRepos(t, s.home, "")
	writeConversation(t, s.home, filepath.Join(src, "sub"))
	res := prepare(t, s, wire.RelayTargetOtherHost, wire.RelaySourceKeep)
	carry(t, s, d, res)
	_, err := startRelay(d, res.RelayID, nil)
	var we *wire.Error
	if !errors.As(err, &we) || we.Code != wire.ErrRepoMissing || !strings.HasPrefix(we.Message, "repo: ") {
		t.Fatalf("relay.start without a checkout: %v", err)
	}
	var tl wire.TaskListResult
	d.a.call(t, wire.MethodTaskList, struct{}{}, &tl)
	if len(tl.Tasks) != 0 {
		t.Fatalf("repo_missing made tasks: %+v", tl.Tasks)
	}
	started, err := startRelay(d, res.RelayID, &wire.RelayClone{})
	if err != nil {
		t.Fatalf("relay.start with clone: %v", err)
	}
	t.Cleanup(func() { killSession(d, started.Session.ID) })
	if started.Cloned != filepath.Join(d.home, "origin") {
		t.Fatalf("cloned %q", started.Cloned)
	}
	tk := waitTask(t, d, started.Task.ID, wire.TaskReady)
	sameTree(t, src, tk.Worktree)
}

// A relay within one host: no bytes carried, a new task of the source's
// own checkout on <branch>-relay with the changes applied, both ends of
// the trace recorded.
func TestRelaySameHost(t *testing.T) {
	bin := buildStagent(t)
	s := startRelayHost(t, bin)
	_, src := relayRepos(t, s.home, "")
	writeConversation(t, s.home, filepath.Join(src, "sub"))
	res := prepare(t, s, wire.RelayTargetSameHost, wire.RelaySourceKeep)
	if res.Size != 0 || res.SHA256 != "" || res.Parts.BundleBytes != 0 {
		t.Fatalf("same-host relay.prepare = %+v", res)
	}
	started, err := startRelay(s, res.RelayID, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { killSession(s, started.Session.ID) })
	if started.Task == nil || started.Task.Branch != "feat/relay-relay" {
		t.Fatalf("relay.start = %+v", started)
	}
	tk := waitTask(t, s, started.Task.ID, wire.TaskReady)
	sameTree(t, src, tk.Worktree)
	var cl wire.ConversationsListResult
	s.a.call(t, wire.MethodConversationsList, wire.ConversationsListParams{Harness: []string{wire.HarnessClaude}}, &cl)
	j := slices.IndexFunc(cl.Conversations, func(c wire.Conversation) bool { return c.ID == relayConv })
	if j < 0 || cl.Conversations[j].Relay == nil || cl.Conversations[j].Relay.Target == nil ||
		cl.Conversations[j].Relay.Target.SessionID != started.Session.ID {
		t.Fatalf("source end of the trace: %+v", cl.Conversations)
	}
}

// A relay whose task fails (its setup) is undone by relay.abort: no task,
// worktree, branch, document or trace stays; the received package does,
// and relay.start goes again.
func TestRelayAbortAfterFailure(t *testing.T) {
	bin := buildStagent(t)
	s := startRelayHost(t, bin)
	d := startRelayHost(t, bin)
	origin, src := relayRepos(t, s.home, "exit 3")
	gitAt(t, d.home, "clone", "-q", origin, "mine")
	mine := filepath.Join(d.home, "mine")
	writeConversation(t, s.home, filepath.Join(src, "sub"))
	res := prepare(t, s, wire.RelayTargetOtherHost, wire.RelaySourceKeep)
	carry(t, s, d, res)
	started, err := startRelay(d, res.RelayID, nil)
	if err != nil {
		t.Fatal(err)
	}
	tk := waitTask(t, d, started.Task.ID, wire.TaskFailed)
	if tk.Stage != wire.TaskStageSetup {
		t.Fatalf("task failed at %q", tk.Stage)
	}
	if _, err := startRelay(d, res.RelayID, nil); !errors.As(err, new(*wire.Error)) || !strings.Contains(err.Error(), wire.ErrBusy) {
		t.Fatalf("relay.start of a started relay: %v", err)
	}
	d.a.call(t, wire.MethodRelayAbort, wire.RelayRef{RelayID: res.RelayID}, nil)

	var tl wire.TaskListResult
	d.a.call(t, wire.MethodTaskList, struct{}{}, &tl)
	if len(tl.Tasks) != 0 {
		t.Errorf("tasks left: %+v", tl.Tasks)
	}
	if _, err := os.Stat(tk.Worktree); !os.IsNotExist(err) {
		t.Errorf("worktree left (%v)", err)
	}
	if out := gitAt(t, mine, "branch", "--list", "feat/relay"); out != "" {
		t.Errorf("branch left: %q", out)
	}
	ents, _ := os.ReadDir(filepath.Join(d.l.StateDir, "relay", res.RelayID))
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	if !slices.Contains(names, "package.tar.gz") || slices.Contains(names, "relay.md") || slices.Contains(names, "tree") {
		t.Errorf("relay directory after relay.abort: %v", names)
	}
	var sl wire.SessionsListResult
	d.a.call(t, wire.MethodSessionsList, struct{}{}, &sl)
	for _, x := range sl.Sessions {
		if x.Relay != nil {
			t.Errorf("trace left on %s: %+v", x.ID, x.Relay)
		}
	}
	// The package is still here: trying again needs no carrying.
	again, err := startRelay(d, res.RelayID, nil)
	if err != nil {
		t.Fatalf("relay.start again: %v", err)
	}
	t.Cleanup(func() { killSession(d, again.Session.ID) })
	waitTask(t, d, again.Task.ID, wire.TaskFailed)
}

// fakeMemoClaude is a live Claude Code stand-in that reports through
// stagent's hooks as Claude's installed hooks do: at rest after it starts,
// and for each line typed into it working, a reply appended to its
// transcript, then at rest again — unless $HOME/no-reply exists, when it
// stays working and never replies.
const fakeMemoClaude = `#!/bin/sh
S='%[1]s'
T='%[2]s'
P='{"session_id":"%[3]s","transcript_path":"'"$T"'"}'
"$S" hook claude SessionStart "$P"
"$S" hook claude Stop "$P"
echo FAKE-CLAUDE-READY
while read -r line; do
  "$S" hook claude UserPromptSubmit "$P"
  [ -f "$HOME/no-reply" ] && continue
  sleep 1
  ts=$(date -u +%%Y-%%m-%%dT%%H:%%M:%%S.000Z)
  printf '{"type":"user","sessionId":"%[3]s","timestamp":"%%s","message":{"role":"user","content":"MEMO-REQUEST"}}\n' "$ts" >> "$T"
  printf '{"type":"assistant","sessionId":"%[3]s","timestamp":"%%s","message":{"role":"assistant","model":"m","content":[{"type":"text","text":"HANDOFF-MEMO-TEXT: goal, done, left, next."}]}}\n' "$ts" >> "$T"
  "$S" hook claude Stop "$P"
done
`

// unpackPackage reads relay id's package from h and unpacks it.
func unpackPackage(t *testing.T, h *relayHost, res wire.RelayPrepareResult) string {
	t.Helper()
	dir := t.TempDir()
	pkg := filepath.Join(dir, "package.tar.gz")
	var data []byte
	for {
		var chunk wire.RelayReadResult
		h.a.call(t, wire.MethodRelayRead, wire.RelayReadParams{RelayID: res.RelayID, Offset: int64(len(data))}, &chunk)
		data = append(data, chunk.Data...)
		if chunk.EOF {
			break
		}
	}
	if err := os.WriteFile(pkg, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := relay.Unpack(pkg, filepath.Join(dir, "out"), nil); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "out")
}

// The hand-off memo of a live agent at rest: the request is typed into
// its session, its reply (the assistant rows after the request) is the
// package's memo.md and the document's memo section, not its full text.
// An agent that does not reply within the limit leaves the relay without
// a memo, the document saying so.
func TestRelayMemo(t *testing.T) {
	bin := buildStagent(t)
	s := startRelayHost(t, bin)
	writeConversation(t, s.home, s.home)
	tr := filepath.Join(s.home, ".claude", "projects", "-src", relayConv+".jsonl")
	agentDir := filepath.Join(s.home, "memo-bin")
	if err := os.Mkdir(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "claude"), []byte(fmt.Sprintf(fakeMemoClaude, bin, tr, relayConv)), 0o755); err != nil {
		t.Fatal(err)
	}
	var sp wire.SpawnResult
	s.a.call(t, wire.MethodSessionSpawn, wire.SpawnParams{Command: []string{filepath.Join(agentDir, "claude")}, Cwd: s.home, Cols: 80, Rows: 24}, &sp)
	id := sp.Session.ID
	t.Cleanup(func() { killSession(s, id) })
	s.a.waitFor(t, "the agent at rest with its conversation", func(m *wire.Msg) bool {
		return sessionUpdated(m, id, func(x wire.Session) bool {
			return x.Harness == wire.HarnessClaude && x.ConversationID == relayConv && x.State == wire.StateIdle
		})
	})
	prepareMemo := func(timeoutMS int) wire.RelayPrepareResult {
		t.Helper()
		var res wire.RelayPrepareResult
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := s.a.Call(ctx, wire.MethodRelayPrepare, wire.RelayPrepareParams{
			SessionID: id, Target: wire.RelayTargetOtherHost, SourceAgent: wire.RelaySourceKeep,
			SourceHost: wire.RelayHost{Label: "source-host"}, Memo: true, MemoTimeoutMS: timeoutMS,
		}, &res); err != nil {
			t.Fatalf("relay.prepare: %v", err)
		}
		return res
	}

	res := prepareMemo(30_000)
	if res.Memo.Status != wire.RelayMemoIncluded {
		t.Fatalf("memo %+v", res.Memo)
	}
	out := unpackPackage(t, s, res)
	memo, err := os.ReadFile(filepath.Join(out, "memo.md"))
	if err != nil || !strings.Contains(string(memo), "HANDOFF-MEMO-TEXT") || strings.Contains(string(memo), "MEMO-REQUEST") {
		t.Fatalf("memo.md %q, %v", memo, err)
	}
	doc, _ := os.ReadFile(filepath.Join(out, "relay.md"))
	if strings.Count(string(doc), "HANDOFF-MEMO-TEXT") != 1 || strings.Contains(string(doc), "MEMO-REQUEST") {
		t.Fatalf("document:\n%s", doc)
	}
	if b, _ := os.ReadFile(tr); !strings.Contains(string(b), "MEMO-REQUEST") {
		t.Fatal("the request did not reach the agent")
	}

	// No reply within the limit: no memo, and the relay goes on.
	s.a.waitFor(t, "the agent at rest again", func(m *wire.Msg) bool {
		return sessionUpdated(m, id, func(x wire.Session) bool { return x.State == wire.StateIdle })
	})
	if err := os.WriteFile(filepath.Join(s.home, "no-reply"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res = prepareMemo(3_000)
	if res.Memo.Status != wire.RelayMemoTimeout || res.Size == 0 {
		t.Fatalf("relay.prepare without a reply = %+v", res)
	}
	if d := time.Since(start); d < 3*time.Second {
		t.Fatalf("gave up after %v, before the limit", d)
	}
	out = unpackPackage(t, s, res)
	if _, err := os.Stat(filepath.Join(out, "memo.md")); !os.IsNotExist(err) {
		t.Errorf("memo.md without a reply (%v)", err)
	}
	// The agent is working now: a memo is not asked for.
	res = prepareMemo(3_000)
	if res.Memo != (wire.RelayMemo{Status: wire.RelayMemoSkipped, Reason: wire.RelayMemoBusy}) {
		t.Fatalf("memo of a working agent %+v", res.Memo)
	}
}
