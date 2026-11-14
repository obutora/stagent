package notify

import (
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

func usageAlert(level string) wire.NotificationData {
	return wire.NotificationData{Title: "Claude 5h", Body: "80% used · resets at 14:20", Level: level, Reason: "usage_limit",
		Harness: wire.HarnessClaude, Window: wire.UsageWindowFiveHour, ResetsAt: 1_763_010_000_000, ConfigDir: "/home/u/.claude"}
}

// A limit window's notifications replace one another per account, window
// and reset time (a valid ntfy sequence ID as is), settle with no session,
// and link to the host's usage screen with u=<harness>; a digest folding
// one links with an empty u=.
func TestUsageLimitReplaceKeyAndLinks(t *testing.T) {
	n := usageAlert(wire.LevelWarn)
	k := ReplaceKey(n)
	if len(k) > 64 || SequenceID(k) != k || IsSessionKey(k) {
		t.Fatalf("ReplaceKey = %q", k)
	}
	full := n
	full.Level = wire.LevelError
	if ReplaceKey(full) != k {
		t.Errorf("100%% key %q, 80%% key %q", ReplaceKey(full), k)
	}
	for _, other := range []func(*wire.NotificationData){
		func(o *wire.NotificationData) { o.ResetsAt += 5 * 3600_000 },
		func(o *wire.NotificationData) { o.Window = wire.UsageWindowSevenDay },
		func(o *wire.NotificationData) { o.ConfigDir = "/home/u/.claude-work" },
		func(o *wire.NotificationData) { o.Harness = wire.HarnessCodex },
	} {
		o := n
		other(&o)
		if ReplaceKey(o) == k {
			t.Errorf("%+v shares the key %q", o, k)
		}
	}
	if got := ClickURLOf("sshtermx://open", "hid", n); got != "sshtermx://open?h=hid&u=claude" {
		t.Errorf("ClickURLOf = %q", got)
	}
	if got := PageURLOf("https://x.example/open", "hid", n); got != "https://x.example/open#h=hid&u=claude" {
		t.Errorf("PageURLOf = %q", got)
	}
	d := Fold([]wire.NotificationData{n, full}, wire.LangEn)
	if d.Level != wire.LevelError || d.Title != "2 limit window notifications" || !d.Usage {
		t.Errorf("digest %+v", d)
	}
	if got := ClickURLOf("sshtermx://open", "hid", d); got != "sshtermx://open?h=hid&u=" {
		t.Errorf("digest ClickURLOf = %q", got)
	}
}

// notify.reasons.usage_limit decides (default on); error pushes urgent.
func TestUsageLimitWantsAndPriority(t *testing.T) {
	def := wire.Config{}.WithDefaults().Notify
	n := usageAlert(wire.LevelWarn)
	if !Wants(def, n) {
		t.Error("usage_limit not pushed by default")
	}
	off := def
	off.Reasons.UsageLimit = new(false)
	if Wants(off, n) {
		t.Error("usage_limit false still pushes")
	}
	for level, want := range map[string]string{wire.LevelError: "urgent", wire.LevelWarn: "high", wire.LevelInfo: "default"} {
		if got := ntfyPriority(usageAlert(level)); got != want {
			t.Errorf("priority of %s = %q, want %q", level, got, want)
		}
	}
}

// Titles name the service and the window (the weekly window of one model
// by the week and the model); bodies are fixed phrases with the reset time
// (with the date when a day or more away).
func TestUsageLimitTexts(t *testing.T) {
	for _, c := range []struct{ lang, h, w, want string }{
		{wire.LangJa, wire.HarnessClaude, wire.UsageWindowFiveHour, "Claude 5時間"},
		{wire.LangJa, wire.HarnessCodex, wire.UsageWindowSevenDay, "Codex 週"},
		{wire.LangJa, wire.HarnessClaude, "seven_day_opus", "Claude 週（Opus）"},
		{wire.LangEn, wire.HarnessClaude, "seven_day_sonnet", "Claude Week (Sonnet)"},
		{wire.LangKo, wire.HarnessClaude, "seven_day_opus", "Claude 주간(Opus)"},
		{wire.LangZh, wire.HarnessClaude, "seven_day_opus", "Claude 每周（Opus）"},
		{wire.LangEn, wire.HarnessCodex, "1440m", "Codex 24h"},
		{wire.LangEn, wire.HarnessCodex, "2880m", "Codex 2d"},
		{wire.LangEn, wire.HarnessCodex, "90m", "Codex 90m"},
	} {
		if got := UsageLimitTitle(c.lang, c.h, c.w); got != c.want {
			t.Errorf("UsageLimitTitle(%s, %s, %s) = %q, want %q", c.lang, c.h, c.w, got, c.want)
		}
	}
	reset := time.Date(2026, 11, 14, 14, 20, 0, 0, time.Local)
	for _, c := range []struct {
		lang      string
		threshold int
		now       time.Time
		want      string
	}{
		{wire.LangJa, 80, reset.Add(-2 * time.Hour), "80% を使いました · 14:20 にリセット"},
		{wire.LangJa, 100, reset.Add(-2 * time.Hour), "100% を使いました · 14:20 にリセット"},
		{wire.LangEn, 100, reset.Add(-2 * time.Hour), "100% used · resets at 14:20"},
		{wire.LangKo, 100, reset.Add(-2 * time.Hour), "100% 사용 · 14:20에 초기화"},
		{wire.LangZh, 100, reset.Add(-2 * time.Hour), "已使用 100% · 14:20 重置"},
		{wire.LangJa, 0, reset, "リセットされました"},
		{wire.LangEn, 80, reset.Add(-3 * 24 * time.Hour), "80% used · resets at 11/14 14:20"},
	} {
		if got := UsageLimitBody(c.lang, c.threshold, reset, c.now); got != c.want {
			t.Errorf("UsageLimitBody(%s, %d) = %q, want %q", c.lang, c.threshold, got, c.want)
		}
	}
}
