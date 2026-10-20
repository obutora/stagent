package bridge

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

// schedules is announced on Linux and macOS only, where the daemon runs
// schedules.
func TestSchedulesAnnouncedOnLinuxAndMacOSOnly(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		_, a := startBridge(t, func(b *Bridge) { b.goos = goos })
		want := goos != "windows"
		if got := slices.Contains(helloCaps(t, a), wire.CapSchedules); got != want {
			t.Errorf("hello on %s announces %s: %v, want %v", goos, wire.CapSchedules, got, want)
		}
	}
}

// schedule.* go to the daemon as they are.
func TestScheduleMethodsForwardedToDaemon(t *testing.T) {
	l, a := startBridge(t)
	got := make(chan *wire.Msg, 4)
	serveFake(t, l.DaemonAddr, func(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
		got <- m
		return json.RawMessage(`{"schedules":[]}`), nil
	})
	for _, method := range []string{wire.MethodScheduleList, wire.MethodScheduleCreate, wire.MethodScheduleRuns, wire.MethodSchedulePreview} {
		if e := a.call(t, method, map[string]any{"id": "x"}, nil); e != nil {
			t.Fatalf("%s: %v", method, e)
		}
		if m := <-got; m.Method != method || string(m.Params) != `{"id":"x"}` {
			t.Errorf("daemon got %s %s, want %s", m.Method, m.Params, method)
		}
	}
}
