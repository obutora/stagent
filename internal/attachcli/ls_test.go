package attachcli

import (
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

func TestAgo(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{-3 * time.Second, "0s ago"}, // clock skew
		{0, "0s ago"},
		{59*time.Second + 999*time.Millisecond, "59s ago"},
		{time.Minute, "1m ago"},
		{59*time.Minute + 59*time.Second, "59m ago"},
		{time.Hour, "1h ago"},
		{23*time.Hour + 59*time.Minute, "23h ago"},
		{24 * time.Hour, "1d ago"},
		{100 * time.Hour, "4d ago"},
	}
	for _, tc := range cases {
		if got := ago(tc.d); got != tc.want {
			t.Errorf("ago(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestTable(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	ms := func(d time.Duration) int64 { return now.Add(-d).UnixMilli() }
	two := 2
	list := []wire.Session{
		{ID: "00000000000000a1", Mode: wire.ModeDetached, State: wire.StateIdle, LastActivityAt: ms(3 * time.Hour),
			Command: []string{"sh", "-c", "echo hi; cat"}, Cwd: "/home/u/proj"},
		{ID: "00000000000000a2", Mode: wire.ModePassthrough, State: wire.StateWorking, LastActivityAt: ms(5 * time.Second),
			Command: []string{"claude", strings.Repeat("x", 60)}, Cwd: "/home/u", Title: "fix\tthe \x1b[31mbug"},
		{ID: "00000000000000a3", Mode: wire.ModeDetached, State: wire.StateExited, LastActivityAt: ms(5 * time.Second),
			StartedAt: 1, Command: []string{"make"}, Cwd: "/srv", ExitCode: &two},
	}
	sortSessions(list)
	var b strings.Builder
	writeTable(&b, list, now, "/home/u")
	want := "" +
		"ID                MODE         STATE       LAST ACTIVITY  COMMAND                                   TITLE/CWD\n" +
		"00000000000000a3  detached     exited (2)  5s ago         make                                      /srv\n" +
		"00000000000000a2  passthrough  working     5s ago         claude xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx…  fix?the ?[31mbug\n" +
		"00000000000000a1  detached     idle        3h ago         sh -c echo hi; cat                        ~/proj\n"
	if got := b.String(); got != want {
		t.Fatalf("table:\n%s\nwant:\n%s", got, want)
	}
}
