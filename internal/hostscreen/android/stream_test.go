package android

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image/jpeg"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/obutora/stagent/internal/hostscreen"
	"github.com/obutora/stagent/internal/hostscreen/android/emupb"
	"github.com/obutora/stagent/internal/wire"
)

// rgb888 is a w×h image: red left half, blue right half.
func rgb888(w, h int) []byte {
	pix := make([]byte, 0, w*h*3)
	for range h {
		for x := range w {
			if x < w/2 {
				pix = append(pix, 0xff, 0, 0)
			} else {
				pix = append(pix, 0, 0, 0xff)
			}
		}
	}
	return pix
}

func TestEncodeRGB888(t *testing.T) {
	var buf bytes.Buffer
	rgba, err := encodeRGB888(&buf, nil, rgb888(32, 16), 32, 16)
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 32 || b.Dy() != 16 {
		t.Fatalf("JPEG is %v, want 32x16", b)
	}
	for _, c := range []struct {
		x       int
		r, g, b uint32
	}{{4, 0xff, 0, 0}, {28, 0, 0, 0xff}} {
		r, g, b, _ := img.At(c.x, 8).RGBA()
		if d := diff(r>>8, c.r) + diff(g>>8, c.g) + diff(b>>8, c.b); d > 48 {
			t.Errorf("pixel (%d, 8) = %d,%d,%d, want about %d,%d,%d", c.x, r>>8, g>>8, b>>8, c.r, c.g, c.b)
		}
	}
	// The next image of the same size reuses the buffer.
	again, err := encodeRGB888(&buf, rgba, rgb888(32, 16), 32, 16)
	if err != nil || again != rgba {
		t.Fatalf("same size: reused %v, err %v", again == rgba, err)
	}
	if _, err := encodeRGB888(&buf, rgba, rgb888(32, 15), 32, 16); err == nil {
		t.Fatal("a short image is refused")
	}
}

func diff(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}

func TestDisplaySizeFollowsRotation(t *testing.T) {
	for _, c := range []struct{ lcdW, lcdH, imgW, imgH, w, h int }{
		{1080, 2424, 540, 1212, 1080, 2424}, // portrait phone
		{1080, 2424, 1212, 540, 2424, 1080}, // turned
		{2560, 1600, 1280, 800, 2560, 1600}, // landscape tablet
		{2560, 1600, 800, 1280, 1600, 2560}, // turned
	} {
		if w, h := displaySize(c.lcdW, c.lcdH, c.imgW, c.imgH); w != c.w || h != c.h {
			t.Errorf("displaySize(%d, %d, %d, %d) = %d, %d, want %d, %d", c.lcdW, c.lcdH, c.imgW, c.imgH, w, h, c.w, c.h)
		}
	}
}

// fakeEmulator is an emulator's gRPC service: a 40×20 display (natural
// landscape) whose screenshots it sends from images.
type fakeEmulator struct {
	emupb.UnimplementedEmulatorControllerServer
	token  string
	images chan *emupb.Image

	mu   sync.Mutex
	asks []*emupb.ImageFormat
	open int // screenshot streams not ended
}

func (f *fakeEmulator) opened() int { f.mu.Lock(); defer f.mu.Unlock(); return f.open }

func (f *fakeEmulator) authorized(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	if a := md.Get("authorization"); len(a) != 1 || a[0] != "Bearer "+f.token {
		return status.Error(codes.Unauthenticated, "no token")
	}
	return nil
}

func (f *fakeEmulator) GetStatus(ctx context.Context, _ *emptypb.Empty) (*emupb.EmulatorStatus, error) {
	if err := f.authorized(ctx); err != nil {
		return nil, err
	}
	return &emupb.EmulatorStatus{HardwareConfig: &emupb.EntryList{Entry: []*emupb.Entry{
		{Key: "hw.lcd.width", Value: "40"}, {Key: "hw.lcd.height", Value: "20"},
	}}}, nil
}

func (f *fakeEmulator) StreamScreenshot(in *emupb.ImageFormat, s grpc.ServerStreamingServer[emupb.Image]) error {
	if err := f.authorized(s.Context()); err != nil {
		return err
	}
	f.mu.Lock()
	f.asks = append(f.asks, in)
	f.open++
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.open--; f.mu.Unlock() }()
	for {
		select {
		case <-s.Context().Done():
			return nil
		case img := <-f.images:
			if err := s.Send(img); err != nil {
				return err
			}
		}
	}
}

func image888(w, h uint32) *emupb.Image {
	return &emupb.Image{Format: &emupb.ImageFormat{Format: emupb.ImageFormat_RGB888, Width: w, Height: h}, Image: rgb888(int(w), int(h))}
}

// serve runs f on a loopback port.
func serve(t *testing.T, f emupb.EmulatorControllerServer) (port int, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	emupb.RegisterEmulatorControllerServer(srv, f)
	go srv.Serve(ln)
	t.Cleanup(srv.Stop)
	return ln.Addr().(*net.TCPAddr).Port, srv.Stop
}

// events records what a stream produces.
type events struct {
	frames chan hostscreen.Frame
	states chan wire.HostScreenState
}

func newEvents() *events {
	return &events{frames: make(chan hostscreen.Frame, 16), states: make(chan wire.HostScreenState, 4)}
}
func (e *events) Frame(f hostscreen.Frame)     { e.frames <- f }
func (e *events) State(s wire.HostScreenState) { e.states <- s }

func (e *events) frame(t *testing.T) hostscreen.Frame {
	t.Helper()
	select {
	case f := <-e.frames:
		return f
	case <-time.After(10 * time.Second):
		t.Fatal("no frame")
	}
	return hostscreen.Frame{}
}

func (e *events) state(t *testing.T) wire.HostScreenState {
	t.Helper()
	select {
	case s := <-e.states:
		return s
	case <-time.After(15 * time.Second):
		t.Fatal("no state")
	}
	return wire.HostScreenState{}
}

func TestStreamSendsHalfResolutionJPEGsAndEndsGone(t *testing.T) {
	f := &fakeEmulator{token: "tok", images: make(chan *emupb.Image, 4)}
	port, stop := serve(t, f)
	st, err := openStream(context.Background(), emulator{serialPort: 5554, avd: "Pixel", grpcPort: port, token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ev := newEvents()
	st.Start(ev)
	if s := ev.state(t); s.State != wire.HostScreenLive || s.Title != "Pixel" {
		t.Fatalf("first state %+v, want live with the AVD name", s)
	}
	f.images <- &emupb.Image{Format: &emupb.ImageFormat{}} // display off: no frame
	f.images <- image888(20, 10)
	fr := ev.frame(t)
	if fr.Width != 40 || fr.Height != 20 {
		t.Fatalf("frame %dx%d, want the display's 40x20", fr.Width, fr.Height)
	}
	data, _ := base64.StdEncoding.DecodeString(fr.Data)
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width != 20 || cfg.Height != 10 {
		t.Fatalf("JPEG %+v, %v; want 20x10", cfg, err)
	}
	f.mu.Lock()
	ask := f.asks[0]
	f.mu.Unlock()
	if ask.GetFormat() != emupb.ImageFormat_RGB888 || ask.GetWidth() != 20 || ask.GetHeight() != 20 {
		t.Fatalf("asked %v, want RGB888 bounded by 20x20 (half the longer side)", ask)
	}
	// Turned: the size follows the image.
	f.images <- image888(10, 20)
	if fr := ev.frame(t); fr.Width != 20 || fr.Height != 40 {
		t.Fatalf("turned frame %dx%d, want 20x40", fr.Width, fr.Height)
	}
	stop()
	if s := ev.state(t); s.State != wire.HostScreenGone {
		t.Fatalf("state after the emulator stopped: %+v, want gone", s)
	}
}

func TestStreamResumeStartsOver(t *testing.T) {
	f := &fakeEmulator{token: "tok", images: make(chan *emupb.Image, 4)}
	port, _ := serve(t, f)
	st, err := openStream(context.Background(), emulator{serialPort: 5554, grpcPort: port, token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ev := newEvents()
	st.Start(ev)
	ev.state(t)
	f.images <- image888(20, 10)
	ev.frame(t)
	st.Pause()
	st.Resume()
	// Resume opens another screenshot stream (the emulator's first image
	// of a stream is the whole display).
	deadline := time.Now().Add(10 * time.Second)
	for {
		f.mu.Lock()
		n := len(f.asks)
		f.mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Resume opened no stream")
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.images <- image888(20, 10)
	ev.frame(t)
}

func TestStreamDropsAPictureLikeTheLast(t *testing.T) {
	f := &fakeEmulator{token: "tok", images: make(chan *emupb.Image, 4)}
	port, _ := serve(t, f)
	st, err := openStream(context.Background(), emulator{serialPort: 5554, grpcPort: port, token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ev := newEvents()
	st.Start(ev)
	ev.state(t)
	f.images <- image888(20, 10)
	ev.frame(t)
	// The emulator sends about 20 a second, changed or not.
	f.images <- image888(20, 10)
	f.images <- image888(20, 10)
	select {
	case <-ev.frames:
		t.Fatal("sent a frame of the same picture")
	case <-time.After(300 * time.Millisecond):
	}
	green := image888(20, 10)
	for i := 1; i < len(green.Image); i += 3 {
		green.Image[i-1], green.Image[i], green.Image[i+1] = 0, 0xff, 0
	}
	f.images <- green
	data, _ := base64.StdEncoding.DecodeString(ev.frame(t).Data)
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if r, g, _, _ := img.At(10, 5).RGBA(); g>>8 < 0xc0 || r>>8 > 0x40 {
		t.Fatalf("the changed picture was not sent: pixel %d,%d", r>>8, g>>8)
	}
}

func TestOpenStreamErrors(t *testing.T) {
	f := &fakeEmulator{token: "tok", images: make(chan *emupb.Image)}
	port, _ := serve(t, f)
	var we *wire.Error
	_, err := openStream(context.Background(), emulator{serialPort: 5554, grpcPort: port, token: "wrong"})
	if !errors.As(err, &we) || we.Code != wire.ErrUnavailable {
		t.Fatalf("wrong token: %v, want unavailable", err)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	_, err = openStream(context.Background(), emulator{serialPort: 5554, grpcPort: closed})
	if !errors.As(err, &we) || we.Code != wire.ErrNotFound {
		t.Fatalf("no gRPC on the port: %v, want not_found", err)
	}
}

// Pause cuts the screenshot stream: the emulator sends nothing while
// paused; Resume opens another.
func TestStreamPauseEndsTheScreenshotStream(t *testing.T) {
	f := &fakeEmulator{token: "tok", images: make(chan *emupb.Image, 4)}
	port, _ := serve(t, f)
	st, err := openStream(context.Background(), emulator{serialPort: 5554, grpcPort: port, token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ev := newEvents()
	st.Start(ev)
	ev.state(t)
	f.images <- image888(20, 10)
	ev.frame(t)
	st.Pause()
	waitFor(t, "the screenshot stream ended", func() bool { return f.opened() == 0 })
	time.Sleep(100 * time.Millisecond)
	if n := f.opened(); n != 0 {
		t.Fatalf("%d screenshot streams open while paused", n)
	}
	st.Resume()
	waitFor(t, "Resume opened a screenshot stream", func() bool { return f.opened() == 1 })
	f.images <- image888(20, 10)
	ev.frame(t)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
