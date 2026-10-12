package ios

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// pasteAuth is whether the app in front may paste what another app put on
// the clipboard (its 「ほかのアプリからペースト」, iOS 16+): the
// kTCCServicePasteboard row of the simulator's TCC.db (#649).
type pasteAuth int

const (
	// pasteNoRow: the app never asked; iOS adds its row (ask) when it
	// first refuses the app a ⌘V, and only then does Settings show the
	// app's item.
	pasteNoRow pasteAuth = iota
	// pasteRefused: ask or deny, or TCC.db could not be read.
	pasteRefused
	// pasteAllowed: allowed (auth_value 2); ⌘V pastes.
	pasteAllowed
)

// Paste's label in the edit menu of a Japanese and an English app, which
// are always looked for: an app shows UIKit in its own localization, which
// need not be the simulator's language.
const (
	pasteJa = "ペースト"
	pasteEn = "Paste"
)

// simHost is what typing a text needs besides the companion: the
// simulator's clipboard and files. Replaceable in tests.
type simHost interface {
	// pbcopy puts text on the simulator's clipboard.
	pbcopy(ctx context.Context, text string) error
	// pasteAuth reads whether app bundle may paste from other apps; it
	// never writes.
	pasteAuth(ctx context.Context, bundle string) pasteAuth
	// pasteLabels are the labels of the edit menu's Paste to look for.
	pasteLabels(ctx context.Context) []string
}

// simctlHost is a simulator's simHost on this Mac: simctl, and sqlite3 and
// plutil (both part of macOS) reading its files.
type simctlHost struct {
	sim   simulator
	env   []string
	xcrun string
	run   func(ctx context.Context, env []string, path string, args ...string) ([]byte, error)
}

const (
	sqlite3 = "/usr/bin/sqlite3"
	plutil  = "/usr/bin/plutil"
)

func (h *simctlHost) pbcopy(ctx context.Context, text string) error {
	ctx, cancel := context.WithTimeout(ctx, simctlWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.xcrun, "simctl", "pbcopy", h.sim.UDID)
	cmd.Env = h.env
	cmd.Stdin = strings.NewReader(text)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("simctl pbcopy: %w: %s", err, msg)
		}
		return fmt.Errorf("simctl pbcopy: %w", err)
	}
	return nil
}

// pasteAuth opens the simulator's TCC.db read only; stagent never changes
// an app's permission (the user does in the simulator's Settings, #649).
func (h *simctlHost) pasteAuth(ctx context.Context, bundle string) pasteAuth {
	if h.sim.DataPath == "" || !bundleID(bundle) {
		return pasteRefused
	}
	ctx, cancel := context.WithTimeout(ctx, simctlWait)
	defer cancel()
	db := filepath.Join(h.sim.DataPath, "Library", "TCC", "TCC.db")
	out, err := h.run(ctx, h.env, sqlite3, "-readonly", db,
		"select auth_value from access where service = 'kTCCServicePasteboard' and client = '"+bundle+"'")
	if err != nil {
		return pasteRefused
	}
	switch strings.TrimSpace(string(out)) {
	case "":
		return pasteNoRow
	case "2":
		return pasteAllowed
	}
	return pasteRefused
}

// bundleID tells whether s is a bundle id (letters, digits, '.', '-'), so
// it can stand in the query as is.
func bundleID(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '.' || c == '-') {
			return false
		}
	}
	return true
}

// pasteLabels are UIKit's Paste in the simulator's language (UIKitCore's
// Localizable.strings of its runtime), then in Japanese and English.
func (h *simctlHost) pasteLabels(ctx context.Context) []string {
	labels := []string{}
	if l := h.localPaste(ctx); l != "" && l != pasteJa && l != pasteEn {
		labels = append(labels, l)
	}
	return append(labels, pasteJa, pasteEn)
}

// localPaste is Paste in the simulator's language; "" when either cannot
// be read.
func (h *simctlHost) localPaste(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, simctlWait)
	defer cancel()
	if h.sim.DataPath == "" {
		return ""
	}
	prefs := filepath.Join(h.sim.DataPath, "Library", "Preferences", ".GlobalPreferences.plist")
	lang, err := h.run(ctx, h.env, plutil, "-extract", "AppleLanguages.0", "raw", "-o", "-", prefs)
	if err != nil {
		return ""
	}
	root := h.runtimeRoot(ctx)
	if root == "" {
		return ""
	}
	uikit := filepath.Join(root, "System", "Library", "PrivateFrameworks", "UIKitCore.framework")
	for _, lproj := range lprojs(strings.TrimSpace(string(lang))) {
		strs := filepath.Join(uikit, lproj+".lproj", "Localizable.strings")
		if out, err := h.run(ctx, h.env, plutil, "-extract", "Paste", "raw", "-o", "-", strs); err == nil {
			if l := strings.TrimSpace(string(out)); l != "" {
				return l
			}
		}
	}
	return ""
}

// runtimeRoot is the root of the simulator's runtime, from `xcrun simctl
// list runtimes --json`; "" when not found.
func (h *simctlHost) runtimeRoot(ctx context.Context) string {
	out, err := h.run(ctx, h.env, h.xcrun, "simctl", "list", "runtimes", "--json")
	if err != nil {
		return ""
	}
	var list struct {
		Runtimes []struct {
			Identifier  string `json:"identifier"`
			RuntimeRoot string `json:"runtimeRoot"`
		} `json:"runtimes"`
	}
	if json.Unmarshal(out, &list) != nil {
		return ""
	}
	for _, r := range list.Runtimes {
		if r.Identifier == h.sim.RuntimeID {
			return r.RuntimeRoot
		}
	}
	return ""
}

// lprojs are the .lproj names to try for language lang (AppleLanguages'
// BCP 47, "ja-JP", "zh-Hans-CN"): all of it, its language and region,
// its language.
func lprojs(lang string) []string {
	if lang == "" {
		return nil
	}
	parts := strings.Split(lang, "-")
	names := []string{strings.Join(parts, "_")}
	if len(parts) > 2 {
		names = append(names, parts[0]+"_"+parts[len(parts)-1])
	}
	if len(parts) > 1 {
		names = append(names, parts[0])
	}
	return names
}
