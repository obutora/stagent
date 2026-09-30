package daemon

import (
	"errors"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/obutora/stagent/internal/transcript"
	"github.com/obutora/stagent/internal/wire"
)

func knownHarness(h string) bool {
	return h == wire.HarnessClaude || h == wire.HarnessCodex || h == wire.HarnessOmp
}

// conversations lists stored conversations, marking those a live session
// is running.
func (d *Daemon) conversations(p wire.ConversationsListParams) (wire.ConversationsListResult, error) {
	for _, h := range p.Harness {
		if !knownHarness(h) {
			return wire.ConversationsListResult{}, wire.Errorf(wire.ErrBadRequest, "unknown harness %q", h)
		}
	}
	list, err := d.index.List(p.Harness, p.Limit)
	if err != nil {
		return wire.ConversationsListResult{}, err
	}
	d.mu.Lock()
	live := map[[2]string]string{}
	for _, s := range d.sessions {
		if !s.ended && s.s.ConversationID != "" {
			live[[2]string{s.s.Harness, s.s.ConversationID}] = s.s.ID
		}
	}
	d.mu.Unlock()
	for i := range list {
		list[i].LiveSessionID = live[[2]string{list[i].Harness, list[i].ID}]
	}
	if list == nil {
		list = []wire.Conversation{}
	}
	return wire.ConversationsListResult{Conversations: list}, nil
}

// resolveTranscript finds the file and harness a transcript request names:
// a live session, a harness conversation id, or a path under a harness
// transcript directory.
func (d *Daemon) resolveTranscript(sessionID, harness, conversationID, path string) (string, string, error) {
	switch {
	case sessionID != "":
		d.mu.Lock()
		s := d.sessions[sessionID]
		var h, p, conv string
		if s != nil {
			h, p, conv = s.s.Harness, s.s.TranscriptPath, s.s.ConversationID
		}
		d.mu.Unlock()
		if s == nil {
			return "", "", wire.Errorf(wire.ErrNotFound, "session %s", sessionID)
		}
		if p == "" && conv != "" && knownHarness(h) {
			p, _ = d.index.Find(h, conv)
		}
		if p == "" {
			return "", "", wire.Errorf(wire.ErrNotFound, "session %s has no transcript yet", sessionID)
		}
		if !knownHarness(h) {
			if rh, ok := d.roots.HarnessOf(p); ok {
				h = rh
			} else {
				return "", "", wire.Errorf(wire.ErrUnsupported, "no transcript format for harness %q", h)
			}
		}
		return p, h, nil
	case harness != "" && conversationID != "":
		if !knownHarness(harness) {
			return "", "", wire.Errorf(wire.ErrBadRequest, "unknown harness %q", harness)
		}
		p, ok := d.index.Find(harness, conversationID)
		if !ok {
			return "", "", wire.Errorf(wire.ErrNotFound, "%s conversation %s", harness, conversationID)
		}
		return p, harness, nil
	case path != "":
		h, ok := d.roots.HarnessOf(path)
		if !ok {
			return "", "", wire.Errorf(wire.ErrBadRequest, "not a transcript path: %s", path)
		}
		return filepath.Clean(path), h, nil
	}
	return "", "", wire.Errorf(wire.ErrBadRequest, "session_id, harness+conversation_id or path required")
}

func (d *Daemon) transcriptGet(p wire.TranscriptGetParams) (wire.TranscriptGetResult, error) {
	path, harness, err := d.resolveTranscript(p.SessionID, p.Harness, p.ConversationID, p.Path)
	if err != nil {
		return wire.TranscriptGetResult{}, err
	}
	page, err := transcript.ReadPage(path, harness, p.Before, p.Limit)
	if errors.Is(err, fs.ErrNotExist) {
		return wire.TranscriptGetResult{}, wire.Errorf(wire.ErrNotFound, "%s", path)
	}
	if err != nil {
		return wire.TranscriptGetResult{}, err
	}
	msgs := page.Messages
	if msgs == nil {
		msgs = []wire.Message{}
	}
	return wire.TranscriptGetResult{Path: path, Harness: harness, Messages: msgs, Cursor: page.Cursor}, nil
}

// tailSub is one transcript subscription of a connection. Session
// subscriptions follow the session's current transcript (a new
// conversation in the same session switches files).
type tailSub struct {
	cs        *connState
	sessionID string
	path      string
	harness   string
	offset    int64
}

func (d *Daemon) transcriptSubscribe(cs *connState, p wire.TranscriptSubscribeParams) error {
	sub := &tailSub{cs: cs, sessionID: p.SessionID}
	if p.SessionID != "" {
		d.mu.Lock()
		s := d.sessions[p.SessionID]
		if s != nil {
			sub.path, sub.harness = s.s.TranscriptPath, s.s.Harness
		}
		d.mu.Unlock()
		if s == nil {
			return wire.Errorf(wire.ErrNotFound, "session %s", p.SessionID)
		}
	} else {
		path, harness, err := d.resolveTranscript("", "", "", p.Path)
		if err != nil {
			return err
		}
		sub.path, sub.harness = path, harness
	}
	if sub.path != "" {
		// Start at the end: history comes from transcript.get.
		sub.offset, _ = transcript.EndOffset(sub.path)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	for _, old := range cs.subs {
		if old.sessionID == sub.sessionID && (sub.sessionID != "" || old.path == sub.path) {
			return nil // already subscribed
		}
	}
	cs.subs = append(cs.subs, sub)
	d.tails[sub] = struct{}{}
	cs.outLocked()
	if !d.tailing {
		d.tailing = true
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.tailLoop()
		}()
	}
	return nil
}

func (d *Daemon) transcriptUnsubscribe(cs *connState, p wire.TranscriptSubscribeParams) {
	path := ""
	if p.Path != "" {
		path = filepath.Clean(p.Path)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	kept := cs.subs[:0]
	for _, sub := range cs.subs {
		match := (p.SessionID != "" && sub.sessionID == p.SessionID) ||
			(p.SessionID == "" && sub.sessionID == "" && sub.path == path)
		if match {
			delete(d.tails, sub)
			continue
		}
		kept = append(kept, sub)
	}
	cs.subs = kept
}

// tailLoop polls subscribed transcripts while any subscription exists and
// pushes new messages as `transcript` notifications. It exits when the
// last subscription goes away.
func (d *Daemon) tailLoop() {
	t := time.NewTicker(d.opts.TranscriptPoll)
	defer t.Stop()
	type job struct {
		sub     *tailSub
		path    string
		harness string
		offset  int64
	}
	for {
		select {
		case <-t.C:
		case <-d.shutdown:
			d.mu.Lock()
			d.tailing = false
			d.mu.Unlock()
			return
		}
		d.mu.Lock()
		if len(d.tails) == 0 {
			d.tailing = false
			d.mu.Unlock()
			return
		}
		jobs := make([]job, 0, len(d.tails))
		for sub := range d.tails {
			if sub.sessionID != "" {
				if s := d.sessions[sub.sessionID]; s != nil && s.s.TranscriptPath != sub.path {
					// A new conversation file in the session: read it whole.
					sub.path, sub.harness, sub.offset = s.s.TranscriptPath, s.s.Harness, 0
					if !knownHarness(sub.harness) {
						sub.harness, _ = d.roots.HarnessOf(sub.path)
					}
				}
			}
			if sub.path != "" && knownHarness(sub.harness) {
				jobs = append(jobs, job{sub, sub.path, sub.harness, sub.offset})
			}
		}
		d.mu.Unlock()

		for _, j := range jobs {
			msgs, next, err := transcript.ReadFrom(j.path, j.harness, j.offset)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				d.logf("daemon: tail %s: %v", j.path, err)
			}
			d.mu.Lock()
			if _, ok := d.tails[j.sub]; ok && j.sub.path == j.path && j.sub.offset == j.offset {
				j.sub.offset = next
				if len(msgs) > 0 {
					j.sub.cs.outLocked().push(notification(wire.NotifyTranscript, wire.TranscriptParams{
						SessionID: j.sub.sessionID, Path: j.path, Messages: msgs,
					}))
				}
			}
			d.mu.Unlock()
		}
	}
}
