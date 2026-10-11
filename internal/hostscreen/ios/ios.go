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
}

// New returns the iOS simulator kind; env gives the environment to find
// and run xcrun and idb_companion with.
func New(env func() []string) *Kind {
	return &Kind{env: env, run: run}
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
// stream.
func (k *Kind) Open(ctx context.Context, t wire.HostScreenTarget) (hostscreen.Stream, error) {
	if t.UDID == "" {
		return nil, wire.Errorf(wire.ErrBadRequest, "host_screen.start: udid is empty")
	}
	env := k.env()
	sims, missing := k.simulators(ctx, env)
	var sim *simulator
	for i := range sims {
		if sims[i].UDID == t.UDID {
			sim = &sims[i]
		}
	}
	if sim == nil {
		why := "not booted"
		if missing != nil {
			why = missing.Message
		}
		return nil, wire.Errorf(wire.ErrNotFound, "host_screen.start: simulator %s: %s", t.UDID, why)
	}
	path := lookPath(env, "idb_companion")
	if path == "" {
		return nil, wire.Errorf(wire.ErrUnavailable, "host_screen.start: idb_companion is not installed (brew install facebook/fb/idb-companion)")
	}
	ctx, cancel := context.WithTimeout(ctx, openWait)
	defer cancel()
	st, err := open(ctx, path, env, sim)
	if err != nil {
		return nil, wire.Errorf(wire.ErrUnavailable, "host_screen.start: simulator %s: %v", t.UDID, err)
	}
	return st, nil
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
