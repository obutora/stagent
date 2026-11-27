package main

import (
	"image"
	"image/color"
	"slices"
	"sync"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// settleWindow: an input is measured only when no frame that looked
// different arrived within this long before it, so the change it is timed
// to is its own and not the tail of the previous one (a scroll's fling).
const settleWindow = 500 * time.Millisecond

const (
	// burstGap: inputs closer than this are one gesture (a scroll burst).
	burstGap = time.Second
	// settleLookout: how long after a gesture's last input the frame left
	// on the screen is looked for (stagent sends its settle frame once the
	// screen rested 300 ms, at the latest 5 s after the last input,
	// PROTOCOL.md "Quality while operating").
	settleLookout = 6 * time.Second
)

type sample struct {
	at time.Time
	n  int
}

type frameRec struct {
	seq           int64
	at            time.Time // arrival at the app
	kind          string    // wire.HostScreenRecordFull or Tiles
	payload       int       // JPEG bytes (all the tiles' of a tiles frame)
	width, height int       // target coordinates, as the frame says
	analyzed      bool
	changed       bool // looks different from the frame before it
	fp            fingerprint
	decode        time.Duration
	quality       int // estimated JPEG quality (0: unknown)
}

type inputState int

const (
	inputPending inputState = iota
	inputMeasured
	inputUnsettled
)

type inputRec struct {
	at      time.Time
	ref     int64 // the frame the app showed when it sent the input
	state   inputState
	latency time.Duration
}

// Recorder keeps what the app side saw: bytes on the wire each way, the
// frames, the inputs and when the screen first changed after each.
// Safe for concurrent use.
type Recorder struct {
	mu      sync.Mutex
	down    []sample
	up      []sample
	frames  []*frameRec
	bySeq   map[int64]*frameRec
	lastFP  *frameRec // the latest analyzed frame
	lastSeq int64     // the latest frame arrived
	inputs  []*inputRec
	sent    []time.Time // every input, timed or not
}

func NewRecorder() *Recorder { return &Recorder{bySeq: map[int64]*frameRec{}} }

// Down records n bytes from stagent arriving at the app at at.
func (r *Recorder) Down(at time.Time, n int) {
	r.mu.Lock()
	r.down = append(r.down, sample{at, n})
	r.mu.Unlock()
}

// Up records n bytes from the app arriving at stagent at at.
func (r *Recorder) Up(at time.Time, n int) {
	r.mu.Lock()
	r.up = append(r.up, sample{at, n})
	r.mu.Unlock()
}

// Frame records that frame seq of kind (payload JPEG bytes, width×height
// target coordinates) arrived at the app at at. Frames arrive in seq order.
func (r *Recorder) Frame(seq int64, at time.Time, kind string, payload, width, height int) {
	r.mu.Lock()
	f := &frameRec{seq: seq, at: at, kind: kind, payload: payload, width: width, height: height}
	r.frames = append(r.frames, f)
	r.bySeq[seq] = f
	r.lastSeq = seq
	r.mu.Unlock()
}

// Sent records an input the app sent at at, not timed.
func (r *Recorder) Sent(at time.Time) {
	r.mu.Lock()
	r.sent = append(r.sent, at)
	r.mu.Unlock()
}

// Input records an input the app sent at at, against the frame it shows,
// to be timed.
func (r *Recorder) Input(at time.Time) {
	r.mu.Lock()
	r.inputs = append(r.inputs, &inputRec{at: at, ref: r.lastSeq})
	r.sent = append(r.sent, at)
	r.mu.Unlock()
}

// analysis is what looking at one frame's picture found.
type analysis struct {
	fp      fingerprint
	decode  time.Duration // decoding its JPEG(s)
	quality int           // estimated JPEG quality, 0 unknown
}

// Analyzed records frame seq's picture. Frames are analyzed in seq order,
// after Frame.
func (r *Recorder) Analyzed(seq int64, a analysis) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.bySeq[seq]
	if f == nil {
		return
	}
	f.analyzed, f.fp, f.decode, f.quality = true, a.fp, a.decode, a.quality
	f.changed = r.lastFP == nil || r.lastFP.fp.differs(a.fp)
	r.lastFP = f
	for _, in := range r.inputs {
		if in.state != inputPending || in.ref >= seq || !f.at.After(in.at) {
			continue
		}
		ref := r.bySeq[in.ref]
		if ref == nil || !r.settledLocked(in.at) {
			in.state = inputUnsettled
			continue
		}
		if ref.fp.differs(a.fp) {
			in.state, in.latency = inputMeasured, f.at.Sub(in.at)
		}
	}
}

// settledLocked: no frame that looked different arrived in the settle
// window before at.
func (r *Recorder) settledLocked(at time.Time) bool {
	for _, f := range r.frames {
		if f.analyzed && f.changed && f.at.After(at.Add(-settleWindow)) && !f.at.After(at) {
			return false
		}
	}
	return true
}

// Latency sums up the inputs of a window.
type Latency struct {
	Measured  int     `json:"measured"`
	Unsettled int     `json:"unsettled"` // the screen was still changing when sent
	NoChange  int     `json:"no_change"` // no different frame arrived after it
	P50Ms     float64 `json:"p50_ms"`
	P95Ms     float64 `json:"p95_ms"`
	MaxMs     float64 `json:"max_ms"`
}

// Settle sums up the frames left on the screen once a gesture ended: per
// gesture (inputs less than burstGap apart) whose last input is in the
// window, the last frame that arrived after that input, before the next
// one and within settleLookout.
type Settle struct {
	Count       int     `json:"count"` // gestures that had such a frame
	Full        int     `json:"full"`  // of them, full frames
	DelayMsMean float64 `json:"delay_ms_mean"`
	QualityMin  int     `json:"quality_min"`
	QualityMax  int     `json:"quality_max"`
	BytesMean   float64 `json:"bytes_mean"`
	ImageWidth  int     `json:"image_width"` // of the last one
	ImageHeight int     `json:"image_height"`
}

// Summary is what one window of a scenario measured, per second of it.
type Summary struct {
	Seconds            float64 `json:"seconds"`
	DownBytesPerSec    float64 `json:"down_bytes_per_sec"` // stagent → app, everything on the wire
	UpBytesPerSec      float64 `json:"up_bytes_per_sec"`   // app → stagent
	FramesPerSec       float64 `json:"frames_per_sec"`
	FullFrames         int     `json:"full_frames"`
	TileFrames         int     `json:"tile_frames"`
	PayloadBytesPerSec float64 `json:"payload_bytes_per_sec"` // JPEG bytes of the frames
	MeanFrameBytes     float64 `json:"mean_frame_bytes"`      // JPEG bytes per frame
	ImageWidth         int     `json:"image_width"`           // px of the last frame's picture
	ImageHeight        int     `json:"image_height"`
	QualityMean        float64 `json:"quality_mean"`   // estimated JPEG quality of the frames
	DecodeMsMean       float64 `json:"decode_ms_mean"` // image/jpeg decode per frame on this machine
	Latency            Latency `json:"latency"`
	Settle             Settle  `json:"settle"`
}

// Summary sums up [from, to).
func (r *Recorder) Summary(from, to time.Time) Summary {
	r.mu.Lock()
	defer r.mu.Unlock()
	in := func(at time.Time) bool { return !at.Before(from) && at.Before(to) }
	sec := to.Sub(from).Seconds()
	s := Summary{Seconds: sec}
	var down, up, payload, frames int
	for _, x := range r.down {
		if in(x.at) {
			down += x.n
		}
	}
	for _, x := range r.up {
		if in(x.at) {
			up += x.n
		}
	}
	var decode time.Duration
	var decoded, qualities, quality int
	for _, f := range r.frames {
		if !in(f.at) {
			continue
		}
		frames++
		if f.kind == wire.HostScreenRecordTiles {
			s.TileFrames++
		} else {
			s.FullFrames++
		}
		payload += f.payload
		if f.analyzed {
			decoded++
			decode += f.decode
			s.ImageWidth, s.ImageHeight = f.fp.w, f.fp.h
			if f.quality > 0 {
				qualities++
				quality += f.quality
			}
		}
	}
	s.DownBytesPerSec = float64(down) / sec
	s.UpBytesPerSec = float64(up) / sec
	s.FramesPerSec = float64(frames) / sec
	s.PayloadBytesPerSec = float64(payload) / sec
	if frames > 0 {
		s.MeanFrameBytes = float64(payload) / float64(frames)
	}
	if decoded > 0 {
		s.DecodeMsMean = float64(decode) / float64(decoded) / float64(time.Millisecond)
	}
	if qualities > 0 {
		s.QualityMean = float64(quality) / float64(qualities)
	}
	var lat []time.Duration
	for _, x := range r.inputs {
		if !in(x.at) {
			continue
		}
		switch x.state {
		case inputMeasured:
			lat = append(lat, x.latency)
		case inputUnsettled:
			s.Latency.Unsettled++
		default:
			s.Latency.NoChange++
		}
	}
	s.Latency.Measured = len(lat)
	if len(lat) > 0 {
		slices.Sort(lat)
		at := func(p float64) float64 {
			return float64(lat[int(p*float64(len(lat)-1)+0.5)]) / float64(time.Millisecond)
		}
		s.Latency.P50Ms, s.Latency.P95Ms, s.Latency.MaxMs = at(0.5), at(0.95), at(1)
	}
	s.Settle = r.settleLocked(in)
	return s
}

// FrameInfo is one frame of a window, for -frames.
type FrameInfo struct {
	AtMs    float64 `json:"at_ms"` // arrival after the window's start
	Seq     int64   `json:"seq"`
	Kind    string  `json:"kind"`
	Bytes   int     `json:"bytes"` // JPEG bytes
	Width   int     `json:"image_width"`
	Height  int     `json:"image_height"`
	Quality int     `json:"quality"`
	Changed bool    `json:"changed"`
}

// Inputs are the times of the inputs sent in [from, to), in ms after from.
func (r *Recorder) Inputs(from, to time.Time) []float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ms []float64
	for _, at := range r.sent {
		if !at.Before(from) && at.Before(to) {
			ms = append(ms, float64(at.Sub(from))/float64(time.Millisecond))
		}
	}
	slices.Sort(ms)
	return ms
}

// Frames lists the frames that arrived in [from, to).
func (r *Recorder) Frames(from, to time.Time) []FrameInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	var fs []FrameInfo
	for _, f := range r.frames {
		if f.at.Before(from) || !f.at.Before(to) {
			continue
		}
		fs = append(fs, FrameInfo{AtMs: float64(f.at.Sub(from)) / float64(time.Millisecond), Seq: f.seq, Kind: f.kind,
			Bytes: f.payload, Width: f.fp.w, Height: f.fp.h, Quality: f.quality, Changed: f.changed})
	}
	return fs
}

func (r *Recorder) settleLocked(in func(time.Time) bool) Settle {
	var st Settle
	sent := slices.Clone(r.sent)
	slices.SortFunc(sent, func(a, b time.Time) int { return a.Compare(b) })
	var delay time.Duration
	var bytes int
	for i, last := range sent {
		if i+1 < len(sent) && sent[i+1].Sub(last) < burstGap || !in(last) {
			continue // not the gesture's last input
		}
		until := last.Add(settleLookout)
		if i+1 < len(sent) && sent[i+1].Before(until) {
			until = sent[i+1]
		}
		var left *frameRec
		for _, f := range r.frames {
			if f.at.After(last) && f.at.Before(until) && f.analyzed {
				left = f
			}
		}
		if left == nil {
			continue
		}
		if st.Count == 0 || left.quality < st.QualityMin {
			st.QualityMin = left.quality
		}
		st.QualityMax = max(st.QualityMax, left.quality)
		st.Count++
		if left.kind != wire.HostScreenRecordTiles {
			st.Full++
		}
		delay += left.at.Sub(last)
		bytes += left.payload
		st.ImageWidth, st.ImageHeight = left.fp.w, left.fp.h
	}
	if st.Count > 0 {
		st.DelayMsMean = float64(delay) / float64(st.Count) / float64(time.Millisecond)
		st.BytesMean = float64(bytes) / float64(st.Count)
	}
	return st
}

// ---------------------------------------------------------------------------
// Pictures

// fingerprintCells is the number of cells along a picture's longer side.
const fingerprintCells = 160

// fingerprint is a picture's mean luma over a grid of square cells: small
// enough to keep for every frame, coarse enough that JPEG noise (a pixel or
// two) does not count as a change while a typed glyph or a scroll does.
// scaled is the same over scaledCells along the longer side, cut in
// proportion to the picture (a px on a cut counts in part to each side),
// so that the same picture at other px (stagent's half scale while
// operating, #1974) has the same scaled grid.
type fingerprint struct {
	w, h       int // the picture's px
	cols, rows int
	cells      []uint8
	nx, ny     int
	scaled     []uint8
}

func fingerprintOf(img image.Image) fingerprint {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	long := max(w, h)
	cell := max((long+fingerprintCells-1)/fingerprintCells, 1)
	cols, rows := (w+cell-1)/cell, (h+cell-1)/cell
	nx := max(min((scaledCells*w+long/2)/long, w), 1)
	ny := max(min((scaledCells*h+long/2)/long, h), 1)
	sum := make([]int, cols*rows)
	cnt := make([]int, cols*rows)
	xs, ys := cuts(w, nx), cuts(h, ny)
	ssum := make([]float64, nx*ny)
	luma := lumaFunc(img)
	for y := range h {
		cy := ys[y]
		for x := range w {
			v := int(luma(b.Min.X+x, b.Min.Y+y))
			i := (y/cell)*cols + x/cell
			sum[i] += v
			cnt[i]++
			cx := xs[x]
			fv := float64(v)
			ssum[cy.i*nx+cx.i] += fv * cy.f * cx.f
			if cx.f < 1 {
				ssum[cy.i*nx+cx.i+1] += fv * cy.f * (1 - cx.f)
			}
			if cy.f < 1 {
				ssum[(cy.i+1)*nx+cx.i] += fv * (1 - cy.f) * cx.f
				if cx.f < 1 {
					ssum[(cy.i+1)*nx+cx.i+1] += fv * (1 - cy.f) * (1 - cx.f)
				}
			}
		}
	}
	cells := make([]uint8, len(sum))
	for i := range sum {
		cells[i] = uint8(sum[i] / cnt[i])
	}
	area := float64(w) / float64(nx) * float64(h) / float64(ny)
	scaled := make([]uint8, len(ssum))
	for i := range ssum {
		scaled[i] = uint8(min(ssum[i]/area+0.5, 255))
	}
	return fingerprint{w: w, h: h, cols: cols, rows: rows, cells: cells, nx: nx, ny: ny, scaled: scaled}
}

// cut is where a px falls on a grid: cell i, and the share f of the px in
// it (the rest is in cell i+1).
type cut struct {
	i int
	f float64
}

// cuts places each of n px on a grid of g cells over them (g ≤ n).
func cuts(n, g int) []cut {
	c := make([]cut, n)
	for p := range n {
		lo, hi := float64(p*g)/float64(n), float64((p+1)*g)/float64(n)
		i := int(lo)
		c[p] = cut{i, 1}
		if edge := float64(i + 1); hi > edge && i+1 < g {
			c[p].f = (edge - lo) / (hi - lo)
		}
	}
	return c
}

// lumaFunc reads Y directly from what image/jpeg decodes to.
func lumaFunc(img image.Image) func(x, y int) uint8 {
	switch m := img.(type) {
	case *image.YCbCr:
		return func(x, y int) uint8 { return m.Y[m.YOffset(x, y)] }
	case *image.Gray:
		return func(x, y int) uint8 { return m.Pix[m.PixOffset(x, y)] }
	case *image.RGBA:
		// As color.GrayModel.
		return func(x, y int) uint8 {
			p := m.Pix[m.PixOffset(x, y):]
			return uint8((19595*uint32(p[0])*0x101 + 38470*uint32(p[1])*0x101 + 7471*uint32(p[2])*0x101 + 1<<15) >> 24)
		}
	}
	return func(x, y int) uint8 { return color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y }
}

const (
	cellDelta    = 12 // mean luma difference that makes a cell different
	changedCells = 2  // differing cells that make the pictures differ
	// scaledCells: the grid along the longer side on which pictures of
	// other px are compared (cells of the fine grids of two sizes do not
	// cover the same area). A 32 px letter still changes two of its cells.
	scaledCells = 80
)

// differs: the pictures look different. Pictures of other px are the same
// picture scaled when their scaled grids match (same sides' ratio, not a
// turn); they are then compared on those.
func (a fingerprint) differs(b fingerprint) bool {
	ac, bc := a.cells, b.cells
	if a.w != b.w || a.h != b.h {
		if a.nx != b.nx || a.ny != b.ny {
			return true
		}
		ac, bc = a.scaled, b.scaled
	}
	n := 0
	for i := range ac {
		d := int(ac[i]) - int(bc[i])
		if d > cellDelta || d < -cellDelta {
			if n++; n >= changedCells {
				return true
			}
		}
	}
	return false
}
