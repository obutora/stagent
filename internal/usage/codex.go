package usage

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strconv"
	"time"
)

// codexReader reads Codex CLI's rollouts,
// <CODEX_HOME>/sessions/YYYY/MM/DD/rollout-*.jsonl. A resumed conversation
// is appended to its rollout. Archived rollouts (archived_sessions) are not
// read: a rollout moved there keeps what was counted while it was in
// sessions.
var codexReader = &reader{
	files: func(root string) ([]string, error) { return listJSONL(filepath.Join(root, "sessions")) },
	parse: parseCodex,
}

var (
	codexSessionMeta = []byte(`"session_meta"`)
	codexTurnContext = []byte(`"turn_context"`)
	codexTokenCount  = []byte(`"token_count"`)
)

// codexUsage is a token_count's usage. cached_input_tokens is part of
// input_tokens and reasoning_output_tokens part of output_tokens.
type codexUsage struct {
	Input  int64 `json:"input_tokens"`
	Cached int64 `json:"cached_input_tokens"`
	Output int64 `json:"output_tokens"`
}

func (u *codexUsage) counts() [3]int64 { return [3]int64{u.Input, u.Cached, u.Output} }

// codexLine is the part of a rollout line usage is read from.
type codexLine struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   struct {
		Type string `json:"type"`
		// session_meta
		ID        string `json:"id"`
		SessionID string `json:"session_id"`
		Cwd       string `json:"cwd"`
		// turn_context
		Model     string `json:"model"`
		ModelName string `json:"model_name"`
		// token_count; also turn_context's model in some versions.
		Info *struct {
			Model string      `json:"model"`
			Total *codexUsage `json:"total_token_usage"`
			Last  *codexUsage `json:"last_token_usage"`
		} `json:"info"`
		// token_count, also with info:null.
		RateLimits json.RawMessage `json:"rate_limits"`
	} `json:"payload"`
}

// parseCodex reads a rollout line: session_meta gives the session and the
// project, turn_context the model of the turn, and each event_msg
// token_count with usage (info not null) is one request: its
// last_token_usage, or, when only total_token_usage is written, the
// difference from the previous total. A total that went back (the
// conversation compacted or resumed by a new process) is the new baseline
// and counts nothing; a total equal to the previous one is the same
// request written again.
func parseCodex(st *lineState, line []byte) (record, bool) {
	if !bytes.Contains(line, codexTokenCount) && !bytes.Contains(line, codexTurnContext) &&
		!bytes.Contains(line, codexSessionMeta) {
		return record{}, false
	}
	var l codexLine
	if json.Unmarshal(line, &l) != nil {
		return record{}, false
	}
	p := &l.Payload
	switch l.Type {
	case "session_meta":
		st.Session = p.ID
		if st.Session == "" {
			st.Session = p.SessionID
		}
		st.Project = p.Cwd
		return record{}, false
	case "turn_context":
		switch {
		case p.Model != "":
			st.Model = p.Model
		case p.ModelName != "":
			st.Model = p.ModelName
		case p.Info != nil && p.Info.Model != "":
			st.Model = p.Info.Model
		}
		return record{}, false
	case "event_msg":
	default:
		return record{}, false
	}
	if p.Type != "token_count" {
		return record{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, l.Timestamp)
	if err != nil {
		return record{}, false
	}
	if rl, ok := codexRateLimits(p.RateLimits, at); ok && (st.Limits == nil || rl.At >= st.Limits.At) {
		st.Limits = &rl
	}
	if p.Info == nil {
		return record{}, false // info:null: rate_limits only
	}
	var u [3]int64 // input (cached included), cached input, output
	var ident [3]int64
	switch total := p.Info.Total; {
	case total != nil:
		cur := total.counts()
		prev := st.Total
		st.Total = cur
		if cur == prev {
			return record{}, false
		}
		ident = cur
		if p.Info.Last != nil {
			u = p.Info.Last.counts()
			break
		}
		for i := range cur {
			if cur[i] < prev[i] {
				return record{}, false
			}
			u[i] = cur[i] - prev[i]
		}
	case p.Info.Last != nil:
		u = p.Info.Last.counts()
		ident = u
	default:
		return record{}, false
	}
	if u == ([3]int64{}) {
		return record{}, false
	}
	cached := min(max(u[1], 0), u[0])
	model := st.Model
	if model == "" {
		model = p.Info.Model
	}
	// The same line copied into a forked rollout has the same time and
	// counts, so it stays counted once.
	key := "codex:" + l.Timestamp
	for _, n := range ident {
		key += ":" + strconv.FormatInt(n, 10)
	}
	return record{
		key:     key,
		at:      at.UnixMilli(),
		model:   model,
		project: st.Project,
		session: st.Session,
		tokens:  tokens{u[0] - cached, u[2], cached},
	}, true
}

// RateLimits are the limit windows a harness wrote in a transcript (codex:
// a token_count's rate_limits), with the time of the event.
type RateLimits struct {
	At      int64    `json:"at"`             // unix ms of the event
	Plan    string   `json:"plan,omitempty"` // the account's plan (codex plan_type: "plus")
	Windows []Window `json:"windows"`
}

// codexRateLimits reads a token_count's rate_limits written at at: its
// primary and secondary windows (readWindowPair) and plan_type. ok is
// false when no window is complete (rate_limits null or empty, both
// windows null).
func codexRateLimits(raw json.RawMessage, at time.Time) (RateLimits, bool) {
	var rl struct {
		Primary   json.RawMessage `json:"primary"`
		Secondary json.RawMessage `json:"secondary"`
		PlanType  string          `json:"plan_type"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &rl) != nil {
		return RateLimits{}, false
	}
	out := RateLimits{At: at.UnixMilli(), Plan: rl.PlanType, Windows: readWindowPair(rl.Primary, rl.Secondary, at)}
	return out, len(out.Windows) > 0
}
