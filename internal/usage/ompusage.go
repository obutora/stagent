package usage

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// omp の記録: omp (oh-my-pi) keeps its own login to Claude and Codex and
// tells their limit windows with `omp usage --json` (a call to the
// providers' usage APIs) and `omp usage --history --json` (the hourly
// snapshots omp recorded, no call). stagent runs those commands and never
// reads omp's tokens.

// OmpTimeout bounds one run of `omp usage`.
const OmpTimeout = 15 * time.Second

// ompHarnesses maps omp's provider ids to the harness whose account the
// windows belong to.
var ompHarnesses = map[string]string{
	"anthropic":    wire.HarnessClaude,
	"openai-codex": wire.HarnessCodex,
}

// OmpMeasurement is what omp told for the account of one harness.
type OmpMeasurement struct {
	Harness string
	Measurement
}

// OmpUsage runs omp's usage command. Run defaults to exec with
// OmpTimeout.
type OmpUsage struct {
	Bin string
	Run func(ctx context.Context, bin string, args ...string) ([]byte, error)
}

// Live runs `omp usage --json`.
func (o OmpUsage) Live(ctx context.Context) ([]OmpMeasurement, *APIError) {
	return o.call(ctx, ParseOmpLive, "usage", "--json")
}

// History runs `omp usage --history --json --days 1` and takes the newest
// snapshot of every limit.
func (o OmpUsage) History(ctx context.Context) ([]OmpMeasurement, *APIError) {
	return o.call(ctx, ParseOmpHistory, "usage", "--history", "--json", "--days", "1")
}

func (o OmpUsage) call(ctx context.Context, parse func([]byte) ([]OmpMeasurement, error), args ...string) ([]OmpMeasurement, *APIError) {
	run := o.Run
	if run == nil {
		run = runOmp
	}
	out, err := run(ctx, o.Bin, args...)
	if err != nil {
		// The message is fixed: omp's output may carry account details.
		return nil, &APIError{Code: wire.UsageErrCommand, Message: "omp usage failed"}
	}
	ms, err := parse(out)
	if err != nil {
		return nil, &APIError{Code: wire.UsageErrBadResponse, Message: "omp usage: unexpected output"}
	}
	return ms, nil
}

func runOmp(ctx context.Context, bin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, OmpTimeout)
	defer cancel()
	return exec.CommandContext(ctx, bin, args...).Output()
}

type ompLive struct {
	Reports *[]struct {
		Provider  string `json:"provider"`
		FetchedAt int64  `json:"fetchedAt"`
		Limits    []struct {
			Scope struct {
				WindowID string `json:"windowId"`
				Tier     string `json:"tier"`
			} `json:"scope"`
			Window *struct {
				DurationMs float64 `json:"durationMs"`
				ResetsAt   int64   `json:"resetsAt"`
			} `json:"window"`
			Amount struct {
				UsedFraction *float64 `json:"usedFraction"`
				Unit         string   `json:"unit"`
			} `json:"amount"`
		} `json:"limits"`
	} `json:"reports"`
}

// ParseOmpLive maps `omp usage --json`: per provider the newest report
// (omp reports each account it holds), each limit given as a percentage
// with a window becoming a Window (ompWindowID).
func ParseOmpLive(b []byte) ([]OmpMeasurement, error) {
	var v ompLive
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	if v.Reports == nil {
		return nil, errors.New("no reports")
	}
	var out []OmpMeasurement
	for _, r := range *v.Reports {
		h, ok := ompHarnesses[r.Provider]
		if !ok {
			continue
		}
		var ws []Window
		for _, l := range r.Limits {
			if l.Window == nil || l.Amount.UsedFraction == nil || l.Amount.Unit != "percent" {
				continue
			}
			id := ompWindowID(l.Scope.WindowID, l.Scope.Tier, l.Window.DurationMs)
			if id == "" || l.Window.ResetsAt <= 0 {
				continue
			}
			ws = append(ws, Window{ID: id, UsedPercent: ompPercent(*l.Amount.UsedFraction), ResetsAt: l.Window.ResetsAt})
		}
		if len(ws) == 0 {
			continue
		}
		m := OmpMeasurement{Harness: h, Measurement: Measurement{Source: wire.UsageSourceOmpUsage, Windows: ws, At: time.UnixMilli(r.FetchedAt)}}
		i := slices.IndexFunc(out, func(o OmpMeasurement) bool { return o.Harness == h })
		switch {
		case i < 0:
			out = append(out, m)
		case m.At.After(out[i].At):
			out[i] = m
		}
	}
	return sortOmp(out), nil
}

type ompHistory struct {
	Entries *[]struct {
		RecordedAt   int64    `json:"recordedAt"`
		Provider     string   `json:"provider"`
		LimitID      string   `json:"limitId"`
		UsedFraction *float64 `json:"usedFraction"`
		ResetsAt     int64    `json:"resetsAt"`
	} `json:"entries"`
}

// ParseOmpHistory maps `omp usage --history --json`: the newest snapshot
// of every limit. A snapshot has no window length, so only the windows
// named in its limit id ("anthropic:5h", "anthropic:7d:fable") count.
func ParseOmpHistory(b []byte) ([]OmpMeasurement, error) {
	var v ompHistory
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	if v.Entries == nil {
		return nil, errors.New("no entries")
	}
	type key struct{ h, id string }
	newest := map[key]int64{}
	var out []OmpMeasurement
	for _, e := range *v.Entries {
		h, ok := ompHarnesses[e.Provider]
		if !ok || e.UsedFraction == nil || e.ResetsAt <= 0 {
			continue
		}
		parts := strings.Split(e.LimitID, ":")
		if len(parts) < 2 || len(parts) > 3 || parts[0] != e.Provider {
			continue
		}
		tier := ""
		if len(parts) == 3 {
			tier = parts[2]
		}
		id := ompWindowID(parts[1], tier, 0)
		if id == "" {
			continue
		}
		if at, seen := newest[key{h, id}]; seen && at >= e.RecordedAt {
			continue
		}
		newest[key{h, id}] = e.RecordedAt
		i := slices.IndexFunc(out, func(o OmpMeasurement) bool { return o.Harness == h })
		if i < 0 {
			out = append(out, OmpMeasurement{Harness: h, Measurement: Measurement{Source: wire.UsageSourceOmpUsage}})
			i = len(out) - 1
		}
		m := &out[i]
		w := Window{ID: id, UsedPercent: ompPercent(*e.UsedFraction), ResetsAt: e.ResetsAt}
		if k := slices.IndexFunc(m.Windows, func(x Window) bool { return x.ID == id }); k >= 0 {
			m.Windows[k] = w
		} else {
			m.Windows = append(m.Windows, w)
		}
		if t := time.UnixMilli(e.RecordedAt); t.After(m.At) {
			m.At = t
		}
	}
	return sortOmp(out), nil
}

// ompWindowID is the window id of omp's window windowID ("5h", "7d", or a
// provider's own name) of durationMs (0: unknown): 5h is five_hour, 7d
// seven_day, 7d of a tier (a model) seven_day_<tier>, any other by its
// length (wire.UsageWindowOfLength); "" when it cannot be told.
func ompWindowID(windowID, tier string, durationMs float64) string {
	var id string
	switch {
	case windowID == "5h":
		id = wire.UsageWindowFiveHour
	case windowID == "7d":
		id = wire.UsageWindowSevenDay
	case durationMs > 0:
		id = wire.UsageWindowOfLength(durationMs / float64(time.Minute/time.Millisecond))
	default:
		return ""
	}
	if tier == "" {
		return id
	}
	if id != wire.UsageWindowSevenDay {
		return ""
	}
	id += "_" + strings.ToLower(tier)
	if _, ok := wire.UsageWindowModel(id); !ok {
		return ""
	}
	return id
}

// sortOmp orders ms by harness and their windows the way usage.get lists
// them.
func sortOmp(ms []OmpMeasurement) []OmpMeasurement {
	slices.SortStableFunc(ms, func(a, b OmpMeasurement) int { return cmp.Compare(a.Harness, b.Harness) })
	for i := range ms {
		slices.SortStableFunc(ms[i].Windows, func(a, b Window) int {
			return cmp.Compare(wire.UsageWindowRank(a.ID), wire.UsageWindowRank(b.ID))
		})
	}
	return ms
}

// ompPercent is a used fraction as a percentage, to two decimals (0.66 is
// 66, not 66.00000000000001).
func ompPercent(f float64) float64 { return math.Round(f*10000) / 100 }
