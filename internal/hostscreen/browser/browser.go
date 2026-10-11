// Package browser is the browser kind of the host screen (ADR 0008): the
// Chromium-family browsers of the user that opened a CDP port, and a page
// target of one shown through Page.startScreencast.
package browser

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/obutora/stagent/internal/hostscreen"
	"github.com/obutora/stagent/internal/ptable"
	"github.com/obutora/stagent/internal/wire"
)

// openWait bounds connecting to a page target.
const openWait = 10 * time.Second

// Kind lists and opens browsers.
type Kind struct {
	// procs returns the process table and the user stagent runs as (see
	// ptable.CurrentOwner); replaceable in tests.
	procs func() (*ptable.Snapshot, string, error)
}

// New returns the browser kind reading the host's process table.
func New() *Kind {
	return &Kind{procs: func() (*ptable.Snapshot, string, error) {
		s, err := ptable.Take()
		if err != nil {
			return nil, "", err
		}
		owner, err := ptable.CurrentOwner()
		return s, owner, err
	}}
}

func (k *Kind) Name() string { return wire.HostScreenBrowser }

// List returns the browser section of host_screen.list.
func (k *Kind) List(ctx context.Context) (any, error) { return k.ListPorts(ctx, nil) }

// ListPorts returns the browser section of host_screen.list with the CDP
// ports named by hand (cdp_ports) that answer on 127.0.0.1.
func (k *Kind) ListPorts(ctx context.Context, ports []int) (any, error) {
	s, owner, err := k.procs()
	if err != nil {
		return nil, wire.Errorf(wire.ErrInternal, "host_screen.list: process table: %v", err)
	}
	return wire.HostScreenSection[wire.HostScreenBrowserItem]{Items: discover(ctx, candidates(s, owner), ports)}, nil
}

// Open connects to page target t.TargetID of the CDP port t.Port on
// 127.0.0.1.
func (k *Kind) Open(ctx context.Context, t wire.HostScreenTarget) (hostscreen.Stream, error) {
	if t.Port < 1 || t.Port > 65535 {
		return nil, wire.Errorf(wire.ErrBadRequest, "host_screen.start: port %d out of range", t.Port)
	}
	if t.TargetID == "" {
		return nil, wire.Errorf(wire.ErrBadRequest, "host_screen.start: target_id is empty")
	}
	ctx, cancel := context.WithTimeout(ctx, openWait)
	defer cancel()
	list, err := targets(ctx, t.Port)
	if err != nil {
		return nil, wire.Errorf(wire.ErrNotFound, "host_screen.start: no CDP on port %d: %v", t.Port, err)
	}
	var page *cdpTarget
	for i := range list {
		if list[i].ID == t.TargetID && list[i].Type == "page" {
			page = &list[i]
		}
	}
	if page == nil {
		return nil, wire.Errorf(wire.ErrNotFound, "host_screen.start: no page %s on port %d", t.TargetID, t.Port)
	}
	// Only the path is taken from Chrome's answer: the connection stays on
	// 127.0.0.1 and the port asked.
	path := "/devtools/page/" + url.PathEscape(page.ID)
	if u, err := url.Parse(page.WebSocketDebuggerURL); err == nil && u.Path != "" {
		path = u.EscapedPath()
	}
	st, err := openStream(ctx, fmt.Sprintf("ws://127.0.0.1:%d%s", t.Port, path))
	if err != nil {
		return nil, wire.Errorf(wire.ErrUnavailable, "host_screen.start: page %s: %v", t.TargetID, err)
	}
	return st, nil
}
