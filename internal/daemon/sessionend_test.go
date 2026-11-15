//go:build !windows

package daemon

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

// startProc runs argv in a session of its own, as a holder runs its
// program; the channel is closed once it exited.
func startProc(t *testing.T, argv ...string) (int, <-chan struct{}) {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-done
	})
	return cmd.Process.Pid, done
}

func procExited(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s still running", what)
	}
}

func procRunning(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
		t.Fatalf("%s was ended", what)
	case <-time.After(100 * time.Millisecond):
	}
}

// procSession is a session whose program and holder have pids program and
// holder and which started at started.
func procSession(id string, program, holder int, started time.Time) wire.Session {
	s := testSession(id)
	s.PID, s.HolderPID, s.StartedAt = program, holder, started.UnixMilli()
	return s
}

// endSession calls session.end, which takes up to endGrace.
func endSession(c *rpc.Client, id, signal string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return c.Call(ctx, wire.MethodSessionEnd, wire.SessionEndParams{ID: id, Signal: signal}, nil)
}

func endedWith(t *testing.T, events chan *wire.Msg) wire.SessionEndedData {
	t.Helper()
	m := await(t, events, "session_ended", event(wire.EventSessionEnded, nil))
	_, se := decodeEvent[wire.SessionEndedData](t, m)
	return se
}

// A session whose holder is registered — its connection open — but
// answers on no socket (deleted by a purge, say) is the zombie the app
// could not end: session.end hangs its program up, kills the program
// (which ignores SIGHUP) and the holder after endGrace, and records the
// end as a hangup.
func TestSessionEndKillsAnUnreachableSession(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	program, programDone := startProc(t, "sh", "-c", "trap '' HUP; exec sleep 60")
	holder, holderDone := startProc(t, "sleep", "60")
	h, _ := e.client()
	call(t, h, wire.MethodHolderRegister, procSession(sid, program, holder, time.Now()), nil)

	bridge, _ := e.client()
	if err := endSession(bridge, sid, wire.SignalHangup); err != nil {
		t.Fatal(err)
	}
	if se := endedWith(t, events); se.ExitCode != 128+int(syscall.SIGKILL) || !se.HungUp {
		t.Fatalf("session_ended %+v, want exit 137 hung up", se)
	}
	procExited(t, programDone, "program")
	procExited(t, holderDone, "holder")
	var sl wire.SessionsListResult
	call(t, watcher, wire.MethodSessionsList, nil, &sl)
	if len(sl.Sessions) != 1 || sl.Sessions[0].ExitCode == nil {
		t.Fatalf("sessions after session.end: %+v", sl.Sessions)
	}

	// An ended session, or one nobody lists, is already over.
	if err := endSession(bridge, sid, wire.SignalHangup); err != nil {
		t.Fatalf("session.end of an ended session: %v", err)
	}
	if err := endSession(bridge, "fedcba9876543210", wire.SignalKill); err != nil {
		t.Fatalf("session.end of an unknown session: %v", err)
	}
	if err := endSession(bridge, sid, wire.SignalInterrupt); errCode(err) != wire.ErrBadRequest {
		t.Fatalf("session.end with interrupt: %v, want bad_request", err)
	}
}

// A holder that is alive reports the exit the hangup caused: that is the
// end (a hangup, though the holder did not send it), and the holder is
// left to exit on its own.
func TestSessionEndLetsALiveHolderReportTheEnd(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	program, programDone := startProc(t, "sleep", "60")
	holder, holderDone := startProc(t, "sleep", "60")
	h, _ := e.client()
	call(t, h, wire.MethodHolderRegister, procSession(sid, program, holder, time.Now()), nil)
	go func() {
		<-programDone
		h.Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: sid, ExitCode: 128 + int(syscall.SIGHUP)})
	}()

	bridge, _ := e.client()
	start := time.Now()
	if err := endSession(bridge, sid, wire.SignalHangup); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d >= endGrace {
		t.Fatalf("session.end took %v though the holder reported the end", d)
	}
	if se := endedWith(t, events); se.ExitCode != 128+int(syscall.SIGHUP) || !se.HungUp {
		t.Fatalf("session_ended %+v, want exit 129 hung up", se)
	}
	procRunning(t, holderDone, "holder")
}

// Pids are reused: processes that started long before the session are not
// its program or holder, and stay. With the holder gone the end is
// recorded at once.
func TestSessionEndSparesProcessesThatAreNotTheSessions(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{GoneGrace: time.Minute})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	program, programDone := startProc(t, "sleep", "60")
	holder, holderDone := startProc(t, "sleep", "60")
	h, _ := e.client()
	call(t, h, wire.MethodHolderRegister, procSession(sid, program, holder, time.Now().Add(-time.Hour)), nil)
	h.Close()
	time.Sleep(100 * time.Millisecond) // the daemon sees the holder go

	bridge, _ := e.client()
	start := time.Now()
	if err := endSession(bridge, sid, wire.SignalTerminate); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d >= endGrace {
		t.Fatalf("session.end took %v with nothing to wait for", d)
	}
	if se := endedWith(t, events); se.ExitCode != 128+int(syscall.SIGTERM) || se.HungUp {
		t.Fatalf("session_ended %+v, want exit 143", se)
	}
	procRunning(t, programDone, "process with the program's pid")
	procRunning(t, holderDone, "process with the holder's pid")
}
