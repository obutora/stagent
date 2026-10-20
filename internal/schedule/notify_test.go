package schedule

import (
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

func TestValidateNotifyAndNormalizeDropsProblemSent(t *testing.T) {
	s := wire.Schedule{Name: "n", Kind: wire.ScheduleKindCommand, Command: "true", Cwd: "/tmp",
		When: wire.ScheduleWhen{Every: wire.EveryDaily, Time: "09:00"}, TZ: "Asia/Tokyo"}
	for _, v := range []string{"", wire.ScheduleNotifyProblems, wire.ScheduleNotifyAlways, wire.ScheduleNotifyOff} {
		s.Notify = v
		if err := Validate(s); err != nil {
			t.Errorf("notify %q: %v", v, err)
		}
	}
	s.Notify = "sometimes"
	if Validate(s) == nil {
		t.Error("notify sometimes passed")
	}
	s.ProblemSent = true
	if Normalize(s).ProblemSent {
		t.Error("Normalize kept problem_sent (the daemon's)")
	}
}
