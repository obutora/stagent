package follow

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/ptable"
	"github.com/obutora/stagent/internal/version"
	"github.com/obutora/stagent/internal/wire"
)

// frame is any frame of the stream.
type frame struct {
	T              string         `json:"t"`
	Version        string         `json:"version"`
	FollowProtocol int            `json:"follow_protocol"`
	OS             string         `json:"os"`
	Arch           string         `json:"arch"`
	Mux            string         `json:"mux"`
	Reason         string         `json:"reason"`
	Diag           string         `json:"diag"`
	Agents         []agentInfo    `json:"agents"`
	Selected       *string        `json:"selected"`
	Key            string         `json:"key"`
	Path           string         `json:"path"`
	Harness        string         `json:"harness"`
	Reset          bool           `json:"reset"`
	Messages       []wire.Message `json:"messages"`
	Cursor         int64          `json:"cursor"`
	Message        string         `json:"message"`
}

type fixture struct {
	t    *testing.T
	tr   *tree
	home string
	mux  *fakeMux
	out  bytes.Buffer
	f    *follower
}

func newFixture(t *testing.T, tr *tree) *fixture {
	x := &fixture{t: t, tr: tr, home: t.TempDir(), mux: &fakeMux{}}
	x.f = newFollower(&x.out, x.home, func() (*ptable.Snapshot, error) { return x.tr.snapshot(), nil },
		testLocator(x.mux), func(string) string { return "" })
	return x
}

// frames returns the frames written since the last call.
func (x *fixture) frames() []frame {
	x.t.Helper()
	var out []frame
	sc := bufio.NewScanner(&x.out)
	for sc.Scan() {
		var f frame
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			x.t.Fatalf("bad frame %q: %v", sc.Bytes(), err)
		}
		out = append(out, f)
	}
	x.out.Reset()
	return out
}

func (x *fixture) do(err error) {
	x.t.Helper()
	if err != nil {
		x.t.Fatal(err)
	}
}

func userRecord(text string) string {
	b, _ := json.Marshal(text)
	return `{"type":"user","message":{"role":"user","content":` + string(b) + `},"timestamp":"2026-09-30T00:00:00Z"}` + "\n"
}

// claude starts a Claude Code process in the fake table with a session
// file and a transcript holding one user message per prompt.
func (x *fixture) claude(pid, ppid int, prompts ...string) (*fakeProc, string) {
	p := x.tr.add(pid, ppid, "claude", "claude")
	p.cwd = "/work"
	return p, x.session(pid, fmt.Sprintf("%08d-0000-4000-8000-000000000000", pid), prompts...)
}

// session points pid's session file at conversation id, creating its
// transcript.
func (x *fixture) session(pid int, id string, prompts ...string) string {
	cfg := filepath.Join(x.home, ".claude")
	path := filepath.Join(cfg, "projects", "-work", id+".jsonl")
	var b strings.Builder
	for _, p := range prompts {
		b.WriteString(userRecord(p))
	}
	write(x.t, path, b.String(), x.tr.procs[pid].Start.Add(time.Minute))
	write(x.t, filepath.Join(cfg, "sessions", fmt.Sprint(pid)+".json"),
		fmt.Sprintf(`{"pid":%d,"sessionId":%q,"startedAt":%d}`, pid, id, x.tr.procs[pid].Start.UnixMilli()+1500), time.Time{})
	return path
}

func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func texts(ms []wire.Message) string {
	var s []string
	for _, m := range ms {
		s = append(s, m.Text)
	}
	return strings.Join(s, ",")
}

func selected(f frame) string {
	if f.Selected == nil {
		return "<null>"
	}
	return *f.Selected
}

func TestFollowPicksForegroundAgentAndTails(t *testing.T) {
	x := newFixture(t, sshTree())
	p102, path := x.claude(102, 101, "a1", "a2")
	p102.fg()
	appendTo(t, path, `{"type":"ai-title","aiTitle":"Fix the build"}`+"\n")
	_, _ = x.claude(103, 101, "b1")
	x.tr.procs[103].bg()

	x.do(x.f.scan())
	fs := x.frames()
	if len(fs) != 2 || fs[0].T != "target" || fs[1].T != "messages" {
		t.Fatalf("frames %+v", fs)
	}
	tg := fs[0]
	if selected(tg) != "claude:102" || tg.Mux != "none" || tg.Reason != "" || len(tg.Agents) != 2 {
		t.Fatalf("target %+v", tg)
	}
	if a := tg.Agents[0]; a.Key != "claude:102" || !a.Foreground || a.Cwd != "/work" || a.TranscriptPath != path || a.Title != "Fix the build" {
		t.Fatalf("agent %+v", a)
	}
	if m := fs[1]; m.Key != "claude:102" || m.Path != path || m.Harness != "claude" || !m.Reset || texts(m.Messages) != "a1,a2" || m.Cursor != 0 {
		t.Fatalf("messages %+v", m)
	}

	x.do(x.f.scan())
	if fs := x.frames(); len(fs) != 0 {
		t.Fatalf("unchanged scan wrote %+v", fs)
	}

	appendTo(t, path, userRecord("a3"))
	x.do(x.f.tail())
	fs = x.frames()
	if len(fs) != 1 || fs[0].T != "messages" || fs[0].Reset || texts(fs[0].Messages) != "a3" {
		t.Fatalf("tail frames %+v", fs)
	}
}

func TestFollowStickySelection(t *testing.T) {
	x := newFixture(t, sshTree())
	p102, _ := x.claude(102, 101, "a1")
	p102.fg()
	_, path103 := x.claude(103, 101, "b1")
	x.do(x.f.scan())
	x.frames()

	x.do(x.f.handle([]byte(`{"op":"select","key":"claude:103"}`)))
	fs := x.frames()
	if len(fs) != 2 || selected(fs[0]) != "claude:103" || fs[1].Path != path103 || !fs[1].Reset || texts(fs[1].Messages) != "b1" {
		t.Fatalf("select frames %+v", fs)
	}

	// The pick holds while the foreground agent keeps running, and while
	// the picked one runs outside the terminal (moved to another pane).
	x.do(x.f.scan())
	x.tr.procs[103].PPID = 1
	x.do(x.f.scan())
	if fs := x.frames(); len(fs) != 0 {
		t.Fatalf("sticky pick moved: %+v", fs)
	}

	x.tr.remove(103)
	x.do(x.f.scan())
	fs = x.frames()
	if len(fs) != 2 || selected(fs[0]) != "claude:102" || len(fs[0].Agents) != 1 || fs[1].Key != "claude:102" || !fs[1].Reset {
		t.Fatalf("after exit %+v", fs)
	}

	x.do(x.f.handle([]byte(`{"op":"select","key":"claude:999"}`)))
	if fs := x.frames(); len(fs) != 1 || fs[0].T != "error" {
		t.Fatalf("unknown key %+v", fs)
	}
}

func TestFollowAutomaticPickIsStable(t *testing.T) {
	x := newFixture(t, sshTree())
	_, path102 := x.claude(102, 101, "a1")
	_, path103 := x.claude(103, 101, "b1")
	// Neither is in front: the newest transcript wins…
	appendTo(t, path103, userRecord("b2"))
	x.do(x.f.scan())
	if fs := x.frames(); selected(fs[0]) != "claude:103" {
		t.Fatalf("first pick %+v", fs)
	}
	// …and keeps the view when the other agent writes later.
	appendTo(t, path102, userRecord("a2"))
	x.do(x.f.scan())
	if fs := x.frames(); len(fs) != 0 {
		t.Fatalf("pick flipped: %+v", fs)
	}
	// An agent brought to the foreground takes over.
	x.tr.procs[102].fg()
	x.do(x.f.scan())
	if fs := x.frames(); len(fs) != 2 || selected(fs[0]) != "claude:102" || fs[1].Path != path102 {
		t.Fatalf("foreground pick %+v", fs)
	}
}

func TestFollowTranscriptSwitch(t *testing.T) {
	x := newFixture(t, sshTree())
	p, _ := x.claude(102, 101, "before")
	p.fg()
	x.do(x.f.scan())
	x.frames()

	// /clear starts a new conversation in the same process.
	next := x.session(102, "99999999-0000-4000-8000-000000000000", "after")
	x.do(x.f.scan())
	fs := x.frames()
	if len(fs) != 2 || fs[0].Agents[0].TranscriptPath != next || fs[1].Path != next || !fs[1].Reset || texts(fs[1].Messages) != "after" {
		t.Fatalf("frames %+v", fs)
	}
}

func TestFollowOlderPages(t *testing.T) {
	x := newFixture(t, sshTree())
	var prompts []string
	for i := range 70 {
		prompts = append(prompts, fmt.Sprint("m", i))
	}
	p, path := x.claude(102, 101, prompts...)
	p.fg()
	x.do(x.f.scan())
	fs := x.frames()
	first := fs[1]
	if len(first.Messages) != pageSize || first.Messages[0].Text != "m10" || first.Cursor == 0 {
		t.Fatalf("first page: %d messages from %q, cursor %d", len(first.Messages), first.Messages[0].Text, first.Cursor)
	}

	x.do(x.f.handle(fmt.Appendf(nil, `{"op":"older","before":%d}`, first.Cursor)))
	fs = x.frames()
	if len(fs) != 1 || fs[0].T != "page" || fs[0].Key != "claude:102" || texts(fs[0].Messages) != "m0,m1,m2,m3,m4,m5,m6,m7,m8,m9" || fs[0].Cursor != 0 {
		t.Fatalf("older %+v", fs)
	}

	// Appended messages carry the oldest cursor handed out.
	appendTo(t, path, userRecord("m70"))
	x.do(x.f.tail())
	if fs := x.frames(); len(fs) != 1 || fs[0].Cursor != 0 || texts(fs[0].Messages) != "m70" {
		t.Fatalf("tail %+v", fs)
	}
}

func TestFollowReasons(t *testing.T) {
	noSSH := newTree()
	noSSH.add(selfPID, 1, "stagent", "stagent", "follow")
	for _, c := range []struct {
		name   string
		tr     *tree
		mux    *fakeMux
		reason string
	}{
		{"no_terminal", noSSH, &fakeMux{}, reasonNoTerminal},
		{"no_agent", sshTree(), &fakeMux{}, reasonNoAgent},
		{"terminal_ambiguous", macTailscale(), &fakeMux{}, reasonTerminalAmbiguous},
		{"mux_ambiguous", zellijTree(), &fakeMux{zellij: map[string][]zellijClient{"work": {
			{id: "1", pane: "terminal_1"}, {id: "2", pane: "terminal_0"},
		}}}, reasonAmbiguous},
	} {
		x := newFixture(t, c.tr)
		*x.mux = *c.mux
		x.do(x.f.scan())
		if fs := x.frames(); fs[0].Reason != c.reason || (fs[0].Diag != "") != (c.reason == reasonNoTerminal) {
			t.Errorf("%s: target %+v", c.name, fs[0])
		}
	}
}

// Of terminals that cannot be told apart, the agents are labelled with
// their terminal and the newest session's is followed.
func TestFollowTerminalAmbiguous(t *testing.T) {
	x := newFixture(t, macTailscale())
	x.do(x.f.scan())
	fs := x.frames()
	if len(fs) != 1 || fs[0].Selected == nil || *fs[0].Selected != "codex:72" || len(fs[0].Agents) != 2 ||
		fs[0].Agents[0].TTY != "ttys003" || fs[0].Agents[1].TTY != "ttys004" {
		t.Fatalf("frames %+v", fs)
	}
}

// The diag's counts move with every scan; that alone sends no new frame.
func TestFollowDiagAloneIsNoChange(t *testing.T) {
	tr := newTree()
	tr.add(40, 1, "teleport", "teleport")
	tr.add(selfPID, 40, "stagent", "stagent", "follow")
	x := newFixture(t, tr)
	x.f.loc.sshConn = tsConn
	x.do(x.f.scan())
	fs := x.frames()
	if len(fs) != 1 || fs[0].Reason != reasonNoTerminal || !strings.Contains(fs[0].Diag, " procs=2 ") {
		t.Fatalf("frames %+v", fs)
	}
	tr.add(50, 1, "cron", "cron")
	x.do(x.f.scan())
	if !strings.Contains(x.f.last.diag, " procs=3 ") {
		t.Fatalf("diag %q", x.f.last.diag)
	}
	if fs := x.frames(); len(fs) != 0 {
		t.Fatalf("frames %+v", fs)
	}
}

func TestFollowRunHelloAndEOF(t *testing.T) {
	x := newFixture(t, sshTree())
	x.do(x.f.run(strings.NewReader("{\"op\":\"nope\"}\r\n\n")))
	fs := x.frames()
	if len(fs) != 3 {
		t.Fatalf("frames %+v", fs)
	}
	h := fs[0]
	if h.T != "hello" || h.Version != version.Version || h.FollowProtocol != version.FollowProtocol || h.OS != runtime.GOOS || h.Arch != runtime.GOARCH {
		t.Fatalf("hello %+v", h)
	}
	if fs[1].T != "target" || fs[1].Selected != nil || fs[1].Agents == nil || fs[2].T != "error" {
		t.Fatalf("frames %+v", fs)
	}
}

func TestFollowFallbackSkipsOtherAgentsTranscripts(t *testing.T) {
	x := newFixture(t, sshTree())
	codexHome := filepath.Join(x.home, ".codex")
	start := x.tr.base.Add(102 * time.Second) // of pid 102
	// Written since 102 started, created before: by agents that resume.
	resumedA := codexRollout(t, codexHome, "/work", "codex-tui", start.Add(-time.Hour), start.Add(time.Minute), 1)
	openC := codexRollout(t, codexHome, "/work", "codex-tui", start.Add(-2*time.Hour), start.Add(2*time.Minute), 2)
	idA := filepath.Base(resumedA)[len("rollout-2006-01-02T15-04-05-") : len(filepath.Base(resumedA))-len(".jsonl")]

	// 102: a fresh codex in the terminal, nothing written yet.
	fresh := x.tr.add(102, 101, "MainThread", "node", "/usr/bin/codex")
	fresh.cwd = "/work"
	x.tr.add(103, 102, "codex", "/v/x86_64-unknown-linux-musl/bin/codex").files = []string{}
	// 104: a codex in the terminal resuming A (it keeps no file open).
	resuming := x.tr.add(104, 101, "codex", "codex", "resume", idA).fg()
	resuming.cwd, resuming.files = "/work", []string{}
	// 97's sibling, in another connection: has C open.
	x.tr.add(98, 96, "codex", "codex").files = []string{openC}

	x.do(x.f.scan())
	fs := x.frames()
	byKey := map[string]agentInfo{}
	for _, a := range fs[0].Agents {
		byKey[a.Key] = a
	}
	if got := byKey["codex:104"].TranscriptPath; got != resumedA {
		t.Fatalf("resuming agent: %q, want %q", got, resumedA)
	}
	if got := byKey["codex:102"].TranscriptPath; got != "" {
		t.Fatalf("fresh agent got another agent's transcript %q", got)
	}

	own := codexRollout(t, codexHome, "/work", "codex-tui", start.Add(3*time.Minute), start.Add(3*time.Minute), 3)
	x.do(x.f.scan())
	for _, a := range x.frames()[0].Agents {
		if a.Key == "codex:102" && a.TranscriptPath != own {
			t.Fatalf("fresh agent after its first write: %q, want %q", a.TranscriptPath, own)
		}
	}
}

// Another user's process cannot take a conversation from the user's agent
// by naming it on its command line or holding it open.
func TestFollowFallbackIgnoresOtherUsersClaims(t *testing.T) {
	x := newFixture(t, sshTree())
	codexHome := filepath.Join(x.home, ".codex")
	start := x.tr.base.Add(102 * time.Second) // of pid 102
	path := codexRollout(t, codexHome, "/work", "codex-tui", start.Add(time.Minute), start.Add(2*time.Minute), 1)
	id := filepath.Base(path)[len("rollout-2006-01-02T15-04-05-") : len(filepath.Base(path))-len(".jsonl")]
	fresh := x.tr.add(102, 101, "codex", "codex").fg()
	fresh.cwd, fresh.files = "/work", []string{}
	x.tr.add(98, 96, "codex", "codex", "resume", id).as(bobUser).files = []string{path}

	x.do(x.f.scan())
	fs := x.frames()
	if len(fs) == 0 || len(fs[0].Agents) != 1 || fs[0].Agents[0].TranscriptPath != path {
		t.Fatalf("frames %+v, want codex:102 following %s", fs, path)
	}
}
