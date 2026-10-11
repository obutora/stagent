package ios

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

const bootedJSON = `{"devices": {"com.apple.CoreSimulator.SimRuntime.iOS-26-2": [
  {"udid": "U1", "name": "iPhone 17", "state": "Booted"}]}}`

// fakeTools answers simctl like a Mac with Xcode, or fails. With hang,
// every call takes until its context ends, as a stuck simctl does.
type fakeTools struct {
	list    string // simctl list's output
	listErr error
	findErr error // of xcrun --find simctl
	hang    bool
}

func (f fakeTools) run(ctx context.Context, _ []string, path string, args ...string) ([]byte, error) {
	if f.hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	switch {
	case filepath.Base(path) != "xcrun":
		return nil, errors.New("unexpected program " + path)
	case slices.Equal(args, []string{"simctl", "list", "devices", "--json"}):
		return []byte(f.list), f.listErr
	case slices.Equal(args, []string{"--find", "simctl"}):
		return []byte("/Applications/Xcode.app/.../simctl\n"), f.findErr
	}
	return nil, errors.New("unexpected arguments " + strings.Join(args, " "))
}

// testKind returns a kind whose PATH is a directory holding the programs
// named (xcrun, idb_companion); companion is what idb_companion runs (an
// empty file when "").
func testKind(t *testing.T, tools fakeTools, programs []string, companion string, env ...string) *Kind {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("xcrun and idb_companion are macOS programs")
	}
	bin := t.TempDir()
	for _, p := range programs {
		dst := filepath.Join(bin, p)
		if p == "idb_companion" && companion != "" {
			if err := os.Symlink(companion, dst); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(dst, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env = append(env, "PATH="+bin)
	return &Kind{env: func() []string { return env }, run: tools.run, every: 5 * time.Millisecond}
}

func listSection(t *testing.T, k *Kind) wire.HostScreenSection[wire.HostScreenSimulatorItem] {
	t.Helper()
	sec, err := k.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// As the app reads it.
	b, _ := json.Marshal(sec)
	var got wire.HostScreenSection[wire.HostScreenSimulatorItem]
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Items == nil {
		t.Fatalf("items is null: %s", b)
	}
	return got
}

func TestListShowsBootedSimulatorsConnectableWithCompanion(t *testing.T) {
	k := testKind(t, fakeTools{list: bootedJSON}, []string{"xcrun", "idb_companion"}, "")
	got := listSection(t, k)
	want := []wire.HostScreenSimulatorItem{{UDID: "U1", Name: "iPhone 17", Runtime: "iOS 26.2", Connectable: true}}
	if got.Missing != nil || !slices.Equal(got.Items, want) {
		t.Fatalf("list = %+v, want %+v", got, want)
	}
}

func TestListWithoutCompanionShowsSimulatorsUnconnectable(t *testing.T) {
	k := testKind(t, fakeTools{list: bootedJSON}, []string{"xcrun"}, "")
	got := listSection(t, k)
	want := []wire.HostScreenSimulatorItem{{UDID: "U1", Name: "iPhone 17", Runtime: "iOS 26.2", Reason: wire.ReasonCompanionNotFound}}
	if got.Missing != nil || !slices.Equal(got.Items, want) {
		t.Fatalf("list = %+v, want %+v", got, want)
	}
}

func TestCompanionInARelativePATHDirectoryIsNotUsed(t *testing.T) {
	k := testKind(t, fakeTools{list: bootedJSON}, []string{"xcrun"}, "")
	t.Chdir(t.TempDir())
	if err := os.Mkdir("bin", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("bin", "idb_companion"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := k.env()
	env[len(env)-1] += string(filepath.ListSeparator) + "bin"
	k.env = func() []string { return env }
	got := listSection(t, k)
	if len(got.Items) != 1 || got.Items[0].Connectable {
		t.Fatalf("list = %+v, want the simulator unconnectable", got)
	}
}

func TestListTellsWhySimctlListsNothing(t *testing.T) {
	failed := errors.New("exit status 1: license not agreed")
	for _, c := range []struct {
		name     string
		tools    fakeTools
		programs []string
		code     string
	}{
		{"no xcrun", fakeTools{}, nil, wire.MissingXcodeNotFound},
		{"no simctl", fakeTools{listErr: failed, findErr: errors.New("unable to find utility")}, []string{"xcrun"}, wire.MissingXcodeNotFound},
		{"simctl fails", fakeTools{listErr: failed}, []string{"xcrun"}, wire.MissingSimctlFailed},
		{"simctl prints no JSON", fakeTools{list: "oops"}, []string{"xcrun"}, wire.MissingSimctlFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := listSection(t, testKind(t, c.tools, c.programs, ""))
			if got.Missing == nil || got.Missing.Code != c.code || got.Missing.Message == "" || len(got.Items) != 0 {
				t.Fatalf("list = %+v, want missing %s", got, c.code)
			}
		})
	}
}

func TestListOfASimctlOutOfTimeIsNotXcodeNotFound(t *testing.T) {
	k := testKind(t, fakeTools{hang: true}, []string{"xcrun"}, "")
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	sec, err := k.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := sec.(wire.HostScreenSection[wire.HostScreenSimulatorItem])
	if got.Missing == nil || got.Missing.Code != wire.MissingSimctlFailed || !strings.Contains(got.Missing.Message, "deadline") {
		t.Fatalf("list = %+v, want simctl_failed with the timeout", got)
	}
}

func TestOpenRefusesWhatItCannotShow(t *testing.T) {
	for _, c := range []struct {
		name     string
		tools    fakeTools
		programs []string
		udid     string
		code     string
	}{
		{"no udid", fakeTools{list: bootedJSON}, []string{"xcrun", "idb_companion"}, "", wire.ErrBadRequest},
		{"not booted, no companion", fakeTools{list: bootedJSON}, []string{"xcrun"}, "U2", wire.ErrUnavailable},
		{"no Xcode", fakeTools{}, []string{"idb_companion"}, "U1", wire.ErrNotFound},
		{"no companion", fakeTools{list: bootedJSON}, []string{"xcrun"}, "U1", wire.ErrUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := testKind(t, c.tools, c.programs, "")
			_, err := k.Open(context.Background(), wire.HostScreenTarget{Kind: wire.HostScreenIOS, UDID: c.udid})
			var we *wire.Error
			if !errors.As(err, &we) || we.Code != c.code {
				t.Fatalf("Open = %v, want %s", err, c.code)
			}
		})
	}
}

func TestCompanionServesTheSimulatorOnItsSocketOnly(t *testing.T) {
	got := companionArgs("U1", "/tmp/stagent-ios-1/c.sock")
	want := []string{
		"--udid", "U1", "--only", "simulator", "--grpc-domain-sock", "/tmp/stagent-ios-1/c.sock",
		"--idle-shutdown-time", "5", "--terminate-offline", "true", "--log-level", "info",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("companionArgs = %q, want %q", got, want)
	}
}
