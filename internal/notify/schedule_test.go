package notify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

func TestScheduleReplaceKeyAndLinks(t *testing.T) {
	run := wire.NotificationData{Reason: "schedule", ScheduleID: "00112233aabbccdd", RunID: "ffeeddccbbaa9988"}
	sched := wire.NotificationData{Reason: "schedule", ScheduleID: "00112233aabbccdd"}
	session := wire.NotificationData{Reason: "exited", SessionID: "0123456789abcdef"}
	if k := ReplaceKey(run); k != "schedule_00112233aabbccdd" || !IsScheduleKey(k) || ReplaceKey(sched) != k {
		t.Errorf("ReplaceKey(run) = %q, (schedule) = %q", k, ReplaceKey(sched))
	}
	if k := ReplaceKey(session); k != session.SessionID || IsScheduleKey(k) {
		t.Errorf("ReplaceKey(session) = %q", k)
	}
	if SequenceID(ReplaceKey(run)) != ReplaceKey(run) {
		t.Errorf("SequenceID hashed %q", ReplaceKey(run))
	}
	for _, c := range []struct {
		n          wire.NotificationData
		click, pag string
	}{
		{run, "sshtermx://open?h=hid&r=ffeeddccbbaa9988&sc=00112233aabbccdd",
			"https://x.example/open#h=hid&r=ffeeddccbbaa9988&sc=00112233aabbccdd"},
		{sched, "sshtermx://open?h=hid&sc=00112233aabbccdd", "https://x.example/open#h=hid&sc=00112233aabbccdd"},
		{session, "sshtermx://open?h=hid&s=0123456789abcdef", "https://x.example/open#h=hid&s=0123456789abcdef"},
		{Fold([]wire.NotificationData{run, session}, "en"), "sshtermx://open?h=hid&sc=", "https://x.example/open#h=hid&sc="},
		{Fold([]wire.NotificationData{session, session}, "en"), "sshtermx://open?h=hid", "https://x.example/open#h=hid"},
	} {
		if got := ClickURLOf("sshtermx://open", "hid", c.n); got != c.click {
			t.Errorf("ClickURLOf(%+v) = %q, want %q", c.n, got, c.click)
		}
		if got := PageURLOf("https://x.example/open", "hid", c.n); got != c.pag {
			t.Errorf("PageURLOf(%+v) = %q, want %q", c.n, got, c.pag)
		}
	}
	if ClickURLOf("", "hid", run) != "" || PageURLOf("https://x.example/open", "", run) != "" {
		t.Error("a link without click_base or host id")
	}
}

func TestWantsSchedule(t *testing.T) {
	n := wire.NotificationData{Reason: "schedule", ScheduleID: "00112233aabbccdd"}
	if !Wants(wire.Config{}.WithDefaults().Notify, n) {
		t.Error("schedule is off by default")
	}
	cfg := wire.Config{Notify: wire.NotifyConfig{Reasons: wire.NotifyReasons{Schedule: new(false)}}}.WithDefaults().Notify
	if Wants(cfg, n) {
		t.Error("schedule: false still pushes")
	}
	// An older stagent's config (no schedule key) reads as on.
	var old wire.NotifyReasons
	if err := json.Unmarshal([]byte(`{"exited":"error"}`), &old); err != nil || old.Schedule != nil {
		t.Fatalf("%v %+v", err, old)
	}
}

// A schedule's ntfy push carries its sequence ID and a click link to the
// schedule and the run.
func TestPushNtfySchedule(t *testing.T) {
	var seq, click string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seq, click = r.Header.Get("X-Sequence-ID"), r.Header.Get("Click")
		json.NewEncoder(w).Encode(map[string]string{"sequence_id": seq})
	}))
	defer srv.Close()
	n := wire.NotificationData{Title: "nightly", Body: "失敗しました", Reason: "schedule",
		ScheduleID: "00112233aabbccdd", RunID: "ffeeddccbbaa9988"}
	replaced, err := pushNtfy(t.Context(), srv.Client(), wire.NtfyConfig{Enabled: true, Server: srv.URL, Topic: "t"}, n,
		ClickURLOf("sshtermx://open", "hid", n))
	if err != nil || !replaced || seq != "schedule_00112233aabbccdd" ||
		click != "sshtermx://open?h=hid&r=ffeeddccbbaa9988&sc=00112233aabbccdd" {
		t.Errorf("replaced %v err %v seq %q click %q", replaced, err, seq, click)
	}
}
