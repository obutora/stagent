package hostscreen

import (
	"context"
	"encoding/base64"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// qualityStream is an inputStream that takes qualities. Its Input lasts
// the input's duration_ms on the server's clock (a drag replayed).
type qualityStream struct {
	*inputStream
	began time.Time
	asked []askedQuality
}

type askedQuality struct {
	q      Quality
	settle bool
	at     time.Duration // since the stream started, on the server's clock
}

func (s *qualityStream) SetQuality(q Quality, settle bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, askedQuality{q, settle, s.clk.Now().Sub(s.began)})
}

func (s *qualityStream) Input(ctx context.Context, in wire.HostScreenInput) (wire.HostScreenInputResult, error) {
	s.clk.advance(time.Duration(in.DurationMS) * time.Millisecond)
	return s.inputStream.Input(ctx, in)
}

// take returns the qualities asked since the last take.
func (s *qualityStream) take() []askedQuality {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.asked
	s.asked = nil
	return a
}

// qualityKind opens qualityStreams on clk.
type qualityKind struct {
	fakeKind
	streams []*qualityStream
}

func (k *qualityKind) Open(context.Context, wire.HostScreenTarget) (Stream, error) {
	s := &qualityStream{inputStream: &inputStream{fakeStream: &fakeStream{clk: k.clk}}, began: k.clk.Now()}
	k.streams = append(k.streams, s)
	return s, nil
}

func newQualityServer(t *testing.T) (*Server, *qualityStream, *sent, *fakeClock) {
	t.Helper()
	clk := newFakeClock()
	k := &qualityKind{fakeKind: fakeKind{clk: clk}}
	out := &sent{}
	s := NewServer(out.notify, k)
	s.now, s.afterFunc, s.spawn = clk.Now, clk.after, inline
	start(t, s, out)
	return s, k.streams[0], out, clk
}

var (
	tap  = wire.HostScreenInput{Type: wire.InputTap, X: 0.5, Y: 0.5}
	drag = wire.HostScreenInput{Type: wire.InputDrag, Points: []wire.HostScreenPoint{{X: 0.5, Y: 0.8}, {X: 0.5, Y: 0.2}}, DurationMS: 1000}
)

func input(t *testing.T, s *Server, in wire.HostScreenInput) {
	t.Helper()
	if _, err := s.Input(t.Context(), in); err != nil {
		t.Fatal(err)
	}
}

func TestInputLowersTheQualityUntil300msAfterTheLastInput(t *testing.T) {
	s, st, _, clk := newQualityServer(t)
	input(t, s, tap)
	got := st.take()
	if len(got) != 1 || got[0].settle || got[0].q.JPEG >= Normal.JPEG {
		t.Fatalf("asked %+v on an input, want one lower quality without settle", got)
	}
	low := got[0].q
	clk.advance(SettleAfter - time.Millisecond)
	if got := st.take(); len(got) != 0 {
		t.Fatalf("asked %+v before 300 ms without input", got)
	}
	// Another input keeps the quality and starts the 300 ms over.
	input(t, s, tap)
	clk.advance(SettleAfter - time.Millisecond)
	if got := st.take(); len(got) != 0 {
		t.Fatalf("asked %+v within 300 ms of the second input", got)
	}
	clk.advance(time.Millisecond)
	want := []askedQuality{{Normal, true, 2*SettleAfter - time.Millisecond}}
	if got := st.take(); !slices.Equal(got, want) {
		t.Fatalf("asked %+v, want %+v: normal with one fresh frame 300 ms after the last input", got, want)
	}
	// The next input lowers it again.
	input(t, s, tap)
	if got := st.take(); len(got) != 1 || got[0].q != low || got[0].settle {
		t.Fatalf("asked %+v on the next input, want %+v", got, low)
	}
}

// A drag arrives once the finger lifted and lasts its duration_ms on the
// target: the 300 ms count from its end.
func TestSettleCountsFromTheEndOfALongInput(t *testing.T) {
	s, st, _, clk := newQualityServer(t)
	input(t, s, drag)
	if got := st.take(); len(got) != 1 || got[0].at != 0 || got[0].settle {
		t.Fatalf("asked %+v, want the lower quality as the drag began", got)
	}
	clk.advance(SettleAfter - time.Millisecond)
	if got := st.take(); len(got) != 0 {
		t.Fatalf("asked %+v within 300 ms of the drag's end", got)
	}
	clk.advance(time.Millisecond)
	if got := st.take(); len(got) != 1 || got[0].q != Normal || !got[0].settle || got[0].at != time.Second+SettleAfter {
		t.Fatalf("asked %+v, want normal with settle 1.3 s after the drag began", got)
	}
}

// The screen may keep moving after the input (a scroll's momentum): the
// frames it sends keep the quality low, and the settle comes 300 ms after
// the last of them.
func TestFramesAfterTheInputPostponeTheSettle(t *testing.T) {
	s, st, out, clk := newQualityServer(t)
	input(t, s, tap)
	st.take()
	for range 10 { // a frame every 100 ms for 1 s
		st.frame("glide")
		ackAll(t, s, out)
	}
	if got := st.take(); len(got) != 0 {
		t.Fatalf("asked %+v while the screen still moved", got)
	}
	clk.advance(SettleAfter - time.Millisecond)
	if got := st.take(); len(got) != 0 {
		t.Fatalf("asked %+v within 300 ms of the last frame", got)
	}
	clk.advance(time.Millisecond)
	want := []askedQuality{{Normal, true, time.Second + SettleAfter}}
	if got := st.take(); !slices.Equal(got, want) {
		t.Fatalf("asked %+v, want %+v: the settle 300 ms after the last frame", got, want)
	}
	// Frames once settled are the normal ones; nothing more is asked.
	st.frame("still moving")
	clk.advance(time.Second)
	if got := st.take(); len(got) != 0 {
		t.Fatalf("asked %+v after the settle", got)
	}
}

// A screen that never stops after an input (a video it started) is at the
// normal quality SettleCap after the input, though its frames go on.
func TestSettleCapEndsTheLowQualityOfAScreenThatKeepsMoving(t *testing.T) {
	s, st, out, _ := newQualityServer(t)
	input(t, s, tap)
	st.take()
	var got []askedQuality
	for range 2 * SettleCap / FrameEvery {
		st.frame("video")
		ackAll(t, s, out)
		got = append(got, st.take()...)
	}
	want := []askedQuality{{Normal, true, SettleCap}}
	if !slices.Equal(got, want) {
		t.Fatalf("asked %+v, want %+v: the settle SettleCap after the input", got, want)
	}
}

// ackAll acks every frame sent since the last take of out.
func ackAll(t *testing.T, s *Server, out *sent) {
	t.Helper()
	for _, m := range out.take() {
		if p := strings.Split(m, ":"); p[0] == "frame" {
			if err := s.Ack(atoi(t, p[1])); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// The quality while operating follows the link: frames acked fast drop
// little, frames acked slowly drop more (and halve the resolution).
func TestOperatingQualityFollowsTheAckedRate(t *testing.T) {
	const size = 100_000 // bytes a frame
	for _, tc := range []struct {
		name  string
		ackIn time.Duration // 0: no frame acked yet
		want  Quality
	}{
		{"unknown", 0, Quality{JPEG: 40, Scale: 1}},
		{"10 MB/s", 10 * time.Millisecond, Quality{JPEG: 50, Scale: 1}},
		{"1 MB/s", 100 * time.Millisecond, Quality{JPEG: 40, Scale: 1}},
		{"200 KB/s", 500 * time.Millisecond, Quality{JPEG: 30, Scale: 0.5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, st, out, clk := newQualityServer(t)
			out.take()
			if tc.ackIn > 0 {
				for range 4 {
					clk.advance(FrameEvery)
					st.frameNow(strings.Repeat("x", size))
					clk.advance(tc.ackIn)
					for _, m := range out.take() {
						seq := strings.Split(m, ":")[1]
						if err := s.Ack(atoi(t, seq)); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			input(t, s, tap)
			if got := st.take(); len(got) != 1 || got[0].q != tc.want {
				t.Fatalf("asked %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Two frames sent back to back share the link: the second's time counts
// from the first's ack, so the rate is the link's, not half of it.
func TestFramesInFlightTogetherAreTimedFromThePreviousAck(t *testing.T) {
	s, st, out, clk := newQualityServer(t)
	out.take()
	// Each frame takes 200 ms on a 2.5 MB/s link; the second, sent 100 ms
	// after the first, waits behind it for 100 ms.
	for range 4 {
		clk.advance(FrameEvery)
		st.frameNow(strings.Repeat("x", 500_000))
		clk.advance(FrameEvery)
		st.frameNow(strings.Repeat("y", 500_000))
		clk.advance(100 * time.Millisecond)
		s.Ack(s.seq.Load() - 1)
		clk.advance(200 * time.Millisecond)
		s.Ack(s.seq.Load())
		out.take()
	}
	input(t, s, tap)
	if got := st.take(); len(got) != 1 || got[0].q != (Quality{JPEG: 50, Scale: 1}) {
		t.Fatalf("asked %+v, want the fast link's quality", got)
	}
}

// Acks may reach stagent together, held up on the way (a full pipe, a
// network buffering): the time the first waited counts for both, so the
// rate is not taken for the fast link a lone 0 ms ack would suggest.
func TestAcksArrivingTogetherDoNotLookFast(t *testing.T) {
	s, st, out, clk := newQualityServer(t)
	out.take()
	for range 4 {
		clk.advance(FrameEvery)
		st.frameNow(strings.Repeat("x", 100_000))
		clk.advance(FrameEvery)
		st.frameNow(strings.Repeat("y", 100_000))
		// 200 KB in 900 ms since the second was sent, both acked at once.
		clk.advance(900 * time.Millisecond)
		s.Ack(s.seq.Load() - 1)
		s.Ack(s.seq.Load())
		out.take()
	}
	input(t, s, tap)
	if got := st.take(); len(got) != 1 || got[0].q != (Quality{JPEG: 30, Scale: 0.5}) {
		t.Fatalf("asked %+v, want the slow link's quality", got)
	}
}

// playLink plays a link of round trip rtt and bw bytes/s between the
// server and the app for d: the stream captures a frame every every, of
// the wire sizes given in turn, and each frame the server sends is acked
// at max(sent + rtt, the previous frame's ack) + its bytes' time (it waits
// behind the frame before it on the link).
func playLink(t *testing.T, s *Server, st *qualityStream, out *sent, clk *fakeClock, rtt time.Duration, bw float64, every, d time.Duration, sizes ...int) {
	t.Helper()
	type ack struct {
		seq int64
		at  time.Time
	}
	var acks []ack
	var lastAck time.Time
	nextFrame := clk.Now()
	end := clk.Now().Add(d)
	for i := 0; clk.Now().Before(end); {
		if now := clk.Now(); !now.Before(nextFrame) {
			// base64 of 3k bytes is 4k: the wire size is sizes[i].
			st.frameNow(strings.Repeat("x", sizes[i%len(sizes)]*3/4))
			i++
			nextFrame = nextFrame.Add(every)
		}
		for _, m := range out.take() {
			p := strings.SplitN(m, ":", 3)
			if p[0] != "frame" {
				continue
			}
			n := base64.StdEncoding.EncodedLen(len(p[2]))
			at := clk.Now().Add(rtt)
			if lastAck.After(at) {
				at = lastAck
			}
			lastAck = at.Add(time.Duration(float64(n) / bw * float64(time.Second)))
			acks = append(acks, ack{atoi(t, p[1]), lastAck})
		}
		for len(acks) > 0 && !acks[0].at.After(clk.Now()) {
			if err := s.Ack(acks[0].seq); err != nil {
				t.Fatal(err)
			}
			acks = acks[1:]
		}
		clk.advance(time.Millisecond)
	}
}

// The operating quality follows the link's bandwidth, not how long a frame
// takes to be acked: on a link with a long round trip the ack time is
// mostly the round trip, whatever the bandwidth. Frames of different
// sizes (operating, normal, tiles) tell the two apart.
func TestOperatingQualityFollowsTheBandwidthNotTheRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rtt   time.Duration
		bw    float64 // bytes/s
		every time.Duration
		sizes []int
		want  Quality
	}{
		{"wide link, long round trip, one frame at a time", 300 * time.Millisecond, 5_000_000, time.Second, []int{30_000, 100_000}, fastQuality},
		{"wide link, long round trip, frames in flight together", 150 * time.Millisecond, 4_000_000, FrameEvery, []int{30_000, 100_000}, fastQuality},
		// #1974's phone link: operating frames go at half the resolution.
		{"5 Mbit/s, 80 ms (#1974's example, the bandwidth bench)", 80 * time.Millisecond, 625_000, 400 * time.Millisecond, []int{37_000, 115_000}, slowQuality},
		{"12 Mbit/s, 80 ms", 80 * time.Millisecond, 1_500_000, 400 * time.Millisecond, []int{37_000, 115_000}, midQuality},
		{"narrow link, short round trip", 50 * time.Millisecond, 200_000, FrameEvery, []int{30_000, 100_000}, slowQuality},
		{"narrow link, long round trip", 300 * time.Millisecond, 300_000, FrameEvery, []int{10_000, 40_000}, slowQuality},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, st, out, clk := newQualityServer(t)
			out.take()
			playLink(t, s, st, out, clk, tc.rtt, tc.bw, tc.every, 8*time.Second, tc.sizes...)
			input(t, s, tap)
			if got := st.take(); len(got) != 1 || got[0].q != tc.want {
				t.Fatalf("asked %+v, want %+v", got, tc.want)
			}
		})
	}
}

// linkAt has the stream send n frames of 100 kB on the wire, one at a time,
// each acked after its time on a link of rate bytes/s.
func linkAt(t *testing.T, s *Server, st *qualityStream, out *sent, clk *fakeClock, rate float64, n int) {
	t.Helper()
	for range n {
		clk.advance(FrameEvery)
		st.frameNow(strings.Repeat("x", 75_000))
		clk.advance(time.Duration(100_000 / rate * float64(time.Second)))
		ackAll(t, s, out)
	}
}

// operateOnce taps, has the screen rest until the settle and returns the
// operating quality asked.
func operateOnce(t *testing.T, s *Server, st *qualityStream, clk *fakeClock) Quality {
	t.Helper()
	st.take()
	input(t, s, tap)
	got := st.take()
	clk.advance(SettleAfter)
	if len(got) != 1 || got[0].settle {
		t.Fatalf("asked %+v on a tap, want one operating quality", got)
	}
	if settled := st.take(); len(settled) != 1 || settled[0].q != Normal {
		t.Fatalf("asked %+v after the tap, want the settle", settled)
	}
	return got[0].q
}

// The operating quality is picked as an operation starts and kept until
// the screen rests, whatever the estimate does meanwhile (each change
// would cost the app a full frame at a new size); the next operation picks
// again.
func TestTheOperatingQualityHoldsUntilTheSettle(t *testing.T) {
	s, st, out, clk := newQualityServer(t)
	out.take()
	linkAt(t, s, st, out, clk, 1_500_000, 30)
	input(t, s, tap)
	if got := st.take(); len(got) != 1 || got[0].q != midQuality {
		t.Fatalf("asked %+v, want %+v", got, midQuality)
	}
	// The link slows down while the user goes on operating.
	for range 30 {
		input(t, s, tap)
		linkAt(t, s, st, out, clk, 600_000, 1)
	}
	if got := st.take(); len(got) != 0 {
		t.Fatalf("asked %+v while operating", got)
	}
	clk.advance(time.Second)
	if got := st.take(); len(got) != 1 || got[0].q != Normal {
		t.Fatalf("asked %+v, want the settle", got)
	}
	if q := operateOnce(t, s, st, clk); q != slowQuality {
		t.Fatalf("the next operation got %+v, want %+v", q, slowQuality)
	}
}

// Between operations the quality moves to another level only once the
// estimate is clearly past the boundary: a quarter above it to go up, a
// fifth below it to go down; an estimate wavering around a boundary keeps
// the level.
func TestTheOperatingQualityMovesPastABoundaryWithAMargin(t *testing.T) {
	s, st, out, clk := newQualityServer(t)
	out.take()
	for _, step := range []struct {
		rate float64
		want Quality
	}{
		{1_500_000, midQuality},
		{900_000, midQuality}, // under 1 MB/s, above 0.8 MB/s
		{700_000, slowQuality},
		{1_100_000, slowQuality}, // above 1 MB/s, under 1.25 MB/s
		{1_300_000, midQuality},
		{2_200_000, midQuality}, // under 2.5 MB/s
		{2_600_000, fastQuality},
		{1_700_000, fastQuality}, // above 1.6 MB/s
		{1_500_000, midQuality},
	} {
		linkAt(t, s, st, out, clk, step.rate, 40)
		if q := operateOnce(t, s, st, clk); q != step.want {
			t.Fatalf("at %v B/s: %+v, want %+v", step.rate, q, step.want)
		}
	}
}

// A stream that takes no quality is simply never asked; inputs and frames
// flow as before.
func TestInputToAStreamWithoutQualityChangesNothingElse(t *testing.T) {
	s, k, out, clk := newTestServer(t)
	k.input = true
	start(t, s, out)
	k.inputs.frame("a")
	input(t, s, tap)
	clk.advance(time.Second)
	k.inputs.frame("b")
	if got, want := out.take(), []string{"reply", "frame:1:a", "frame:2:b"}; !slices.Equal(got, want) {
		t.Fatalf("sent %q, want %q", got, want)
	}
}

// Stop ends the wait: no quality is asked of a closed stream.
func TestStopCancelsTheSettle(t *testing.T) {
	s, st, _, clk := newQualityServer(t)
	input(t, s, tap)
	st.take()
	s.Stop()
	clk.advance(time.Second)
	if got := st.take(); len(got) != 0 {
		t.Fatalf("asked %+v after stop", got)
	}
}

func atoi(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A reviving target passes the quality to its stream, and gives the
// quality asked last to a stream it reopens.
func TestRevivingPassesTheQualityOnAndToTheNextStream(t *testing.T) {
	clk := newFakeClock()
	newStream := func() *qualityStream {
		return &qualityStream{inputStream: &inputStream{fakeStream: &fakeStream{clk: clk}}}
	}
	first, second := newStream(), newStream()
	var mu sync.Mutex
	var next Stream
	r := Reviving(first, time.Millisecond, func(context.Context) Stream {
		mu.Lock()
		defer mu.Unlock()
		s := next
		next = nil
		return s
	})
	defer r.Close()
	r.Start(newRecorder())
	qs, ok := r.(QualitySetter)
	if !ok {
		t.Fatal("a reviving stream takes no quality")
	}
	low := Quality{JPEG: 30, Scale: 0.5}
	qs.SetQuality(low, false)
	if got := first.take(); len(got) != 1 || got[0].q != low {
		t.Fatalf("first stream asked %+v, want %+v", got, low)
	}
	first.events().State(wire.HostScreenState{State: wire.HostScreenGone})
	mu.Lock()
	next = second
	mu.Unlock()
	eventually(t, "the second stream started", func() bool { return second.events() != nil })
	if got := second.take(); len(got) != 1 || got[0].q != low || got[0].settle {
		t.Fatalf("second stream asked %+v, want %+v before it started", got, low)
	}
}
