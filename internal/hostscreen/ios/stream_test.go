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
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/obutora/stagent/internal/hostscreen"
	"github.com/obutora/stagent/internal/hostscreen/ios/idbpb"
	"github.com/obutora/stagent/internal/wire"
)

// envFakeCompanion makes the test binary an idb_companion: its value is
// what the fake does after its first frame ("turn": send a landscape one,
// "exit": exit as for a simulator shut down), or "die": exit at once.
const envFakeCompanion = "STAGENT_TEST_FAKE_COMPANION"

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
	for i, a := range args {
		if a == "--grpc-domain-sock" && i+1 < len(args) {
			sock = args[i+1]
		}
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		os.Exit(2)
	}
	s := grpc.NewServer()
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
	frame := func(b []byte) *idbpb.VideoStreamResponse {
		return &idbpb.VideoStreamResponse{Output: &idbpb.VideoStreamResponse_Payload{Payload: &idbpb.Payload{Source: &idbpb.Payload_Data{Data: b}}}}
	}
	vs.Send(&idbpb.VideoStreamResponse{Output: &idbpb.VideoStreamResponse_LogOutput{LogOutput: []byte("starting")}})
	vs.Send(frame(portrait))
	time.Sleep(300 * time.Millisecond)
	switch f.mode {
	case "turn":
		vs.Send(frame(landscape))
	case "exit":
		os.Exit(0)
	}
	<-vs.Context().Done()
	return nil
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

// openFake opens simulator U1 shown by a fake companion in mode, run with
// env besides.
func openFake(t *testing.T, mode string, env ...string) (*stream, events) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	k := testKind(t, fakeTools{list: bootedJSON}, []string{"xcrun", "idb_companion"}, self, append(env, envFakeCompanion+"="+mode)...)
	st, err := k.Open(context.Background(), wire.HostScreenTarget{Kind: wire.HostScreenIOS, UDID: "U1"})
	if err != nil {
		t.Fatal(err)
	}
	s := st.(*stream)
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

func TestStreamSendsTheNewestFrameAfterAStall(t *testing.T) {
	s, ev := openFake(t, "turn")
	s.Pause()
	if got := ev.next(t, time.Second); got != nil {
		t.Fatalf("paused stream sent %#v", got)
	}
	// The screen stays still: Resume sends the frame received meanwhile.
	s.Resume()
	want := hostscreen.Frame{Data: base64.StdEncoding.EncodeToString(landscape), Width: 874, Height: 402}
	if got := ev.next(t, 5*time.Second); got != want {
		t.Fatalf("after Resume %#v, want the landscape frame in points", got)
	}
	s.Resume()
	if got := ev.next(t, 5*time.Second); got != want {
		t.Fatalf("after another Resume %#v, want the same frame again", got)
	}
	s.Close()
	if !stopped(s) {
		t.Fatal("companion still running after Close")
	}
	if got := ev.next(t, 200*time.Millisecond); got != nil {
		t.Fatalf("event after Close: %#v", got)
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
