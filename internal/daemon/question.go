package daemon

import (
	"encoding/json"
	"strings"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/notify"
	"github.com/obutora/stagent/internal/wire"
)

// A question tool is the tool through which a harness asks the user a
// choice question (harness.Harness.QuestionTool): a PreToolUse or
// PermissionRequest hook naming one puts the session in waiting_input with
// the question (wire.Session.Question), its PostToolUse hook ends it.
// Never an approval.

// questionShownHook is the hook that comes when the question of tool
// shows, which tells it in-app outside a stagent session: that of the
// harness whose question tool it is (daemonHarness.questionShown, its
// PreToolUse by default). "" when tool is no question tool.
func questionShownHook(tool string) string {
	h, ok := harness.QuestionToolHarness(tool)
	if !ok {
		return ""
	}
	if e := daemonHarnesses[h.ID].questionShown; e != "" {
		return e
	}
	return hookPreToolUse
}

func isQuestionTool(tool string) bool { return questionShownHook(tool) != "" }

// Question watch. Output while a question is up is the user moving through
// its UI (the highlight, claude's tabs, a multi-select's checks), so it
// does not end the question (stateTrack.holderReport); the hooks do
// (PostToolUse, UserPromptSubmit, Stop), and the screen: an Esc cancels a
// question without a hook. While a question of the session is in force,
// the daemon asks its holder to watch the screen for the question UI
// (holder.question_watch), and the holder reports once the UI it saw left
// the screen (holder.question_gone); the question then goes, and the
// holder's state takes over. Holders before 0.20.0 ignore the watch: there
// the question goes with the next hook only.

// followQuestionLocked starts s's question watch when q, the question in
// force, came, and ends it when it went. One watch serves a question for
// as long as it is in force (claude's PermissionRequest after its
// PreToolUse starts none).
func (d *Daemon) followQuestionLocked(s *session, q *wire.Question) {
	switch {
	case q != nil && s.questionGen == 0:
		d.promptGens++
		s.questionGen = d.promptGens
		d.sendQuestionWatchLocked(s, true)
	case q == nil && s.questionGen != 0:
		d.sendQuestionWatchLocked(s, false)
		s.questionGen = 0
	}
}

// sendQuestionWatchLocked tells s's holder, if connected, to watch (on)
// for the UI of the question in force, as the harness whose question tool
// asked it draws it, or to stop watching (watch s.questionGen). It goes
// through the connection's outbox.
func (d *Daemon) sendQuestionWatchLocked(s *session, on bool) {
	if s.holder == nil || s.holder.c == nil {
		return
	}
	p := wire.HolderPromptWatchParams{ID: s.s.ID, Gen: s.questionGen, On: on}
	if on {
		p.Harness = s.s.Harness
		if q := s.track.Question; q != nil {
			if h, ok := harness.QuestionToolHarness(q.Tool); ok {
				p.Harness = h.ID
			}
		}
	}
	s.holder.outLocked().push(notification(wire.MethodHolderQuestionWatch, p))
}

// holderQuestionGone ends the question in force when the holder reports
// its UI left the screen (watch p.Gen; a report of an older watch is about
// a question already gone). Its pushes settle.
func (d *Daemon) holderQuestionGone(cs *connState, p wire.HolderPromptGoneParams) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.liveSessionLocked(cs, p.ID)
	if s == nil || s.holder != cs || p.Gen <= 0 || p.Gen != s.questionGen {
		return
	}
	s.track = s.track.questionOffScreen()
	changed := d.recomputeLocked(s, false)
	d.settlePushLocked(s)
	if changed {
		d.broadcastLocked(wire.NotifySessionUpdated, s.s)
	}
}

// questionOf reads the questions of a question tool's input:
// {"questions": [{"id", "question", "header", "multiSelect" (claude) |
// "multi" (omp), "options": [{"label"} | "label"]}]} (claude, codex, omp).
// nil when it holds no question text.
func questionOf(tool string, input json.RawMessage) *wire.Question {
	var in struct {
		Questions []struct {
			ID          string            `json:"id"`
			Question    string            `json:"question"`
			Header      string            `json:"header"`
			MultiSelect bool              `json:"multiSelect"`
			Multi       bool              `json:"multi"`
			Options     []json.RawMessage `json:"options"`
		} `json:"questions"`
	}
	if len(input) == 0 || json.Unmarshal(input, &in) != nil {
		return nil
	}
	q := &wire.Question{Tool: tool}
	for _, it := range in.Questions {
		text := strings.TrimSpace(it.Question)
		if text == "" {
			continue
		}
		item := wire.QuestionItem{ID: it.ID, Question: text, Header: strings.TrimSpace(it.Header), Multi: it.MultiSelect || it.Multi}
		for _, raw := range it.Options {
			if l := optionLabel(raw); l != "" {
				item.Options = append(item.Options, l)
			}
		}
		q.Questions = append(q.Questions, item)
	}
	if len(q.Questions) == 0 {
		return nil
	}
	return q
}

func optionLabel(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var o struct {
		Label string `json:"label"`
	}
	if json.Unmarshal(raw, &o) == nil {
		return strings.TrimSpace(o.Label)
	}
	return ""
}

// waitingInputNotice is the waiting_input notification of a session
// titled title: about q when the agent asked one (its first question, and
// how many when several), else the fixed phrase.
func waitingInputNotice(lang, title string, q *wire.Question) wire.NotificationData {
	n := wire.NotificationData{Title: title, Body: notify.Text(lang, notify.PhraseWaitingInput), Level: wire.LevelInfo, Reason: reasonWaitingInput}
	if q == nil || len(q.Questions) == 0 {
		return n
	}
	n.Body, n.Question = clipLine(q.Questions[0].Question, 200), true
	if c := len(q.Questions); c > 1 {
		n.Body = notify.Text(lang, notify.PhraseQuestions, n.Body, c)
	}
	return n
}
