package usage

import (
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// When the private API is called (PROTOCOL.md "Private API").
const (
	// APIInterval: while the app is in front, at most once per 15 minutes.
	APIInterval = 15 * time.Minute
	// APIOpenedAfter: opening the usage screen calls it when the last call
	// is this old.
	APIOpenedAfter = 5 * time.Minute
	// apiRetryFirst is the wait after a first failure; it doubles with
	// each failure in a row, up to APIInterval.
	apiRetryFirst = 30 * time.Second
	// apiLoginHold is the wait after the token was refused (401), lacks
	// the scope (403) or is missing.
	apiLoginHold = time.Hour
)

// APISchedule decides when one account's private API is called next. The
// zero value calls at once. Not safe for concurrent use.
type APISchedule struct {
	// due: the next call while the app is in front; hold: no call before
	// it, whatever asks.
	due, hold time.Time
	fails     int
}

// Due reports whether to call at now: while the app is in front once the
// interval (or the wait after a failure) is over; when the usage screen
// was opened, once APIOpenedAfter has passed since the last success and
// no failure's wait lasts.
func (s *APISchedule) Due(now time.Time, foreground, opened bool) bool {
	return foreground && !now.Before(s.due) || opened && !now.Before(s.hold)
}

// Done takes the result of a call made at now and returns the earliest
// time of the next call. After a failure the wait is 30 s × 2^n (n the
// failures in a row before it) up to 15 minutes; a 429 waits at least
// until its Retry-After; a refused, unscoped or missing token waits an
// hour.
func (s *APISchedule) Done(now time.Time, e *APIError) time.Time {
	if e == nil {
		s.fails = 0
		s.due, s.hold = now.Add(APIInterval), now.Add(APIOpenedAfter)
		return s.hold
	}
	var next time.Time
	switch e.Code {
	case wire.UsageErrUnauthorized, wire.UsageErrForbidden, wire.UsageErrNoCredentials:
		s.fails = 0
		next = now.Add(apiLoginHold)
	default:
		wait := apiRetryFirst << min(s.fails, 5)
		next = now.Add(min(wait, APIInterval))
		s.fails++
		if e.Code == wire.UsageErrRateLimited && e.RetryAt.After(next) {
			next = e.RetryAt
		}
	}
	s.due, s.hold = next, next
	return next
}
