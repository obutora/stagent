package hostscreen

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

type rgb struct{ r, g, b byte }

var (
	grey  = rgb{128, 128, 128}
	red   = rgb{220, 30, 30}
	blue  = rgb{30, 30, 220}
	green = rgb{30, 200, 30}
)

// solid is a w×h picture of one colour, at quality q.
func solid(w, h int, c rgb, q int) *Raster {
	pix := make([]byte, w*h*3)
	for i := 0; i < len(pix); i += 3 {
		pix[i], pix[i+1], pix[i+2] = c.r, c.g, c.b
	}
	return &Raster{Pix: pix, W: w, H: h, JPEG: q}
}

// paint is r with the rectangle (x, y, w, h) in c.
func paint(r *Raster, x, y, w, h int, c rgb) *Raster {
	n := &Raster{Pix: bytes.Clone(r.Pix), W: r.W, H: r.H, JPEG: r.JPEG}
	for yy := y; yy < y+h; yy++ {
		for xx := x; xx < x+w; xx++ {
			i := (yy*r.W + xx) * 3
			n.Pix[i], n.Pix[i+1], n.Pix[i+2] = c.r, c.g, c.b
		}
	}
	return n
}

// picture is a picture captured FrameEvery after the previous frame.
func (s *fakeStream) picture(r *Raster) {
	if s.clk != nil {
		s.clk.advance(FrameEvery)
	}
	s.mu.Lock()
	ev := s.ev
	s.mu.Unlock()
	ev.Frame(Frame{Raster: r, Width: 800, Height: 600})
}

// records returns the records written since the last take, each as
// "full:<seq> <w>x<h>" or "tiles:<seq>/<prev> [x,y wxh ...]", the
// payload's JPEGs checked against the header.
func (k *fakeSink) records(t *testing.T) []string {
	t.Helper()
	k.mu.Lock()
	defer k.mu.Unlock()
	var recs []string
	for k.buf.Len() > 0 {
		h, p, err := ReadRecord(&k.buf)
		if err != nil {
			t.Fatal(err)
		}
		if h.Width != 800 || h.Height != 600 {
			t.Fatalf("record header %+v: not the frame's size", h)
		}
		switch h.Kind {
		case wire.HostScreenRecordFull:
			cfg, err := jpeg.DecodeConfig(bytes.NewReader(p))
			if err != nil {
				t.Fatalf("full record %d: %v", h.Seq, err)
			}
			// A picture tiles may follow tells its size.
			if h.ImageWidth != cfg.Width || h.ImageHeight != cfg.Height {
				t.Fatalf("full record %+v of a %dx%d JPEG", h, cfg.Width, cfg.Height)
			}
			recs = append(recs, fmt.Sprintf("full:%d %dx%d", h.Seq, cfg.Width, cfg.Height))
		case wire.HostScreenRecordTiles:
			var tiles []string
			off := 0
			for _, tl := range h.Tiles {
				if off+tl.Len > len(p) {
					t.Fatalf("tiles record %d: tile %+v beyond the payload of %d", h.Seq, tl, len(p))
				}
				cfg, err := jpeg.DecodeConfig(bytes.NewReader(p[off : off+tl.Len]))
				if err != nil || cfg.Width != tl.W || cfg.Height != tl.H {
					t.Fatalf("tiles record %d: tile %+v is a %dx%d JPEG (%v)", h.Seq, tl, cfg.Width, cfg.Height, err)
				}
				off += tl.Len
				tiles = append(tiles, fmt.Sprintf("%d,%d %dx%d", tl.X, tl.Y, tl.W, tl.H))
			}
			if off != len(p) {
				t.Fatalf("tiles record %d: %d payload bytes, tiles take %d", h.Seq, len(p), off)
			}
			recs = append(recs, fmt.Sprintf("tiles:%d/%d %dx%d [%s]", h.Seq, h.Prev, h.ImageWidth, h.ImageHeight, strings.Join(tiles, " ")))
		default:
			t.Fatalf("record kind %q", h.Kind)
		}
	}
	return recs
}

// lastTile decodes the last tile of the last tiles record written.
func tileImage(t *testing.T, payload []byte, tl wire.HostScreenTile, off int) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(payload[off : off+tl.Len]))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// newTilesServer has a stream started for an app that takes tiles, over
// an attached channel.
func newTilesServer(t *testing.T) (*Server, *fakeStream, *fakeSink, *sent, *fakeClock) {
	t.Helper()
	s, k, out, clk := newTestServer(t)
	s.SetTiles(true)
	sink := &fakeSink{}
	s.Attach(sink)
	start(t, s, out)
	out.take()
	return s, k.streams[0], sink, out, clk
}

func wantRecords(t *testing.T, sink *fakeSink, want ...string) {
	t.Helper()
	if got := sink.records(t); !slices.Equal(got, want) {
		t.Fatalf("records %q, want %q", got, want)
	}
}

// A picture is compared with the one sent before it in cells of TileSize
// px: only the cells changed go, as JPEGs of their own, side by side ones
// in one rectangle; the rest of the picture stays as the app has it.
func TestOnlyTheTilesChangedSinceTheFrameSentAreSent(t *testing.T) {
	s, st, sink, _, _ := newTilesServer(t)
	// 4×2 cells, the right column and the bottom row cut short.
	base := solid(250, 120, grey, 70)
	st.picture(base)
	wantRecords(t, sink, "full:1 250x120")
	s.Ack(1)

	one := paint(base, 70, 10, 20, 20, red) // within cell (1, 0)
	st.picture(one)
	wantRecords(t, sink, "tiles:2/1 250x120 [64,0 64x64]")
	s.Ack(2)

	// Compared with what was sent last (one), not with base: cell (1, 0)
	// is unchanged now. Cells (2, 1) and (3, 1) side by side are one
	// rectangle, cut at the picture's edges.
	two := paint(one, 130, 70, 120, 50, blue)
	st.picture(two)
	wantRecords(t, sink, "tiles:3/2 250x120 [128,64 122x56]")
	s.Ack(3)

	// Cells one above the other with the same span are one rectangle;
	// others stay apart.
	three := paint(two, 0, 0, 10, 120, green)
	three = paint(three, 200, 0, 10, 10, green)
	st.picture(three)
	wantRecords(t, sink, "tiles:4/3 250x120 [0,0 64x120 192,0 58x64]")
}

// The tile's JPEG is the new picture's pixels there.
func TestATileCarriesTheNewPixels(t *testing.T) {
	s, st, sink, _, _ := newTilesServer(t)
	base := solid(128, 128, grey, 70)
	st.picture(base)
	sink.records(t)
	s.Ack(1)
	st.picture(paint(base, 64, 64, 64, 64, red))
	sink.mu.Lock()
	h, p, err := ReadRecord(&sink.buf)
	sink.mu.Unlock()
	if err != nil || len(h.Tiles) != 1 {
		t.Fatalf("record %+v, %v", h, err)
	}
	r, g, b, _ := tileImage(t, p, h.Tiles[0], 0).At(32, 32).RGBA()
	if r>>8 < 180 || g>>8 > 70 || b>>8 > 70 {
		t.Fatalf("tile pixel (%d, %d, %d), want about red", r>>8, g>>8, b>>8)
	}
}

// Tiles go while they cost less than the whole picture: past 90% of the
// cells changed, or past 16 rectangles (each a JPEG with its headers), the
// whole picture goes.
func TestALargeChangeGoesWhole(t *testing.T) {
	s, st, sink, _, _ := newTilesServer(t)
	base := solid(640, 64, grey, 70) // 10 cells in a row
	st.picture(base)
	sink.records(t)
	s.Ack(1)
	nine := paint(base, 0, 0, 576, 64, red) // 9 of 10
	st.picture(nine)
	wantRecords(t, sink, "tiles:2/1 640x64 [0,0 576x64]")
	s.Ack(2)
	st.picture(paint(nine, 0, 0, 640, 64, blue)) // 10 of 10
	wantRecords(t, sink, "full:3 640x64")
}

func TestCellsChangedHereAndThereGoWhole(t *testing.T) {
	s, st, sink, _, _ := newTilesServer(t)
	base := solid(33*64, 64, grey, 70) // 33 cells in a row
	st.picture(base)
	sink.records(t)
	s.Ack(1)
	every := func(r *Raster, n int, c rgb) *Raster {
		for i := range n {
			r = paint(r, 2*i*64, 0, 1, 1, c)
		}
		return r
	}
	sixteen := every(base, 16, red)
	st.picture(sixteen)
	recs := sink.records(t)
	if len(recs) != 1 || !strings.HasPrefix(recs[0], "tiles:2/1") || strings.Count(recs[0], "64x64") != 16 {
		t.Fatalf("16 cells apart: %q, want a tiles record of 16", recs)
	}
	s.Ack(2)
	st.picture(every(sixteen, 17, blue))
	wantRecords(t, sink, "full:3 2112x64")
}

// Tiles follow a full frame for less than FullEvery: the frame after
// that goes whole, however little changed. A still screen sends nothing
// meanwhile.
func TestAFullFrameGoesEveryTenSeconds(t *testing.T) {
	s, st, sink, _, clk := newTilesServer(t)
	pic := solid(128, 128, grey, 70)
	st.picture(pic) // at 100 ms
	wantRecords(t, sink, "full:1 128x128")
	s.Ack(1)
	clk.advance(FullEvery - 2*FrameEvery)
	pic = paint(pic, 0, 0, 1, 1, red)
	st.picture(pic) // at FullEvery: 9.9 s after the full frame
	wantRecords(t, sink, "tiles:2/1 128x128 [0,0 64x64]")
	s.Ack(2)
	pic = paint(pic, 0, 0, 1, 1, blue)
	st.picture(pic) // 10 s after
	wantRecords(t, sink, "full:3 128x128")
	s.Ack(3)
	pic = paint(pic, 0, 0, 1, 1, green)
	st.picture(pic)
	wantRecords(t, sink, "tiles:4/3 128x128 [0,0 64x64]")
}

// The frame after any of these goes whole: the app's picture may not be
// the frame sent last, or tiles could not build on it.
func TestAFullFrameAfter(t *testing.T) {
	small := func(r *Raster) *Raster { return paint(r, 0, 0, 1, 1, rgb{r.Pix[0] + 1, 0, 0}) }
	for _, c := range []struct {
		name string
		// do happens after frame 1 was sent and acked; it returns the
		// picture to send next and the records to want (the frame's seq
		// is 2 unless do sent some).
		do   func(t *testing.T, s *Server, st *fakeStream, sink **fakeSink, clk *fakeClock, base *Raster) *Raster
		want []string
	}{
		{"a new size (the display turned, a lower resolution)", func(_ *testing.T, _ *Server, _ *fakeStream, _ **fakeSink, _ *fakeClock, base *Raster) *Raster {
			return solid(base.H, base.W, grey, 70)
		}, []string{"full:2 128x192"}},
		{"a better quality (the settle after operating)", func(t *testing.T, s *Server, st *fakeStream, sink **fakeSink, _ *fakeClock, base *Raster) *Raster {
			low := small(base)
			low.JPEG = 40
			st.picture(low)
			wantRecords(t, *sink, "tiles:2/1 192x128 [0,0 64x64]")
			s.Ack(2)
			// The same pixels again at 70: the app has (0,0) at 40.
			again := *low
			again.JPEG = 70
			return &again
		}, []string{"full:3 192x128"}},
		{"host_screen.resume", func(_ *testing.T, s *Server, _ *fakeStream, _ **fakeSink, _ *fakeClock, base *Raster) *Raster {
			s.Pause()
			s.Resume()
			return small(base)
		}, []string{"full:2 192x128"}},
		{"a stall", func(t *testing.T, s *Server, st *fakeStream, sink **fakeSink, clk *fakeClock, base *Raster) *Raster {
			b := small(base)
			st.picture(b)
			c := small(b)
			st.picture(c)
			wantRecords(t, *sink, "tiles:2/1 192x128 [0,0 64x64]", "tiles:3/2 192x128 [0,0 64x64]")
			if !clk.fire() {
				t.Fatal("no stall")
			}
			s.Ack(2)
			return small(c)
		}, []string{"full:4 192x128"}},
		{"a new channel", func(_ *testing.T, s *Server, _ *fakeStream, sink **fakeSink, _ *fakeClock, base *Raster) *Raster {
			*sink = &fakeSink{}
			s.Attach(*sink)
			return small(base)
		}, []string{"full:2 192x128"}},
		{"a frame sent as host_screen.frame", func(t *testing.T, s *Server, st *fakeStream, sink **fakeSink, _ *fakeClock, base *Raster) *Raster {
			(*sink).mu.Lock()
			(*sink).err = errors.New("broken")
			(*sink).mu.Unlock()
			b := small(base)
			st.picture(b) // seq 2, as JSON
			s.Ack(2)
			*sink = &fakeSink{}
			s.Attach(*sink)
			return small(b)
		}, []string{"full:3 192x128"}},
		{"the target back after gone", func(_ *testing.T, _ *Server, st *fakeStream, _ **fakeSink, _ *fakeClock, base *Raster) *Raster {
			st.ev.State(wire.HostScreenState{State: wire.HostScreenGone})
			st.ev.State(wire.HostScreenState{State: wire.HostScreenLive})
			return small(base)
		}, []string{"full:2 192x128"}},
		{"host_screen.keyframe (the picture sent again)", func(t *testing.T, s *Server, _ *fakeStream, _ **fakeSink, clk *fakeClock, _ *Raster) *Raster {
			if err := s.Keyframe(); err != nil {
				t.Fatal(err)
			}
			clk.advance(FrameEvery)
			return nil
		}, []string{"full:2 192x128"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, st, sink, _, clk := newTilesServer(t)
			base := solid(192, 128, grey, 70)
			st.picture(base)
			wantRecords(t, sink, "full:1 192x128")
			s.Ack(1)
			next := c.do(t, s, st, &sink, clk, base)
			if next != nil {
				st.picture(next)
			}
			wantRecords(t, sink, c.want...)
		})
	}
}

// host_screen.keyframe with no frame held sends the picture sent last
// again, whole, once a place is free; one held goes whole instead.
func TestAKeyframeResendsThePictureOrSendsTheHeldOneWhole(t *testing.T) {
	s, st, sink, _, clk := newTilesServer(t)
	base := solid(128, 128, grey, 70)
	st.picture(base)
	b := paint(base, 0, 0, 1, 1, red)
	st.picture(b)
	c := paint(b, 0, 0, 1, 1, blue)
	st.picture(c) // held: 1 and 2 in flight
	wantRecords(t, sink, "full:1 128x128", "tiles:2/1 128x128 [0,0 64x64]")
	s.Keyframe()
	wantRecords(t, sink)
	s.Ack(1)
	wantRecords(t, sink, "full:3 128x128")
	s.Ack(2)
	s.Ack(3)
	clk.advance(FrameEvery)
	s.Keyframe()
	wantRecords(t, sink, "full:4 128x128")
}

// Tiles go only to an app that takes them, over the binary channel.
func TestTilesOnlyToAnAppThatTakesThemOverTheChannel(t *testing.T) {
	s, k, out, _ := newTestServer(t)
	sink := &fakeSink{}
	detach := s.Attach(sink)
	start(t, s, out)
	st := k.streams[0]
	base := solid(128, 128, grey, 70)
	st.picture(base)
	s.Ack(1)
	st.picture(paint(base, 0, 0, 1, 1, red))
	wantRecords(t, sink, "full:1 128x128", "full:2 128x128")

	s.SetTiles(true)
	detach()
	out.take()
	s.Ack(2)
	st.picture(paint(base, 0, 0, 1, 1, blue))
	got := out.take()
	if len(got) != 1 || !strings.HasPrefix(got[0], "frame:3:\xff\xd8") {
		t.Fatalf("without a channel: %q, want frame 3 as host_screen.frame, a JPEG", got)
	}
}

// held collects the senders the flow starts instead of running them, so a
// test sees what happens while a frame is being encoded.
type held struct{ jobs []func() }

func (h *held) spawn(f func()) { h.jobs = append(h.jobs, f) }

// run runs the senders started so far, and those they start.
func (h *held) run() {
	for len(h.jobs) > 0 {
		f := h.jobs[0]
		h.jobs = h.jobs[1:]
		f()
	}
}

// A picture is encoded by a sender of its own, not by whoever let it go
// (the stream's Frame, an ack, a keyframe): acks, keyframes and pauses are
// answered while it waits, and the frames keep their seqs, places in
// flight and tiles' prev.
func TestPicturesAreEncodedOffTheCaller(t *testing.T) {
	s, st, sink, _, _ := newTilesServer(t)
	h := &held{}
	s.spawn = h.spawn
	base := solid(128, 128, grey, 70)
	st.picture(base)
	if len(h.jobs) != 1 {
		t.Fatalf("%d senders started, want the frame handed to one", len(h.jobs))
	}
	wantRecords(t, sink)
	h.run()
	wantRecords(t, sink, "full:1 128x128")

	b := paint(base, 0, 0, 1, 1, red)
	st.picture(b) // let go with 1 in flight
	s.Ack(1)
	s.Keyframe()
	s.Pause()
	// Paused before it went: dropped like a held frame.
	h.run()
	wantRecords(t, sink)

	// Resumed: the next frame goes whole on 1, then tiles on it.
	s.Resume()
	c := paint(b, 0, 0, 1, 1, blue)
	st.picture(c)
	h.run()
	st.picture(paint(c, 0, 0, 1, 1, green))
	h.run()
	wantRecords(t, sink, "full:2 128x128", "tiles:3/2 128x128 [0,0 64x64]")
}

// With senders on goroutines of their own, pictures, acks, keyframes and
// pauses may come at any time (go test -race): every record is whole, in
// seq order, and tiles build on the record before them.
func TestSendersRaceNothing(t *testing.T) {
	s, st, sink, _, _ := newTilesServer(t)
	s.spawn = func(f func()) { go f() }
	s.now = time.Now
	s.afterFunc = func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			for seq := range s.seq.Load() + 1 {
				s.Ack(seq)
			}
			switch i % 50 {
			case 10:
				s.Keyframe()
			case 20:
				s.Pause()
			case 21:
				s.Resume()
			}
			time.Sleep(time.Millisecond)
		}
	}()
	pic := solid(256, 256, grey, 70)
	for i := range 60 {
		pic = paint(pic, (i%4)*64, 0, 1, 1, rgb{byte(i), 0, 0})
		st.mu.Lock()
		ev := st.ev
		st.mu.Unlock()
		ev.Frame(Frame{Raster: pic, Width: 800, Height: 600})
		time.Sleep(5 * time.Millisecond)
	}
	close(stop)
	<-done
	time.Sleep(50 * time.Millisecond)
	s.Stop()
	var last int64
	for _, rec := range sink.records(t) {
		var seq, prev int64
		if _, err := fmt.Sscanf(rec, "full:%d", &seq); err != nil {
			if _, err := fmt.Sscanf(rec, "tiles:%d/%d", &seq, &prev); err != nil || prev != last {
				t.Fatalf("record %q after %d", rec, last)
			}
		}
		if seq <= last {
			t.Fatalf("record %q after %d", rec, last)
		}
		last = seq
	}
	if last == 0 {
		t.Fatal("no record")
	}
}

// A full frame asked for while tiles are encoded (a keyframe, a new
// channel) is that frame: the tiles do not go, the picture goes whole.
func TestAFullFrameAskedWhileTilesAreEncodedIsThatPicture(t *testing.T) {
	for name, ask := range map[string]func(s *Server, sink **fakeSink){
		"keyframe": func(s *Server, _ **fakeSink) { s.Keyframe() },
		"new channel": func(s *Server, sink **fakeSink) {
			*sink = &fakeSink{}
			s.Attach(*sink)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, st, sink, _, _ := newTilesServer(t)
			h := &held{}
			s.spawn = h.spawn
			base := solid(128, 128, grey, 70)
			st.picture(base)
			h.run()
			wantRecords(t, sink, "full:1 128x128")
			s.Ack(1)
			st.picture(paint(base, 0, 0, 1, 1, red))
			ask(s, &sink)
			h.run()
			wantRecords(t, sink, "full:2 128x128")
		})
	}
}
