// Package ios is the iOS simulator kind of the host screen (ADR 0008,
// macOS only): the booted simulators simctl lists, one shown through an
// idb_companion stagent starts for the stream (#630).
package ios

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/hostscreen"
	"github.com/obutora/stagent/internal/wire"
)

const (
	// openWait bounds starting a companion until it describes the
	// simulator (about a second, #630).
	openWait = 10 * time.Second
	// simctlWait bounds one simctl call.
	simctlWait = 10 * time.Second
)

// Kind lists and opens iOS simulators.
type Kind struct {
	// env is the environment xcrun and idb_companion are looked up in
	// (PATH) and run with.
	env func() []string
	// run runs a program and returns its standard output; replaceable in
	// tests.
	run func(ctx context.Context, env []string, path string, args ...string) ([]byte, error)
	// every is how often a gone simulator is looked for again; shorter
	// in tests.
	every time.Duration
}

// New returns the iOS simulator kind; env gives the environment to find
// and run xcrun and idb_companion with.
func New(env func() []string) *Kind {
	return &Kind{env: env, run: run, every: hostscreen.WatchEvery}
}

func (k *Kind) Name() string { return wire.HostScreenIOS }

// List returns the ios section of host_screen.list.
func (k *Kind) List(ctx context.Context) (any, error) {
	env := k.env()
	sims, missing := k.simulators(ctx, env)
	return wire.HostScreenSection[wire.HostScreenSimulatorItem]{
		Items:   items(sims, lookPath(env, "idb_companion") != ""),
		Missing: missing,
	}, nil
}

// Open starts a companion for booted simulator t.UDID and its video
// stream. When the simulator shuts down the stream is gone until it boots
// again; one not booted is gone from the start, the same way, and so is
// any while simctl fails (a timeout: the watcher lists again). Only
// without Xcode is it not_found.
func (k *Kind) Open(ctx context.Context, t wire.HostScreenTarget) (hostscreen.Stream, error) {
	if t.UDID == "" {
		return nil, wire.Errorf(wire.ErrBadRequest, "host_screen.start: udid is empty")
	}
	env := k.env()
	sims, missing := k.simulators(ctx, env)
	if missing != nil && missing.Code == wire.MissingXcodeNotFound {
		return nil, wire.Errorf(wire.ErrNotFound, "host_screen.start: simulator %s: %s", t.UDID, missing.Message)
	}
	path := lookPath(env, "idb_companion")
	if path == "" {
		return nil, wire.Errorf(wire.ErrUnavailable, "host_screen.start: idb_companion is not installed (brew install facebook/fb/idb-companion)")
	}
	reopen := k.reopen(t.UDID)
	sim := booted(sims, t.UDID)
	if sim == nil {
		return hostscreen.Reviving(nil, k.every, reopen), nil
	}
	st, err := connect(ctx, path, env, sim)
	if err != nil {
		return nil, wire.Errorf(wire.ErrUnavailable, "host_screen.start: simulator %s: %v", t.UDID, err)
	}
	st.host = k.simHost(env, sim)
	return hostscreen.Reviving(st, k.every, reopen), nil
}

// simHost is sim's clipboard and files for typing text.
func (k *Kind) simHost(env []string, sim *simulator) simHost {
	return &simctlHost{sim: *sim, env: env, xcrun: lookPath(env, "xcrun"), run: k.run}
}

// reopen returns what looks for simulator udid booted and opens it (nil
// while it is not).
func (k *Kind) reopen(udid string) func(context.Context) hostscreen.Stream {
	return func(ctx context.Context) hostscreen.Stream {
		env := k.env()
		sims, _ := k.simulators(ctx, env)
		sim := booted(sims, udid)
		path := lookPath(env, "idb_companion")
		if sim == nil || path == "" {
			return nil
		}
		if st, err := connect(ctx, path, env, sim); err == nil {
			st.host = k.simHost(env, sim)
			return st
		}
		return nil
	}
}

// booted is simulator udid of sims; nil when it is not booted.
func booted(sims []simulator, udid string) *simulator {
	for i := range sims {
		if sims[i].UDID == udid {
			return &sims[i]
		}
	}
	return nil
}

// connect opens sim, within openWait.
func connect(ctx context.Context, path string, env []string, sim *simulator) (*stream, error) {
	ctx, cancel := context.WithTimeout(ctx, openWait)
	defer cancel()
	return open(ctx, path, env, sim)
}

// open starts sim's companion and its video stream; on failure the
// companion is stopped.
func open(ctx context.Context, path string, env []string, sim *simulator) (*stream, error) {
	comp, err := startCompanion(path, env, sim.UDID)
	if err != nil {
		return nil, err
	}
	d, err := comp.describe(ctx)
	if err == nil && (d.GetScreenDimensions().GetWidthPoints() == 0 || d.GetScreenDimensions().GetHeightPoints() == 0) {
		err = errors.New("idb_companion did not tell the screen's size")
	}
	var st *stream
	if err == nil {
		sd := d.GetScreenDimensions()
		st, err = openStream(comp, sim.Name, int(sd.GetWidthPoints()), int(sd.GetHeightPoints()))
	}
	if err != nil {
		comp.stop()
		return nil, err
	}
	return st, nil
}

// simulators are the booted iOS simulators; without them missing tells
// why simctl could not list any.
func (k *Kind) simulators(ctx context.Context, env []string) ([]simulator, *wire.HostScreenMissing) {
	xcrun := lookPath(env, "xcrun")
	if xcrun == "" {
		return nil, xcodeNotFound()
	}
	ctx, cancel := context.WithTimeout(ctx, simctlWait)
	defer cancel()
	out, err := k.run(ctx, env, xcrun, "simctl", "list", "devices", "--json")
	if err != nil {
		// Without Xcode (Command Line Tools only, or none selected) xcrun
		// cannot find simctl at all. A simctl that ran out of time was
		// found: asking again with the time used up would fail too.
		if ctx.Err() == nil {
			if _, ferr := k.run(ctx, env, xcrun, "--find", "simctl"); ferr != nil {
				return nil, xcodeNotFound()
			}
		}
		return nil, &wire.HostScreenMissing{Code: wire.MissingSimctlFailed, Message: fmt.Sprintf("xcrun simctl list: %v", err)}
	}
	sims, err := parseDevices(out)
	if err != nil {
		return nil, &wire.HostScreenMissing{Code: wire.MissingSimctlFailed, Message: fmt.Sprintf("xcrun simctl list: %v", err)}
	}
	return sims, nil
}

func xcodeNotFound() *wire.HostScreenMissing {
	return &wire.HostScreenMissing{Code: wire.MissingXcodeNotFound, Message: "Xcode is not installed (xcrun cannot find simctl)"}
}

// run runs path and returns its standard output; a failure carries its
// standard error.
func run(ctx context.Context, env []string, path string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}
	return out, nil
}

// lookPath finds the executable name in env's PATH; "" when it is not
// there.
func lookPath(env []string, name string) string {
	var pathVar string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			pathVar = v
		}
	}
	for _, dir := range filepath.SplitList(pathVar) {
		// A relative entry would find a program in whatever directory
		// stagent runs in.
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}
