package main

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"math/rand/v2"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

func encode(t *testing.T, img image.Image, q int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: q}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func crop(img image.Image, r image.Rectangle) image.Image {
	out := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	draw.Draw(out, out.Bounds(), img, r.Min, draw.Src)
	return out
}

func fullFrame(t *testing.T, seq int64, img image.Image, q int) Frame {
	b := img.Bounds()
	return Frame{Kind: wire.HostScreenRecordFull, Seq: seq, JPEG: encode(t, img, q), Width: b.Dx(), Height: b.Dy(),
		ImageWidth: b.Dx(), ImageHeight: b.Dy()}
}

// tilesFrame is the tiles record that turns frame prev into img: rects of
// img, each its own JPEG.
func tilesFrame(t *testing.T, seq, prev int64, img image.Image, rects ...image.Rectangle) Frame {
	b := img.Bounds()
	f := Frame{Kind: wire.HostScreenRecordTiles, Seq: seq, Prev: prev, Width: b.Dx(), Height: b.Dy(),
		ImageWidth: b.Dx(), ImageHeight: b.Dy()}
	for _, r := range rects {
		j := encode(t, crop(img, r), 70)
		f.JPEG = append(f.JPEG, j...)
		f.Tiles = append(f.Tiles, wire.HostScreenTile{X: r.Min.X, Y: r.Min.Y, W: r.Dx(), H: r.Dy(), Len: len(j)})
	}
	return f
}

func TestTilesAreDrawnOnThePictureBefore(t *testing.T) {
	r := NewRecorder()
	a := NewAnalyzer(r)
	before := screen(640, 384, 8, 8)
	after := screen(640, 384, 8, 8).(*image.RGBA)
	draw.Draw(after, image.Rect(320, 192, 384, 256), image.NewUniform(color.Gray{20}), image.Point{}, draw.Src)

	a.Add(t0, fullFrame(t, 1, before, 70))
	r.Input(t0.Add(ms(1000)))
	nothing := tilesFrame(t, 2, 1, before) // no tile changed
	a.Add(t0.Add(ms(1100)), nothing)
	tile := tilesFrame(t, 3, 2, after, image.Rect(320, 192, 384, 256))
	a.Add(t0.Add(ms(1300)), tile)
	a.Close()

	s := r.Summary(t0, t0.Add(ms(2000)))
	if s.Latency.Measured != 1 || s.Latency.P50Ms != 300 {
		t.Fatalf("latency = %+v, want measured at the tile's frame (300 ms)", s.Latency)
	}
	if s.FullFrames != 1 || s.TileFrames != 2 {
		t.Fatalf("%d full and %d tiles frames, want 1 and 2", s.FullFrames, s.TileFrames)
	}
	want := float64(len(fullFrame(t, 1, before, 70).JPEG)+len(tile.JPEG)) / 3
	if s.MeanFrameBytes != want {
		t.Fatalf("mean frame bytes %v, want %v (tiles count their JPEGs)", s.MeanFrameBytes, want)
	}
	if s.ImageWidth != 640 || s.ImageHeight != 384 {
		t.Fatalf("image %d×%d, want the composed picture's 640×384", s.ImageWidth, s.ImageHeight)
	}
}

// page is a phone-sized picture with small dark marks on white, like text.
func page(w, h int, seed uint64) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	rnd := rand.New(rand.NewPCG(seed, 1))
	for y := 40; y+20 < h; y += 36 {
		for x := 20; x+12 < w-20; x += 14 {
			if rnd.IntN(5) > 0 {
				draw.Draw(img, image.Rect(x, y, x+10, y+20), image.NewUniform(color.Gray{30}), image.Point{}, draw.Src)
			}
		}
	}
	return img
}

// half is img at half its size, each px the mean of the px it covers
// (stagent's 2×2 average; an odd side is scaled whole into its half, as
// Chrome's maxHeight does).
func half(img *image.RGBA) *image.RGBA {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	ow, oh := w/2, h/2
	xs, ys := cuts(w, ow), cuts(h, oh)
	sum := make([][3]float64, ow*oh)
	for y := range h {
		for x := range w {
			c := img.RGBAAt(x, y)
			v := [3]float64{float64(c.R), float64(c.G), float64(c.B)}
			for _, p := range []struct {
				i int
				f float64
			}{
				{ys[y].i*ow + xs[x].i, ys[y].f * xs[x].f},
				{ys[y].i*ow + xs[x].i + 1, ys[y].f * (1 - xs[x].f)},
				{(ys[y].i+1)*ow + xs[x].i, (1 - ys[y].f) * xs[x].f},
				{(ys[y].i+1)*ow + xs[x].i + 1, (1 - ys[y].f) * (1 - xs[x].f)},
			} {
				if p.f > 0 {
					for k := range 3 {
						sum[p.i][k] += v[k] * p.f
					}
				}
			}
		}
	}
	out := image.NewRGBA(image.Rect(0, 0, ow, oh))
	area := float64(w) / float64(ow) * float64(h) / float64(oh)
	for i, s := range sum {
		out.SetRGBA(i%ow, i/ow, color.RGBA{uint8(s[0]/area + 0.5), uint8(s[1]/area + 0.5), uint8(s[2]/area + 0.5), 255})
	}
	return out
}

func fingerprintOfJPEG(t *testing.T, img image.Image, q int) fingerprint {
	m, err := jpeg.Decode(bytes.NewReader(encode(t, img, q)))
	if err != nil {
		t.Fatal(err)
	}
	return fingerprintOf(m)
}

func TestTheSamePictureAtHalfSizeAndLowQualityDoesNotLookDifferent(t *testing.T) {
	for _, size := range [][2]int{{540, 1212}, {1280, 713}, {604, 1312}} {
		w, h := size[0], size[1]
		normal := page(w, h, 1)
		moved := page(w, h, 2)
		if fingerprintOfJPEG(t, normal, 70).differs(fingerprintOfJPEG(t, half(normal), 30)) {
			t.Errorf("%d×%d: q70 and half q30 of one picture differ", w, h)
		}
		if !fingerprintOfJPEG(t, normal, 70).differs(fingerprintOfJPEG(t, half(moved), 30)) {
			t.Errorf("%d×%d: another picture at half size does not differ", w, h)
		}
		if fingerprintOfJPEG(t, normal, 70).differs(fingerprintOf(normal)) != false {
			t.Errorf("%d×%d: a picture and its JPEG differ", w, h)
		}
	}
	if !fingerprintOf(screen(800, 1280, -1, 0)).differs(fingerprintOf(screen(1280, 800, -1, 0))) {
		t.Error("a turned picture does not differ")
	}
}

// typed is a white w×h picture with n letter-sized marks (16×24 px, thin
// strokes, as 32 px text) in a row.
func typed(w, h, n int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	ink := image.NewUniform(color.Gray{40})
	for i := range n {
		x, y := 41+i*19, 101
		draw.Draw(img, image.Rect(x, y, x+3, y+24), ink, image.Point{}, draw.Src)
		draw.Draw(img, image.Rect(x+13, y, x+16, y+24), ink, image.Point{}, draw.Src)
		draw.Draw(img, image.Rect(x, y+10, x+16, y+13), ink, image.Point{}, draw.Src)
	}
	return img
}

func TestALetterTypedShowsAtHalfSizeAndLowQuality(t *testing.T) {
	for _, size := range [][2]int{{540, 1212}, {1280, 713}, {604, 1312}} {
		w, h := size[0], size[1]
		before := fingerprintOfJPEG(t, typed(w, h, 5), 70)
		if before.differs(fingerprintOfJPEG(t, half(typed(w, h, 5)), 30)) {
			t.Errorf("%d×%d: the same letters at half size differ", w, h)
		}
		if !before.differs(fingerprintOfJPEG(t, half(typed(w, h, 6)), 30)) {
			t.Errorf("%d×%d: a letter more at half size does not differ", w, h)
		}
	}
}

func TestJPEGQualityIsReadFromItsTables(t *testing.T) {
	img := page(320, 240, 3)
	for _, q := range []int{30, 40, 50, 70, 90} {
		if got := jpegQuality(encode(t, img, q)); got != q {
			t.Errorf("quality of a q%d JPEG = %d", q, got)
		}
	}
	if got := jpegQuality([]byte("not a jpeg")); got != 0 {
		t.Errorf("quality of garbage = %d, want 0", got)
	}
}

func TestTheFrameLeftAfterInputsStopIsSummedUp(t *testing.T) {
	r := NewRecorder()
	a := NewAnalyzer(r)
	normal := page(540, 1212, 1)
	scrolled := page(540, 1212, 2)
	a.Add(t0, fullFrame(t, 1, normal, 70))
	for i := range 5 { // a burst of inputs 80 ms apart, 0–320 ms
		r.Sent(t0.Add(ms(80 * i)))
	}
	lowRes := fullFrame(t, 2, half(scrolled), 30)
	lowRes.Width, lowRes.Height = 540, 1212
	a.Add(t0.Add(ms(200)), lowRes)
	a.Add(t0.Add(ms(700)), fullFrame(t, 3, scrolled, 70)) // the settle frame
	r.Sent(t0.Add(ms(5000)))                              // the next burst
	a.Add(t0.Add(ms(5300)), fullFrame(t, 4, normal, 40))
	a.Close()

	s := r.Summary(t0, t0.Add(ms(6000)))
	st := s.Settle
	if st.Count != 2 {
		t.Fatalf("settle = %+v, want 2 bursts", st)
	}
	if st.Full != 2 || st.QualityMin != 40 || st.QualityMax != 70 {
		t.Fatalf("settle = %+v, want both full, q 40 and 70", st)
	}
	if st.DelayMsMean != (380+300)/2 {
		t.Fatalf("settle delay %v ms, want 340 (380 and 300 after the last inputs)", st.DelayMsMean)
	}
	if st.ImageWidth != 540 || st.ImageHeight != 1212 {
		t.Fatalf("settle px %d×%d, want 540×1212", st.ImageWidth, st.ImageHeight)
	}
}
