package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"

	"github.com/obutora/stagent/internal/hostscreen"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/version"
	"github.com/obutora/stagent/internal/wire"
)

// binaryTransport is the app since #1974: hello announces the binary frame
// channel and tiles, then a second exec (`stagent host-screen-channel`) on
// the same Conn carries the frames as records (PROTOCOL.md "Binary frame
// channel", "Tiles"). Both channels' bytes go over the Conn's links.
// Frames sent as host_screen.frame before the channel is up (or after it
// drops) still arrive on the bridge.
type binaryTransport struct {
	jsonTransport
	gate *frameGate
	ch   io.ReadWriteCloser
	done chan struct{}
}

func openBinary(ctx context.Context, c *Conn, sink func(Frame)) (Transport, error) {
	rw, err := c.Exec("bridge")
	if err != nil {
		return nil, err
	}
	t := &binaryTransport{done: make(chan struct{})}
	t.gate = &frameGate{
		ack: func(seq int64) {
			ack, _ := json.Marshal(wire.HostScreenAckParams{Seq: seq})
			t.client.Go(wire.MethodHostScreenAck, ack, func(*wire.Msg) {})
		},
		keyframe: func() { t.client.Go(wire.MethodHostScreenKeyframe, nil, func(*wire.Msg) {}) },
		sink:     sink,
	}
	t.client = rpc.NewClient(rw, func(m *wire.Msg) {
		switch m.Method {
		case wire.NotifyHostScreenFrame:
			var f wire.HostScreenFrame
			if json.Unmarshal(m.Params, &f) != nil {
				return
			}
			jpeg, err := base64.StdEncoding.DecodeString(f.Data)
			if err != nil {
				return
			}
			t.gate.offer(Frame{Kind: wire.HostScreenRecordFull, Seq: f.Seq, JPEG: jpeg, Width: f.Width, Height: f.Height})
		case wire.NotifyHostScreenState:
			logState(m)
		}
	})
	var hello wire.HelloResult
	caps := []string{wire.CapHostScreenBinary, wire.CapHostScreenTiles}
	if err := t.client.Call(ctx, wire.MethodHello, wire.HelloParams{Protocol: version.Protocol, Client: "hostscreen-bench", Capabilities: caps}, &hello); err != nil {
		t.client.Close()
		return nil, fmt.Errorf("hello: %w", err)
	}
	t.version = hello.Version
	if !slices.Contains(hello.Capabilities, wire.CapHostScreenBinary) {
		t.client.Close()
		return nil, fmt.Errorf("stagent %s has no %s", hello.Version, wire.CapHostScreenBinary)
	}
	// As the app: the channel is opened right after hello, before start.
	var tok wire.HostScreenChannelResult
	if err := t.client.Call(ctx, wire.MethodHostScreenChannel, nil, &tok); err != nil {
		t.client.Close()
		return nil, fmt.Errorf("host_screen.channel: %w", err)
	}
	if t.ch, err = c.Exec("host-screen-channel"); err != nil {
		t.client.Close()
		return nil, err
	}
	if _, err := io.WriteString(t.ch, tok.Token+"\n"); err != nil {
		t.Close()
		return nil, err
	}
	go func() {
		defer close(t.done)
		if err := readRecords(t.ch, t.gate); !errors.Is(err, io.EOF) {
			fmt.Fprintln(os.Stderr, "  binary frame channel:", err)
		}
	}()
	return t, nil
}

func (t *binaryTransport) Close() {
	t.ch.Close()
	t.client.Close()
}

// readRecords reads the binary frame channel until it ends and offers each
// full or tiles record to g. Kinds it does not know are skipped, as the
// app does.
func readRecords(r io.Reader, g *frameGate) error {
	for {
		h, payload, err := hostscreen.ReadRecord(r)
		if err != nil {
			return err
		}
		switch h.Kind {
		case wire.HostScreenRecordFull, wire.HostScreenRecordTiles:
			g.offer(Frame{Kind: h.Kind, Seq: h.Seq, Prev: h.Prev, JPEG: payload, Width: h.Width, Height: h.Height,
				ImageWidth: h.ImageWidth, ImageHeight: h.ImageHeight, Tiles: h.Tiles})
		}
	}
}

// frameGate is what the app's HostScreenBridge and HostScreenController do
// with a frame from either path: ack it on arrival; show it only if it is
// newer than every frame so far (a host_screen.frame overtaken at the
// switch to the channel is acked, not shown); show tiles only on top of
// the frame shown last, else drop them and ask for a keyframe once until a
// full frame comes.
type frameGate struct {
	ack      func(seq int64)
	keyframe func()
	sink     func(Frame)

	mu            sync.Mutex
	newest        int64 // the newest seq that arrived
	shown         *Frame
	keyframeAsked bool
}

func (g *frameGate) offer(f Frame) {
	g.ack(f.Seq)
	g.mu.Lock()
	defer g.mu.Unlock()
	if f.Seq <= g.newest {
		return
	}
	g.newest = f.Seq
	if f.Kind == wire.HostScreenRecordTiles {
		s := g.shown
		if s == nil || s.Seq != f.Prev || s.Width != f.Width || s.Height != f.Height ||
			s.ImageWidth != f.ImageWidth || s.ImageHeight != f.ImageHeight {
			if !g.keyframeAsked {
				g.keyframeAsked = true
				g.keyframe()
			}
			return
		}
	} else {
		g.keyframeAsked = false
	}
	g.shown = &f
	g.sink(f)
}
