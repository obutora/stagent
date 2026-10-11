// Package android is the Android emulator kind of the host screen (ADR
// 0008): the emulators `adb devices` lists, matched with the files in which
// they advertise their gRPC port, and one shown through the emulator's
// gRPC streamScreenshot. Physical devices are never listed, and there is
// no adb screencap fallback.
package android

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"time"

	"github.com/obutora/stagent/internal/hostscreen"
	"github.com/obutora/stagent/internal/wire"
)

// adbWait bounds one adb command (`adb devices -l` may start adb's
// server).
const adbWait = 10 * time.Second

// Kind lists and opens Android emulators.
type Kind struct {
	// host returns the OS, environment and home to discover with;
	// replaceable in tests.
	host func() host
	// adb runs the adb found with args; replaceable in tests.
	adb func(ctx context.Context, adb string, env []string, args ...string) ([]byte, error)
	// every is how often a gone emulator is looked for again; shorter in
	// tests.
	every time.Duration
}

// New returns the Android kind; env returns the environment of the user's
// sessions (its PATH and ANDROID_HOME find adb, as in their terminal).
func New(env func() []string) *Kind {
	return &Kind{
		host:  func() host { return thisHost(env()) },
		every: hostscreen.WatchEvery,
		adb: func(ctx context.Context, adb string, env []string, args ...string) ([]byte, error) {
			ctx, cancel := context.WithTimeout(ctx, adbWait)
			defer cancel()
			cmd := exec.CommandContext(ctx, adb, args...)
			cmd.Env = env
			return cmd.Output()
		},
	}
}

func (k *Kind) Name() string { return wire.HostScreenAndroid }

// List returns the android section of host_screen.list.
func (k *Kind) List(ctx context.Context) (any, error) {
	h := k.host()
	adb := h.adbPath()
	if adb == "" {
		return wire.HostScreenSection[wire.HostScreenEmulatorItem]{
			Items: []wire.HostScreenEmulatorItem{},
			Note:  hostscreen.WSLNote(h.wsl),
			Missing: &wire.HostScreenMissing{
				Code:    wire.HostScreenADBNotFound,
				Message: "adb is not on PATH, in $ANDROID_HOME/platform-tools or $ANDROID_SDK_ROOT/platform-tools, nor in the default Android SDK",
			},
		}, nil
	}
	// adb failing (its server cannot start, say) lists no emulator rather
	// than failing the other kinds' sections.
	out, _ := k.adb(ctx, adb, h.env, "devices", "-l")
	running := readRunning(h.runningDir())
	list := items(emulatorSerials(out, running), running)
	for i := range list {
		// Without a discovery file (no gRPC) the console tells the name.
		if list[i].AVD == "" {
			out, _ := k.adb(ctx, adb, h.env, "-s", list[i].Serial, "emu", "avd", "name")
			list[i].AVD = avdName(out)
		}
	}
	return wire.HostScreenSection[wire.HostScreenEmulatorItem]{Items: list, Note: hostscreen.WSLNote(h.wsl)}, nil
}

// Open connects to the gRPC port of emulator t.Serial. The stream follows
// the AVD (t.AVD, else the one on t.Serial): when its emulator stops, it
// is gone until the AVD runs again, on any serial. An AVD that is not
// running is gone from the start, the same way.
func (k *Kind) Open(ctx context.Context, t wire.HostScreenTarget) (hostscreen.Stream, error) {
	if t.Serial == "" {
		return nil, wire.Errorf(wire.ErrBadRequest, "host_screen.start: serial is empty")
	}
	h := k.host()
	e, ok := readRunning(h.runningDir())[t.Serial]
	avd := t.AVD
	if ok && avd == "" {
		avd = e.avd // an app before 0.12.0 names the serial only
	}
	reopen := k.reopen(avd)
	if ok && e.avd == avd {
		if e.grpcPort == 0 {
			return nil, wire.Errorf(wire.ErrUnavailable, "host_screen.start: %s has no gRPC port (%s)", t.Serial, wire.HostScreenNoGRPC)
		}
		st, err := k.connect(ctx, h, e)
		var we *wire.Error
		switch {
		case err == nil:
			return hostscreen.Reviving(st, k.every, reopen), nil
		case avd == "" || !errors.As(err, &we) || we.Code != wire.ErrNotFound:
			return nil, err
		}
		// No gRPC on the port: the file of an emulator that ended.
	}
	if !ok && isEmulatorSerial(t.Serial) {
		// An emulator adb lists without a discovery file has no gRPC
		// either; only adb tells it from one that is not running (or is
		// offline, not an emulator at all).
		if adb := h.adbPath(); adb != "" {
			out, _ := k.adb(ctx, adb, h.env, "devices", "-l")
			if slices.Contains(emulatorSerials(out, nil), t.Serial) {
				return nil, wire.Errorf(wire.ErrUnavailable, "host_screen.start: %s has no gRPC port (%s)", t.Serial, wire.HostScreenNoGRPC)
			}
		}
	}
	if avd == "" {
		return nil, wire.Errorf(wire.ErrNotFound, "host_screen.start: no emulator %s", t.Serial)
	}
	// Stopped, or the serial is another AVD's now: the AVD may run on
	// another serial already, else the stream is gone until it does.
	return hostscreen.Reviving(reopen(ctx), k.every, reopen), nil
}

// connect opens a stream of running emulator e.
func (k *Kind) connect(ctx context.Context, h host, e emulator) (*stream, error) {
	st, err := openStream(ctx, e)
	if err != nil {
		return nil, err
	}
	st.serial = e.serial()
	if adb := h.adbPath(); adb != "" {
		st.adb = func(ctx context.Context, args ...string) ([]byte, error) {
			return k.adb(ctx, adb, h.env, args...)
		}
	}
	return st, nil
}

// reopen returns what looks for a running emulator of avd and opens it
// (nil while there is none): the emulators adb lists whose discovery file
// names avd and a gRPC port (adb itself failing: every such file), the
// lowest serial first.
func (k *Kind) reopen(avd string) func(context.Context) hostscreen.Stream {
	return func(ctx context.Context) hostscreen.Stream {
		h := k.host()
		running := readRunning(h.runningDir())
		var listed []string
		if adb := h.adbPath(); adb != "" {
			if out, err := k.adb(ctx, adb, h.env, "devices", "-l"); err == nil {
				listed = emulatorSerials(out, running)
				if listed == nil {
					return nil
				}
			}
		}
		var found []emulator
		for serial, e := range running {
			if e.avd == avd && e.grpcPort > 0 && (listed == nil || slices.Contains(listed, serial)) {
				found = append(found, e)
			}
		}
		slices.SortFunc(found, func(a, b emulator) int { return a.serialPort - b.serialPort })
		for _, e := range found {
			if st, err := k.connect(ctx, h, e); err == nil {
				return st
			}
		}
		return nil
	}
}
