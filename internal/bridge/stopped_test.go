//go:build !windows

package bridge

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

const stoppedID = "00000000000000a1"

// stoppedDaemon is a fake daemon answering stopped.claim with claim (or
// claimErr) and recording the claims, unclaims and other calls.
type stoppedDaemon struct {
	mu       sync.Mutex
	claim    wire.StoppedClaimResult
	claimErr *wire.Error
	claims   []wire.StoppedClaimParams
	unclaims []wire.StoppedClaimParams
	calls    []string
}

func (d *stoppedDaemon) handle(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, m.Method)
	switch m.Method {
	case wire.MethodStoppedClaim:
		var p wire.StoppedClaimParams
		rpc.Decode(m, &p)
		d.claims = append(d.claims, p)
		if d.claimErr != nil {
			return nil, d.claimErr
		}
		return d.claim, nil
	case wire.MethodStoppedUnclaim:
		var p wire.StoppedClaimParams
		rpc.Decode(m, &p)
		d.unclaims = append(d.unclaims, p)
	}
	return struct{}{}, nil
}

func (d *stoppedDaemon) state() (claims, unclaims []wire.StoppedClaimParams, calls []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.claims), slices.Clone(d.unclaims), slices.Clone(d.calls)
}

func stoppedAgent(cwd string) *wire.StoppedAgent {
	return &wire.StoppedAgent{
		ID: stoppedID, Harness: wire.HarnessClaude, ConversationID: "conv-a.1", Cwd: cwd,
		TaskID: "task1", Reason: wire.StoppedReboot,
	}
}

// stopped.resume claims the stopped agent for a new session and starts it:
// a kept shell in the agent's cwd, tied to its task, whose /bin/sh runs the
// harness's resume command and then the login shell (quoted).
func TestStoppedResumeStartsKeptShell(t *testing.T) {
	cwd := t.TempDir()
	t.Setenv("SHELL", "/opt/it's/zsh")
	d := &stoppedDaemon{claim: wire.StoppedClaimResult{Agent: stoppedAgent(cwd)}}
	a, starts := startSpawnBridge(t, d.handle)
	var res wire.StoppedResumeResult
	if e := a.call(t, wire.MethodStoppedResume, wire.StoppedResumeParams{ID: stoppedID, Cols: 100, Rows: 30, Env: map[string]string{"FOO": "bar"}}, &res); e != nil {
		t.Fatal(e)
	}
	claims, unclaims, _ := d.state()
	if len(claims) != 1 || claims[0].ID != stoppedID || !validSessionID(claims[0].SessionID) || len(unclaims) != 0 {
		t.Fatalf("claims %+v, unclaims %+v", claims, unclaims)
	}
	id := claims[0].SessionID
	if res.Existed || res.Session.ID != id {
		t.Fatalf("resume = %+v, want new session %s", res, id)
	}
	want := []string{
		fakeExe, "run", "--detached", "--id", id, "--cols", "100", "--rows", "30", "--cwd", cwd, "--task=task1", "--",
		"/bin/sh", "-c", `trap : INT; claude --resume conv-a.1; exec '/opt/it'\''s/zsh' -l`,
	}
	if s := starts(); len(s) != 1 || !slices.Equal(s[0].args, want) {
		t.Fatalf("started %+v\nwant %q", s, want)
	}
}

// A live session already carrying the conversation is answered, nothing
// starts.
func TestStoppedResumeExisting(t *testing.T) {
	live := wire.Session{ID: "00000000000000f1", Harness: wire.HarnessClaude, ConversationID: "conv-a.1"}
	d := &stoppedDaemon{claim: wire.StoppedClaimResult{Session: &live}}
	a, starts := startSpawnBridge(t, d.handle)
	var res wire.StoppedResumeResult
	if e := a.call(t, wire.MethodStoppedResume, wire.StoppedResumeParams{ID: stoppedID}, &res); e != nil {
		t.Fatal(e)
	}
	if !res.Existed || res.Session.ID != live.ID || len(starts()) != 0 {
		t.Fatalf("resume = %+v, started %+v", res, starts())
	}
}

// Under a coding agent nothing is asked or started (ADR 0004); off Linux
// and macOS stopped.resume is unsupported; the daemon's not_found and busy
// come back as they are.
func TestStoppedResumeRefusals(t *testing.T) {
	d := &stoppedDaemon{claim: wire.StoppedClaimResult{Agent: stoppedAgent(t.TempDir())}}
	a, starts := startSpawnBridge(t, d.handle, func(b *Bridge) {
		b.agentRefusal = func() *wire.Error { return wire.Errorf(wire.ErrAgentRefused, "from claude") }
	})
	if e := a.call(t, wire.MethodStoppedResume, wire.StoppedResumeParams{ID: stoppedID}, nil); e == nil || e.Code != wire.ErrAgentRefused {
		t.Fatalf("resume under an agent: %v", e)
	}
	if _, _, calls := d.state(); len(calls) != 0 || len(starts()) != 0 {
		t.Fatalf("daemon calls %v, started %+v", calls, starts())
	}

	w, _ := startSpawnBridge(t, d.handle, func(b *Bridge) { b.goos = "windows" })
	if e := w.call(t, wire.MethodStoppedResume, wire.StoppedResumeParams{ID: stoppedID}, nil); e == nil || e.Code != wire.ErrUnsupported {
		t.Fatalf("resume on windows: %v", e)
	}

	for _, code := range []string{wire.ErrNotFound, wire.ErrBusy} {
		d := &stoppedDaemon{claimErr: wire.Errorf(code, "no")}
		a, starts := startSpawnBridge(t, d.handle)
		if e := a.call(t, wire.MethodStoppedResume, wire.StoppedResumeParams{ID: stoppedID}, nil); e == nil || e.Code != code || len(starts()) != 0 {
			t.Fatalf("resume with the daemon's %s: %v, started %+v", code, e, starts())
		}
	}
}

// A resume that cannot start its session releases the claim: a holder that
// fails to start, a cwd that is no longer a directory.
func TestStoppedResumeUnclaimsOnFailure(t *testing.T) {
	d := &stoppedDaemon{claim: wire.StoppedClaimResult{Agent: stoppedAgent(t.TempDir())}}
	a, _ := startSpawnBridge(t, d.handle, func(b *Bridge) {
		b.spawnProc = func(string, []string, string, []string, string) (int, error) { return 0, errors.New("no fork") }
	})
	if e := a.call(t, wire.MethodStoppedResume, wire.StoppedResumeParams{ID: stoppedID}, nil); e == nil {
		t.Fatal("resume succeeded without a holder")
	}
	claims, unclaims, _ := d.state()
	if len(unclaims) != 1 || unclaims[0] != claims[0] {
		t.Fatalf("claims %+v, unclaims %+v", claims, unclaims)
	}

	d = &stoppedDaemon{claim: wire.StoppedClaimResult{Agent: stoppedAgent(filepath.Join(t.TempDir(), "gone"))}}
	a, starts := startSpawnBridge(t, d.handle)
	if e := a.call(t, wire.MethodStoppedResume, wire.StoppedResumeParams{ID: stoppedID}, nil); e == nil || e.Code != wire.ErrBadRequest {
		t.Fatalf("resume into a missing cwd: %v", e)
	}
	if _, unclaims, _ := d.state(); len(unclaims) != 1 || len(starts()) != 0 {
		t.Fatalf("unclaims %+v, started %+v", unclaims, starts())
	}
}

// stopped_agents is announced on Linux and macOS only, where the daemon
// keeps them; stopped.remove goes to the daemon.
func TestStoppedAgentsAnnouncedAndRemoveForwarded(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		_, a := startBridge(t, func(b *Bridge) { b.goos = goos })
		want := goos != "windows"
		if got := slices.Contains(helloCaps(t, a), wire.CapStoppedAgents); got != want {
			t.Errorf("hello on %s announces %s: %v, want %v", goos, wire.CapStoppedAgents, got, want)
		}
	}
	d := &stoppedDaemon{}
	a, _ := startSpawnBridge(t, d.handle)
	if e := a.call(t, wire.MethodStoppedRemove, wire.StoppedRef{ID: stoppedID}, nil); e != nil {
		t.Fatal(e)
	}
	if _, _, calls := d.state(); !slices.Equal(calls, []string{wire.MethodStoppedRemove}) {
		t.Fatalf("daemon calls %v", calls)
	}
}
