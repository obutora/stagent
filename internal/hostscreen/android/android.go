// Package android is the Android emulator kind of the host screen (ADR
// 0008): the emulators `adb devices` lists, matched with the files in which
// they advertise their gRPC port, and one shown through the emulator's
// gRPC streamScreenshot. Physical devices are never listed, and there is
// no adb screencap fallback.
package android

import (
	"context"
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
}

// New returns the Android kind; env returns the environment of the user's
// sessions (its PATH and ANDROID_HOME find adb, as in their terminal).
func New(env func() []string) *Kind {
	return &Kind{
		host: func() host { return thisHost(env()) },
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
			Missing: &wire.HostScreenMissing{
				Code:    wire.HostScreenADBNotFound,
				Message: "adb is not on PATH, in $ANDROID_HOME/platform-tools or $ANDROID_SDK_ROOT/platform-tools, nor in the default Android SDK",
			},
		}, nil
	}
	// adb failing (its server cannot start, say) lists no emulator rather
	// than failing the other kinds' sections.
	out, _ := k.adb(ctx, adb, h.env, "devices", "-l")
	list := items(emulatorSerials(out), readRunning(h.runningDir()))
	for i := range list {
		// Without a discovery file (no gRPC) the console tells the name.
		if list[i].AVD == "" {
			out, _ := k.adb(ctx, adb, h.env, "-s", list[i].Serial, "emu", "avd", "name")
			list[i].AVD = avdName(out)
		}
	}
	return wire.HostScreenSection[wire.HostScreenEmulatorItem]{Items: list}, nil
}

// Open connects to the gRPC port of emulator t.Serial.
func (k *Kind) Open(ctx context.Context, t wire.HostScreenTarget) (hostscreen.Stream, error) {
	if t.Serial == "" {
		return nil, wire.Errorf(wire.ErrBadRequest, "host_screen.start: serial is empty")
	}
	h := k.host()
	e, ok := readRunning(h.runningDir())[t.Serial]
	if ok && e.grpcPort > 0 {
		st, err := openStream(ctx, e)
		if err != nil {
			return nil, err
		}
		st.serial = t.Serial
		if adb := h.adbPath(); adb != "" {
			st.adb = func(ctx context.Context, args ...string) ([]byte, error) {
				return k.adb(ctx, adb, h.env, args...)
			}
		}
		return st, nil
	}
	if !ok && isEmulatorSerial(t.Serial) {
		// An emulator adb lists without a discovery file has no gRPC
		// either; only adb tells it from one that is not running.
		if adb := h.adbPath(); adb != "" {
			out, _ := k.adb(ctx, adb, h.env, "devices", "-l")
			ok = slices.Contains(emulatorSerials(out), t.Serial)
		}
	}
	if ok {
		return nil, wire.Errorf(wire.ErrUnavailable, "host_screen.start: %s has no gRPC port (%s)", t.Serial, wire.HostScreenNoGRPC)
	}
	return nil, wire.Errorf(wire.ErrNotFound, "host_screen.start: no emulator %s", t.Serial)
}
