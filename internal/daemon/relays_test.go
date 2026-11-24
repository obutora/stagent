package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/pathstest"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/transcript"
	"github.com/obutora/stagent/internal/wire"
)

func relayID(n int) string { return fmt.Sprintf("%032x", n) }

func relayPeer(sessionID string) wire.RelayEnd {
	return wire.RelayEnd{Host: &wire.RelayHost{HostID: "host-mac", Label: "mac"}, Harness: wire.HarnessCodex, SessionID: sessionID}
}

func readRelaysFile(t *testing.T, e *testEnv) relaysFile {
	t.Helper()
	b, err := os.ReadFile(e.layout.Relays)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(e.layout.Relays); fi != nil && fi.Mode().Perm() != 0o600 && os.PathSeparator == '/' {
		t.Errorf("relays.json mode %v", fi.Mode().Perm())
	}
	var f relaysFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func listedSession(t *testing.T, c *rpc.Client, id string) wire.Session {
	t.Helper()
	var sl wire.SessionsListResult
	call(t, c, wire.MethodSessionsList, nil, &sl)
	for _, s := range sl.Sessions {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("session %s not listed: %+v", id, sl)
	return wire.Session{}
}

func listedConversation(t *testing.T, c *rpc.Client, conv string) wire.Conversation {
	t.Helper()
	var cl wire.ConversationsListResult
	call(t, c, wire.MethodConversationsList, wire.ConversationsListParams{}, &cl)
	for _, cv := range cl.Conversations {
		if cv.ID == conv {
			return cv
		}
	}
	t.Fatalf("conversation %s not listed: %+v", conv, cl)
	return wire.Conversation{}
}

func relayUpdate(t *testing.T, events chan *wire.Msg, what string, pred func(*wire.RelayLinks) bool) wire.Session {
	t.Helper()
	return sessionUpdate(t, events, what, sid, func(s wire.Session) bool { return pred(s.Relay) })
}

// The trace of a session's conversation: recorded by its session id, the
// conversation id it gets later is filled in; recorded again, what was
// empty is filled; a newer source link replaces the older; relay.forget
// drops it; it outlives a restart; sessions.list, session.updated and
// conversations.list carry it.
func TestRelayTrace(t *testing.T) {
	home := t.TempDir()
	pathstest.SetHome(t, home)
	roots := transcript.Roots{wire.HarnessClaude: filepath.Join(home, "claude", "projects")}
	conv := "2a425a50-3f53-4e94-8acb-f2d54d242f33"
	tpath := filepath.Join(roots[wire.HarnessClaude], "-home-u-proj", conv+".jsonl")
	os.MkdirAll(filepath.Dir(tpath), 0o700)
	os.WriteFile(tpath, []byte(`{"type":"user","cwd":"/home/u/proj","entrypoint":"cli","timestamp":"2026-09-01T00:00:00Z","message":{"role":"user","content":"hello"}}`+"\n"), 0o600)

	e := startDaemonAt(t, fastConfig(), Options{Roots: roots})
	c, events := e.client()
	var w wire.WatchResult
	call(t, c, wire.MethodWatch, wire.WatchParams{}, &w)
	h, _ := e.client()
	call(t, h, wire.MethodHolderRegister, testSession(sid), nil)

	r1, r2, r3 := relayID(1), relayID(2), relayID(3)
	call(t, c, wire.MethodRelayRecord, wire.RelayRecordParams{
		RelayID: r1, Role: wire.RelayRoleSource,
		Local: wire.RelayEnd{Harness: wire.HarnessClaude, SessionID: sid}, Peer: relayPeer("t1"),
	}, nil)
	relayUpdate(t, events, "source link pushed", func(r *wire.RelayLinks) bool {
		return r != nil && r.Source == nil && r.Target != nil && r.Target.RelayID == r1 &&
			r.Target.SessionID == "t1" && r.Target.Harness == wire.HarnessCodex &&
			r.Target.Host == wire.RelayHost{HostID: "host-mac", Label: "mac"} && r.Target.At > 0
	})
	at := listedSession(t, c, sid).Relay.Target.At

	// The session gets its conversation id: the daemon fills it in.
	tpathJSON, _ := json.Marshal(tpath)
	call(t, h, wire.MethodHookEvent, wire.HookEventParams{Harness: wire.HarnessClaude, Event: "SessionStart", SessionID: sid,
		Payload: json.RawMessage(`{"session_id":"` + conv + `","transcript_path":` + string(tpathJSON) + `,"cwd":"/home/u/proj"}`)}, nil)
	sessionUpdate(t, events, "conversation id", sid, func(s wire.Session) bool {
		return s.ConversationID == conv && s.Relay != nil && s.Relay.Target != nil && s.Relay.Target.RelayID == r1
	})
	if f := readRelaysFile(t, e); len(f.Links) != 1 || f.Links[0].Local.ConversationID != conv || f.Links[0].Local.SessionID != sid {
		t.Fatalf("relays.json after the conversation id: %+v", f)
	}
	if cv := listedConversation(t, c, conv); cv.Relay == nil || cv.Relay.Target == nil || cv.Relay.Target.RelayID != r1 {
		t.Fatalf("conversation relay %+v", cv.Relay)
	}

	// Recorded again: the peer's conversation id fills in, at stays.
	peer := relayPeer("t1")
	peer.ConversationID = "peer-conv"
	call(t, c, wire.MethodRelayRecord, wire.RelayRecordParams{
		RelayID: r1, Role: wire.RelayRoleSource,
		Local: wire.RelayEnd{Harness: wire.HarnessClaude, ConversationID: conv}, Peer: peer,
	}, nil)
	relayUpdate(t, events, "peer conversation id", func(r *wire.RelayLinks) bool {
		return r != nil && r.Target != nil && r.Target.ConversationID == "peer-conv"
	})
	if got := listedSession(t, c, sid).Relay.Target; got.At != at || got.SessionID != "t1" {
		t.Fatalf("re-recorded link %+v (at was %d)", got, at)
	}

	// Relayed again: the newer source link replaces the older; a target
	// link stands beside it.
	time.Sleep(2 * time.Millisecond)
	call(t, c, wire.MethodRelayRecord, wire.RelayRecordParams{
		RelayID: r2, Role: wire.RelayRoleSource,
		Local: wire.RelayEnd{Harness: wire.HarnessClaude, ConversationID: conv}, Peer: relayPeer("t2"),
	}, nil)
	relayUpdate(t, events, "newer source link", func(r *wire.RelayLinks) bool {
		return r != nil && r.Target != nil && r.Target.RelayID == r2
	})
	call(t, c, wire.MethodRelayRecord, wire.RelayRecordParams{
		RelayID: r3, Role: wire.RelayRoleTarget,
		Local: wire.RelayEnd{Harness: wire.HarnessClaude, SessionID: sid},
		Peer:  wire.RelayEnd{Host: &wire.RelayHost{Label: "linux"}, Harness: wire.HarnessClaude, ConversationID: "from-conv"},
	}, nil)
	relayUpdate(t, events, "target link", func(r *wire.RelayLinks) bool {
		return r != nil && r.Target != nil && r.Target.RelayID == r2 && r.Source != nil && r.Source.RelayID == r3 &&
			r.Source.Host.Label == "linux" && r.Source.ConversationID == "from-conv"
	})
	f := readRelaysFile(t, e)
	if len(f.Links) != 2 {
		t.Fatalf("relays.json keeps %+v", f)
	}
	for _, l := range f.Links {
		if l.RelayID == r1 {
			t.Fatalf("replaced link kept: %+v", f)
		}
	}

	// The trace outlives a restart.
	e.stop()
	e2 := startDaemonAt(t, fastConfig(), Options{Roots: roots})
	c2, _ := e2.client()
	cv := listedConversation(t, c2, conv)
	if cv.Relay == nil || cv.Relay.Target == nil || cv.Relay.Target.RelayID != r2 || cv.Relay.Source == nil || cv.Relay.Source.RelayID != r3 {
		t.Fatalf("conversation relay after restart %+v", cv.Relay)
	}
	h2, _ := e2.client()
	call(t, h2, wire.MethodHolderRegister, testSession(sid), nil)
	if r := listedSession(t, c2, sid).Relay; r == nil || r.Source == nil || r.Source.RelayID != r3 {
		t.Fatalf("session relay after restart %+v", r)
	}

	// relay.forget drops the relay's links; an unknown one is {}.
	call(t, c2, wire.MethodRelayForget, wire.RelayRef{RelayID: r2}, nil)
	call(t, c2, wire.MethodRelayForget, wire.RelayRef{RelayID: relayID(99)}, nil)
	cv = listedConversation(t, c2, conv)
	if cv.Relay == nil || cv.Relay.Target != nil || cv.Relay.Source == nil {
		t.Fatalf("conversation relay after forget %+v", cv.Relay)
	}
	call(t, c2, wire.MethodRelayForget, wire.RelayRef{RelayID: r3}, nil)
	if r := listedSession(t, c2, sid).Relay; r != nil {
		t.Fatalf("session relay after forgetting all %+v", r)
	}
	if f := readRelaysFile(t, e2); len(f.Links) != 0 {
		t.Fatalf("relays.json after forget %+v", f)
	}
}

// relays.json keeps the newest relaysKeep links.
func TestRelayTraceKeepsNewest(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{})
	c, _ := e.client()
	for i := 0; i <= relaysKeep; i++ {
		call(t, c, wire.MethodRelayRecord, wire.RelayRecordParams{
			RelayID: relayID(i), Role: wire.RelayRoleTarget,
			Local: wire.RelayEnd{Harness: wire.HarnessClaude, ConversationID: fmt.Sprintf("conv-%d", i)}, Peer: relayPeer(""),
		}, nil)
	}
	f := readRelaysFile(t, e)
	if len(f.Links) != relaysKeep || f.Links[0].RelayID != relayID(1) || f.Links[relaysKeep-1].RelayID != relayID(relaysKeep) {
		t.Fatalf("relays.json keeps %d links, first %s", len(f.Links), f.Links[0].RelayID)
	}
}

func TestRelayRecordBadParams(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{})
	c, _ := e.client()
	good := wire.RelayRecordParams{
		RelayID: relayID(1), Role: wire.RelayRoleSource,
		Local: wire.RelayEnd{Harness: wire.HarnessClaude, SessionID: sid}, Peer: relayPeer("t1"),
	}
	for name, change := range map[string]func(*wire.RelayRecordParams){
		"short relay_id":   func(p *wire.RelayRecordParams) { p.RelayID = "abc" },
		"upper relay_id":   func(p *wire.RelayRecordParams) { p.RelayID = "0123456789ABCDEF0123456789abcdef" },
		"bad role":         func(p *wire.RelayRecordParams) { p.Role = "middle" },
		"no local harness": func(p *wire.RelayRecordParams) { p.Local.Harness = "" },
		"no peer harness":  func(p *wire.RelayRecordParams) { p.Peer.Harness = "" },
		"no peer host":     func(p *wire.RelayRecordParams) { p.Peer.Host = nil },
		"no peer label":    func(p *wire.RelayRecordParams) { p.Peer.Host = &wire.RelayHost{HostID: "x"} },
	} {
		p := good
		change(&p)
		if err := callErr(c, wire.MethodRelayRecord, p, nil); errCode(err) != wire.ErrBadRequest {
			t.Errorf("%s: %v, want %s", name, err, wire.ErrBadRequest)
		}
	}
	if err := callErr(c, wire.MethodRelayForget, wire.RelayRef{RelayID: "nope"}, nil); errCode(err) != wire.ErrBadRequest {
		t.Errorf("relay.forget of a bad relay_id: %v, want %s", err, wire.ErrBadRequest)
	}
	if _, err := os.Stat(e.layout.Relays); !os.IsNotExist(err) {
		t.Errorf("relays.json written for bad params: %v", err)
	}
}

// A coding agent's process tree may not change the relay trace.
func TestRelayMethodsRefusedToAgents(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{Guard: guardAs("claude")})
	c, _ := e.client()
	for _, m := range []struct {
		method string
		params any
	}{
		{wire.MethodRelayRecord, wire.RelayRecordParams{RelayID: relayID(1), Role: wire.RelayRoleSource,
			Local: wire.RelayEnd{Harness: wire.HarnessClaude, SessionID: sid}, Peer: relayPeer("t1")}},
		{wire.MethodRelayForget, wire.RelayRef{RelayID: relayID(1)}},
	} {
		if err := callErr(c, m.method, m.params, nil); errCode(err) != wire.ErrAgentRefused {
			t.Errorf("%s from an agent: %v, want %s", m.method, err, wire.ErrAgentRefused)
		}
	}
	if _, err := os.Stat(e.layout.Relays); !os.IsNotExist(err) {
		t.Errorf("relays.json written for an agent: %v", err)
	}
}

// A relay within one host: once the target session's conversation id is
// known, the daemon fills it in on both ends — the target link's local
// conversation and the source link's peer — and pushes the source session.
func TestRelayTraceSameHostFillsTarget(t *testing.T) {
	home := t.TempDir()
	pathstest.SetHome(t, home)
	e := startDaemonAt(t, fastConfig(), Options{})
	c, events := e.client()
	var w wire.WatchResult
	call(t, c, wire.MethodWatch, wire.WatchParams{}, &w)
	const tid = "fedcba9876543210"
	const srcConv, tgtConv = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	hs, _ := e.client()
	src := testSession(sid)
	src.ConversationID = srcConv
	call(t, hs, wire.MethodHolderRegister, src, nil)
	ht, _ := e.client()
	call(t, ht, wire.MethodHolderRegister, testSession(tid), nil)

	host := &wire.RelayHost{HostID: "host-mac", Label: "mac"}
	r := relayID(7)
	call(t, c, wire.MethodRelayRecord, wire.RelayRecordParams{
		RelayID: r, Role: wire.RelayRoleTarget,
		Local: wire.RelayEnd{Harness: wire.HarnessClaude, SessionID: tid},
		Peer:  wire.RelayEnd{Host: host, Harness: wire.HarnessClaude, ConversationID: srcConv, SessionID: sid},
	}, nil)
	call(t, c, wire.MethodRelayRecord, wire.RelayRecordParams{
		RelayID: r, Role: wire.RelayRoleSource,
		Local: wire.RelayEnd{Harness: wire.HarnessClaude, ConversationID: srcConv, SessionID: sid},
		Peer:  wire.RelayEnd{Host: host, Harness: wire.HarnessClaude, SessionID: tid},
	}, nil)
	if got := listedSession(t, c, sid).Relay; got == nil || got.Target == nil || got.Target.ConversationID != "" {
		t.Fatalf("source trace before: %+v", got)
	}

	call(t, ht, wire.MethodHookEvent, wire.HookEventParams{Harness: wire.HarnessClaude, Event: "SessionStart", SessionID: tid,
		Payload: json.RawMessage(`{"session_id":"` + tgtConv + `"}`)}, nil)
	sessionUpdate(t, events, "source's target filled", sid, func(s wire.Session) bool {
		return s.Relay != nil && s.Relay.Target != nil && s.Relay.Target.ConversationID == tgtConv && s.Relay.Target.SessionID == tid
	})
	for _, l := range readRelaysFile(t, e).Links {
		if l.Role == wire.RelayRoleSource && l.Peer.ConversationID != tgtConv || l.Role == wire.RelayRoleTarget && l.Local.ConversationID != tgtConv {
			t.Errorf("relays.json link %+v", l)
		}
	}
}
