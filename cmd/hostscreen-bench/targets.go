package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// kind shows a scenario's page on one kind of target and picks that target
// out of host_screen.list.
type kind interface {
	name() string
	// open shows the page at path (e.g. "/scroll") of the harness's server.
	open(ctx context.Context, path string) error
	listParams() wire.HostScreenListParams
	pick(l hostScreenList) (wire.HostScreenTarget, error)
	// close undoes open.
	close()
	// textAt is where the scenario page's textarea is, in frame fractions.
	textAt() (x, y float64)
}

// ---------------------------------------------------------------------------
// Desktop Chrome through its CDP port (one new tab per scenario).

type browserKind struct {
	port     int // CDP
	pagePort int
	target   string
}

func (k *browserKind) name() string { return wire.HostScreenBrowser }

func (k *browserKind) cdp(ctx context.Context, method, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", k.port, path), nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("CDP port %d: %w (start Chrome with --remote-debugging-port=%d)", k.port, err, k.port)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CDP %s: %s %s", path, resp.Status, b)
	}
	return b, err
}

func (k *browserKind) open(ctx context.Context, path string) error {
	u := fmt.Sprintf("http://127.0.0.1:%d%s", k.pagePort, path)
	b, err := k.cdp(ctx, http.MethodPut, "/json/new?"+url.QueryEscape(u))
	if err != nil {
		return err
	}
	var t struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &t); err != nil {
		return err
	}
	k.target = t.ID
	_, err = k.cdp(ctx, http.MethodGet, "/json/activate/"+t.ID)
	return err
}

func (k *browserKind) listParams() wire.HostScreenListParams {
	return wire.HostScreenListParams{CDPPorts: []int{k.port}}
}

func (k *browserKind) pick(l hostScreenList) (wire.HostScreenTarget, error) {
	if l.Browser != nil {
		for _, it := range l.Browser.Items {
			for _, p := range it.Pages {
				if it.Port == k.port && it.Connectable && p.TargetID == k.target {
					return wire.HostScreenTarget{Kind: wire.HostScreenBrowser, Port: k.port, TargetID: p.TargetID}, nil
				}
			}
		}
	}
	return wire.HostScreenTarget{}, fmt.Errorf("tab %s of CDP port %d not listed", k.target, k.port)
}

func (k *browserKind) close() {
	if k.target != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		k.cdp(ctx, http.MethodGet, "/json/close/"+k.target)
		k.target = ""
	}
}

func (k *browserKind) textAt() (float64, float64) { return 0.5, 0.2 }

// ---------------------------------------------------------------------------
// Chrome on an Android emulator (the page through VIEW intents).

type androidKind struct {
	serial   string // "" = the first connectable emulator
	pagePort int
	adb      string
}

func (k *androidKind) name() string { return wire.HostScreenAndroid }

func (k *androidKind) open(ctx context.Context, path string) error {
	if k.adb == "" {
		k.adb = findADB()
	}
	args := []string{}
	if k.serial != "" {
		args = append(args, "-s", k.serial)
	}
	args = append(args, "shell", "am", "start", "-a", "android.intent.action.VIEW",
		"-d", fmt.Sprintf("http://10.0.2.2:%d%s", k.pagePort, path), "com.android.chrome")
	if b, err := exec.CommandContext(ctx, k.adb, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("adb am start: %v\n%s", err, b)
	}
	return nil
}

func findADB() string {
	if p, err := exec.LookPath("adb"); err == nil {
		return p
	}
	for _, root := range []string{os.Getenv("ANDROID_HOME"), os.Getenv("ANDROID_SDK_ROOT")} {
		if root != "" {
			return filepath.Join(root, "platform-tools", "adb")
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Android", "sdk", "platform-tools", "adb")
}

func (k *androidKind) listParams() wire.HostScreenListParams { return wire.HostScreenListParams{} }

func (k *androidKind) pick(l hostScreenList) (wire.HostScreenTarget, error) {
	if l.Android != nil {
		for _, it := range l.Android.Items {
			if it.Connectable && (k.serial == "" || it.Serial == k.serial) {
				k.serial = it.Serial
				return wire.HostScreenTarget{Kind: wire.HostScreenAndroid, Serial: it.Serial, AVD: it.AVD}, nil
			}
		}
	}
	return wire.HostScreenTarget{}, fmt.Errorf("no connectable Android emulator listed (%+v)", l.Android)
}

func (k *androidKind) close() {}

// Below the status bar and Chrome's address bar.
func (k *androidKind) textAt() (float64, float64) { return 0.5, 0.25 }

// ---------------------------------------------------------------------------
// Safari on a booted iOS simulator (the page through simctl openurl).

type iosKind struct {
	udid     string // "" = the first connectable simulator
	pagePort int
}

func (k *iosKind) name() string { return wire.HostScreenIOS }

func (k *iosKind) open(ctx context.Context, path string) error {
	dev := k.udid
	if dev == "" {
		dev = "booted"
	}
	// A fresh Safari: no dialog left over from the previous scenario (the
	// paste prompt of a text) keeps the page from opening.
	exec.CommandContext(ctx, "xcrun", "simctl", "terminate", dev, "com.apple.mobilesafari").Run()
	u := fmt.Sprintf("http://127.0.0.1:%d%s", k.pagePort, path)
	if b, err := exec.CommandContext(ctx, "xcrun", "simctl", "openurl", dev, u).CombinedOutput(); err != nil {
		return fmt.Errorf("simctl openurl: %v\n%s", err, b)
	}
	return nil
}

func (k *iosKind) listParams() wire.HostScreenListParams { return wire.HostScreenListParams{} }

func (k *iosKind) pick(l hostScreenList) (wire.HostScreenTarget, error) {
	if l.IOS != nil {
		for _, it := range l.IOS.Items {
			if it.Connectable && (k.udid == "" || it.UDID == k.udid) {
				k.udid = it.UDID
				return wire.HostScreenTarget{Kind: wire.HostScreenIOS, UDID: it.UDID}, nil
			}
		}
	}
	return wire.HostScreenTarget{}, fmt.Errorf("no connectable iOS simulator listed (%+v)", l.IOS)
}

func (k *iosKind) close() {}

func (k *iosKind) textAt() (float64, float64) { return 0.5, 0.22 }
