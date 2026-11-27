package main

import (
	"image"
	"image/color"
	"image/draw"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// screen is a w×h gray picture with a dark w/8 square at (x, y) when x ≥ 0.
func screen(w, h, x, y int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.Gray{200}), image.Point{}, draw.Src)
	if x >= 0 {
		s := w / 8
		draw.Draw(img, image.Rect(x, y, x+s, y+s), image.NewUniform(color.Gray{20}), image.Point{}, draw.Src)
	}
	return img
}

// frame records frame seq arriving at t0+at showing img.
func frame(r *Recorder, seq int64, at time.Duration, img image.Image) {
	r.Frame(seq, t0.Add(at), wire.HostScreenRecordFull, 1000, img.Bounds().Dx(), img.Bounds().Dy())
	r.Analyzed(seq, analysis{fp: fingerprintOf(img), decode: ms(5)})
}

func TestSummaryCountsWhatArrivedInTheWindowPerSecond(t *testing.T) {
	r := NewRecorder()
	r.Down(t0.Add(-ms(100)), 5000) // before the window
	r.Down(t0.Add(ms(500)), 40_000)
	r.Down(t0.Add(ms(1500)), 60_000)
	r.Up(t0.Add(ms(700)), 300)
	r.Down(t0.Add(ms(2500)), 7000) // after it
	still := screen(640, 400, -1, 0)
	frame(r, 1, -ms(50), still)
	frame(r, 2, ms(400), still)
	frame(r, 3, ms(1200), still)
	frame(r, 4, ms(1900), still)
	frame(r, 5, ms(2100), still)

	s := r.Summary(t0, t0.Add(2*time.Second))
	if s.DownBytesPerSec != 50_000 || s.UpBytesPerSec != 150 {
		t.Errorf("down %v B/s up %v B/s, want 50000 and 150", s.DownBytesPerSec, s.UpBytesPerSec)
	}
	if s.FramesPerSec != 1.5 || s.PayloadBytesPerSec != 1500 {
		t.Errorf("%v frames/s %v payload B/s, want 1.5 and 1500", s.FramesPerSec, s.PayloadBytesPerSec)
	}
}

func TestInputLatencyRunsToTheFirstFrameThatLooksDifferent(t *testing.T) {
	r := NewRecorder()
	frame(r, 1, 0, screen(640, 400, 10, 10))
	r.Input(t0.Add(ms(1000)))
	frame(r, 2, ms(1100), screen(640, 400, 10, 10)) // same picture again
	frame(r, 3, ms(1250), screen(640, 400, 300, 200))
	frame(r, 4, ms(1300), screen(640, 400, 10, 10))

	s := r.Summary(t0, t0.Add(2*time.Second))
	if s.Latency.Measured != 1 || s.Latency.P50Ms != 250 {
		t.Fatalf("latency = %+v, want 1 measured at 250 ms", s.Latency)
	}
}

func TestInputWhileTheScreenStillMovesIsNotMeasured(t *testing.T) {
	r := NewRecorder()
	frame(r, 1, 0, screen(640, 400, 10, 10))
	frame(r, 2, ms(800), screen(640, 400, 100, 10)) // still settling
	r.Input(t0.Add(ms(1000)))
	frame(r, 3, ms(1100), screen(640, 400, 200, 10))

	s := r.Summary(t0, t0.Add(2*time.Second))
	if s.Latency.Measured != 0 || s.Latency.Unsettled != 1 {
		t.Fatalf("latency = %+v, want 1 unsettled and none measured", s.Latency)
	}
}

func TestInputTheScreenNeverShowsIsCountedAsUnchanged(t *testing.T) {
	r := NewRecorder()
	frame(r, 1, 0, screen(640, 400, 10, 10))
	r.Input(t0.Add(ms(1000)))
	frame(r, 2, ms(1500), screen(640, 400, 10, 10))

	s := r.Summary(t0, t0.Add(2*time.Second))
	if s.Latency.Measured != 0 || s.Latency.NoChange != 1 {
		t.Fatalf("latency = %+v, want 1 without change", s.Latency)
	}
}

func TestPicturesDiffer(t *testing.T) {
	speck := screen(1280, 800, -1, 0).(*image.RGBA)
	speck.Set(500, 500, color.Black) // one pixel: JPEG-noise sized
	for _, c := range []struct {
		name string
		a, b image.Image
		want bool
	}{
		{"same picture", screen(1280, 800, 40, 40), screen(1280, 800, 40, 40), false},
		{"one pixel", screen(1280, 800, -1, 0), speck, false},
		{"moved square", screen(1280, 800, 40, 40), screen(1280, 800, 600, 300), true},
		{"other size", screen(1280, 800, -1, 0), screen(800, 1280, -1, 0), true},
	} {
		if got := fingerprintOf(c.a).differs(fingerprintOf(c.b)); got != c.want {
			t.Errorf("%s: differs = %v, want %v", c.name, got, c.want)
		}
	}
}
