package schedule

import (
	"os"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// OffsetAt is the UTC offset in seconds (east positive) of loc at at.
func OffsetAt(loc *time.Location, at time.Time) int {
	_, offset := at.In(loc).Zone()
	return offset
}

// HostClockAt is the host's own clock, host (time.Local), at at when it is
// not loc's (their UTC offsets then differ); nil when it is.
func HostClockAt(loc, host *time.Location, at time.Time) *wire.HostClock {
	tzOffset := OffsetAt(loc, at)
	abbr, offset := at.In(host).Zone()
	if offset == tzOffset {
		return nil
	}
	return &wire.HostClock{TZ: hostZone(host, abbr), At: at.UnixMilli(), Offset: offset, TZOffset: tzOffset}
}

// hostZone names host: its IANA name (for time.Local, $TZ, else what
// /etc/localtime links to), else abbr (its abbreviation then).
func hostZone(host *time.Location, abbr string) string {
	if name := host.String(); name != "Local" {
		return name
	}
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); ianaName(tz) {
		return tz
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if _, name, ok := strings.Cut(target, "zoneinfo/"); ok && ianaName(name) {
			return name
		}
	}
	return abbr
}

func ianaName(name string) bool {
	if name == "" || name == "Local" || strings.HasPrefix(name, "/") {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}
