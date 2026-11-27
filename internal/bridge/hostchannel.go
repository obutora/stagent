package bridge

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/obutora/stagent/internal/ipc"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/wire"
)

// The binary frame channel of host_screen (CapHostScreenBinary): the app
// runs `stagent host-screen-channel` in a second exec channel of the
// tab's SSH connection and writes it the token host_screen.channel
// handed out; that process dials this bridge's channel listener, a
// same-user socket (named pipe on Windows) in the run directory named by
// the token's first channelIDLen chars, and copies what the bridge writes
// there (hostscreen.WriteRecord records) to its stdout, raw. The token's
// secret, known to this bridge and its app only, keeps the user's other
// sessions from taking the frames; the same-user socket keeps other users
// out. A token opens one channel, within channelTokenTTL, and the next
// host_screen.channel replaces it.

const (
	// channelIDLen and channelTokenLen: a token is the listener's id then
	// a 128-bit secret, in lowercase hex.
	channelIDLen    = 16
	channelTokenLen = channelIDLen + 32
	// channelTokenTTL is how long a token opens a channel after
	// host_screen.channel handed it out: the app asks for one right before
	// it opens the second exec channel, which takes an SSH round trip and
	// a stagent start (seconds on a slow Windows host); unused longer, a
	// token that leaked (a log, a crash report) opens nothing.
	channelTokenTTL = 10 * time.Second
	// channelTokenWait bounds how long a connection may take to send the
	// token.
	channelTokenWait = 5 * time.Second
	// channelDialWait bounds host-screen-channel's dial of the bridge.
	channelDialWait = 2 * time.Second
)

// hostChannel is the bridge's channel listener, started by the first
// host_screen.channel.
type hostChannel struct {
	mu    sync.Mutex
	ln    net.Listener
	id    string
	token string // the token a connection may present; "" once used
	// issued is when token was handed out; now is the clock (replaceable
	// in tests).
	issued time.Time
	now    func() time.Time
	conns  map[net.Conn]bool
}

func randomHex(n int) (string, error) {
	b := make([]byte, n/2)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// hostScreenChannel answers host_screen.channel: a new token, which
// replaces the one before.
func (b *Bridge) hostScreenChannel(m *wire.Msg) {
	if !b.appBinary.Load() {
		b.replyResult(m.ID, nil, wire.Errorf(wire.ErrUnsupported, "%s: the app did not announce %s", m.Method, wire.CapHostScreenBinary))
		return
	}
	tok, err := b.newChannelToken()
	if err != nil {
		b.replyResult(m.ID, nil, wire.Errorf(wire.ErrUnavailable, "%s: %v", m.Method, err))
		return
	}
	b.replyResult(m.ID, wire.HostScreenChannelResult{Token: tok}, nil)
}

func (b *Bridge) newChannelToken() (string, error) {
	hc := &b.hostChan
	hc.mu.Lock()
	defer hc.mu.Unlock()
	if hc.ln == nil {
		if err := b.listenChannel(); err != nil {
			return "", err
		}
	}
	secret, err := randomHex(channelTokenLen - channelIDLen)
	if err != nil {
		return "", err
	}
	hc.token, hc.issued = hc.id+secret, hc.now()
	return hc.token, nil
}

// listenChannel starts the listener (under hostChan.mu); it closes, with
// the connections it accepted, when the bridge ends.
func (b *Bridge) listenChannel() error {
	if b.ctx.Err() != nil {
		return errors.New("the bridge is ending")
	}
	if err := b.l.EnsureDirs(); err != nil {
		return err
	}
	id, err := randomHex(channelIDLen)
	if err != nil {
		return err
	}
	ln, err := ipc.Listen(b.l.ChannelAddr(id))
	if err != nil {
		return err
	}
	hc := &b.hostChan
	hc.ln, hc.id, hc.conns = ln, id, map[net.Conn]bool{}
	context.AfterFunc(b.ctx, func() {
		hc.mu.Lock()
		defer hc.mu.Unlock()
		ln.Close()
		for c := range hc.conns {
			c.Close()
		}
	})
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			hc.mu.Lock()
			if b.ctx.Err() != nil {
				hc.mu.Unlock()
				c.Close()
				return
			}
			hc.conns[c] = true
			hc.mu.Unlock()
			b.wg.Add(1)
			go func() {
				defer b.wg.Done()
				b.serveChannel(c)
				hc.mu.Lock()
				delete(hc.conns, c)
				hc.mu.Unlock()
				c.Close()
			}()
		}
	}()
	return nil
}

// serveChannel attaches c as the frame channel once it sent the current
// token, until c ends; c is closed unused otherwise.
func (b *Bridge) serveChannel(c net.Conn) {
	c.SetReadDeadline(time.Now().Add(channelTokenWait))
	got, err := readToken(c)
	if err != nil {
		return
	}
	hc := &b.hostChan
	hc.mu.Lock()
	ok := hc.token != "" && hc.now().Sub(hc.issued) <= channelTokenTTL &&
		subtle.ConstantTimeCompare([]byte(got), []byte(hc.token)) == 1
	if ok {
		hc.token = ""
	}
	hc.mu.Unlock()
	if !ok {
		return
	}
	c.SetReadDeadline(time.Time{})
	detach := b.hostScreen.Attach(c)
	defer detach()
	// Nothing more comes from the app's side: EOF is the channel closing.
	io.Copy(io.Discard, c)
}

// ChannelMain runs `stagent host-screen-channel`: the second exec channel
// of a host screen tab (see hostChannel).
func ChannelMain(args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "usage: stagent host-screen-channel  (reads a host_screen.channel token on stdin, writes frames to stdout)")
		return 2
	}
	l, err := paths.Resolve()
	if err != nil {
		fmt.Fprintln(os.Stderr, "stagent host-screen-channel:", err)
		return 1
	}
	if err := runChannel(l, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "stagent host-screen-channel:", err)
		return 1
	}
	return 0
}

// runChannel reads the token line from stdin, hands it to the bridge it
// names and copies the bridge's records to stdout until the bridge closes
// the channel, stdout fails or stdin ends (the app closed the channel).
func runChannel(l *paths.Layout, stdin io.Reader, stdout io.Writer) error {
	tok, err := readToken(stdin)
	if err != nil {
		return fmt.Errorf("reading the token: %w", err)
	}
	if !validToken(tok) {
		return errors.New("not a host_screen.channel token")
	}
	c, err := ipc.Dial(l.ChannelAddr(tok[:channelIDLen]), channelDialWait)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := io.WriteString(c, tok+"\n"); err != nil {
		return err
	}
	go func() {
		io.Copy(io.Discard, stdin)
		c.Close()
	}()
	_, err = io.Copy(stdout, c)
	if errors.Is(err, net.ErrClosed) {
		err = nil // stdin ended
	}
	return err
}

// readToken reads the token line from r, without its line end; at most a
// token's length and a CRLF is read.
func readToken(r io.Reader) (string, error) {
	line, err := bufio.NewReaderSize(io.LimitReader(r, channelTokenLen+2), channelTokenLen+2).ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}

func validToken(tok string) bool {
	if len(tok) != channelTokenLen {
		return false
	}
	for i := range len(tok) {
		if c := tok[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
