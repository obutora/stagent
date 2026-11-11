package follow

import (
	"path/filepath"
	"testing"
	"time"

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
	x.record = wire.Session{ID: testSessionID, Harness: wire.HarnessPi, ConversationID: v7(time.Now().Add(-time.Minute), 2), TranscriptPath: own}
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
	piSession(t, root, "/work", false, time.Now().Add(-time.Minute), time.Now().Add(-time.Second), 1) // newer: the other session's
	own := piSession(t, root, "/work", false, time.Now().Add(-time.Minute), time.Now().Add(-2*time.Second), 2)
	x.record = wire.Session{ID: testSessionID, Harness: wire.HarnessPi, ConversationID: v7(time.Now().Add(-time.Minute), 2)}
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
	x.pi(602, 601) // x.record stays zero: sessionRecord answers "no"
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
	x.record = wire.Session{ID: testSessionID, Harness: wire.HarnessPi, TranscriptPath: filepath.Join(root, "gone.jsonl")}
	x.pi(602, 601)
	x.do(x.f.scan())

	fs := x.frames()
	if len(fs) != 2 || fs[1].Path != newest {
		t.Fatalf("frames: %+v", fs)
	}
}

// recordFixture is a follower of a stagent session (as --session sets up:
// the holder's program as the only root) whose daemon record is x.record.
type recordFixture struct {
	*fixture
	record wire.Session
}

func newRecordFixture(t *testing.T) *recordFixture {
	h := &fakeHolder{pid: 601}
	x := &recordFixture{fixture: sessionFixture(t, h)}
	// The session's program tree, as --session walks it: the holder's
	// program (a kept shell) with the Pi agent in it.
	x.tr.add(600, 1, "stagent", "stagent", "run", "--handoff=auto", "--", "bash", "-l")
	x.tr.add(601, 600, "bash", "bash", "-l")
	x.f.sessionRecord = func() (wire.Session, bool) { return x.record, x.record.ID != "" }
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
