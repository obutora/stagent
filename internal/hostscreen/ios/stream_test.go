package ios

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/jpeg"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/obutora/stagent/internal/hostscreen"
	"github.com/obutora/stagent/internal/hostscreen/ios/idbpb"
	"github.com/obutora/stagent/internal/wire"
)

// envFakeCompanion makes the test binary an idb_companion: its value is
// what the fake does after its first frame ("turn": send a landscape one,
// "exit": exit as for a simulator shut down, "still": nothing), or "die":
// exit at once.
const envFakeCompanion = "STAGENT_TEST_FAKE_COMPANION"

// envFakeVideo names the file the fake companion writes "open" and
// "close" to, one line each, as video streams begin and end.
const envFakeVideo = "STAGENT_TEST_FAKE_VIDEO"

func TestMain(m *testing.M) {
	if mode := os.Getenv(envFakeCompanion); mode != "" {
		fakeCompanion(mode, os.Args[1:])
		return
	}
	os.Exit(m.Run())
}

func testJPEG(w, h int) []byte {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewGray(image.Rect(0, 0, w, h)), nil); err != nil {
		panic(err)
	}
	return b.Bytes()
}

var portrait, landscape = testJPEG(20, 40), testJPEG(40, 20)

func fakeCompanion(mode string, args []string) {
	if mode == "die" {
		os.Stderr.WriteString("simulator U1 is not booted\n")
		os.Exit(1)
	}
	var sock string
	idle := time.Duration(0)
	for i, a := range args {
		if i+1 >= len(args) {
			break
		}
		switch a {
		case "--grpc-domain-sock":
			sock = args[i+1]
		case "--idle-shutdown-time":
			n, _ := strconv.Atoi(args[i+1])
			idle = time.Duration(n) * time.Second
		}
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		os.Exit(2)
	}
	// Like idb_companion, it ends idle seconds after its last request
	// ended.
	var mu sync.Mutex
	active, since := 0, time.Now()
	track := func() func() {
		mu.Lock()
		active++
		mu.Unlock()
		return func() {
			mu.Lock()
			active--
			since = time.Now()
			mu.Unlock()
		}
	}
	if idle > 0 {
		go func() {
			for range time.Tick(100 * time.Millisecond) {
				mu.Lock()
				out := active == 0 && time.Since(since) > idle
				mu.Unlock()
				if out {
					os.Exit(0)
				}
			}
		}()
	}
	s := grpc.NewServer(
		grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			defer track()()
			return h(ctx, req)
		}),
		grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			defer track()()
			return h(srv, ss)
		}),
	)
	idbpb.RegisterCompanionServiceServer(s, &fakeService{mode: mode})
	s.Serve(l)
}

type fakeService struct {
	idbpb.UnimplementedCompanionServiceServer
	mode string
}

func (f *fakeService) Describe(context.Context, *idbpb.TargetDescriptionRequest) (*idbpb.TargetDescriptionResponse, error) {
	return &idbpb.TargetDescriptionResponse{TargetDescription: &idbpb.TargetDescription{
		Udid: "U1", Name: "iPhone 17", State: "Booted",
		ScreenDimensions: &idbpb.ScreenDimensions{Width: 1206, Height: 2622, Density: 3, WidthPoints: 402, HeightPoints: 874},
	}}, nil
}

func (f *fakeService) VideoStream(vs grpc.BidiStreamingServer[idbpb.VideoStreamRequest, idbpb.VideoStreamResponse]) error {
	req, err := vs.Recv()
	if err != nil {
		return err
	}
	if st := req.GetStart(); st == nil || st.Format != idbpb.VideoStreamRequest_MJPEG {
		return errors.New("not an MJPEG start")
	}
	videoLog("open")
	defer videoLog("close")
	frame := func(b []byte) *idbpb.VideoStreamResponse {
		return &idbpb.VideoStreamResponse{Output: &idbpb.VideoStreamResponse_Payload{Payload: &idbpb.Payload{Source: &idbpb.Payload_Data{Data: b}}}}
	}
	vs.Send(&idbpb.VideoStreamResponse{Output: &idbpb.VideoStreamResponse_LogOutput{LogOutput: []byte("starting")}})
	vs.Send(frame(portrait))
	select {
	case <-time.After(300 * time.Millisecond):
	case <-vs.Context().Done():
		return nil
	}
	switch f.mode {
	case "turn":
		vs.Send(frame(landscape))
	case "exit":
		os.Exit(0)
	}
	<-vs.Context().Done()
	return nil
}

func videoLog(what string) {
	path := os.Getenv(envFakeVideo)
	if path == "" {
		return
	}
	if out, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600); err == nil {
		out.WriteString(what + "\n")
		out.Close()
	}
}

// events records what a stream sends.
type events struct{ c chan any }

func (e events) Frame(f hostscreen.Frame)     { e.c <- f }
func (e events) State(s wire.HostScreenState) { e.c <- s }
func (e events) next(t *testing.T, d time.Duration) any {
	t.Helper()
	select {
	case v := <-e.c:
		return v
	case <-time.After(d):
		return nil
	}
}

// openFake opens the stream of simulator U1 shown by a fake companion in
// mode, run with env besides.
func openFake(t *testing.T, mode string, env ...string) (*stream, events) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	k := testKind(t, fakeTools{list: bootedJSON}, []string{"xcrun", "idb_companion"}, self, append(env, envFakeCompanion+"="+mode)...)
	kenv := k.env()
	s, err := connect(context.Background(), lookPath(kenv, "idb_companion"), kenv, &simulator{UDID: "U1", Name: "iPhone 17"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ev := events{c: make(chan any, 16)}
	s.Start(ev)
	if got := ev.next(t, 5*time.Second); got != (wire.HostScreenState{State: wire.HostScreenLive, Title: "iPhone 17"}) {
		t.Fatalf("first event %#v, want live with the simulator's name", got)
	}
	want := hostscreen.Frame{Data: base64.StdEncoding.EncodeToString(portrait), Width: 402, Height: 874}
	if got := ev.next(t, 5*time.Second); got != want {
		t.Fatalf("first frame %#v, want the payload in points", got)
	}
	return s, ev
}

// stopped tells whether the stream's companion was waited for and its
// socket's directory removed.
func stopped(s *stream) bool {
	select {
	case <-s.comp.exited:
	default:
		return false
	}
	_, err := os.Stat(s.comp.dir)
	return errors.Is(err, os.ErrNotExist)
}

// videoLines waits until the fake companion wrote want to the video log.
func videoLines(t *testing.T, log string, want ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(log)
		got := strings.Fields(string(b))
		if slices.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("video streams %q, want %q", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Pause closes the MJPEG stream but keeps the companion, past its idle
// shutdown; Resume sends the newest frame at once and opens another.
func TestPauseClosesTheVideoStreamButKeepsTheCompanion(t *testing.T) {
	log := filepath.Join(t.TempDir(), "video")
	s, ev := openFake(t, "still", envFakeVideo+"="+log)
	s.Pause()
	videoLines(t, log, "open", "close")
	if got := ev.next(t, (idleShutdown+1)*time.Second); got != nil {
		t.Fatalf("paused stream sent %#v", got)
	}
	select {
	case <-s.comp.exited:
		t.Fatal("the companion ended while paused")
	default:
	}
	s.Resume()
	want := hostscreen.Frame{Data: base64.StdEncoding.EncodeToString(portrait), Width: 402, Height: 874}
	if got := ev.next(t, time.Second); got != want {
		t.Fatalf("after Resume %#v, want the newest frame", got)
	}
	videoLines(t, log, "open", "close", "open")
	// The new video stream's own first frame.
	if got := ev.next(t, 5*time.Second); got != want {
		t.Fatalf("from the new video stream %#v, want %#v", got, want)
	}
	s.Close()
	if !stopped(s) {
		t.Fatal("companion still running after Close")
	}
	if got := ev.next(t, 200*time.Millisecond); got != nil {
		t.Fatalf("event after Close: %#v", got)
	}
}

// A simulator shut down while paused ends the stream gone.
func TestPausedStreamIsGoneWhenTheCompanionExits(t *testing.T) {
	s, ev := openFake(t, "still")
	s.Pause()
	s.comp.cmd.Process.Kill()
	if got := ev.next(t, 5*time.Second); got != (wire.HostScreenState{State: wire.HostScreenGone}) {
		t.Fatalf("after the companion exited %#v, want gone", got)
	}
}

func TestStreamIsGoneWhenTheCompanionExits(t *testing.T) {
	s, ev := openFake(t, "exit")
	if got := ev.next(t, 5*time.Second); got != (wire.HostScreenState{State: wire.HostScreenGone}) {
		t.Fatalf("after the companion exited %#v, want gone", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !stopped(s) {
		if time.Now().After(deadline) {
			t.Fatal("companion's directory left after gone")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestOpenFailsWhenTheCompanionExits(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	k := testKind(t, fakeTools{list: bootedJSON}, []string{"xcrun", "idb_companion"}, self, envFakeCompanion+"=die")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp) // where the companion's directory is made
	_, err = k.Open(context.Background(), wire.HostScreenTarget{Kind: wire.HostScreenIOS, UDID: "U1"})
	var we *wire.Error
	if !errors.As(err, &we) || we.Code != wire.ErrUnavailable || !strings.Contains(we.Message, "not booted") {
		t.Fatalf("Open = %v, want unavailable with the companion's output", err)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Fatalf("Open left %v", left)
	}
}
