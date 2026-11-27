package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/version"
	"github.com/obutora/stagent/internal/wire"
)

// Conn stands for the host screen tab's own SSH connection: each Exec is
// one exec channel running a stagent command, its stdout through the down
// link and its stdin through the up link. All channels of a Conn share the
// two links, as channels of one SSH connection share its bandwidth.
type Conn struct {
	stagent string
	env     []string
	down    *Link
	up      *Link
	stderr  io.Writer

	mu   sync.Mutex
	cmds []*exec.Cmd
}

// Exec starts `stagent args…` as a channel would and returns its stdio as
// the app sees it.
func (c *Conn) Exec(args ...string) (io.ReadWriteCloser, error) {
	cmd := exec.Command(c.stagent, args...)
	cmd.Env = c.env
	cmd.Stderr = c.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.cmds = append(c.cmds, cmd)
	c.mu.Unlock()
	return channel{c.down.Reader(stdout), c.up.Writer(stdin)}, nil
}

type channel struct {
	io.Reader
	io.WriteCloser
}

// PIDs are the processes of the channels (for CPU sampling).
func (c *Conn) PIDs() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	pids := make([]int, 0, len(c.cmds))
	for _, cmd := range c.cmds {
		pids = append(pids, cmd.Process.Pid)
	}
	return pids
}

// Wait waits for every channel's process to exit (the transport closed
// their stdin), killing what is still running after d, and returns the CPU
// time they used over their whole life (user + system, as the OS reports
// it at exit: getrusage on unix, GetProcessTimes on Windows).
func (c *Conn) Wait(d time.Duration) time.Duration {
	c.mu.Lock()
	cmds := c.cmds
	c.mu.Unlock()
	var total time.Duration
	deadline := time.Now().Add(d)
	for _, cmd := range cmds {
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Until(deadline)):
			cmd.Process.Kill()
			<-done
		}
		total += cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()
	}
	return total
}

// Frame is one picture as the app receives it: a whole JPEG ("full"), or
// the JPEGs of Tiles one after the other, to go on top of frame Prev
// ("tiles", ImageWidth×ImageHeight px).
type Frame struct {
	Kind          string // wire.HostScreenRecordFull or wire.HostScreenRecordTiles
	Seq, Prev     int64
	JPEG          []byte
	Width, Height int // target coordinates
	// ImageWidth, ImageHeight: the picture's px, when the record says.
	ImageWidth, ImageHeight int
	Tiles                   []wire.HostScreenTile
}

// Transport is how the app side talks to stagent over a Conn. It acks
// every frame the moment it arrives, as the app does
// (HostScreenController._onFrame), then hands it to the sink.
//
// "json" is the path before #1974: everything on the bridge's
// stdin/stdout, frames as host_screen.frame with base64 JPEG. "binary"
// (binary.go) is the app since #1974: frames as records on a second exec
// channel, tiles included.
type Transport interface {
	// Version is stagent's, from hello.
	Version() string
	List(ctx context.Context, p wire.HostScreenListParams) (hostScreenList, error)
	Start(ctx context.Context, t wire.HostScreenTarget) error
	// Input sends an input without waiting for its answer, as the app does.
	Input(in wire.HostScreenInput)
	Stop(ctx context.Context) error
	// Close ends the channels (stdin EOF: the bridge exits).
	Close()
}

// openTransport opens a transport over c; sink receives every frame.
type openTransport func(ctx context.Context, c *Conn, sink func(Frame)) (Transport, error)

var transports = map[string]openTransport{
	"json":   openJSON,
	"binary": openBinary,
}

// hostScreenList is host_screen.list's result.
type hostScreenList struct {
	Browser *wire.HostScreenSection[wire.HostScreenBrowserItem]   `json:"browser"`
	Android *wire.HostScreenSection[wire.HostScreenEmulatorItem]  `json:"android"`
	IOS     *wire.HostScreenSection[wire.HostScreenSimulatorItem] `json:"ios"`
}

type jsonTransport struct {
	client  *rpc.Client
	version string
}

func openJSON(ctx context.Context, c *Conn, sink func(Frame)) (Transport, error) {
	rw, err := c.Exec("bridge")
	if err != nil {
		return nil, err
	}
	t := &jsonTransport{}
	t.client = rpc.NewClient(rw, func(m *wire.Msg) {
		switch m.Method {
		case wire.NotifyHostScreenFrame:
			var f wire.HostScreenFrame
			if json.Unmarshal(m.Params, &f) != nil {
				return
			}
			ack, _ := json.Marshal(wire.HostScreenAckParams{Seq: f.Seq})
			t.client.Go(wire.MethodHostScreenAck, ack, func(*wire.Msg) {})
			jpeg, err := base64.StdEncoding.DecodeString(f.Data)
			if err != nil {
				return
			}
			sink(Frame{Kind: wire.HostScreenRecordFull, Seq: f.Seq, JPEG: jpeg, Width: f.Width, Height: f.Height})
		case wire.NotifyHostScreenState:
			logState(m)
		}
	})
	var hello wire.HelloResult
	if err := t.client.Call(ctx, wire.MethodHello, wire.HelloParams{Protocol: version.Protocol, Client: "hostscreen-bench"}, &hello); err != nil {
		t.client.Close()
		return nil, fmt.Errorf("hello: %w", err)
	}
	t.version = hello.Version
	return t, nil
}

// logState shows a host_screen.state other than live.
func logState(m *wire.Msg) {
	var s wire.HostScreenState
	if json.Unmarshal(m.Params, &s) == nil && s.State != wire.HostScreenLive {
		fmt.Fprintln(os.Stderr, "  host_screen.state:", s.State)
	}
}

func (t *jsonTransport) Version() string { return t.version }

func (t *jsonTransport) List(ctx context.Context, p wire.HostScreenListParams) (hostScreenList, error) {
	var l hostScreenList
	err := t.client.Call(ctx, wire.MethodHostScreenList, p, &l)
	return l, err
}

func (t *jsonTransport) Start(ctx context.Context, target wire.HostScreenTarget) error {
	return t.client.Call(ctx, wire.MethodHostScreenStart, wire.HostScreenStartParams{Target: target}, nil)
}

func (t *jsonTransport) Input(in wire.HostScreenInput) {
	p, _ := json.Marshal(in)
	t.client.Go(wire.MethodHostScreenInput, p, func(m *wire.Msg) {
		if m != nil && m.Error != nil {
			fmt.Fprintf(os.Stderr, "  host_screen.input %s: %v\n", in.Type, m.Error)
		}
	})
}

func (t *jsonTransport) Stop(ctx context.Context) error {
	return t.client.Call(ctx, wire.MethodHostScreenStop, nil, nil)
}

func (t *jsonTransport) Close() { t.client.Close() }
