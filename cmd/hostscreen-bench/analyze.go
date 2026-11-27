package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/draw"
	"image/jpeg"
	"math"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// Analyzer decodes frames one after another off the receiving goroutine
// (the app acks before it draws) and hands each picture to the recorder.
// A tiles frame is drawn on the picture before it, as the app's
// compositor does, and the whole picture is what is looked at.
type Analyzer struct {
	r    *Recorder
	ch   chan Frame
	done chan struct{}
}

func NewAnalyzer(r *Recorder) *Analyzer {
	a := &Analyzer{r: r, ch: make(chan Frame, 4096), done: make(chan struct{})}
	go func() {
		defer close(a.done)
		var c composer
		for f := range a.ch {
			if an, ok := c.add(f); ok {
				r.Analyzed(f.Seq, an)
			}
		}
	}()
	return a
}

// Add records that f arrived at the app at at and queues its picture.
func (a *Analyzer) Add(at time.Time, f Frame) {
	kind := f.Kind
	if kind == "" {
		kind = wire.HostScreenRecordFull
	}
	a.r.Frame(f.Seq, at, kind, len(f.JPEG), f.Width, f.Height)
	a.ch <- f
}

// Close waits for the queued frames.
func (a *Analyzer) Close() {
	close(a.ch)
	<-a.done
}

// composer keeps the picture the app shows.
type composer struct {
	shown image.Image // the last full frame's, as decoded
	drawn *image.RGBA // shown with the tiles since drawn on it; nil before tiles
}

func (c *composer) add(f Frame) (analysis, bool) {
	start := time.Now()
	if f.Kind != wire.HostScreenRecordTiles {
		img, err := jpeg.Decode(bytes.NewReader(f.JPEG))
		took := time.Since(start)
		if err != nil {
			c.shown, c.drawn = nil, nil
			return analysis{}, false
		}
		c.shown, c.drawn = img, nil
		return analysis{fp: fingerprintOf(img), decode: took, quality: jpegQuality(f.JPEG)}, true
	}
	if c.shown == nil {
		return analysis{}, false
	}
	if c.drawn == nil {
		c.drawn = image.NewRGBA(c.shown.Bounds())
		draw.Draw(c.drawn, c.drawn.Bounds(), c.shown, c.shown.Bounds().Min, draw.Src)
	}
	var decode time.Duration
	quality, off := 0, 0
	for _, t := range f.Tiles {
		if t.Len < 0 || off+t.Len > len(f.JPEG) {
			c.shown, c.drawn = nil, nil
			return analysis{}, false
		}
		j := f.JPEG[off : off+t.Len]
		off += t.Len
		s := time.Now()
		img, err := jpeg.Decode(bytes.NewReader(j))
		decode += time.Since(s)
		if err != nil {
			c.shown, c.drawn = nil, nil
			return analysis{}, false
		}
		if quality == 0 {
			quality = jpegQuality(j)
		}
		draw.Draw(c.drawn, image.Rect(t.X, t.Y, t.X+t.W, t.Y+t.H), img, img.Bounds().Min, draw.Src)
	}
	return analysis{fp: fingerprintOf(c.drawn), decode: decode, quality: quality}, true
}

// stdLuma is the IJG luminance quantization table (JPEG Annex K), the base
// that libjpeg and Go's image/jpeg scale by quality.
var stdLuma = [64]int{
	16, 11, 10, 16, 24, 40, 51, 61, 12, 12, 14, 19, 26, 58, 60, 55,
	14, 13, 16, 24, 40, 57, 69, 56, 14, 17, 22, 29, 51, 87, 80, 62,
	18, 22, 37, 56, 68, 109, 103, 77, 24, 35, 55, 64, 81, 104, 113, 92,
	49, 64, 78, 87, 103, 121, 120, 101, 72, 92, 95, 98, 112, 100, 103, 99,
}

// jpegQuality estimates the IJG quality a JPEG was encoded at from its
// luminance table (table 0): the table's sum against the standard one
// gives the scale, which maps back to the quality. 0 when there is none.
// Encoders with tables of their own (Apple's) get the IJG quality whose
// table is closest in sum.
func jpegQuality(b []byte) int {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 0
	}
	for i := 2; i+4 <= len(b); {
		if b[i] != 0xFF {
			return 0
		}
		marker := b[i+1]
		if marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			i += 2
			continue
		}
		n := int(binary.BigEndian.Uint16(b[i+2:]))
		if n < 2 || i+2+n > len(b) {
			return 0
		}
		if marker == 0xDA { // start of scan: no table before it
			return 0
		}
		if marker == 0xDB {
			seg := b[i+4 : i+2+n]
			for len(seg) > 0 {
				precision, id := seg[0]>>4, seg[0]&0x0F
				size := 64
				if precision != 0 {
					size = 128
				}
				if len(seg) < 1+size {
					return 0
				}
				if id == 0 {
					sum := 0
					for k := range 64 {
						if precision != 0 {
							sum += int(binary.BigEndian.Uint16(seg[1+2*k:]))
						} else {
							sum += int(seg[1+k])
						}
					}
					return qualityOfTableSum(sum)
				}
				seg = seg[1+size:]
			}
		}
		i += 2 + n
	}
	return 0
}

// qualityOfTableSum is the IJG quality whose scaled luminance table has
// the sum closest to sum.
func qualityOfTableSum(sum int) int {
	best, bestD := 0, math.MaxInt
	for q := 1; q <= 100; q++ {
		scale := 200 - 2*q
		if q < 50 {
			scale = 5000 / q
		}
		s := 0
		for _, v := range stdLuma {
			s += min(max((v*scale+50)/100, 1), 255)
		}
		if d := abs(s - sum); d < bestD {
			best, bestD = q, d
		}
	}
	return best
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
