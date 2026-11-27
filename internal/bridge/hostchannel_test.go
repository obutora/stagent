package bridge

import (
	"encoding/json"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/hostscreen"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/wire"
)

// helloAs says hello with the app capabilities caps and returns stagent's.
func helloAs(t *testing.T, a *app, caps ...string) []string {
	t.Helper()
	var res wire.HelloResult
	if e := a.call(t, wire.MethodHello, wire.HelloParams{Protocol: 1, Client: "test", Capabilities: caps}, &res); e != nil {
		t.Fatal(e)
	}
	return res.Capabilities
}

// startScreen starts a stream of k and takes its first frame
// (host_screen.frame seq 1), acking it.
func startScreen(t *testing.T, a *app) {
	t.Helper()
	target := wire.HostScreenStartParams{Target: wire.HostScreenTarget{Kind: wire.HostScreenBrowser, Port: 9222, TargetID: "P"}}
	if e := a.call(t, wire.MethodHostScreenStart, target, nil); e != nil {
		t.Fatal(e)
	}
	var fr wire.HostScreenFrame
	json.Unmarshal(a.nextNotify(t, wire.NotifyHostScreenFrame).Params, &fr)
	if e := a.call(t, wire.MethodHostScreenAck, wire.HostScreenAckParams{Seq: fr.Seq}, nil); e != nil {
		t.Fatal(e)
	}
}

// channel is `stagent host-screen-channel` run against the bridge with
// the line written to its stdin; out is its stdout, ended reports its
// return.
type channel struct {
	out   *io.PipeReader
	stdin *io.PipeWriter
	ended chan error
}

func openChannel(t *testing.T, l *paths.Layout, line string) *channel {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &channel{out: outR, stdin: inW, ended: make(chan error, 1)}
	go func() {
		err := runChannel(l, inR, outW)
		outW.CloseWithError(io.EOF)
		c.ended <- err
	}()
	go inW.Write([]byte(line + "\n"))
	t.Cleanup(func() {
		inW.Close()
		outR.Close()
	})
	return c
}

func channelToken(t *testing.T, a *app) string {
	t.Helper()
	var res wire.HostScreenChannelResult
	if e := a.call(t, wire.MethodHostScreenChannel, nil, &res); e != nil {
		t.Fatal(e)
	}
	return res.Token
}

// stagent announces host_screen_binary; host_screen.channel is served only
// to an app that announced it too, so an older app keeps host_screen.frame.
func TestHostScreenBinaryIsNegotiatedInHello(t *testing.T) {
	_, a := startBridge(t, withScreen(&screenKind{}))
	if caps := helloAs(t, a); !slices.Contains(caps, wire.CapHostScreenBinary) {
		t.Fatalf("hello capabilities %q lack %s", caps, wire.CapHostScreenBinary)
	}
	if e := a.call(t, wire.MethodHostScreenChannel, nil, nil); e == nil || e.Code != wire.ErrUnsupported {
		t.Fatalf("host_screen.channel to an app without %s: %v, want unsupported", wire.CapHostScreenBinary, e)
	}
	helloAs(t, a, wire.CapHostScreenBinary)
	if tok := channelToken(t, a); len(tok) != channelTokenLen {
		t.Fatalf("token %q, want %d chars", tok, channelTokenLen)
	}
}

// Frames go over the channel the app opened with its token, as records,
// and back to host_screen.frame once it closes.
func TestHostScreenFramesGoOverTheChannelWhileItIsOpen(t *testing.T) {
	k := &screenKind{}
	l, a := startBridge(t, withScreen(k))
	helloAs(t, a, wire.CapHostScreenBinary)
	startScreen(t, a)
	st := k.stream.Load()

	c := openChannel(t, l, channelToken(t, a))
	// The bridge attaches the channel once it read the token: frames sent
	// until then are still host_screen.frame.
	deadline := time.Now().Add(5 * time.Second)
	var h wire.HostScreenRecordHeader
	var payload []byte
	got := make(chan error, 1)
	go func() {
		var err error
		h, payload, err = hostscreen.ReadRecord(c.out)
		got <- err
	}()
	for {
		// Frames are paced: one may only replace the one held, which the
		// pacing timer sends.
		st.ev.Frame(hostscreen.Frame{JPEG: []byte("bin"), Width: 3, Height: 4})
		select {
		case err := <-got:
			if err != nil {
				t.Fatal(err)
			}
			if h.Kind != wire.HostScreenRecordFull || h.Seq < 2 || h.Width != 3 || h.Height != 4 || string(payload) != "bin" {
				t.Fatalf("record %+v %q", h, payload)
			}
			goto attached
		case m := <-a.notifs:
			// Not attached yet: the frame came as JSON; ack it.
			var fr wire.HostScreenFrame
			json.Unmarshal(m.Params, &fr)
			a.call(t, wire.MethodHostScreenAck, wire.HostScreenAckParams{Seq: fr.Seq}, nil)
		}
		if time.Now().After(deadline) {
			t.Fatal("no frame over the channel")
		}
		time.Sleep(20 * time.Millisecond)
	}
attached:
	// Acks are still host_screen.ack, on the same seq counter.
	if e := a.call(t, wire.MethodHostScreenAck, wire.HostScreenAckParams{Seq: h.Seq}, nil); e != nil {
		t.Fatal(e)
	}

	// The app closes the channel: the frames come back as host_screen.frame.
	c.stdin.Close()
	select {
	case <-c.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("host-screen-channel did not end with its stdin")
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		st.ev.Frame(hostscreen.Frame{JPEG: []byte("json"), Width: 3, Height: 4})
		var fr wire.HostScreenFrame
		select {
		case m := <-a.notifs:
			json.Unmarshal(m.Params, &fr)
		case <-time.After(200 * time.Millisecond):
		}
		if fr.Data == "anNvbg==" { // "json"
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("frames did not come back as host_screen.frame")
		}
	}
}

// Only the token of the last host_screen.channel opens a channel, once:
// another one (another session's, a guess, a replay) is closed unused and
// the frames stay host_screen.frame.
func TestHostScreenChannelRefusesAWrongToken(t *testing.T) {
	k := &screenKind{}
	l, a := startBridge(t, withScreen(k))
	helloAs(t, a, wire.CapHostScreenBinary)
	startScreen(t, a)
	tok := channelToken(t, a)
	wrong := tok[:channelIDLen] + strings.Repeat("0", channelTokenLen-channelIDLen)

	for name, line := range map[string]string{
		"wrong secret": wrong,
		"not a token":  "hello",
	} {
		t.Run(name, func(t *testing.T) {
			c := openChannel(t, l, line)
			if n, _ := io.Copy(io.Discard, c.out); n != 0 {
				t.Fatalf("a refused channel got %d bytes", n)
			}
			select {
			case <-c.ended:
			case <-time.After(5 * time.Second):
				t.Fatal("a refused channel did not end")
			}
		})
	}

	// The right token still works once; a replay is refused.
	first := openChannel(t, l, tok)
	go func() {
		for range 50 {
			k.stream.Load().ev.Frame(hostscreen.Frame{JPEG: []byte("x"), Width: 1, Height: 1})
			time.Sleep(20 * time.Millisecond)
		}
	}()
	go func() {
		for m := range a.notifs {
			var fr wire.HostScreenFrame
			json.Unmarshal(m.Params, &fr)
			a.Go(wire.MethodHostScreenAck, mustJSON(wire.HostScreenAckParams{Seq: fr.Seq}), func(*wire.Msg) {})
		}
	}()
	if _, _, err := hostscreen.ReadRecord(first.out); err != nil {
		t.Fatalf("the right token: %v", err)
	}
	replay := openChannel(t, l, tok)
	if n, _ := io.Copy(io.Discard, replay.out); n != 0 {
		t.Fatalf("a replayed token got %d bytes", n)
	}
}

// A token not used within channelTokenTTL of host_screen.channel no longer
// opens a channel; one used just within it does.
func TestHostScreenChannelTokenExpires(t *testing.T) {
	var mu sync.Mutex
	now := time.Unix(1000, 0)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}
	k := &screenKind{}
	l, a := startBridge(t, withScreen(k), func(b *Bridge) { b.hostChan.now = clock })
	helloAs(t, a, wire.CapHostScreenBinary)
	startScreen(t, a)
	go func() {
		for m := range a.notifs {
			var fr wire.HostScreenFrame
			json.Unmarshal(m.Params, &fr)
			a.Go(wire.MethodHostScreenAck, mustJSON(wire.HostScreenAckParams{Seq: fr.Seq}), func(*wire.Msg) {})
		}
	}()
	go func() {
		for range 100 {
			k.stream.Load().ev.Frame(hostscreen.Frame{JPEG: []byte("x"), Width: 1, Height: 1})
			time.Sleep(20 * time.Millisecond)
		}
	}()

	stale := channelToken(t, a)
	advance(channelTokenTTL + time.Millisecond)
	c := openChannel(t, l, stale)
	got := make(chan int64, 1)
	go func() {
		n, _ := io.Copy(io.Discard, c.out)
		got <- n
	}()
	select {
	case n := <-got:
		if n != 0 {
			t.Fatalf("an expired token got %d bytes", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an expired token opened a channel")
	}

	fresh := channelToken(t, a)
	advance(channelTokenTTL)
	if _, _, err := hostscreen.ReadRecord(openChannel(t, l, fresh).out); err != nil {
		t.Fatalf("a token used within %v: %v", channelTokenTTL, err)
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// greyRaster is a w×h grey picture as pixels, with pixel (0, 0) set to v.
func greyRaster(w, h int, v byte) *hostscreen.Raster {
	pix := make([]byte, w*h*3)
	for i := range pix {
		pix[i] = 0x80
	}
	pix[0] = v
	return &hostscreen.Raster{Pix: pix, W: w, H: h, JPEG: 70}
}

// An app that announced host_screen_tiles (stagent announces it too) gets
// the tiles changed over the channel, and may ask for a full frame with
// host_screen.keyframe, answered at once.
func TestHostScreenTilesGoToAnAppThatAnnouncedThem(t *testing.T) {
	k := &screenKind{}
	l, a := startBridge(t, withScreen(k))
	if e := a.call(t, wire.MethodHostScreenKeyframe, nil, nil); e == nil || e.Code != wire.ErrBadRequest {
		t.Fatalf("host_screen.keyframe without a stream: %v, want bad_request", e)
	}
	if caps := helloAs(t, a, wire.CapHostScreenBinary, wire.CapHostScreenTiles); !slices.Contains(caps, wire.CapHostScreenTiles) {
		t.Fatalf("hello capabilities %q lack %s", caps, wire.CapHostScreenTiles)
	}
	startScreen(t, a)
	st := k.stream.Load()
	c := openChannel(t, l, channelToken(t, a))
	recs := make(chan wire.HostScreenRecordHeader, 8)
	go func() {
		for {
			h, _, err := hostscreen.ReadRecord(c.out)
			if err != nil {
				close(recs)
				return
			}
			recs <- h
		}
	}()
	// Until the channel is attached the frames come as JSON (acked); the
	// first record is a full one.
	deadline := time.Now().Add(5 * time.Second)
	var first wire.HostScreenRecordHeader
	for first.Seq == 0 {
		st.ev.Frame(hostscreen.Frame{Raster: greyRaster(128, 128, 0), Width: 3, Height: 4})
		select {
		case first = <-recs:
		case m := <-a.notifs:
			var fr wire.HostScreenFrame
			json.Unmarshal(m.Params, &fr)
			a.call(t, wire.MethodHostScreenAck, wire.HostScreenAckParams{Seq: fr.Seq}, nil)
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("no record over the channel")
		}
	}
	if first.Kind != wire.HostScreenRecordFull {
		t.Fatalf("first record %+v, want full", first)
	}
	a.call(t, wire.MethodHostScreenAck, wire.HostScreenAckParams{Seq: first.Seq}, nil)
	st.ev.Frame(hostscreen.Frame{Raster: greyRaster(128, 128, 0xff), Width: 3, Height: 4})
	var h wire.HostScreenRecordHeader
	select {
	case h = <-recs:
	case <-time.After(5 * time.Second):
		t.Fatal("no record for the changed picture")
	}
	if h.Kind != wire.HostScreenRecordTiles || h.Prev != first.Seq || len(h.Tiles) != 1 || h.Tiles[0].X != 0 || h.Tiles[0].W != 64 {
		t.Fatalf("record %+v, want the tile at 0,0 on %d", h, first.Seq)
	}
	a.call(t, wire.MethodHostScreenAck, wire.HostScreenAckParams{Seq: h.Seq}, nil)
	if e := a.call(t, wire.MethodHostScreenKeyframe, nil, nil); e != nil {
		t.Fatal(e)
	}
	select {
	case h = <-recs:
	case <-time.After(5 * time.Second):
		t.Fatal("no record after host_screen.keyframe")
	}
	if h.Kind != wire.HostScreenRecordFull {
		t.Fatalf("record after host_screen.keyframe %+v, want full", h)
	}
}
