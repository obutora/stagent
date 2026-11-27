package hostscreen

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"slices"
	"time"
)

// Tiles (#1974): a kind that has its pictures as pixels (Frame.Raster: the
// Android emulator's RGB888) lets the binary channel carry only what
// changed since the frame sent before, as "tiles" records, to an app that
// takes them (wire.CapHostScreenTiles). A picture is compared with the one
// sent last in cells of TileSize px; the changed cells go as JPEGs of
// their own, side by side ones joined into one rectangle. The frame goes
// whole instead (a "full" record) when there is nothing to build on or the
// app's picture may not be the one sent last (see flow.tileableLocked),
// and when the tiles would cost about as much as the whole (tilesUpTo,
// maxTileRects).

const (
	// TileSize is the side of the cells pictures are compared in, in the
	// picture's px.
	TileSize = 64
	// FullEvery is how long tiles may follow the last full frame: a frame
	// sent later goes whole, which puts right anything the app's picture
	// got wrong.
	FullEvery = 10 * time.Second
	// tilesUpTo is the largest share of the cells changed that goes as
	// tiles. Tiles cost about their share of the whole picture (scrolling
	// the emulator's Settings: 82% of the picture as tiles took 87% of the
	// bytes of full frames), so they pay until nearly all changed; past
	// this the whole picture goes, which also starts the app's picture
	// over.
	tilesUpTo = 0.9
	// maxTileRects bounds the rectangles of one frame: each is a JPEG with
	// its own headers (about 600 bytes from image/jpeg), so cells changed
	// here and there go whole instead.
	maxTileRects = 16
)

// Raster is a picture as pixels: RGB888, 3 bytes a pixel, rows top down,
// no padding between them. It is not changed once handed over.
type Raster struct {
	Pix  []byte
	W, H int // in px
	JPEG int // the JPEG quality it is encoded at (Quality.JPEG)
}

func (r *Raster) check() error {
	if r.W <= 0 || r.H <= 0 || len(r.Pix) < r.W*r.H*3 {
		return fmt.Errorf("hostscreen: RGB888 picture of %d bytes is not %dx%d", len(r.Pix), r.W, r.H)
	}
	return nil
}

// Whole is the frame as one JPEG: its JPEG, or its Raster encoded.
func (fr Frame) Whole() ([]byte, error) {
	if fr.Raster == nil {
		return fr.JPEG, nil
	}
	var e encoder
	var buf bytes.Buffer
	if err := e.encode(&buf, fr.Raster, image.Rect(0, 0, fr.Raster.W, fr.Raster.H)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// encoder encodes parts of rasters as JPEGs, reusing its pixels.
type encoder struct {
	pix []byte // RGBA
}

// encode appends rect of r to buf as a JPEG of quality r.JPEG.
func (e *encoder) encode(buf *bytes.Buffer, r *Raster, rect image.Rectangle) error {
	if err := r.check(); err != nil {
		return err
	}
	w, h := rect.Dx(), rect.Dy()
	if cap(e.pix) < w*h*4 {
		e.pix = make([]byte, w*h*4)
	}
	img := &image.RGBA{Pix: e.pix[:w*h*4], Stride: w * 4, Rect: image.Rect(0, 0, w, h)}
	for y := range h {
		src := r.Pix[((rect.Min.Y+y)*r.W+rect.Min.X)*3:][:w*3]
		dst := img.Pix[y*img.Stride:][:w*4]
		for i, j := 0, 0; j < len(src); i, j = i+4, j+3 {
			dst[i], dst[i+1], dst[i+2], dst[i+3] = src[j], src[j+1], src[j+2], 0xff
		}
	}
	return jpeg.Encode(buf, img, &jpeg.Options{Quality: r.JPEG})
}

// changedTiles compares b with a, of the same size, in cells of TileSize
// px: it returns the rectangles of the cells changed (see tileRects), how
// many cells changed and how many there are.
func changedTiles(a, b *Raster) (rects []image.Rectangle, changed, cells int) {
	cols, rows := (b.W+TileSize-1)/TileSize, (b.H+TileSize-1)/TileSize
	grid := make([]bool, cols*rows)
	stride := b.W * 3
	for y := range b.H {
		la, lb := a.Pix[y*stride:][:stride], b.Pix[y*stride:][:stride]
		if bytes.Equal(la, lb) {
			continue
		}
		row := grid[y/TileSize*cols:][:cols]
		for c := range cols {
			if row[c] {
				continue
			}
			x0, x1 := c*TileSize*3, min(b.W, (c+1)*TileSize)*3
			if !bytes.Equal(la[x0:x1], lb[x0:x1]) {
				row[c] = true
				changed++
			}
		}
	}
	return tileRects(grid, cols, rows, b.W, b.H), changed, cols * rows
}

// tileRects joins the changed cells of grid (cols×rows, row by row) of a
// w×h picture into rectangles: the cells side by side in a row, and such
// runs one above the other over the same columns. They are cut at the
// picture's edges and ordered by their top, then their left.
func tileRects(grid []bool, cols, rows, w, h int) []image.Rectangle {
	// A rectangle in cells: columns c0–c1, rows r0–r1 (ends excluded).
	type cells struct{ c0, c1, r0, r1 int }
	var open, rects []cells
	for r := range rows {
		var next []cells
		for c := 0; c < cols; {
			if !grid[r*cols+c] {
				c++
				continue
			}
			cur := cells{c0: c, r0: r}
			for c < cols && grid[r*cols+c] {
				c++
			}
			cur.c1, cur.r1 = c, r+1
			if i := slices.IndexFunc(open, func(o cells) bool { return o.c0 == cur.c0 && o.c1 == cur.c1 }); i >= 0 {
				cur.r0 = open[i].r0
				open = slices.Delete(open, i, i+1)
			}
			next = append(next, cur)
		}
		rects = append(rects, open...)
		open = next
	}
	rects = append(rects, open...)
	slices.SortFunc(rects, func(a, b cells) int {
		if a.r0 != b.r0 {
			return a.r0 - b.r0
		}
		return a.c0 - b.c0
	})
	out := make([]image.Rectangle, len(rects))
	for i, c := range rects {
		out[i] = image.Rect(c.c0*TileSize, c.r0*TileSize, min(w, c.c1*TileSize), min(h, c.r1*TileSize))
	}
	return out
}
