package bridge

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/relay"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/task"
	"github.com/obutora/stagent/internal/transcript"
	"github.com/obutora/stagent/internal/wire"
)

// targetRelay is the queue of the relay methods (PROTOCOL.md "Relay"),
// but relay.discard, served at once, and relay.record, the daemon's.
const targetRelay = "relay"

const (
	// relayProgressEvery bounds relay.progress while a stage moves on.
	relayProgressEvery = 500 * time.Millisecond
	// relayMemoPoll is how often the memo's wait looks at the session.
	relayMemoPoll = 250 * time.Millisecond
	// relayDiscardWait bounds waiting for a cancelled relay.prepare.
	relayDiscardWait = 30 * time.Second
	// relayLabelMax bounds a host's label, in characters.
	relayLabelMax = 100
	// relayInstructionMax bounds relay.prepare's instruction.
	relayInstructionMax = 64 << 10
	// relayTitleScan is how many conversations of the harness are looked
	// through for the conversation's title.
	relayTitleScan = 500
)

// relayQueued are the relay methods served on targetRelay.
var relayQueued = map[string]bool{
	wire.MethodRelayPrepare: true, wire.MethodRelayRead: true, wire.MethodRelayReceive: true,
	wire.MethodRelayWrite: true, wire.MethodRelayStart: true, wire.MethodRelayAbort: true,
}

// errDiscarded is the cause of a relay.prepare cancelled by relay.discard.
var errDiscarded = errors.New("discarded")

// relayRun is a relay.prepare running: relay.discard cancels it and waits
// for done.
type relayRun struct {
	cancel context.CancelCauseFunc
	done   chan struct{}
}

// serveRelay serves a queued relay method. Nothing happens for a coding
// agent's process tree (ADR 0004). What has passed its time goes first.
func (b *Bridge) serveRelay(m *wire.Msg) {
	if werr := b.agentRefusal(); werr != nil {
		b.replyResult(m.ID, nil, werr)
		return
	}
	relay.Sweep(b.l, time.Now())
	var res any
	var err error
	switch m.Method {
	case wire.MethodRelayPrepare:
		var p wire.RelayPrepareParams
		if err = rpc.Decode(m, &p); err == nil {
			res, err = b.relayPrepare(p)
		}
	case wire.MethodRelayRead:
		var p wire.RelayReadParams
		if err = rpc.Decode(m, &p); err == nil {
			res, err = b.relayRead(p)
		}
	case wire.MethodRelayReceive:
		var p wire.RelayReceiveParams
		if err = rpc.Decode(m, &p); err == nil {
			res, err = b.relayReceive(p)
		}
	case wire.MethodRelayWrite:
		var p wire.RelayWriteParams
		if err = rpc.Decode(m, &p); err == nil {
			res, err = b.relayWrite(p)
		}
	case wire.MethodRelayStart:
		var p wire.RelayStartParams
		if err = rpc.Decode(m, &p); err == nil {
			res, err = b.relayStart(p)
		}
	case wire.MethodRelayAbort:
		var p wire.RelayRef
		if err = rpc.Decode(m, &p); err == nil {
			res, err = b.relayAbort(p)
		}
	}
	b.replyResult(m.ID, res, err)
}

// relayDiscard serves relay.discard at once, beside the queue: a
// relay.prepare of the relay still running is cancelled first.
func (b *Bridge) relayDiscard(m *wire.Msg) {
	var p wire.RelayRef
	if err := rpc.Decode(m, &p); err != nil {
		b.replyResult(m.ID, nil, err)
		return
	}
	if werr := b.agentRefusal(); werr != nil {
		b.replyResult(m.ID, nil, werr)
		return
	}
	if !wire.ValidRelayID(p.RelayID) {
		b.replyResult(m.ID, nil, badRelayID(wire.MethodRelayDiscard, p.RelayID))
		return
	}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.relayMu.Lock()
		run := b.relayRuns[p.RelayID]
		b.relayMu.Unlock()
		if run != nil {
			run.cancel(errDiscarded)
			select {
			case <-run.done:
			case <-time.After(relayDiscardWait):
			}
		}
		if err := relay.Discard(b.l, p.RelayID); err != nil {
			b.replyResult(m.ID, nil, wire.Errorf(wire.ErrInternal, "%s: %v", wire.MethodRelayDiscard, err))
			return
		}
		b.replyResult(m.ID, struct{}{}, nil)
	}()
}

func badRelayID(method, id string) error {
	return wire.Errorf(wire.ErrBadRequest, "%s: invalid relay_id %q", method, id)
}

// progress sends relay.progress of one relay: begin when a stage begins,
// move (at most every relayProgressEvery) while it moves on.
type progress struct {
	b     *Bridge
	id    string
	stage string
	last  time.Time
}

func (p *progress) begin(stage string) {
	p.stage, p.last = stage, time.Now()
	p.b.out.Notify(wire.NotifyRelayProgress, wire.RelayProgressParams{RelayID: p.id, Stage: stage})
}

func (p *progress) move(done, total int64) {
	if time.Since(p.last) < relayProgressEvery {
		return
	}
	p.last = time.Now()
	p.b.out.Notify(wire.NotifyRelayProgress, wire.RelayProgressParams{RelayID: p.id, Stage: p.stage, Done: done, Total: total})
}

// fail is err as the answer of the stage it failed at: "<stage>: …",
// internal unless it is a wire error already (its message then gets the
// stage).
func (p *progress) fail(err error) error {
	var we *wire.Error
	if errors.As(err, &we) {
		return wire.Errorf(we.Code, "%s: %s", p.stage, we.Message)
	}
	return wire.Errorf(wire.ErrInternal, "%s: %s", p.stage, oneLine(err.Error()))
}

func validLabel(h wire.RelayHost) bool {
	n := utf8.RuneCountInString(h.Label)
	return n >= 1 && n <= relayLabelMax && strings.TrimSpace(h.Label) != ""
}

// ---------------------------------------------------------------------------
// relay.prepare

// relaySource is the conversation relay.prepare relays and its session at
// the moment it is read.
type relaySource struct {
	session    *wire.Session // nil: no session (a stored conversation)
	harness    string
	conv       string
	path       string
	title      string
	cwd        string
	approvals  []wire.Approval
	transcript int64 // the transcript's size when read
}

func (b *Bridge) relayPrepare(p wire.RelayPrepareParams) (*wire.RelayPrepareResult, error) {
	const method = wire.MethodRelayPrepare
	switch {
	case p.Target != wire.RelayTargetSameHost && p.Target != wire.RelayTargetOtherHost:
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: target must be same_host or other_host", method)
	case p.SourceAgent != wire.RelaySourceKeep && p.SourceAgent != wire.RelaySourceEnd:
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: source_agent must be keep or end", method)
	case !validLabel(p.SourceHost):
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: source_host.label must be 1-%d characters", method, relayLabelMax)
	case len(p.Instruction) > relayInstructionMax:
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: instruction over %d bytes", method, relayInstructionMax)
	case p.MemoTimeoutMS < 0 || p.MemoTimeoutMS > wire.RelayMemoTimeoutMax:
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: memo_timeout_ms must be 0-%d", method, wire.RelayMemoTimeoutMax)
	case p.SessionID == "" && (p.Harness == "" || p.ConversationID == ""):
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: session_id or harness and conversation_id required", method)
	}
	if p.Lang != wire.LangJa {
		p.Lang = wire.LangEn
	}
	dc, err := b.daemonConn(b.dialDaemon)
	if err != nil {
		return nil, upstreamError(err, wire.Errorf(wire.ErrUnavailable, "daemon: %v", err))
	}
	b.rewatch(dc)

	id := wire.NewRelayID()
	ctx, cancel := context.WithCancelCause(b.ctx)
	run := &relayRun{cancel: cancel, done: make(chan struct{})}
	b.relayMu.Lock()
	b.relayRuns[id] = run
	b.relayMu.Unlock()
	defer func() {
		b.relayMu.Lock()
		delete(b.relayRuns, id)
		b.relayMu.Unlock()
		cancel(nil)
		close(run.done)
	}()

	out := relay.OutDir(b.l, id)
	pr := &progress{b: b, id: id}
	res, err := b.prepare(ctx, dc, id, out, p, pr)
	if err != nil {
		os.RemoveAll(out)
		if errors.Is(context.Cause(ctx), errDiscarded) {
			err = wire.Errorf(wire.ErrNotFound, "%s: %v", cmp.Or(pr.stage, wire.RelayStageTranscript), errDiscarded)
		}
		return nil, withRelayID(err, id)
	}
	return res, nil
}

// withRelayID is relay.prepare's error with the relay's id as its data
// (wire.RelayErrorData), for the client's relay.discard.
func withRelayID(err error, id string) error {
	we := &wire.Error{Code: wire.ErrInternal, Message: oneLine(err.Error())}
	if e, ok := errors.AsType[*wire.Error](err); ok {
		we = &wire.Error{Code: e.Code, Message: e.Message}
	} else if ue := upstreamError(err, nil); ue != nil {
		we = ue
	}
	we.Data, _ = json.Marshal(wire.RelayErrorData{RelayID: id})
	return we
}

// prepare builds relay id's package in out (see relayPrepare).
func (b *Bridge) prepare(ctx context.Context, dc *daemonConn, id, out string, p wire.RelayPrepareParams, pr *progress) (*wire.RelayPrepareResult, error) {
	src, err := b.relaySourceOf(ctx, dc, p)
	if err != nil {
		return nil, err
	}
	env := b.worktreeEnv()
	sameHost := p.Target == wire.RelayTargetSameHost
	inPlace := sameHost && p.SourceAgent == wire.RelaySourceEnd

	// (1) transcript: up to its size now, the session's state with it.
	pr.begin(wire.RelayStageTranscript)
	if err := relay.MkdirPrivate(out); err != nil {
		return nil, pr.fail(err)
	}
	entries, err := transcript.ReadEntries(src.path, src.harness, src.transcript)
	if err != nil {
		return nil, pr.fail(err)
	}
	var co *relay.Checkout
	if task.IsDir(src.cwd) {
		if co, err = relay.Inspect(ctx, env, src.cwd); err != nil {
			return nil, pr.fail(err)
		}
	}
	source := wire.RelaySource{
		Host: p.SourceHost, Harness: src.harness, ConversationID: src.conv, Title: src.title,
		Cwd: src.cwd, Target: p.Target, SourceAgent: p.SourceAgent, PreparedAt: time.Now().UnixMilli(),
	}
	var question *wire.Question
	if s := src.session; s != nil {
		source.SessionID, source.State, source.TaskID = s.ID, s.State, s.TaskID
		question = s.Question
	}
	if co != nil {
		source.Repo, source.CwdRel, source.Remote, source.RemoteURL = co.Repo, co.CwdRel, co.Remote, co.RemoteURL
		source.Branch, source.Head, source.Base = co.Branch, co.Head, co.Base
	}

	// (2) memo
	var memo wire.RelayMemo
	var memoDoc *relay.MemoDoc
	if p.Memo {
		pr.begin(wire.RelayStageMemo)
		timeout := time.Duration(cmp.Or(p.MemoTimeoutMS, wire.RelayMemoTimeoutDefault)) * time.Millisecond
		text, m, err := b.relayMemo(ctx, dc, src, len(entries), p.Lang, timeout, pr)
		if err != nil {
			return nil, pr.fail(err)
		}
		memo = m
		switch m.Status {
		case wire.RelayMemoIncluded:
			memoDoc = &relay.MemoDoc{Text: text}
		case wire.RelayMemoTimeout:
			memoDoc = &relay.MemoDoc{TimedOut: true}
		}
	}

	// (3)–(5) the working tree.
	var parts wire.RelayParts
	var tree *relay.TreeState
	var untracked *relay.Untracked
	treeDir := filepath.Join(out, relay.TreeDir)
	tooLarge := func(stage string) error {
		total := parts.BundleBytes + parts.PatchBytes + parts.UntrackedBytes
		if total <= wire.RelayPackageMax {
			return nil
		}
		return wire.Errorf(wire.ErrTooLarge, "%s: %v", stage, relay.TooLarge(total, parts.BundleBytes, untracked))
	}
	if co != nil && !inPlace {
		if err := relay.MkdirPrivate(treeDir); err != nil {
			return nil, pr.fail(err)
		}
		if !sameHost {
			pr.begin(wire.RelayStageBundle)
			bundle := filepath.Join(treeDir, task.RelayBundle)
			made, err := co.Bundle(ctx, env, bundle)
			if err != nil {
				return nil, pr.fail(err)
			}
			if made {
				st, err := os.Stat(bundle)
				if err != nil {
					return nil, pr.fail(err)
				}
				parts.BundleCommits, parts.BundleBytes = co.Unpushed, st.Size()
			}
			if err := tooLarge(wire.RelayStageBundle); err != nil {
				return nil, err
			}
		}
		pr.begin(wire.RelayStageDiff)
		staged, unstaged := filepath.Join(treeDir, task.RelayStaged), filepath.Join(treeDir, task.RelayUnstaged)
		if parts.PatchBytes, err = co.Diff(ctx, env, staged, unstaged); err != nil {
			return nil, pr.fail(err)
		}
		if err := tooLarge(wire.RelayStageDiff); err != nil {
			return nil, err
		}
		pr.begin(wire.RelayStageUntracked)
		if untracked, err = co.Untracked(ctx, env, filepath.Join(treeDir, task.RelayUntracked), func(done int64) { pr.move(done, 0) }); err != nil {
			return nil, pr.fail(err)
		}
		parts.UntrackedFiles, parts.UntrackedBytes = len(untracked.Files), untracked.Bytes
		if err := tooLarge(wire.RelayStageUntracked); err != nil {
			return nil, err
		}
		tree = co.TreeState(ctx, env, parts.BundleCommits, fileSize(staged), fileSize(unstaged), untracked)
	}
	if err := ctx.Err(); err != nil {
		return nil, pr.fail(err)
	}

	// The document, its images and the memo, as the conversation stood.
	doc, err := relay.WriteDocument(out, relay.DocInput{
		Lang: p.Lang, Instruction: p.Instruction, Source: source,
		InPlace: inPlace || sameHost && co == nil, SourceMayWork: sameHost && co == nil && p.SourceAgent == wire.RelaySourceKeep,
		Entries: entries, Question: question, Approvals: src.approvals, Tree: tree, Memo: memoDoc,
	})
	if err != nil {
		return nil, pr.fail(err)
	}
	parts.DocumentBytes, parts.Messages, parts.Images, parts.ImageBytes = doc.Bytes, doc.Messages, doc.Images, doc.ImageBytes
	if memo.Status == wire.RelayMemoIncluded {
		if err := os.WriteFile(filepath.Join(out, relay.MemoFile), []byte(memoDoc.Text), 0o600); err != nil {
			return nil, pr.fail(err)
		}
	}
	if err := relay.WriteManifest(out, &relay.Manifest{Version: relay.ManifestVersion, RelayID: id, Source: source, Parts: parts, Memo: memo}); err != nil {
		return nil, pr.fail(err)
	}
	res := &wire.RelayPrepareResult{RelayID: id, Source: source, Parts: parts, Memo: memo}
	if sameHost {
		return res, nil
	}

	// (6) pack
	pr.begin(wire.RelayStagePack)
	if total := parts.DocumentBytes + parts.ImageBytes + parts.BundleBytes + parts.PatchBytes + parts.UntrackedBytes; total > wire.RelayPackageMax {
		return nil, wire.Errorf(wire.ErrTooLarge, "%s: %v", wire.RelayStagePack, relay.TooLarge(total, parts.BundleBytes, untracked))
	}
	pkg := filepath.Join(out, relay.PackageFile)
	size, sum, err := relay.Pack(out, pkg, pr.move)
	if err != nil {
		return nil, pr.fail(err)
	}
	if size > wire.RelayPackageMax {
		return nil, wire.Errorf(wire.ErrTooLarge, "%s: %v", wire.RelayStagePack, relay.TooLarge(size, parts.BundleBytes, untracked))
	}
	// Only the package stays.
	ents, _ := os.ReadDir(out)
	for _, e := range ents {
		if e.Name() != relay.PackageFile {
			os.RemoveAll(filepath.Join(out, e.Name()))
		}
	}
	res.Size, res.SHA256 = size, sum
	return res, nil
}

func fileSize(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return st.Size()
}

// relaySourceOf finds the conversation relay.prepare names: the session's
// (one the daemon lists, with a conversation id and an agent harness), or
// harness's conversation id with its live session when it has one.
func (b *Bridge) relaySourceOf(ctx context.Context, dc *daemonConn, p wire.RelayPrepareParams) (*relaySource, error) {
	const method = wire.MethodRelayPrepare
	cctx, cancel := context.WithTimeout(ctx, daemonCallWait)
	defer cancel()
	var list wire.SessionsListResult
	if err := dc.c.Call(cctx, wire.MethodSessionsList, struct{}{}, &list); err != nil {
		return nil, err
	}
	src := &relaySource{}
	get := wire.TranscriptGetParams{Limit: 1}
	if p.SessionID != "" {
		i := slices.IndexFunc(list.Sessions, func(s wire.Session) bool { return s.ID == p.SessionID })
		if i < 0 {
			return nil, wire.Errorf(wire.ErrNotFound, "%s: session %s", method, p.SessionID)
		}
		s := list.Sessions[i]
		if s.ConversationID == "" || !harness.Known(s.Harness) {
			return nil, wire.Errorf(wire.ErrBadRequest, "%s: session %s has no agent conversation", method, s.ID)
		}
		src.session, src.harness, src.conv, src.cwd = &s, s.Harness, s.ConversationID, s.Cwd
		get.SessionID = s.ID
	} else {
		if !harness.Known(p.Harness) {
			return nil, wire.Errorf(wire.ErrBadRequest, "%s: unknown harness %q", method, p.Harness)
		}
		src.harness, src.conv = p.Harness, p.ConversationID
		for i := range list.Sessions {
			s := list.Sessions[i]
			if !ended(s) && s.Harness == p.Harness && s.ConversationID == p.ConversationID {
				src.session, src.cwd = &s, s.Cwd
				break
			}
		}
		get.Harness, get.ConversationID = p.Harness, p.ConversationID
	}
	var tr wire.TranscriptGetResult
	if err := dc.c.Call(cctx, wire.MethodTranscriptGet, get, &tr); err != nil {
		var we *wire.Error
		if errors.As(err, &we) {
			return nil, wire.Errorf(we.Code, "%s: %s", method, we.Message)
		}
		return nil, err
	}
	src.path, src.harness = tr.Path, tr.Harness
	var convs wire.ConversationsListResult
	if err := dc.c.Call(cctx, wire.MethodConversationsList, wire.ConversationsListParams{Harness: []string{src.harness}, Limit: relayTitleScan}, &convs); err == nil {
		for _, c := range convs.Conversations {
			if c.ID == src.conv {
				src.title = c.Title
				src.cwd = cmp.Or(src.cwd, c.Cwd)
			}
		}
	}
	if src.title == "" && src.session != nil {
		src.title = src.session.Title
	}
	if src.cwd == "" {
		src.cwd = b.l.Home
	}
	// The conversation as it stands now: the transcript's size and the
	// session's state, question and approvals taken together.
	end, err := transcript.EndOffset(src.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, wire.Errorf(wire.ErrNotFound, "%s: %s", method, src.path)
		}
		return nil, err
	}
	src.transcript = end
	if src.session != nil && !ended(*src.session) {
		var al wire.ApprovalsListResult
		if err := dc.c.Call(cctx, wire.MethodApprovalsList, struct{}{}, &al); err == nil {
			for _, a := range al.Approvals {
				if a.SessionID == src.session.ID {
					src.approvals = append(src.approvals, a)
				}
			}
		}
	}
	return src, nil
}

// relayMemo asks the source agent for its hand-off memo when its session
// is live and at rest, and waits until it has gone working and come back
// to rest, at most timeout. The memo is the text of the assistant rows
// the transcript gained after the request's row.
func (b *Bridge) relayMemo(ctx context.Context, dc *daemonConn, src *relaySource, read int, lang string, timeout time.Duration, pr *progress) (string, wire.RelayMemo, error) {
	s := src.session
	if s == nil || ended(*s) {
		return "", wire.RelayMemo{Status: wire.RelayMemoSkipped, Reason: wire.RelayMemoNotLive}, nil
	}
	if !atRest(*s) {
		return "", wire.RelayMemo{Status: wire.RelayMemoSkipped, Reason: wire.RelayMemoBusy}, nil
	}
	c, err := b.holderConn(s.ID)
	if err != nil {
		return "", wire.RelayMemo{Status: wire.RelayMemoSkipped, Reason: wire.RelayMemoNotLive}, nil
	}
	cctx, cancel := context.WithTimeout(ctx, daemonCallWait)
	err = c.Call(cctx, wire.MethodSessionInput, wire.InputParams{ID: s.ID, Paste: relay.MemoRequest(lang), Submit: true}, nil)
	cancel()
	var we *wire.Error
	switch {
	case errors.As(err, &we) && we.Code == wire.ErrMenuOpen:
		return "", wire.RelayMemo{Status: wire.RelayMemoSkipped, Reason: wire.RelayMemoBusy}, nil
	case errors.As(err, &we) && we.Code == wire.ErrSessionEnded:
		return "", wire.RelayMemo{Status: wire.RelayMemoSkipped, Reason: wire.RelayMemoNotLive}, nil
	case err != nil:
		return "", wire.RelayMemo{}, err
	}
	start := time.Now()
	deadline := start.Add(timeout)
	worked := false
	for {
		select {
		case <-ctx.Done():
			return "", wire.RelayMemo{}, context.Cause(ctx)
		case <-time.After(relayMemoPoll):
		}
		pr.move(time.Since(start).Milliseconds(), timeout.Milliseconds())
		now := b.relaySession(ctx, dc, s.ID)
		if now == nil || ended(*now) {
			break
		}
		if now.State == wire.StateWorking {
			worked = true
		}
		if atRest(*now) {
			// A reply too quick to be seen working still shows in the
			// transcript.
			if text := memoText(src, read); worked || text != "" {
				if text != "" {
					return text, wire.RelayMemo{Status: wire.RelayMemoIncluded}, nil
				}
			}
		}
		if time.Now().After(deadline) {
			break
		}
	}
	if text := memoText(src, read); text != "" && time.Now().Before(deadline) {
		return text, wire.RelayMemo{Status: wire.RelayMemoIncluded}, nil
	}
	return "", wire.RelayMemo{Status: wire.RelayMemoTimeout}, nil
}

// atRest: idle, or waiting for input with no question.
func atRest(s wire.Session) bool {
	return s.State == wire.StateIdle || s.State == wire.StateWaitingInput && s.Question == nil
}

// relaySession is session id as the daemon lists it now, nil when gone.
func (b *Bridge) relaySession(ctx context.Context, dc *daemonConn, id string) *wire.Session {
	cctx, cancel := context.WithTimeout(ctx, daemonCallWait)
	defer cancel()
	var list wire.SessionsListResult
	if err := dc.c.Call(cctx, wire.MethodSessionsList, struct{}{}, &list); err != nil {
		return nil
	}
	for i := range list.Sessions {
		if list.Sessions[i].ID == id {
			return &list.Sessions[i]
		}
	}
	return nil
}

// memoText is the text of the assistant rows after the first user row the
// transcript gained past its first read entries, joined by blank lines.
func memoText(src *relaySource, read int) string {
	end, err := transcript.EndOffset(src.path)
	if err != nil {
		return ""
	}
	entries, err := transcript.ReadEntries(src.path, src.harness, end)
	if err != nil || len(entries) <= read {
		return ""
	}
	gained := entries[read:]
	if i := slices.IndexFunc(gained, func(e transcript.Entry) bool { return e.Role == transcript.RoleUser }); i >= 0 {
		gained = gained[i+1:]
	}
	var texts []string
	for _, e := range gained {
		if e.Role == transcript.RoleAssistant && strings.TrimSpace(e.Text) != "" {
			texts = append(texts, strings.TrimSpace(e.Text))
		}
	}
	return strings.Join(texts, "\n\n")
}

// ---------------------------------------------------------------------------
// relay.read, relay.receive, relay.write

func (b *Bridge) relayRead(p wire.RelayReadParams) (*wire.RelayReadResult, error) {
	const method = wire.MethodRelayRead
	if !wire.ValidRelayID(p.RelayID) {
		return nil, badRelayID(method, p.RelayID)
	}
	n := p.MaxBytes
	switch {
	case n == 0:
		n = wire.RelayChunkDefault
	case n < 0 || n > wire.RelayChunkMax:
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: max_bytes must be 1-%d", method, wire.RelayChunkMax)
	}
	f, err := os.Open(filepath.Join(relay.OutDir(b.l, p.RelayID), relay.PackageFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, wire.Errorf(wire.ErrNotFound, "%s: no package of relay %s here", method, p.RelayID)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if p.Offset < 0 || p.Offset > st.Size() {
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: offset %d past the package's %d bytes", method, p.Offset, st.Size())
	}
	buf := make([]byte, min(int64(n), st.Size()-p.Offset))
	if _, err := f.ReadAt(buf, p.Offset); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return &wire.RelayReadResult{Data: buf, EOF: p.Offset+int64(len(buf)) == st.Size()}, nil
}

// receiving is the package relay.receive gets ready for.
type receiving struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

const receiveFile = "receive.json"

func readReceiving(dir string) *receiving {
	b, err := os.ReadFile(filepath.Join(dir, receiveFile))
	if err != nil {
		return nil
	}
	var r receiving
	if json.Unmarshal(b, &r) != nil {
		return nil
	}
	return &r
}

func validSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range []byte(s) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (b *Bridge) relayReceive(p wire.RelayReceiveParams) (*wire.RelayReceiveResult, error) {
	const method = wire.MethodRelayReceive
	switch {
	case !wire.ValidRelayID(p.RelayID):
		return nil, badRelayID(method, p.RelayID)
	case !validSHA256(p.SHA256):
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: sha256 must be 64 lowercase hex chars", method)
	case p.Size <= 0:
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: size must be positive", method)
	case p.Size > wire.RelayPackageMax:
		return nil, wire.Errorf(wire.ErrTooLarge, "%s: the package's %d bytes pass %d", wire.RelayStageReceive, p.Size, wire.RelayPackageMax)
	}
	dir := relay.InDir(b.l, p.RelayID)
	if relay.ReadStarted(dir) != nil {
		return nil, wire.Errorf(wire.ErrBusy, "%s: relay %s is started (relay.abort first)", method, p.RelayID)
	}
	pkg := filepath.Join(dir, relay.PackageFile)
	if r := readReceiving(dir); r != nil && r.Size == p.Size && r.SHA256 == p.SHA256 {
		return &wire.RelayReceiveResult{Received: fileSize(pkg)}, nil
	}
	if err := relay.MkdirPrivate(dir); err != nil {
		return nil, err
	}
	b2, _ := json.Marshal(receiving{Size: p.Size, SHA256: p.SHA256})
	if err := os.WriteFile(filepath.Join(dir, receiveFile), b2, 0o600); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(pkg, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return &wire.RelayReceiveResult{}, nil
}

func (b *Bridge) relayWrite(p wire.RelayWriteParams) (*wire.RelayWriteResult, error) {
	const method = wire.MethodRelayWrite
	if !wire.ValidRelayID(p.RelayID) {
		return nil, badRelayID(method, p.RelayID)
	}
	dir := relay.InDir(b.l, p.RelayID)
	r := readReceiving(dir)
	if r == nil {
		return nil, wire.Errorf(wire.ErrNotFound, "%s: relay %s is not being received (relay.receive first)", method, p.RelayID)
	}
	pkg := filepath.Join(dir, relay.PackageFile)
	got := fileSize(pkg)
	switch {
	case len(p.Data) > wire.RelayChunkMax:
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: data over %d bytes; received %d", wire.RelayStageReceive, wire.RelayChunkMax, got)
	case p.Offset != got:
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: offset %d is not the bytes received, %d", wire.RelayStageReceive, p.Offset, got)
	case got+int64(len(p.Data)) > r.Size:
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: data past the package's %d bytes; received %d", wire.RelayStageReceive, r.Size, got)
	}
	f, err := os.OpenFile(pkg, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	_, werr := f.Write(p.Data)
	if err := errors.Join(werr, f.Close()); err != nil {
		return nil, err
	}
	return &wire.RelayWriteResult{Received: got + int64(len(p.Data))}, nil
}

// ---------------------------------------------------------------------------
// relay.start

func (b *Bridge) relayStart(p wire.RelayStartParams) (*wire.RelayStartResult, error) {
	const method = wire.MethodRelayStart
	h, known := harness.Lookup(p.Harness)
	switch {
	case !wire.ValidRelayID(p.RelayID):
		return nil, badRelayID(method, p.RelayID)
	case !known || p.Harness == wire.HarnessOther:
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: harness must be an agent harness", method)
	case p.Model != "" && !relay.ValidModel(p.Model):
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: invalid model %q", method, p.Model)
	case !validLabel(p.TargetHost):
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: target_host.label must be 1-%d characters", method, relayLabelMax)
	case p.Clone != nil && p.Clone.Dir != "" && (!filepath.IsAbs(b.l.ExpandHome(p.Clone.Dir)) || !relay.IsEmptyOrAbsent(b.l.ExpandHome(p.Clone.Dir))):
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: clone.dir must be an absolute path that does not exist or is empty", method)
	case p.Cwd != "" && !task.IsDir(b.l.ExpandHome(p.Cwd)):
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: cwd %q is not a directory", method, p.Cwd)
	}
	cols, rows, err := termSize(method, p.Cols, p.Rows)
	if err != nil {
		return nil, err
	}
	if _, err := addEnv(method, nil, p.Env); err != nil {
		return nil, err
	}
	dc, err := b.daemonConn(b.dialDaemon)
	if err != nil {
		return nil, upstreamError(err, wire.Errorf(wire.ErrUnavailable, "daemon: %v", err))
	}
	b.rewatch(dc)
	ctx, cancel := context.WithTimeout(b.ctx, taskRemoveWait)
	defer cancel()
	in := relay.InDir(b.l, p.RelayID)
	if st := relay.ReadStarted(in); st != nil {
		if b.relayStillThere(ctx, dc, st) {
			return nil, wire.Errorf(wire.ErrBusy, "%s: relay %s is started (relay.abort first)", method, p.RelayID)
		}
	}
	pr := &progress{b: b, id: p.RelayID}
	res, err := b.start(ctx, dc, p, h, in, cols, rows, pr)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// relayStillThere: what a relay.start made is there yet — its task, or
// without a task its live session.
func (b *Bridge) relayStillThere(ctx context.Context, dc *daemonConn, st *relay.Started) bool {
	if st.TaskID != "" {
		_, err := findTask(ctx, dc, st.TaskID)
		return err == nil
	}
	s := b.relaySession(ctx, dc, st.SessionID)
	return s != nil && !ended(*s)
}

// start is relayStart's stages: unpack, repo / clone, task or spawn.
func (b *Bridge) start(ctx context.Context, dc *daemonConn, p wire.RelayStartParams, h harness.Harness, in string, cols, rows int, pr *progress) (*wire.RelayStartResult, error) {
	const method = wire.MethodRelayStart
	env := b.worktreeEnv()

	// (1) unpack
	pr.begin(wire.RelayStageUnpack)
	if err := relay.Undo(b.l, p.RelayID); err != nil {
		return nil, pr.fail(err)
	}
	out := relay.OutDir(b.l, p.RelayID)
	var m *relay.Manifest
	var treeDir string
	if rc := readReceiving(in); rc != nil {
		pkg := filepath.Join(in, relay.PackageFile)
		size, sum, err := relay.FileSHA256(pkg)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil, wire.Errorf(wire.ErrNotFound, "%s: no package of relay %s here", wire.RelayStageUnpack, p.RelayID)
		case err != nil:
			return nil, pr.fail(err)
		case size != rc.Size:
			return nil, wire.Errorf(wire.ErrBadRequest, "%s: %d of the package's %d bytes received", wire.RelayStageUnpack, size, rc.Size)
		case sum != rc.SHA256:
			return nil, wire.Errorf(wire.ErrBadRequest, "%s: sha256 mismatch", wire.RelayStageUnpack)
		}
		if m, err = relay.Unpack(pkg, in, pr.move); err != nil {
			relay.Undo(b.l, p.RelayID)
			return nil, pr.fail(err)
		}
		treeDir = filepath.Join(in, relay.TreeDir)
	} else {
		var err error
		if m, err = relay.ReadManifest(out); err != nil || m.Source.Target != wire.RelayTargetSameHost {
			return nil, wire.Errorf(wire.ErrNotFound, "%s: no package of relay %s here", wire.RelayStageUnpack, p.RelayID)
		}
		if err := relay.CopyParts(out, in); err != nil {
			return nil, pr.fail(err)
		}
		treeDir = filepath.Join(out, relay.TreeDir)
	}
	if m.RelayID != p.RelayID {
		relay.Undo(b.l, p.RelayID)
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: the package is relay %s's", wire.RelayStageUnpack, m.RelayID)
	}
	src := m.Source
	sameHost := src.Target == wire.RelayTargetSameHost
	document := filepath.Join(in, relay.DocumentFile)
	argv, err := relay.AgentArgv(h.ID, p.Model, document, runtime.GOOS)
	if err != nil {
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: %v", method, err)
	}
	env = ensureOnPath(env, argv[0], b.l.Home)
	env = append(env, wire.EnvRelayDocument+"="+document, wire.EnvRelayID+"="+p.RelayID)
	if env, err = addEnv(method, env, p.Env); err != nil {
		return nil, err
	}
	undo := func(err error) error {
		relay.Undo(b.l, p.RelayID)
		return err
	}

	// No task: the source's own place (within one host, source_agent end,
	// or no git checkout), or with no git checkout cwd on another host.
	if sameHost && src.SourceAgent == wire.RelaySourceEnd || src.Repo == "" {
		pr.begin(wire.RelayStageSpawn)
		cwd, taskID := src.Cwd, ""
		switch {
		case sameHost && src.SourceAgent == wire.RelaySourceEnd:
			taskID = src.TaskID
		case !sameHost:
			cwd = cmp.Or(b.l.ExpandHome(p.Cwd), b.l.Home)
		}
		if !task.IsDir(cwd) {
			return nil, undo(wire.Errorf(wire.ErrBadRequest, "%s: %s is not a directory", wire.RelayStageSpawn, cwd))
		}
		var s *wire.Session
		err := b.onSpawnQueue(func() error {
			var err error
			s, err = b.startSession(wire.NewSessionID(), argv, cwd, cols, rows, env, taskID)
			return err
		})
		if err != nil {
			return nil, undo(pr.fail(err))
		}
		if err := relay.WriteStarted(in, relay.Started{SessionID: s.ID, At: time.Now().UnixMilli()}); err != nil {
			return nil, pr.fail(err)
		}
		b.relayRecord(ctx, dc, p, src, *s)
		return &wire.RelayStartResult{Session: *s, Document: document}, nil
	}

	// (2) the place to work: the checkout of the repository.
	pr.begin(wire.RelayStageRepo)
	repo, remote, cloned, err := b.relayRepo(ctx, dc, env, p, src, pr)
	if err != nil {
		return nil, undo(err)
	}

	// (3) the task, a named one on a new branch.
	pr.begin(wire.RelayStageTask)
	reg, err := b.relayTask(ctx, dc, env, p, src, repo)
	if err != nil {
		return nil, undo(err)
	}
	if err := task.SaveRelayPrep(in, task.RelayPrep{
		ID: p.RelayID, Tree: treeDir, Remote: remote, Head: src.Head, CwdRel: src.CwdRel,
	}); err != nil {
		return nil, undo(pr.fail(err))
	}
	var tc *wire.TaskCreateResult
	err = b.onSpawnQueue(func() error {
		var err error
		tc, err = b.startPrepared(ctx, dc, reg, func(*wire.Task) []string {
			return []string{"--relay", p.RelayID}
		}, argv, cols, rows, env)
		return err
	})
	if err != nil {
		return nil, undo(pr.fail(err))
	}
	if tc.Existed || tc.Session == nil {
		return nil, undo(wire.Errorf(wire.ErrInternal, "%s: task %s exists already", wire.RelayStageTask, tc.Task.ID))
	}
	if err := relay.WriteStarted(in, relay.Started{
		TaskID: tc.Task.ID, SessionID: tc.Session.ID, Branch: reg.Branch, Repo: repo, At: time.Now().UnixMilli(),
	}); err != nil {
		return nil, pr.fail(err)
	}
	b.relayRecord(ctx, dc, p, src, *tc.Session)
	t := tc.Task
	return &wire.RelayStartResult{Task: &t, Session: *tc.Session, Document: document, Cloned: cloned}, nil
}

// onSpawnQueue runs fn on the spawn queue (sessions start one at a time)
// and waits for it.
func (b *Bridge) onSpawnQueue(fn func() error) error {
	done := make(chan error, 1)
	b.enqueue(targetSpawn, &wire.Msg{}, func(*wire.Msg) { done <- fn() })
	select {
	case err := <-done:
		return err
	case <-b.ctx.Done():
		return context.Cause(b.ctx)
	}
}

// relayRepo finds the target's checkout of the package's repository (see
// PROTOCOL.md "Relay"), cloning it with p.Clone; it answers the checkout,
// the remote whose URL is the package's and the clone made.
func (b *Bridge) relayRepo(ctx context.Context, dc *daemonConn, env []string, p wire.RelayStartParams, src wire.RelaySource, pr *progress) (repo, remote, cloned string, err error) {
	stage := wire.RelayStageRepo
	if src.Target == wire.RelayTargetSameHost {
		repo, err := task.Canonical(ctx, env, src.Repo)
		if err != nil {
			return "", "", "", wire.Errorf(wire.ErrNotFound, "%s: the source checkout %s: %v", stage, src.Repo, err)
		}
		return repo, "", "", nil
	}
	if p.Repo != "" {
		repo, err := task.Canonical(ctx, env, b.l.ExpandHome(p.Repo))
		if err != nil {
			return "", "", "", wire.Errorf(wire.ErrBadRequest, "%s: %s: %v", stage, p.Repo, err)
		}
		return repo, relay.MatchRemote(ctx, env, repo, src.RemoteURL), "", nil
	}
	missing := wire.Errorf(wire.ErrRepoMissing, "%s: no checkout of %s on this host", stage, cmp.Or(src.RemoteURL, "the source's repository (it has no remote)"))
	if src.RemoteURL == "" {
		return "", "", "", missing
	}
	h, err := b.hostWorktrees(ctx, env)
	if err != nil {
		return "", "", "", pr.fail(err)
	}
	// The checkout of the newest task first, then path order.
	tasks := slices.Clone(h.tasks)
	slices.SortStableFunc(tasks, func(a, b wire.Task) int { return b.CreatedAt.Compare(a.CreatedAt) })
	var candidates []string
	for _, t := range tasks {
		candidates = append(candidates, t.Repo)
	}
	for _, r := range h.inv.Repos {
		candidates = append(candidates, r.Path)
	}
	seen := map[string]bool{}
	for _, c := range candidates {
		if seen[c] || !task.IsDir(c) {
			continue
		}
		seen[c] = true
		if r := relay.MatchRemote(ctx, env, c, src.RemoteURL); r != "" {
			return c, r, "", nil
		}
	}
	if p.Clone == nil {
		return "", "", "", missing
	}
	pr.begin(wire.RelayStageClone)
	dir := ""
	if p.Clone.Dir != "" {
		dir = b.l.ExpandHome(p.Clone.Dir)
	} else {
		parent := b.l.Home
		if len(h.cfg.WorktreeWatch) > 0 {
			parent = h.cfg.WorktreeWatch[0]
		}
		dir = relay.CloneDir(parent, src.RemoteURL)
	}
	if err := relay.Clone(ctx, env, src.RemoteURL, dir); err != nil {
		return "", "", "", wire.Errorf(wire.ErrInternal, "%s: %s", wire.RelayStageClone, oneLine(err.Error()))
	}
	if repo, err = task.Canonical(ctx, env, dir); err != nil {
		return "", "", "", wire.Errorf(wire.ErrInternal, "%s: %v", wire.RelayStageClone, err)
	}
	repos := slices.Clone(h.cfg.WorktreeRepos)
	if !slices.ContainsFunc(repos, func(r string) bool { return task.SamePath(r, repo) }) {
		repos = append(repos, repo)
	}
	if _, err := setConfig(ctx, dc, map[string]any{"worktree_repos": repos}); err != nil {
		return "", "", "", pr.fail(err)
	}
	pr.begin(wire.RelayStageRepo)
	pr.stage = wire.RelayStageTask
	return repo, relay.MatchRemote(ctx, env, repo, src.RemoteURL), repo, nil
}

// relayTask decides the relay's named task in repo: its branch, name,
// place and base (see PROTOCOL.md "Relay").
func (b *Bridge) relayTask(ctx context.Context, dc *daemonConn, env []string, p wire.RelayStartParams, src wire.RelaySource, repo string) (wire.TaskRegisterParams, error) {
	stage := wire.RelayStageTask
	bad := func(format string, args ...any) error {
		return wire.Errorf(wire.ErrBadRequest, "%s: "+format, append([]any{stage}, args...)...)
	}
	hasBranch := func(name string) bool { return task.HasRef(ctx, env, repo, "refs/heads/"+name) }
	branch := p.Branch
	if branch != "" {
		if err := task.CheckBranch(ctx, env, repo, branch); err != nil {
			return wire.TaskRegisterParams{}, bad("%v", err)
		}
		if hasBranch(branch) {
			return wire.TaskRegisterParams{}, bad("the branch %s exists", branch)
		}
	} else {
		branch = relay.RelayBranch(p.RelayID, src.Branch, hasBranch)
	}
	var list wire.TaskListResult
	if err := dc.c.Call(ctx, wire.MethodTaskList, struct{}{}, &list); err != nil {
		return wire.TaskRegisterParams{}, err
	}
	folder := task.SavedWorktreeFolder(b.l.Config)
	place := func(name string) string {
		if folder != "" {
			return filepath.Join(folder, filepath.Base(repo), name)
		}
		return filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-"+name)
	}
	taken := func(name string) bool {
		if slices.ContainsFunc(list.Tasks, func(t wire.Task) bool { return task.SameKey(&t, repo, wire.TaskKindName, 0, name) }) {
			return true
		}
		return !relay.IsEmptyOrAbsent(place(name))
	}
	name := p.Name
	if name != "" {
		if err := task.ValidName(ctx, env, repo, name); err != nil {
			return wire.TaskRegisterParams{}, bad("%v", err)
		}
		if taken(name) {
			return wire.TaskRegisterParams{}, bad("the name %s is taken", name)
		}
	} else {
		base := relay.NameOf(branch)
		if task.ValidName(ctx, env, repo, base) != nil {
			base = "relay-" + p.RelayID[:8]
		}
		name = base
		for i := 2; taken(name); i++ {
			name = fmt.Sprintf("%s-%d", base, i)
		}
	}
	defaultBranch, _ := task.DefaultBranch(ctx, env, repo)
	return wire.TaskRegisterParams{
		Repo: repo, Kind: wire.TaskKindName, Name: name, Branch: branch, Base: defaultBranch,
		Worktree: place(name), RelayID: p.RelayID,
	}, nil
}

// relayRecord records the target end of the trace on this host's daemon
// and, within one host, the source end too. Failing to is not the
// relay's failure: the agent runs.
func (b *Bridge) relayRecord(ctx context.Context, dc *daemonConn, p wire.RelayStartParams, src wire.RelaySource, s wire.Session) {
	srcHost := src.Host
	call := func(r wire.RelayRecordParams) {
		cctx, cancel := context.WithTimeout(ctx, daemonCallWait)
		defer cancel()
		if err := dc.c.Call(cctx, wire.MethodRelayRecord, r, nil); err != nil {
			fmt.Fprintf(os.Stderr, "stagent bridge: relay.record %s: %v\n", r.Role, err)
		}
	}
	call(wire.RelayRecordParams{
		RelayID: p.RelayID, Role: wire.RelayRoleTarget,
		Local: wire.RelayEnd{Harness: p.Harness, SessionID: s.ID},
		Peer:  wire.RelayEnd{Host: &srcHost, Harness: src.Harness, ConversationID: src.ConversationID, SessionID: src.SessionID},
	})
	if src.Target == wire.RelayTargetSameHost {
		tgtHost := p.TargetHost
		call(wire.RelayRecordParams{
			RelayID: p.RelayID, Role: wire.RelayRoleSource,
			Local: wire.RelayEnd{Harness: src.Harness, ConversationID: src.ConversationID, SessionID: src.SessionID},
			Peer:  wire.RelayEnd{Host: &tgtHost, Harness: p.Harness, SessionID: s.ID},
		})
	}
}

// ---------------------------------------------------------------------------
// relay.abort

// relayAbort undoes what relay.start made for the relay here: its task
// removed (force, stop_sessions, skip_archive) and the branch it made
// deleted, the relay's directory but for the received package, and the
// trace. A relay whose agent runs (its task ready, or without a task its
// session live) is refused.
func (b *Bridge) relayAbort(p wire.RelayRef) (any, error) {
	const method = wire.MethodRelayAbort
	if !wire.ValidRelayID(p.RelayID) {
		return nil, badRelayID(method, p.RelayID)
	}
	dc, err := b.daemonConn(b.dialDaemon)
	if err != nil {
		return nil, upstreamError(err, wire.Errorf(wire.ErrUnavailable, "daemon: %v", err))
	}
	b.rewatch(dc)
	ctx, cancel := context.WithTimeout(b.ctx, taskRemoveWait)
	defer cancel()
	in := relay.InDir(b.l, p.RelayID)
	if st := relay.ReadStarted(in); st != nil {
		if st.TaskID != "" {
			t, err := findTask(ctx, dc, st.TaskID)
			switch {
			case err == nil && t.State == wire.TaskReady:
				return nil, wire.Errorf(wire.ErrRefused, "%s: the relay's agent runs (task %s is ready)", method, t.ID)
			case err == nil:
				if _, _, err := b.doTaskRemove(wire.TaskRemoveParams{ID: t.ID, Force: true, StopSessions: true, SkipArchive: true}, ""); err != nil {
					return nil, err
				}
			default:
				var we *wire.Error
				if !errors.As(err, &we) || we.Code != wire.ErrNotFound {
					return nil, err
				}
			}
			if st.Branch != "" && task.IsDir(st.Repo) {
				env := b.worktreeEnv()
				if task.HasRef(ctx, env, st.Repo, "refs/heads/"+st.Branch) {
					if _, err := task.Git(ctx, env, st.Repo, "branch", "-D", st.Branch); err != nil {
						return nil, wire.Errorf(wire.ErrInternal, "%s: %v", method, err)
					}
				}
				task.Git(ctx, env, st.Repo, "update-ref", "-d", task.RelayRef(p.RelayID))
			}
		} else if s := b.relaySession(ctx, dc, st.SessionID); s != nil && !ended(*s) {
			return nil, wire.Errorf(wire.ErrRefused, "%s: the relay's agent runs (session %s)", method, s.ID)
		}
	}
	cctx, ccancel := context.WithTimeout(ctx, daemonCallWait)
	defer ccancel()
	if err := dc.c.Call(cctx, wire.MethodRelayForget, p, nil); err != nil {
		return nil, err
	}
	if err := relay.Undo(b.l, p.RelayID); err != nil {
		return nil, wire.Errorf(wire.ErrInternal, "%s: %v", method, err)
	}
	return struct{}{}, nil
}
