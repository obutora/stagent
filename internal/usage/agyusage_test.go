package usage

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

func rfc3339ms(t *testing.T, s string) int64 {
	t.Helper()
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return at.UnixMilli()
}

// Antigravity CLI 1.5.14's answer: its own models' 5h and week plain,
// the other groups scoped and labelled in agy's order; disabled buckets,
// other windows and empty scopes left out.
func TestAgyUsageReal(t *testing.T) {
	got, err := ParseAgyUsage(readFixture(t, "agy-usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Window{
		{ID: wire.UsageWindowFiveHour, UsedPercent: 14.27, ResetsAt: rfc3339ms(t, "2026-11-23T10:04:31Z")},
		{ID: wire.UsageWindowSevenDay, UsedPercent: 9.42, ResetsAt: rfc3339ms(t, "2026-11-29T01:09:02Z")},
		{ID: "five_hour_3p", Label: "Claude and GPT models", UsedPercent: 0, ResetsAt: rfc3339ms(t, "2026-11-23T10:28:10Z")},
		{ID: "seven_day_3p", Label: "Claude and GPT models", UsedPercent: 0, ResetsAt: rfc3339ms(t, "2026-11-30T05:28:10Z")},
		{ID: "seven_day_geminiargonwtu", Label: "Gemini 4 Argon", UsedPercent: 100, ResetsAt: rfc3339ms(t, "2026-11-30T05:28:10Z")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	for _, w := range got {
		if w.ID != wire.UsageWindowFiveHour && w.ID != wire.UsageWindowSevenDay && !wire.UsageWindowScoped(w.ID) {
			t.Errorf("window id %q is not a scoped window id", w.ID)
		}
	}
}

// A failed run is command_failed; output that is not agy's usage answer
// (not JSON, another status, no groups) is bad_response. Neither message
// carries agy's output.
func TestAgyUsageErrors(t *testing.T) {
	now := func() time.Time { return time.UnixMilli(1000) }
	for _, c := range []struct {
		name string
		out  string
		err  error
		code string
	}{
		{"exit status", "secret", errors.New("exit status 1"), wire.UsageErrCommand},
		{"not JSON", "Error: not logged in secret", nil, wire.UsageErrBadResponse},
		{"status", `{"status":"ERROR","command":{"name":"usage","data":{"groups":[]}}}`, nil, wire.UsageErrBadResponse},
		{"no command data", `{"status":"SUCCESS","response":"hi"}`, nil, wire.UsageErrBadResponse},
		{"bad reset time", `{"status":"SUCCESS","command":{"data":{"groups":[{"name":"G","buckets":[{"id":"gemini-5h","window":"5h","remaining_fraction":1,"reset_time":"soon"}]}]}}}`, nil, wire.UsageErrBadResponse},
	} {
		a := AgyUsage{Bin: "/fake/agy", Run: func(context.Context, string, ...string) ([]byte, error) { return []byte(c.out), c.err }}
		_, aerr := a.Measure(context.Background(), now)
		if aerr == nil || aerr.Code != c.code || strings.Contains(aerr.Message, "secret") {
			t.Errorf("%s: %+v", c.name, aerr)
		}
	}
}

// A success is measured with source antigravity_usage at the time the
// answer came, running `agy -p /usage --output-format json`.
func TestAgyUsageMeasure(t *testing.T) {
	var args []string
	a := AgyUsage{Bin: "/fake/agy", Run: func(_ context.Context, bin string, as ...string) ([]byte, error) {
		args = append([]string{bin}, as...)
		return readFixture(t, "agy-usage.json"), nil
	}}
	m, aerr := a.Measure(context.Background(), func() time.Time { return time.UnixMilli(5000) })
	if aerr != nil {
		t.Fatal(aerr)
	}
	if !reflect.DeepEqual(args, []string{"/fake/agy", "-p", "/usage", "--output-format", "json"}) {
		t.Errorf("ran %q", args)
	}
	if m.Source != wire.UsageSourceAntigravityUsage || !m.At.Equal(time.UnixMilli(5000)) || len(m.Windows) != 5 {
		t.Errorf("measurement %+v", m)
	}
}
