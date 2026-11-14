package usage

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// omp 14's `omp usage --json --redact` on a Max account whose five-hour
// window Anthropic does not send: the week and the Fable week.
func TestOmpLiveReal(t *testing.T) {
	got, err := ParseOmpLive(readFixture(t, "omp-usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := []OmpMeasurement{{Harness: wire.HarnessClaude, Measurement: Measurement{
		Source: wire.UsageSourceOmpUsage, At: time.UnixMilli(1794630417861),
		Windows: []Window{
			{ID: wire.UsageWindowSevenDay, UsedPercent: 66, ResetsAt: 1794801600000},
			{ID: "seven_day_fable", UsedPercent: 28, ResetsAt: 1794801600157},
		},
	}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

// Five hours, a tier in capitals, a Codex window by its length, and what
// is no share of a window (spend in dollars, an unknown provider) left out.
func TestOmpLiveMapping(t *testing.T) {
	got, err := ParseOmpLive(readFixture(t, "omp-usage-more.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := []OmpMeasurement{
		{Harness: wire.HarnessClaude, Measurement: Measurement{Source: wire.UsageSourceOmpUsage, At: time.UnixMilli(1794630400000),
			Windows: []Window{
				{ID: wire.UsageWindowFiveHour, UsedPercent: 42.5, ResetsAt: 1794640000000},
				{ID: "seven_day_opus", UsedPercent: 10, ResetsAt: 1794801600000},
			}}},
		{Harness: wire.HarnessCodex, Measurement: Measurement{Source: wire.UsageSourceOmpUsage, At: time.UnixMilli(1794630410000),
			Windows: []Window{
				{ID: wire.UsageWindowFiveHour, UsedPercent: 7, ResetsAt: 1794641000000},
				{ID: "1440m", UsedPercent: 50, ResetsAt: 1794700000000},
			}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

// Two accounts of one provider: the newer report counts.
func TestOmpLiveNewestReportPerProvider(t *testing.T) {
	b := []byte(`{"reports":[
	 {"provider":"anthropic","fetchedAt":2000,"limits":[{"scope":{"windowId":"7d"},"window":{"durationMs":604800000,"resetsAt":9000},"amount":{"usedFraction":0.2,"unit":"percent"}}]},
	 {"provider":"anthropic","fetchedAt":1000,"limits":[{"scope":{"windowId":"7d"},"window":{"durationMs":604800000,"resetsAt":9000},"amount":{"usedFraction":0.9,"unit":"percent"}}]}]}`)
	got, err := ParseOmpLive(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Windows[0].UsedPercent != 20 {
		t.Fatalf("got %+v", got)
	}
}

func TestOmpBadJSON(t *testing.T) {
	for _, b := range []string{"", "not json", "[]", `{"reports":5}`} {
		if _, err := ParseOmpLive([]byte(b)); err == nil {
			t.Errorf("ParseOmpLive(%q): no error", b)
		}
		if _, err := ParseOmpHistory([]byte(b)); err == nil {
			t.Errorf("ParseOmpHistory(%q): no error", b)
		}
	}
}

// `omp usage --history --json --redact`: the newest snapshot of each limit.
func TestOmpHistoryReal(t *testing.T) {
	got, err := ParseOmpHistory(readFixture(t, "omp-usage-history.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := []OmpMeasurement{{Harness: wire.HarnessClaude, Measurement: Measurement{
		Source: wire.UsageSourceOmpUsage, At: time.UnixMilli(1794630416288),
		Windows: []Window{
			{ID: wire.UsageWindowSevenDay, UsedPercent: 67, ResetsAt: 1794801600157},
			{ID: "seven_day_fable", UsedPercent: 28, ResetsAt: 1794801600157},
		},
	}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestOmpHistoryMapping(t *testing.T) {
	b := []byte(`{"entries":[
	 {"recordedAt":3000,"provider":"openai-codex","limitId":"openai-codex:5h","usedFraction":0.4,"resetsAt":9000},
	 {"recordedAt":1000,"provider":"openai-codex","limitId":"openai-codex:5h","usedFraction":0.9,"resetsAt":9000},
	 {"recordedAt":2000,"provider":"openai-codex","limitId":"openai-codex:7d","usedFraction":0.1,"resetsAt":19000},
	 {"recordedAt":2000,"provider":"openai-codex","limitId":"openai-codex:daily","usedFraction":0.1,"resetsAt":19000},
	 {"recordedAt":2000,"provider":"zai","limitId":"zai:5h","usedFraction":0.1,"resetsAt":19000},
	 {"recordedAt":2000,"provider":"anthropic","limitId":"anthropic:5h","resetsAt":19000}]}`)
	got, err := ParseOmpHistory(b)
	if err != nil {
		t.Fatal(err)
	}
	want := []OmpMeasurement{{Harness: wire.HarnessCodex, Measurement: Measurement{
		Source: wire.UsageSourceOmpUsage, At: time.UnixMilli(3000),
		Windows: []Window{
			{ID: wire.UsageWindowFiveHour, UsedPercent: 40, ResetsAt: 9000},
			{ID: wire.UsageWindowSevenDay, UsedPercent: 10, ResetsAt: 19000},
		},
	}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestOmpUsageCommand(t *testing.T) {
	var gotArgs []string
	o := OmpUsage{Bin: "/x/omp", Run: func(_ context.Context, bin string, args ...string) ([]byte, error) {
		gotArgs = append([]string{bin}, args...)
		return readFixture(t, "omp-usage.json"), nil
	}}
	ms, aerr := o.Live(context.Background())
	if aerr != nil || len(ms) != 1 {
		t.Fatalf("Live: %v %v", ms, aerr)
	}
	if strings.Join(gotArgs, " ") != "/x/omp usage --json" {
		t.Fatalf("args %q", gotArgs)
	}
	if _, aerr := o.History(context.Background()); aerr == nil || strings.Join(gotArgs, " ") != "/x/omp usage --history --json --days 1" {
		t.Fatalf("History args %q err %v (the live fixture has no entries)", gotArgs, aerr)
	}

	o.Run = func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("exit status 1: secret token xyz")
	}
	_, aerr = o.Live(context.Background())
	if aerr == nil || aerr.Code != wire.UsageErrCommand || strings.Contains(aerr.Message, "xyz") {
		t.Fatalf("failed run: %+v", aerr)
	}
	o.Run = func(context.Context, string, ...string) ([]byte, error) { return []byte("oops"), nil }
	if _, aerr = o.Live(context.Background()); aerr == nil || aerr.Code != wire.UsageErrBadResponse {
		t.Fatalf("bad output: %+v", aerr)
	}
}
