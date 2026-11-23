package usage

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// Antigravity usage: Antigravity CLI tells the quota of the account it is
// logged in to with `agy -p /usage --output-format json`, a read-only
// slash command in print mode (no agent turn, no quota spent, no hook
// events). agy calls Google's quota API with its own login; stagent never
// reads agy's token.

// AgyTimeout bounds one run of `agy -p /usage`.
const AgyTimeout = 30 * time.Second

// AgyUsage runs Antigravity CLI's /usage. Run defaults to exec with
// AgyTimeout from os.TempDir.
type AgyUsage struct {
	Bin string
	Run func(ctx context.Context, bin string, args ...string) ([]byte, error)
}

// Measure runs `agy -p /usage --output-format json` and maps its answer
// (ParseAgyUsage); At is when the answer came.
func (a AgyUsage) Measure(ctx context.Context, now func() time.Time) (Measurement, *APIError) {
	run := a.Run
	if run == nil {
		run = runAgy
	}
	out, err := run(ctx, a.Bin, "-p", "/usage", "--output-format", "json")
	if err != nil {
		// The message is fixed: agy's output may carry account details.
		return Measurement{}, &APIError{Code: wire.UsageErrCommand, Message: "agy /usage failed"}
	}
	ws, err := ParseAgyUsage(out)
	if err != nil {
		return Measurement{}, &APIError{Code: wire.UsageErrBadResponse, Message: "agy /usage: unexpected output"}
	}
	return Measurement{Source: wire.UsageSourceAntigravityUsage, Windows: ws, At: now()}, nil
}

// runAgy runs agy from a neutral directory (no project of the daemon's
// working directory is opened), with the daemon's environment.
func runAgy(ctx context.Context, bin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, AgyTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = os.TempDir()
	return cmd.Output()
}

type agyAnswer struct {
	Status  string `json:"status"`
	Command *struct {
		Data *struct {
			Groups []struct {
				Name    string `json:"name"`
				Buckets []struct {
					ID                string   `json:"id"`
					Window            string   `json:"window"`
					RemainingFraction *float64 `json:"remaining_fraction"`
					ResetTime         string   `json:"reset_time"`
				} `json:"buckets"`
			} `json:"groups"`
		} `json:"data"`
	} `json:"command"`
}

// agyOwnBuckets are the buckets of Antigravity's own models (the "Gemini
// Models" group): its plain five_hour and seven_day.
var agyOwnBuckets = map[string]string{
	"gemini-5h":     wire.UsageWindowFiveHour,
	"gemini-weekly": wire.UsageWindowSevenDay,
}

// ParseAgyUsage maps the answer of `agy -p /usage --output-format json`:
// every bucket with a remaining_fraction becomes a window — 5h five_hour,
// weekly seven_day, the buckets of Antigravity's own models plain, any
// other scoped "<base>_<scope>" and labelled with its group's name — in
// the order five_hour, seven_day, then the scoped ones in agy's group
// order, a group's 5h window before its weekly one (agy lists weekly
// first).
// A disabled bucket (no remaining_fraction), one of another window or
// with an empty scope is left out. An answer whose status is not SUCCESS
// or that carries no groups is an error.
func ParseAgyUsage(b []byte) ([]Window, error) {
	var a agyAnswer
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, err
	}
	if a.Status != "SUCCESS" || a.Command == nil || a.Command.Data == nil || a.Command.Data.Groups == nil {
		return nil, errors.New("agy /usage: not a usage answer")
	}
	var own [2]*Window // five_hour, seven_day
	var scoped []Window
	for _, g := range a.Command.Data.Groups {
		var weekly []Window // the group's scoped weekly windows, after its 5h ones
		for _, bk := range g.Buckets {
			if bk.RemainingFraction == nil {
				continue
			}
			var base, suffix string
			switch bk.Window {
			case "5h":
				base, suffix = wire.UsageWindowFiveHour, "-5h"
			case "weekly":
				base, suffix = wire.UsageWindowSevenDay, "-weekly"
			default:
				continue
			}
			w := Window{UsedPercent: agyPercent(*bk.RemainingFraction)}
			if bk.ResetTime != "" {
				t, err := time.Parse(time.RFC3339, bk.ResetTime)
				if err != nil {
					return nil, err
				}
				w.ResetsAt = t.UnixMilli()
			}
			if id, ok := agyOwnBuckets[bk.ID]; ok && id == base {
				w.ID = id
				i := 0
				if id == wire.UsageWindowSevenDay {
					i = 1
				}
				own[i] = &w
				continue
			}
			scope := agyScope(strings.TrimSuffix(bk.ID, suffix))
			if scope == "" {
				continue
			}
			w.ID, w.Label = base+"_"+scope, g.Name
			if base == wire.UsageWindowSevenDay {
				weekly = append(weekly, w)
			} else {
				scoped = append(scoped, w)
			}
		}
		scoped = append(scoped, weekly...)
	}
	out := make([]Window, 0, 2+len(scoped))
	for _, w := range own {
		if w != nil {
			out = append(out, *w)
		}
	}
	return append(out, scoped...), nil
}

// agyScope is id lowercased with every character outside [a-z0-9]
// dropped.
func agyScope(id string) string {
	var sb strings.Builder
	for _, c := range strings.ToLower(id) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			sb.WriteRune(c)
		}
	}
	return sb.String()
}

// agyPercent is the used share of a remaining fraction as a percentage,
// to two decimals, within 0–100.
func agyPercent(remaining float64) float64 {
	return math.Max(0, math.Min(100, math.Round((1-remaining)*10000)/100))
}
