package follow

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

// Two Pi agents in one working directory, one per stagent session: the
// cwd fallback hands the directory's newest conversation — the other
// session's — to each of them (Pi opens its file only to append, and its
// process title hides the arguments, so neither open files nor the
// command line tell them apart). The daemon's record of the followed
// session, what its hooks reported (Pi's extension reports session_id
// and transcript_path), names the conversation to follow.
func TestFollowSessionRecordNamesPiTranscript(t *testing.T) {
	x := newRecordFixture(t)
	root := x.piRoot()
	other := piSession(t, root, "/work", false, time.Now().Add(-time.Minute), time.Now().Add(-time.Second), 1) // newer: the other session's
	own := piSession(t, root, "/work", false, time.Now().Add(-time.Minute), time.Now().Add(-2*time.Second), 2)
	x.d.record = wire.Session{ID: testSessionID, Harness: wire.HarnessPi, ConversationID: v7(time.Now().Add(-time.Minute), 2), TranscriptPath: own}
	x.pi(602, 601)
	x.do(x.f.scan())

	fs := x.frames()
	if len(fs) != 2 || len(fs[0].Agents) != 1 || fs[0].Agents[0].TranscriptPath != own {
		t.Fatalf("target: %+v", fs)
	}
	if fs[1].Path != own {
		t.Fatalf("messages path %q, want the session's own %q (not %q)", fs[1].Path, own, other)
	}
}

// A record without a path (the transcript not reported yet) names the
// conversation by id; Pi's conversation ids are the session ids its
// extension reports.
func TestFollowSessionRecordByConversationID(t *testing.T) {
	x := newRecordFixture(t)
	root := x.piRoot()
	// One instant: the conversation id embeds its milliseconds.
	created := time.Now().Add(-time.Minute)
	piSession(t, root, "/work", false, created, time.Now().Add(-time.Second), 1) // newer: the other session's
	own := piSession(t, root, "/work", false, created, time.Now().Add(-2*time.Second), 2)
	x.d.record = wire.Session{ID: testSessionID, Harness: wire.HarnessPi, ConversationID: v7(created, 2)}
	x.pi(602, 601)
	x.do(x.f.scan())

	fs := x.frames()
	if len(fs) != 2 || fs[1].Path != own {
		t.Fatalf("frames: %+v", fs)
	}
}

// Without a record (the daemon not running, or none of its sessions
// matching) the fallback stands: the directory's newest conversation.
func TestFollowSessionRecordMissingFallsBack(t *testing.T) {
	x := newRecordFixture(t)
	root := x.piRoot()
	newest := piSession(t, root, "/work", false, time.Now().Add(-time.Minute), time.Now().Add(-time.Second), 1)
	piSession(t, root, "/work", false, time.Now().Add(-time.Minute), time.Now().Add(-2*time.Second), 2)
	x.pi(602, 601) // x.d.record stays zero: the daemon lists only another session
	x.do(x.f.scan())

	fs := x.frames()
	if len(fs) != 2 || fs[1].Path != newest {
		t.Fatalf("frames: %+v", fs)
	}
}

// A record whose transcript file is gone (the path reported, the file
// since deleted) must not pin the agent to a missing file.
func TestFollowSessionRecordGoneFileFallsBack(t *testing.T) {
	x := newRecordFixture(t)
	root := x.piRoot()
	newest := piSession(t, root, "/work", false, time.Now().Add(-time.Minute), time.Now().Add(-time.Second), 1)
	x.d.record = wire.Session{ID: testSessionID, Harness: wire.HarnessPi, TranscriptPath: filepath.Join(root, "gone.jsonl")}
	x.pi(602, 601)
	x.do(x.f.scan())

	fs := x.frames()
	if len(fs) != 2 || fs[1].Path != newest {
		t.Fatalf("frames: %+v", fs)
	}
}

// Any process of the user can report a hook, so a record's path outside
// the harness's transcript directory — a key, a token file, anything that
// happens to be JSON lines — is not streamed, even through a ".." that
// starts below the root.
func TestFollowSessionRecordOutsideRootsFallsBack(t *testing.T) {
	for name, report := range map[string]func(home, root, file string) string{
		"outside": func(home, _, file string) string { return filepath.Join(home, "outside", file) },
		// Unclean, as a hook may send it: lexically below the root first.
		"traversal": func(_, root, file string) string { return root + "/../../../outside/" + file },
	} {
		t.Run(name, func(t *testing.T) {
			x := newRecordFixture(t)
			root := x.piRoot()
			newest := piSession(t, root, "/work", false, time.Now().Add(-time.Minute), time.Now().Add(-time.Second), 1)
			// A readable Pi transcript, so only where it lies keeps it out.
			secret := piSession(t, filepath.Join(x.home, "outside"), "/work", true, time.Now().Add(-time.Minute), time.Now(), 2)
			reported := report(x.home, root, filepath.Base(secret))
			x.d.record = wire.Session{ID: testSessionID, Harness: wire.HarnessPi, TranscriptPath: reported}
			x.pi(602, 601)
			x.do(x.f.scan())

			fs := x.frames()
			if len(fs) != 2 || fs[1].Path != newest {
				t.Fatalf("frames: %+v, want the fallback %q, not %q", fs, newest, reported)
			}
		})
	}
}

// The record names the conversation of its harness's agent only: a kept
// shell running a Pi agent and a Claude Code one keeps the Claude agent on
// its own conversation, not the Pi session's.
func TestFollowSessionRecordOnlyForItsHarness(t *testing.T) {
	x := newRecordFixture(t)
	root := x.piRoot()
	own := piSession(t, root, "/work", false, time.Now().Add(-time.Minute), time.Now().Add(-2*time.Second), 2)
	x.d.record = wire.Session{ID: testSessionID, Harness: wire.HarnessPi, TranscriptPath: own}
	x.pi(602, 601)
	_, claudePath := x.claude(603, 601, "hi")
	x.do(x.f.scan())

	got := map[string]string{}
	for _, a := range x.f.agents {
		got[a.info.Harness] = a.info.TranscriptPath
	}
	if got[wire.HarnessPi] != own || got[wire.HarnessClaude] != claudePath {
		t.Fatalf("paths %v, want pi %q and claude %q", got, own, claudePath)
	}
}

// A daemon that stops answering (restarting) keeps the last record, so the
// agent does not jump to the directory's newest conversation for a scan;
// once the daemon answers again without the session, the record is
// dropped and the fallback stands.
func TestFollowSessionRecordAcrossDaemonRestart(t *testing.T) {
	x := newRecordFixture(t)
	root := x.piRoot()
	newest := piSession(t, root, "/work", false, time.Now().Add(-time.Minute), time.Now().Add(-time.Second), 1)
	own := piSession(t, root, "/work", false, time.Now().Add(-time.Minute), time.Now().Add(-2*time.Second), 2)
	x.d.record = wire.Session{ID: testSessionID, Harness: wire.HarnessPi, TranscriptPath: own}
	x.pi(602, 601)
	path := func() string {
		t.Helper()
		if len(x.f.agents) != 1 {
			t.Fatalf("agents: %+v", x.f.agents)
		}
		return x.f.agents[0].info.TranscriptPath
	}

	x.do(x.f.scan())
	if p := path(); p != own {
		t.Fatalf("with the record: %q, want %q", p, own)
	}
	x.d.stop()
	x.do(x.f.scan())
	if p := path(); p != own {
		t.Fatalf("daemon down: %q, want the last record's %q", p, own)
	}
	x.d.set(func(d *fakeDaemon) { d.down, d.record = false, wire.Session{} })
	x.do(x.f.scan())
	if p := path(); p != newest {
		t.Fatalf("daemon back without the session: %q, want the fallback %q", p, newest)
	}
}

// fakeDaemon serves sessions.list over in-memory pipes: another session,
// and record when its ID is set.
type fakeDaemon struct {
	mu     sync.Mutex
	record wire.Session
	down   bool // dialing fails, as with no socket
	conns  []net.Conn
}

func (d *fakeDaemon) dial() (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.down {
		return nil, errors.New("connect: no such file or directory")
	}
	c, s := net.Pipe()
	d.conns = append(d.conns, s)
	go rpc.Serve(context.Background(), s, func(_ context.Context, _ *rpc.Conn, m *wire.Msg) (any, error) {
		if m.Method != wire.MethodSessionsList {
			return nil, wire.Errorf(wire.ErrUnknownMethod, "%s", m.Method)
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		res := wire.SessionsListResult{Sessions: []wire.Session{{ID: "fedcba9876543210", Harness: wire.HarnessClaude}}}
		if d.record.ID != "" {
			res.Sessions = append(res.Sessions, d.record)
		}
		return res, nil
	})
	return c, nil
}

// stop makes the daemon go away: its connections close and its socket is
// gone.
func (d *fakeDaemon) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.down = true
	for _, c := range d.conns {
		c.Close()
	}
	d.conns = nil
}

func (d *fakeDaemon) set(f func(d *fakeDaemon)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f(d)
}

// recordFixture is a follower of a stagent session (as --session sets up:
// the holder's program as the only root) whose daemon is x.d.
type recordFixture struct {
	*fixture
	d *fakeDaemon
}

func newRecordFixture(t *testing.T) *recordFixture {
	h := &fakeHolder{pid: 601}
	x := &recordFixture{fixture: sessionFixture(t, h), d: &fakeDaemon{}}
	// The session's program tree, as --session walks it: the holder's
	// program (a kept shell) with the Pi agent in it.
	x.tr.add(600, 1, "stagent", "stagent", "run", "--handoff=auto", "--", "bash", "-l")
	x.tr.add(601, 600, "bash", "bash", "-l")
	r := &daemonRecord{id: testSessionID, dial: x.d.dial}
	t.Cleanup(r.drop)
	x.f.record = r
	return x
}

// piRoot is this fixture's Pi sessions root (<home>/.pi/agent/sessions),
// as the follower's roots resolve them.
func (x *recordFixture) piRoot() string {
	return filepath.Join(x.home, ".pi", "agent", "sessions")
}

// pi adds a Pi agent process below ppid in /work, as a holder session's
// program tree holds it (its title hides any session arguments).
func (x *recordFixture) pi(pid, ppid int) *fakeProc {
	p := x.tr.add(pid, ppid, "pi", "pi")
	p.cwd = "/work"
	return p.fg()
}
