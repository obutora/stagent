package android

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/obutora/stagent/internal/hostscreen"
	"github.com/obutora/stagent/internal/hostscreen/android/emupb"
	"github.com/obutora/stagent/internal/wire"
)

const (
	// jpegQuality of the frames (#624).
	jpegQuality = 70
	// callWait bounds one unary gRPC call to the emulator.
	callWait = 5 * time.Second
	// retryWait spaces reopening a screenshot stream the emulator ended
	// while it still answers.
	retryWait = 500 * time.Millisecond
	// maxRecv is the largest message accepted from the emulator: a full
	// resolution RGB888 screenshot of a large display (gRPC's default is
	// 4 MiB).
	maxRecv = 64 << 20
)

// stream shows one emulator: its gRPC streamScreenshot at half the
// display's resolution in RGB888, encoded to JPEG. The newest image waits
// for the encoder; older ones are dropped.
type stream struct {
	cc     *grpc.ClientConn
	client emupb.EmulatorControllerClient
	avd    string
	// lcdW, lcdH: the display in px (in its natural orientation).
	lcdW, lcdH int
	// serial is adb's name of the emulator; adb runs adb with args (nil:
	// no adb, so no keys or text).
	serial string
	adb    func(ctx context.Context, args ...string) ([]byte, error)

	ctx    context.Context // ends with Close or the emulator
	cancel context.CancelFunc
	wg     sync.WaitGroup

	ev      atomic.Pointer[hostscreen.Events] // set by Start
	closing atomic.Bool
	gone    sync.Once

	mu     sync.Mutex
	paused bool
	stop   context.CancelFunc // ends the current streamScreenshot
	latest *emupb.Image       // newest image not yet encoded
	// rot: how the last frame showed the display turned (inputs map
	// through it).
	rot emupb.Rotation_SkinRotation

	kick  chan struct{} // wakes capture after Resume
	ready chan struct{} // wakes encode for latest
}

// bearer sends the emulator's grpc.token with every call.
type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}

func (bearer) RequireTransportSecurity() bool { return false }

// openStream connects to e's gRPC port on the loopback: [::1] first, then
// 127.0.0.1 (emulator 37 on macOS listens there only). It reads the
// display's size before answering.
func openStream(ctx context.Context, e emulator) (*stream, error) {
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxRecv)),
	}
	if e.token != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(bearer(e.token)))
	}
	var lastErr error
	for _, host := range []string{"[::1]", "127.0.0.1"} {
		cc, err := grpc.NewClient("passthrough:///"+host+":"+strconv.Itoa(e.grpcPort), opts...)
		if err != nil {
			return nil, wire.Errorf(wire.ErrInternal, "host_screen.start: %v", err)
		}
		s := &stream{cc: cc, client: emupb.NewEmulatorControllerClient(cc), avd: e.avd}
		err = s.readSize(ctx)
		if err == nil {
			s.ctx, s.cancel = context.WithCancel(context.Background())
			s.kick, s.ready = make(chan struct{}, 1), make(chan struct{}, 1)
			return s, nil
		}
		cc.Close()
		if status.Code(err) != codes.Unavailable && status.Code(err) != codes.DeadlineExceeded {
			return nil, wire.Errorf(wire.ErrUnavailable, "host_screen.start: %s: %v", e.serial(), err)
		}
		lastErr = err
	}
	return nil, wire.Errorf(wire.ErrNotFound, "host_screen.start: %s: no gRPC on port %d: %v", e.serial(), e.grpcPort, lastErr)
}

// readSize learns the display's size: from the hardware configuration,
// else from one full-resolution screenshot.
func (s *stream) readSize(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, callWait)
	defer cancel()
	st, err := s.client.GetStatus(ctx, &emptypb.Empty{})
	if err != nil {
		return err
	}
	cfg := st.GetPlatformConfig()
	for _, en := range st.GetHardwareConfig().GetEntry() {
		if _, ok := cfg[en.GetKey()]; !ok {
			if cfg == nil {
				cfg = map[string]string{}
			}
			cfg[en.GetKey()] = en.GetValue()
		}
	}
	s.lcdW, _ = strconv.Atoi(cfg["hw.lcd.width"])
	s.lcdH, _ = strconv.Atoi(cfg["hw.lcd.height"])
	if s.lcdW > 0 && s.lcdH > 0 {
		return nil
	}
	img, err := s.client.GetScreenshot(ctx, &emupb.ImageFormat{Format: emupb.ImageFormat_RGB888})
	if err != nil {
		return err
	}
	s.lcdW, s.lcdH = int(img.GetFormat().GetWidth()), int(img.GetFormat().GetHeight())
	if s.lcdW <= 0 || s.lcdH <= 0 {
		return status.Error(codes.FailedPrecondition, "the display has no size")
	}
	return nil
}

func (s *stream) Start(ev hostscreen.Events) {
	s.ev.Store(&ev)
	ev.State(wire.HostScreenState{State: wire.HostScreenLive, Title: s.avd})
	s.wg.Go(s.capture)
	s.wg.Go(s.encode)
}

// Pause ends the screenshot stream; Resume opens another, whose first
// image is the whole display.
func (s *stream) Pause() {
	s.mu.Lock()
	s.paused = true
	s.latest = nil
	stop := s.stop
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
}

func (s *stream) Resume() {
	s.mu.Lock()
	s.paused = false
	stop := s.stop
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
	poke(s.kick)
}

func (s *stream) Close() {
	s.closing.Store(true)
	s.cancel()
	s.wg.Wait()
	s.cc.Close()
}

func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// end reports the emulator gone, once, unless the stream is being closed.
func (s *stream) end() {
	s.gone.Do(func() {
		s.cancel()
		if s.closing.Load() {
			return
		}
		if ev := s.ev.Load(); ev != nil {
			(*ev).State(wire.HostScreenState{State: wire.HostScreenGone})
		}
	})
}

// capture keeps one streamScreenshot open while not paused. When the
// emulator ends it, the stream ends gone unless the emulator still
// answers.
func (s *stream) capture() {
	for s.ctx.Err() == nil {
		s.mu.Lock()
		paused := s.paused
		s.mu.Unlock()
		if paused {
			select {
			case <-s.ctx.Done():
			case <-s.kick:
			}
			continue
		}
		err := s.receive()
		if s.ctx.Err() != nil || err == nil {
			continue
		}
		if !s.alive() {
			s.end()
			return
		}
		select {
		case <-s.ctx.Done():
		case <-time.After(retryWait):
		}
	}
}

// receive runs one streamScreenshot until Pause or Resume (nil) or its
// end (the error).
func (s *stream) receive() error {
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	s.mu.Lock()
	if s.paused {
		s.mu.Unlock()
		return nil
	}
	s.stop = cancel
	s.mu.Unlock()
	// Width and height both bound the image (the aspect ratio is kept):
	// half the longer side gives half the resolution in either
	// orientation.
	half := uint32((max(s.lcdW, s.lcdH) + 1) / 2)
	st, err := s.client.StreamScreenshot(ctx, &emupb.ImageFormat{Format: emupb.ImageFormat_RGB888, Width: half, Height: half})
	for err == nil {
		var img *emupb.Image
		if img, err = st.Recv(); err != nil {
			break
		}
		// An empty image: the display is off.
		if img.GetFormat().GetWidth() == 0 || img.GetFormat().GetHeight() == 0 {
			continue
		}
		s.mu.Lock()
		s.latest = img
		s.mu.Unlock()
		poke(s.ready)
	}
	if ctx.Err() != nil {
		return nil
	}
	if err == nil {
		err = errors.New("stream ended")
	}
	return err
}

// alive tells whether the emulator still answers.
func (s *stream) alive() bool {
	ctx, cancel := context.WithTimeout(s.ctx, callWait)
	defer cancel()
	_, err := s.client.GetStatus(ctx, &emptypb.Empty{})
	return err == nil
}

// encode turns the newest image into a frame whenever there is one.
func (s *stream) encode() {
	var rgba *image.RGBA
	var buf bytes.Buffer
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.ready:
		}
		s.mu.Lock()
		img := s.latest
		s.latest = nil
		s.mu.Unlock()
		if img == nil {
			continue
		}
		w, h := int(img.GetFormat().GetWidth()), int(img.GetFormat().GetHeight())
		var err error
		if rgba, err = encodeRGB888(&buf, rgba, img.GetImage(), w, h); err != nil {
			continue
		}
		dw, dh := displaySize(s.lcdW, s.lcdH, w, h)
		s.mu.Lock()
		s.rot = img.GetFormat().GetRotation().GetRotation()
		s.mu.Unlock()
		if ev := s.ev.Load(); ev != nil && s.ctx.Err() == nil {
			(*ev).Frame(hostscreen.Frame{Data: base64.StdEncoding.EncodeToString(buf.Bytes()), Width: dw, Height: dh})
		}
	}
}

// encodeRGB888 writes pix (w×h, 3 bytes a pixel, rows top down) to buf as
// a JPEG. rgba is reused when it has the size (else one is made), and
// returned for the next image.
func encodeRGB888(buf *bytes.Buffer, rgba *image.RGBA, pix []byte, w, h int) (*image.RGBA, error) {
	if w <= 0 || h <= 0 || len(pix) < w*h*3 {
		return rgba, fmt.Errorf("RGB888 image of %d bytes is not %dx%d", len(pix), w, h)
	}
	if rgba == nil || rgba.Rect.Dx() != w || rgba.Rect.Dy() != h {
		rgba = image.NewRGBA(image.Rect(0, 0, w, h))
	}
	dst := rgba.Pix
	for i, j := 0, 0; j < w*h*3; i, j = i+4, j+3 {
		dst[i], dst[i+1], dst[i+2], dst[i+3] = pix[j], pix[j+1], pix[j+2], 0xff
	}
	buf.Reset()
	return rgba, jpeg.Encode(buf, rgba, &jpeg.Options{Quality: jpegQuality})
}

// displaySize is the display in px as the image shows it: the natural
// size, turned when the image's orientation differs (the emulator
// rotates the image with the device).
func displaySize(lcdW, lcdH, imgW, imgH int) (int, int) {
	if (imgW > imgH) != (lcdW > lcdH) {
		return lcdH, lcdW
	}
	return lcdW, lcdH
}
