package holder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/ipc"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

const testTimeout = 10 * time.Second

// isolate points the installation at a temp dir and makes the daemon
// impossible to auto-start (STAGENT_EXE does not exist).
func isolate(t *testing.T) *paths.Layout {
	t.Helper()
	home := t.TempDir()
	t.Setenv(paths.EnvHome, home)
	t.Setenv("STAGENT_EXE", filepath.Join(home, "no-such-stagent"))
	l, err := paths.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	// Long temp paths move the socket directory out of home (into a
	// directory namespaced by this home); remove it with the test.
	if !strings.HasPrefix(l.RunDir, home) {
		t.Cleanup(func() { os.RemoveAll(l.RunDir) })
	}
	return l
}

type runResult struct {
	code int
	err  error
}

func startDetached(t *testing.T, l *paths.Layout, cols, rows int, command ...string) (string, <-chan runResult) {
	t.Helper()
	id := wire.NewSessionID()
	done := make(chan runResult, 1)
	go func() {
		code, err := Run(context.Background(), Options{
			Command: command, Detached: true, ID: id, Cols: cols, Rows: rows,
			Dir: t.TempDir(), Layout: l, Logf: func(string, ...any) {},
		})
		done <- runResult{code, err}
	}()
	return id, done
}

func waitExit(t *testing.T, done <-chan runResult) runResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(testTimeout):
		t.Fatal("holder did not exit")
		return runResult{}
	}
}

// testClient is a holder connection as the bridge would open it.
type testClient struct {
	*rpc.Client
	mu    sync.Mutex
	notes []*wire.Msg
	cond  *sync.Cond
}

func dialHolder(t *testing.T, l *paths.Layout, id string) *testClient {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for {
		conn, err := ipc.Dial(l.HolderAddr(id), time.Second)
		if err == nil {
			tc := &testClient{}
			tc.cond = sync.NewCond(&tc.mu)
			tc.Client = rpc.NewClient(conn, func(m *wire.Msg) {
				tc.mu.Lock()
				tc.notes = append(tc.notes, m)
				tc.cond.Broadcast()
				tc.mu.Unlock()
			})
			t.Cleanup(func() { tc.Close() })
			return tc
		}
		if time.Now().After(deadline) {
			t.Fatalf("holder socket never answered: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (tc *testClient) call(t *testing.T, method string, params, result any) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	return tc.Call(ctx, method, params, result)
}

// next returns the first notification after index *from matching method,
// advancing *from past it.
func (tc *testClient) next(t *testing.T, from *int, method string) *wire.Msg {
	t.Helper()
	timer := time.AfterFunc(testTimeout, func() {
		tc.mu.Lock()
		tc.cond.Broadcast()
		tc.mu.Unlock()
	})
	defer timer.Stop()
	deadline := time.Now().Add(testTimeout)
	tc.mu.Lock()
	defer tc.mu.Unlock()
	for {
		for ; *from < len(tc.notes); *from++ {
			if m := tc.notes[*from]; m.Method == method {
				*from++
				return m
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %q notification within %v", method, testTimeout)
		}
		tc.cond.Wait()
	}
}

// outputUntil concatenates output data from *from on until it contains
// want; it returns everything collected.
func (tc *testClient) outputUntil(t *testing.T, from *int, want string) []byte {
	t.Helper()
	var all []byte
	for !bytes.Contains(all, []byte(want)) {
		var p wire.OutputParams
		json.Unmarshal(tc.next(t, from, wire.NotifyOutput).Params, &p)
		all = append(all, p.Data...)
	}
	return all
}

func errCode(err error) string {
	var we *wire.Error
	if errors.As(err, &we) {
		return we.Code
	}
	return ""
}

// fakeDaemon records what holders send and answers holder.register.
type fakeDaemon struct {
	mu      sync.Mutex
	msgs    []*wire.Msg
	conns   []net.Conn
	holders []*rpc.Conn // connections a holder registered on
	cond    *sync.Cond
}

func startFakeDaemon(t *testing.T, l *paths.Layout) *fakeDaemon {
	t.Helper()
	if err := l.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	ln, err := ipc.Listen(l.DaemonAddr)
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDaemon{}
	d.cond = sync.NewCond(&d.mu)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			d.mu.Lock()
			d.conns = append(d.conns, c)
			d.mu.Unlock()
			go rpc.Serve(ctx, c, func(_ context.Context, rc *rpc.Conn, m *wire.Msg) (any, error) {
				d.mu.Lock()
				d.msgs = append(d.msgs, m)
				if m.Method == wire.MethodHolderRegister {
					d.holders = append(d.holders, rc)
				}
				d.cond.Broadcast()
				d.mu.Unlock()
				if m.Method == wire.MethodHolderRegister {
					return wire.HolderRegisterResult{IdleAfterMs: 60000}, nil
				}
				return struct{}{}, nil
			})
		}
	}()
	return d
}

// waitFor returns the first message from *from on satisfying ok.
func (d *fakeDaemon) waitFor(t *testing.T, from *int, what string, ok func(*wire.Msg) bool) *wire.Msg {
	t.Helper()
	timer := time.AfterFunc(testTimeout, func() {
		d.mu.Lock()
		d.cond.Broadcast()
		d.mu.Unlock()
	})
	defer timer.Stop()
	deadline := time.Now().Add(testTimeout)
	d.mu.Lock()
	defer d.mu.Unlock()
	for {
		for ; *from < len(d.msgs); *from++ {
			if m := d.msgs[*from]; ok(m) {
				*from++
				return m
			}
		}
		if time.Now().After(deadline) {
			var seen []string
			for _, m := range d.msgs {
				seen = append(seen, m.Method+" "+string(m.Params))
			}
			t.Fatalf("daemon never got %s; got:\n%s", what, strings.Join(seen, "\n"))
		}
		d.cond.Wait()
	}
}

func (d *fakeDaemon) dropConnections() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.conns {
		c.Close()
	}
	d.conns, d.holders = nil, nil
}

// notifyHolders sends a daemon → holder notification on every connection
// a holder registered on.
func (d *fakeDaemon) notifyHolders(t *testing.T, method string, params any) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.holders {
		if err := c.Notify(method, params); err != nil {
			t.Fatal(err)
		}
	}
}

// promptGones returns the holder.prompt_gone messages from from on.
func (d *fakeDaemon) promptGones(from int) []wire.HolderPromptGoneParams {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []wire.HolderPromptGoneParams
	for _, m := range d.msgs[min(from, len(d.msgs)):] {
		if m.Method == wire.MethodHolderPromptGone {
			var p wire.HolderPromptGoneParams
			json.Unmarshal(m.Params, &p)
			out = append(out, p)
		}
	}
	return out
}

func isPatch(check func(p wire.SessionPatch) bool) func(*wire.Msg) bool {
	return func(m *wire.Msg) bool {
		if m.Method != wire.MethodHolderUpdate {
			return false
		}
		var p wire.SessionPatch
		json.Unmarshal(m.Params, &p)
		return check(p)
	}
}

