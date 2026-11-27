package hostscreen

import (
	"slices"
	"time"
)

// SettleAfter is how long the screen must rest, without input and without
// a frame from the target, before the frames go back to Normal, beginning
// with one fresh frame (#1974): the still screen is not left at the lower
// quality of the last frame sent while operating. A screen still after the
// input settles SettleAfter after it; one the input set moving (a
// scroll's momentum) SettleAfter after its last frame.
const SettleAfter = 300 * time.Millisecond

// SettleCap bounds the operating quality of a screen that keeps moving
// after the last input (a video a tap started): it settles this long after
// that input however it moves. iOS momentum lasts about 3.5 s.
const SettleCap = 5 * time.Second

// Quality is how a stream encodes the frames it captures.
type Quality struct {
	// JPEG is the JPEG quality, 1–100, on the scale of Normal's 70 (a
	// kind whose encoder has another scale maps it in proportion).
	JPEG int
	// Scale is the part of the kind's normal resolution, each side: 1 or
	// 0.5.
	Scale float64
}

// Normal is the quality of the frames while the user does not operate the
// target (#624).
var Normal = Quality{JPEG: 70, Scale: 1}

// QualitySetter is a Stream whose frames can be made smaller while the
// user operates its target (host_screen.input). A Stream without it keeps
// Normal.
type QualitySetter interface {
	// SetQuality has the frames captured from now on encoded at q. With
	// settle, the stream also sends one frame at q at once, captured
	// afresh, though the target did not change; while paused it only
	// keeps q (Resume sends a full frame anyway). Like Pause, it does not
	// block on the target.
	SetQuality(q Quality, settle bool)
}

// The quality while operating, by the link's bandwidth estimated from the
// acks (bytes a second on the wire): a fast link drops little; a slow one
// drops more and halves the resolution, which a still screen gets back
// after SettleAfter. slowLink is above #1974's example of a phone's link,
// 5 Mbit/s (625 KB/s): on such a link the operating frames are meant to go
// at half the resolution.
const (
	fastLink = 2_000_000 // bytes/s, at or above: fastQuality
	slowLink = 1_000_000 // bytes/s, below: slowQuality
	// Past the last operation's level, the estimate must be a quarter
	// above a boundary to go up, a fifth below it to go down: an estimate
	// wavering around a boundary keeps the level.
	marginUp   = 1.25
	marginDown = 0.8
)

var (
	fastQuality = Quality{JPEG: 50, Scale: 1}
	midQuality  = Quality{JPEG: 40, Scale: 1} // also before any ack
	slowQuality = Quality{JPEG: 30, Scale: 0.5}
	// levels from slow to fast; levels[i+1] from bounds[i] on.
	levels = [...]Quality{slowQuality, midQuality, fastQuality}
	bounds = [...]float64{slowLink, fastLink}
)

// operatingQuality is the quality of an operation on a link of rate
// bytes/s (0: not estimated yet), last the previous operation's (zero:
// none yet).
func operatingQuality(rate float64, last Quality) Quality {
	i := slices.Index(levels[:], last)
	switch {
	case rate == 0 && i < 0:
		return midQuality
	case rate == 0:
		return last
	case i < 0:
		i = 0
		for i < len(bounds) && rate >= bounds[i] {
			i++
		}
		return levels[i]
	}
	for i < len(bounds) && rate >= bounds[i]*marginUp {
		i++
	}
	for i > 0 && rate < bounds[i-1]*marginDown {
		i--
	}
	return levels[i]
}

// linkRate estimates the link's bandwidth from the frames' acks. On a link
// of round trip rtt and bandwidth bw, a frame of n bytes sent at s is acked
// at max(s + rtt, a') + n/bw, a' the ack of the frame before it (it waits
// behind that one at most). Its ack time a − s is thus at least
// rtt + n/bw: n over the ack time is bound by the round trip, not the
// bandwidth (a 30 kB frame on an 80 ms round trip never looks faster than
// 375 KB/s).
//
// The frames that waited behind none lie on the line rtt + n/bw, so two
// of different sizes give bw = Δn/Δt, the round trip cancelling: each
// frame is compared with the quickest of the last lowWindow (the line's
// lowest point seen) when it is at least lowSpread bigger. A frame that
// waited behind another lies above the line, so it gives less: the
// estimate errs low. Frames of one size tell nothing of the line; until
// sizes differ (operating, normal and settle frames, tiles), the estimate
// is a lower bound: n over the time from when the frame was sent, or from
// the previous ack when it was sent before that, to its ack.
//
// Samples' bytes and times are averaged apart and divided (see mean).
type linkRate struct {
	slope, bound mean
	low          [lowWindow]ackTime // the last frames' ack times, a ring
	nLow         int                // frames put in low
	lastAck      time.Time
	inFlight     map[int64]sentFrame // by seq
}

const (
	lowWindow = 16
	lowSpread = 4 // a frame a lowSpread-th bigger than the lowest point
)

// ackTime is a frame's size on the way and its ack time.
type ackTime struct {
	n int
	d time.Duration
}

// mean averages bytes and times apart, each moving a quarter of the way
// to a sample, and divides them: acks that arrive together, held up on
// the way, give the second 0 ms but leave the time the first waited in
// the average.
type mean struct{ bytes, secs float64 }

func (m *mean) add(bytes, secs float64) {
	if m.secs == 0 {
		m.bytes, m.secs = bytes, secs
		return
	}
	m.bytes += (bytes - m.bytes) / 4
	m.secs += (secs - m.secs) / 4
}

// rate is bytes/s; 0 before any sample.
func (m *mean) rate() float64 {
	if m.secs == 0 {
		return 0
	}
	return m.bytes / m.secs
}

// sentFrame is what the estimate needs of a frame in flight.
type sentFrame struct {
	at    time.Time
	bytes int
}

// sent notes frame seq, n bytes on the way to the app, sent at.
func (r *linkRate) sent(seq int64, n int, at time.Time) {
	if r.inFlight == nil {
		r.inFlight = make(map[int64]sentFrame, MaxInFlight)
	}
	r.inFlight[seq] = sentFrame{at: at, bytes: n}
}

// acked samples the link with frame seq, acked at; a frame forgotten
// changes nothing.
func (r *linkRate) acked(seq int64, at time.Time) {
	sf, ok := r.inFlight[seq]
	if !ok {
		return
	}
	delete(r.inFlight, seq)
	r.sample(sf.bytes, sf.at, at)
}

// forget drops the frames in flight: their acks, should they come, are
// not samples.
func (r *linkRate) forget() { clear(r.inFlight) }

// rate is the estimate in bytes/s; 0 before any sample.
func (r *linkRate) rate() float64 {
	if v := r.slope.rate(); v > 0 {
		return v
	}
	return r.bound.rate()
}

func (r *linkRate) sample(n int, sent, acked time.Time) {
	from := sent
	if r.lastAck.After(from) {
		from = r.lastAck
	}
	r.lastAck = acked
	r.bound.add(float64(n), max(acked.Sub(from), time.Millisecond).Seconds())

	d := acked.Sub(sent)
	if lo, ok := r.lowest(); ok && n >= lo.n+lo.n/lowSpread && d > lo.d {
		r.slope.add(float64(n-lo.n), (d - lo.d).Seconds())
	}
	r.low[r.nLow%lowWindow] = ackTime{n, d}
	r.nLow++
}

// lowest is the quickest ack of the last lowWindow frames.
func (r *linkRate) lowest() (ackTime, bool) {
	seen := r.low[:min(r.nLow, lowWindow)]
	if len(seen) == 0 {
		return ackTime{}, false
	}
	lo := seen[0]
	for _, a := range seen[1:] {
		if a.d < lo.d {
			lo = a
		}
	}
	return lo, true
}

// operation is the quality asked of the stream around inputs: lower from
// an input until the screen rests SettleAfter after it, at most SettleCap.
type operation struct {
	quality Quality
	// level is the operating quality picked last (zero: none yet), kept
	// from an operation's first input until its settle.
	level Quality
	// inputEnd is when the last input ended, lastFrame when the stream's
	// last frame came; stop stops the settle timer.
	inputEnd, lastFrame time.Time
	stop                func() bool
}

// settleAt is when the screen counts as resting: SettleAfter after the
// input or the last frame since, at most SettleCap after the input.
func (o *operation) settleAt() time.Time {
	rest := o.inputEnd
	if o.lastFrame.After(rest) {
		rest = o.lastFrame
	}
	due := rest.Add(SettleAfter)
	if capAt := o.inputEnd.Add(SettleCap); capAt.Before(due) {
		due = capAt
	}
	return due
}

// operate starts an input: no settle is due while it lasts. The first of
// an operation (the stream at Normal) picks the operating quality, which
// the stream drops to and keeps until the settle.
func (f *flow) operate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.stopSettleLocked()
	if f.op.quality != Normal {
		return
	}
	f.op.level = operatingQuality(f.link.rate(), f.op.level)
	f.setQualityLocked(f.op.level, false)
}

// operated ends an input: the settle is due once the screen rested
// SettleAfter (no frame came for that long), at most SettleCap later,
// unless another input comes first.
func (f *flow) operated() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || f.op.quality == Normal {
		return
	}
	f.stopSettleLocked()
	f.op.inputEnd = f.s.now()
	f.op.stop = f.s.afterFunc(SettleAfter, f.settle)
}

// settle asks Normal with a fresh frame, or waits on when a frame came
// since the input ended.
func (f *flow) settle() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.op.stop = nil
	if f.closed {
		return
	}
	if wait := f.op.settleAt().Sub(f.s.now()); wait > 0 {
		f.op.stop = f.s.afterFunc(wait, f.settle)
		return
	}
	f.setQualityLocked(Normal, true)
}

// setQualityLocked asks q of the stream when it takes qualities and q is
// new, or for a settle. It is called under f.mu, like Pause and Resume.
func (f *flow) setQualityLocked(q Quality, settle bool) {
	qs, ok := f.st.(QualitySetter)
	if !ok || q == f.op.quality && !settle {
		return
	}
	f.op.quality = q
	qs.SetQuality(q, settle)
}

func (f *flow) stopSettleLocked() {
	if f.op.stop != nil {
		f.op.stop()
		f.op.stop = nil
	}
}
