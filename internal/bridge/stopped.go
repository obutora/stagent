package bridge

import (
	"context"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

// stoppedCallWait bounds each daemon call of stopped.resume.
const stoppedCallWait = 10 * time.Second

// conversationIDPattern is what a conversation id must look like to go into
// the resume command line unquoted.
var conversationIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func (b *Bridge) stoppedResume(m *wire.Msg) {
	var p wire.StoppedResumeParams
	if err := rpc.Decode(m, &p); err != nil {
		b.replyResult(m.ID, nil, err)
		return
	}
	res, err := b.doStoppedResume(p)
	b.replyResult(m.ID, res, err)
}

// doStoppedResume continues a stopped agent's conversation in a new kept
// shell: a session in the agent's cwd, tied to its task, whose shell runs
// the harness's resume command and then the login shell. The daemon's
// stopped.claim reserves the stopped agent for the new session first and
// answers the live session instead when one already carries the
// conversation (nothing starts). It runs on the spawn queue.
func (b *Bridge) doStoppedResume(p wire.StoppedResumeParams) (*wire.StoppedResumeResult, error) {
	const method = wire.MethodStoppedResume
	// Its holder would start, refuse this bridge and keep running out of
	// reach (ADR 0004).
	if werr := b.agentRefusal(); werr != nil {
		return nil, werr
	}
	if !wire.StoppedAgentsServedOn(b.goos) {
		return nil, wire.Errorf(wire.ErrUnsupported, "%s: stopped agents are kept on Linux and macOS only", method)
	}
	cols, rows, err := termSize(method, p.Cols, p.Rows)
	if err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: id is empty", method)
	}
	if _, err := addEnv(method, nil, p.Env); err != nil {
		return nil, err
	}
	dc, err := b.daemonConn(b.dialDaemon)
	if err != nil {
		return nil, upstreamError(err, wire.Errorf(wire.ErrUnavailable, "daemon: %v", err))
	}
	b.rewatch(dc)
	id := wire.NewSessionID()
	claim := wire.StoppedClaimParams{ID: p.ID, SessionID: id}
	ctx, cancel := context.WithTimeout(b.ctx, stoppedCallWait)
	var res wire.StoppedClaimResult
	err = dc.c.Call(ctx, wire.MethodStoppedClaim, claim, &res)
	cancel()
	if err != nil {
		return nil, err
	}
	if res.Session != nil {
		return &wire.StoppedResumeResult{Session: *res.Session, Existed: true}, nil
	}
	a := res.Agent
	if a == nil {
		return nil, wire.Errorf(wire.ErrInternal, "%s: stopped.claim answered nothing", method)
	}
	// From here on a failure releases the claim.
	fail := func(err error) (*wire.StoppedResumeResult, error) {
		ctx, cancel := context.WithTimeout(b.ctx, stoppedCallWait)
		defer cancel()
		var none struct{}
		dc.c.Call(ctx, wire.MethodStoppedUnclaim, claim, &none)
		return nil, err
	}
	argv, ok := harness.ResumeArgv(a.Harness, a.ConversationID)
	if !ok || !conversationIDPattern.MatchString(a.ConversationID) {
		return fail(wire.Errorf(wire.ErrBadRequest, "%s: cannot resume conversation %q of %s", method, a.ConversationID, a.Harness))
	}
	if st, err := os.Stat(a.Cwd); err != nil || !st.IsDir() {
		return fail(wire.Errorf(wire.ErrBadRequest, "%s: cwd %q is not a directory", method, a.Cwd))
	}
	env := ensureOnPath(b.sessionEnv(), argv[0], b.l.Home)
	if env, err = addEnv(method, env, p.Env); err != nil {
		return fail(err)
	}
	// The user stays in a kept shell when the agent exits. trap keeps the
	// wrapper going when a SIGINT reaches the process group.
	shell := shellCommand(env)[0]
	command := []string{"/bin/sh", "-c", "trap : INT; " + strings.Join(argv, " ") + "; exec " + shQuote(shell) + " -l"}
	s, err := b.startSession(id, command, a.Cwd, cols, rows, env, a.TaskID)
	if err != nil {
		return fail(err)
	}
	return &wire.StoppedResumeResult{Session: *s}, nil
}

// shQuote quotes s for a POSIX shell.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
