package main

import (
	"bytes"
	"io"
	"math"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/hostscreen"
	"github.com/obutora/stagent/internal/wire"
)

// appSide stands for the app's bridge and controller around a frameGate.
type appSide struct {
	acks      []int64
	keyframes int
	shown     []Frame
}

func (a *appSide) gate() *frameGate {
	return &frameGate{
		ack:      func(seq int64) { a.acks = append(a.acks, seq) },
		keyframe: func() { a.keyframes++ },
		sink:     func(f Frame) { a.shown = append(a.shown, f) },
	}
}

func (a *appSide) shownSeqs() []int64 {
	var s []int64
	for _, f := range a.shown {
		s = append(s, f.Seq)
	}
	return s
}

func fullRec(seq int64) (wire.HostScreenRecordHeader, []byte) {
	return wire.HostScreenRecordHeader{Kind: wire.HostScreenRecordFull, Seq: seq, Width: 1080, Height: 2424,
		ImageWidth: 540, ImageHeight: 1212}, []byte("jpeg-full")
}

func tilesRec(seq, prev int64) (wire.HostScreenRecordHeader, []byte) {
	return wire.HostScreenRecordHeader{Kind: wire.HostScreenRecordTiles, Seq: seq, Prev: prev, Width: 1080, Height: 2424,
		ImageWidth: 540, ImageHeight: 1212,
		Tiles: []wire.HostScreenTile{{X: 0, Y: 0, W: 64, H: 64, Len: 3}, {X: 64, Y: 128, W: 128, H: 64, Len: 4}}}, []byte("abcdefg")
}

func records(t *testing.T, recs ...func() (wire.HostScreenRecordHeader, []byte)) io.Reader {
	var b bytes.Buffer
	for _, r := range recs {
		h, p := r()
		if _, err := hostscreen.WriteRecord(&b, h, p); err != nil {
			t.Fatal(err)
		}
	}
	return &b
}

func TestRecordsAreAckedAndShownAsTheAppDoes(t *testing.T) {
	a := &appSide{}
	err := readRecords(records(t,
		func() (wire.HostScreenRecordHeader, []byte) { return fullRec(1) },
		func() (wire.HostScreenRecordHeader, []byte) { return tilesRec(2, 1) },
		func() (wire.HostScreenRecordHeader, []byte) { return tilesRec(3, 2) },
	), a.gate())
	if err != io.EOF {
		t.Fatalf("readRecords = %v, want io.EOF", err)
	}
	if !slices.Equal(a.acks, []int64{1, 2, 3}) || !slices.Equal(a.shownSeqs(), []int64{1, 2, 3}) {
		t.Fatalf("acks %v shown %v, want 1 2 3 both", a.acks, a.shownSeqs())
	}
	f := a.shown[1]
	if f.Kind != wire.HostScreenRecordTiles || f.Prev != 1 || len(f.Tiles) != 2 || string(f.JPEG) != "abcdefg" ||
		f.ImageWidth != 540 || f.ImageHeight != 1212 {
		t.Fatalf("tiles frame = %+v", f)
	}
	if a.keyframes != 0 {
		t.Fatalf("%d keyframes asked, want none", a.keyframes)
	}
}

func TestTilesOnAFrameNotShownAreDroppedAndAKeyframeAskedOnce(t *testing.T) {
	a := &appSide{}
	g := a.gate()
	// Tiles before any full frame, then a chain on the dropped one.
	readRecords(records(t,
		func() (wire.HostScreenRecordHeader, []byte) { return tilesRec(4, 3) },
		func() (wire.HostScreenRecordHeader, []byte) { return tilesRec(5, 4) },
		func() (wire.HostScreenRecordHeader, []byte) { return fullRec(6) },
		func() (wire.HostScreenRecordHeader, []byte) { return tilesRec(7, 6) },
	), g)
	if !slices.Equal(a.acks, []int64{4, 5, 6, 7}) {
		t.Fatalf("acks %v, want every record acked", a.acks)
	}
	if !slices.Equal(a.shownSeqs(), []int64{6, 7}) || a.keyframes != 1 {
		t.Fatalf("shown %v keyframes %d, want 6 7 and one keyframe", a.shownSeqs(), a.keyframes)
	}
}

func TestAJSONFrameOvertakenByTheChannelIsAckedButNotShown(t *testing.T) {
	a := &appSide{}
	g := a.gate()
	readRecords(records(t, func() (wire.HostScreenRecordHeader, []byte) { return fullRec(8) }), g)
	g.offer(Frame{Kind: wire.HostScreenRecordFull, Seq: 7, JPEG: []byte("late json")})
	if !slices.Equal(a.acks, []int64{8, 7}) || !slices.Equal(a.shownSeqs(), []int64{8}) {
		t.Fatalf("acks %v shown %v, want 8 7 acked and 8 shown", a.acks, a.shownSeqs())
	}
}

func TestBytesOfBothChannelsCountOnTheSameLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runs /bin/sh as the stagent")
	}
	rec := NewRecorder()
	down := NewLink(1e9, 0)
	down.OnDeliver = rec.Down
	c := &Conn{stagent: "/bin/sh", down: down, up: NewLink(1e9, 0), stderr: io.Discard}
	for _, out := range []string{"bridge-bytes", "channel"} {
		ch, err := c.Exec("-c", "printf "+out)
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := io.ReadAll(ch); string(b) != out {
			t.Fatalf("read %q, want %q", b, out)
		}
		ch.Close()
	}
	now := time.Now()
	s := rec.Summary(now.Add(-time.Hour), now.Add(time.Hour))
	if got := math.Round(s.DownBytesPerSec * s.Seconds); got != 19 {
		t.Fatalf("down bytes = %v, want 19 (12 + 7)", got)
	}
}
