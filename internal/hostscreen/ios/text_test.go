package ios

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/hostscreen/ios/idbpb"
	"github.com/obutora/stagent/internal/wire"
)

// envFakeAX names the directory the fake companion answers
// accessibility_info and list_apps from: root.json for the screen's root
// (depth 1), tree-<n>.json for the whole screen once it performed n touch
// lifts ("[]" without the file), apps.json for list_apps.
const envFakeAX = "STAGENT_TEST_FAKE_AX"

func (f *fakeService) AccessibilityInfo(_ context.Context, r *idbpb.AccessibilityInfoRequest) (*idbpb.AccessibilityInfoResponse, error) {
	dir := os.Getenv(envFakeAX)
	name := "root.json"
	if r.GetDepth() != 1 {
		hid, _ := os.ReadFile(os.Getenv(envFakeHID))
		name = fmt.Sprintf("tree-%d.json", strings.Count(string(hid), "touch up"))
	}
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		b = []byte("[]")
	}
	return &idbpb.AccessibilityInfoResponse{Json: string(b)}, nil
}

func (f *fakeService) ListApps(context.Context, *idbpb.ListAppsRequest) (*idbpb.ListAppsResponse, error) {
	b, err := os.ReadFile(filepath.Join(os.Getenv(envFakeAX), "apps.json"))
	if err != nil {
		return nil, err
	}
	var apps []struct {
		Bundle string `json:"bundle_id"`
		Name   string `json:"name"`
		PID    uint64 `json:"pid"`
	}
	if err := json.Unmarshal(b, &apps); err != nil {
		return nil, err
	}
	res := &idbpb.ListAppsResponse{}
	for _, a := range apps {
		res.Apps = append(res.Apps, &idbpb.InstalledAppInfo{
			BundleId: a.Bundle, Name: a.Name, ProcessIdentifier: a.PID, ProcessState: idbpb.InstalledAppInfo_RUNNING,
		})
	}
	return res, nil
}

// fakeHost is a simulator whose apps have auth; what it puts on the
// clipboard it writes to the HID log too ("pbcopy <text>"), so the order
// shows.
type fakeHost struct {
	log  string
	auth map[string]pasteAuth

	mu      sync.Mutex
	queried []string // the bundle ids pasteAuth was asked about
}

func (h *fakeHost) pbcopy(_ context.Context, text string) error {
	out, err := os.OpenFile(h.log, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = fmt.Fprintf(out, "%d pbcopy %s\n", time.Now().UnixMilli(), text)
	return err
}

func (h *fakeHost) pasteAuth(_ context.Context, bundle string) pasteAuth {
	h.mu.Lock()
	h.queried = append(h.queried, bundle)
	h.mu.Unlock()
	if a, ok := h.auth[bundle]; ok {
		return a
	}
	return pasteNoRow
}

func (h *fakeHost) pasteLabels(context.Context) []string { return []string{pasteJa, pasteEn} }

// Apps in the fake simulator.
const (
	flutterApp = "com.obutora.sshtermxxx"
	safariApp  = "com.apple.mobilesafari"
)

// textStream opens simulator U1 (402 × 874 pt) by a fake companion whose
// screen shows app (pid 7) in front, the paste permissions auth; ax holds
// more files of envFakeAX. It returns the stream and its HID log.
func textStream(t *testing.T, app string, auth map[string]pasteAuth, ax map[string]string) (*stream, string) {
	t.Helper()
	return textStreamIn(t, "still", app, auth, ax)
}

// textStreamIn is textStream by a fake companion in mode.
func textStreamIn(t *testing.T, mode, app string, auth map[string]pasteAuth, ax map[string]string) (*stream, string) {
	t.Helper()
	t.Parallel()
	dir := t.TempDir()
	log := filepath.Join(dir, "hid")
	names := map[string]string{flutterApp: "SSH Term X", safariApp: "Safari"}
	files := map[string]string{
		"root.json": fmt.Sprintf(`[{"type": "Application", "AXLabel": %q, "pid": 7, "children": []}]`, names[app]),
		"apps.json": fmt.Sprintf(`[{"bundle_id": "com.apple.mobilecal", "name": "MobileCal", "pid": 3}, {"bundle_id": %q, "name": "Runner", "pid": 7}]`, app),
	}
	for name, body := range ax {
		files[name] = body
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := openFake(t, mode, envFakeHID+"="+log, envFakeAX+"="+dir)
	s.host = &fakeHost{log: log, auth: auth}
	return s, log
}

var (
	cmdVKeys  = []string{"key down 227", "key down 25", "key up 25", "key up 227"}
	returnKey = []string{"key down 40", "key up 40"}
	lang2Key  = []string{"key down 145", "key up 145"}
)

func TestTextIntoAnAllowedAppIsPastedWithCmdVAndKeysWaitForItsBanner(t *testing.T) {
	s, log := textStream(t, flutterApp, map[string]pasteAuth{flutterApp: pasteAllowed}, nil)
	res, err := s.Input(t.Context(), wire.HostScreenInput{Type: wire.InputText, Text: "日本語", X: 0.5, Y: 0.25})
	if err != nil {
		t.Fatal(err)
	}
	if want := (wire.HostScreenInputResult{Via: wire.InputViaCmdV, App: "SSH Term X"}); res != want {
		t.Fatalf("text answered %+v, want %+v", res, want)
	}
	// A tap goes at once, the banner taking no taps away; the Return of
	// an enter waits for the banner to go.
	start := time.Now()
	if _, err := s.Input(t.Context(), wire.HostScreenInput{Type: wire.InputTap, X: 0.5, Y: 0.25}); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("tap after the ⌘V took %v, want at once", took)
	}
	if _, err := s.Input(t.Context(), wire.HostScreenInput{Type: wire.InputText, Enter: true, X: 0.5, Y: 0.25}); err != nil {
		t.Fatal(err)
	}
	evs := performed(t, log, 1+len(cmdVKeys)+2+len(returnKey))
	want := slices.Concat([]string{"pbcopy 日本語"}, cmdVKeys, []string{"touch down 201,218.5", "touch up 201,218.5"}, returnKey)
	if got := whats(evs); !slices.Equal(got, want) {
		t.Fatalf("text performed %q, want %q", got, want)
	}
	if gap := evs[len(evs)-2].at.Sub(evs[4].at); gap < pasteBlock-100*time.Millisecond {
		t.Fatalf("Return %v after the ⌘V, want %v", gap, pasteBlock)
	}
}

func TestASCIIIntoAnAllowedAppIsPastedToo(t *testing.T) {
	s, log := textStream(t, flutterApp, map[string]pasteAuth{flutterApp: pasteAllowed}, nil)
	res, err := s.Input(t.Context(), wire.HostScreenInput{Type: wire.InputText, Text: "example.com"})
	if err != nil || res.Via != wire.InputViaCmdV {
		t.Fatalf("ASCII answered %+v, %v, want pasted with ⌘V", res, err)
	}
	want := slices.Concat([]string{"pbcopy example.com"}, cmdVKeys)
	if got := whats(performed(t, log, len(want))); !slices.Equal(got, want) {
		t.Fatalf("text performed %q, want %q", got, want)
	}
}

func TestASCIIIntoAnAppNotAllowedIsTypedAfterTheEisuKey(t *testing.T) {
	s, log := textStream(t, safariApp, map[string]pasteAuth{safariApp: pasteRefused}, nil)
	res, err := s.Input(t.Context(), wire.HostScreenInput{Type: wire.InputText, Text: "aZ?1 ", Enter: true, X: 0.5, Y: 0.1})
	if err != nil {
		t.Fatal(err)
	}
	if want := (wire.HostScreenInputResult{Via: wire.InputViaKeys, App: "Safari"}); res != want {
		t.Fatalf("text answered %+v, want %+v", res, want)
	}
	want := slices.Concat(lang2Key, []string{
		"key down 4", "key up 4", // a
		"key down 225", "key down 29", "key up 29", "key up 225", // Z
		"key down 225", "key down 56", "key up 56", "key up 225", // ?
		"key down 30", "key up 30", // 1
		"key down 44", "key up 44", // space
	}, returnKey)
	if got := whats(performed(t, log, len(want))); !slices.Equal(got, want) {
		t.Fatalf("text performed %q, want %q", got, want)
	}
}

// editMenu is a screen showing the edit menu: Paste and Paste and Search
// as menu items, and a text "ペースト" that is no menu item.
const editMenu = `[{"type": "Application", "AXLabel": "Safari", "frame": {"x": 0, "y": 0, "width": 402, "height": 874}, "children": [
  {"type": "StaticText", "AXLabel": "ペースト", "traits": ["StaticText"], "frame": {"x": 0, "y": 600, "width": 100, "height": 20}, "children": []},
  {"type": "Group", "AXLabel": null, "children": [
    {"type": "StaticText", "AXLabel": "ペーストして検索", "traits": ["MenuItem", "TextOperationsAvailable", "Scrollable"], "frame": {"x": 180, "y": 300, "width": 120, "height": 40}, "children": []},
    {"type": "StaticText", "AXLabel": "ペースト", "traits": ["MenuItem", "TextOperationsAvailable", "Scrollable"], "frame": {"x": 100, "y": 300, "width": 80, "height": 40}, "children": []}
  ]}
]}]`

func TestTextIntoAnAppNotAllowedIsPastedFromTheEditMenuAtTheCursor(t *testing.T) {
	// The first tap only moves the caret; the second shows the menu.
	s, log := textStream(t, safariApp, map[string]pasteAuth{safariApp: pasteRefused}, map[string]string{"tree-2.json": editMenu})
	res, err := s.Input(t.Context(), wire.HostScreenInput{Type: wire.InputText, Text: "日本語", Enter: true, X: 0.5, Y: 0.25})
	if err != nil {
		t.Fatal(err)
	}
	if want := (wire.HostScreenInputResult{Via: wire.InputViaEditMenu, App: "Safari"}); res != want {
		t.Fatalf("text answered %+v, want %+v", res, want)
	}
	cursor := []string{"touch down 201,218.5", "touch up 201,218.5"}
	want := slices.Concat([]string{"pbcopy 日本語"}, cursor, cursor, []string{"touch down 140,320", "touch up 140,320"}, returnKey)
	evs := performed(t, log, len(want))
	if got := whats(evs); !slices.Equal(got, want) {
		t.Fatalf("text performed %q, want %q", got, want)
	}
	// The fake companion stamps events as they arrive, so a loaded machine
	// can shorten the measured gap; half the wait still tells it from none.
	if gap := evs[7].at.Sub(evs[6].at); gap < pasteSettle/2 {
		t.Fatalf("Return %v after Paste, want about %v for it to insert", gap, pasteSettle)
	}
}

func TestTextIntoAnAppThatNeverAskedSendsACmdVFirst(t *testing.T) {
	// The ⌘V iOS refuses adds the app's row: Settings shows its item. The
	// refused content is copied again, as the edit menu does not paste it.
	s, log := textStream(t, flutterApp, nil, map[string]string{"tree-1.json": editMenu})
	res, err := s.Input(t.Context(), wire.HostScreenInput{Type: wire.InputText, Text: "日本語", X: 0.5, Y: 0.25})
	if err != nil || res.Via != wire.InputViaEditMenu {
		t.Fatalf("text answered %+v, %v, want pasted from the edit menu", res, err)
	}
	want := slices.Concat([]string{"pbcopy 日本語"}, cmdVKeys, []string{"pbcopy 日本語", "touch down 201,218.5", "touch up 201,218.5", "touch down 140,320", "touch up 140,320"})
	if got := whats(performed(t, log, len(want))); !slices.Equal(got, want) {
		t.Fatalf("text performed %q, want %q", got, want)
	}
}

func TestASCIIIntoAnAppThatNeverAskedSendsACmdVFirst(t *testing.T) {
	// The row, and so Settings' item, comes of the refused ⌘V whatever
	// the text; the text is then typed.
	s, log := textStream(t, flutterApp, nil, nil)
	res, err := s.Input(t.Context(), wire.HostScreenInput{Type: wire.InputText, Text: "ab"})
	if err != nil || res.Via != wire.InputViaKeys {
		t.Fatalf("ASCII answered %+v, %v, want typed", res, err)
	}
	want := slices.Concat([]string{"pbcopy ab"}, cmdVKeys, lang2Key, []string{"key down 4", "key up 4", "key down 5", "key up 5"})
	if got := whats(performed(t, log, len(want))); !slices.Equal(got, want) {
		t.Fatalf("text performed %q, want %q", got, want)
	}
}

func TestCmdVFirstToACompanionSlowToEndTheHIDStream(t *testing.T) {
	// The companion answers the end of the hid stream after flushHID
	// stopped waiting; the next keys go on a new stream (run with -race).
	s, log := textStreamIn(t, "hidslow", flutterApp, nil, nil)
	if _, err := s.Input(t.Context(), wire.HostScreenInput{Type: wire.InputText, Text: "ab"}); err != nil {
		t.Fatal(err)
	}
	want := slices.Concat([]string{"pbcopy ab"}, cmdVKeys, lang2Key, []string{"key down 4", "key up 4", "key down 5", "key up 5"})
	if got := whats(performed(t, log, len(want))); !slices.Equal(got, want) {
		t.Fatalf("text performed %q, want %q", got, want)
	}
}

func TestTextWithoutTheEditMenuIsLeftOnTheClipboardForTheUser(t *testing.T) {
	s, log := textStream(t, safariApp, map[string]pasteAuth{safariApp: pasteRefused}, nil)
	_, err := s.Input(t.Context(), wire.HostScreenInput{Type: wire.InputText, Text: "日本語", Enter: true, X: 0.5, Y: 0.25})
	var we *wire.Error
	if !errors.As(err, &we) || we.Code != wire.ErrPasteByHand || !strings.Contains(we.Message, "Safari") {
		t.Fatalf("text without a menu = %v, want paste_by_hand naming the app", err)
	}
	// Tapped twice, then nothing more: no Return either.
	cursor := []string{"touch down 201,218.5", "touch up 201,218.5"}
	want := slices.Concat([]string{"pbcopy 日本語"}, cursor, cursor)
	if got := whats(performed(t, log, len(want))); !slices.Equal(got, want) {
		t.Fatalf("text performed %q, want %q", got, want)
	}
}

func TestTextWithoutTheCursorIsLeftOnTheClipboardForTheUser(t *testing.T) {
	s, log := textStream(t, safariApp, map[string]pasteAuth{safariApp: pasteRefused}, map[string]string{"tree-0.json": editMenu})
	_, err := s.Input(t.Context(), wire.HostScreenInput{Type: wire.InputText, Text: "日本語"})
	var we *wire.Error
	if !errors.As(err, &we) || we.Code != wire.ErrPasteByHand {
		t.Fatalf("text without x, y = %v, want paste_by_hand", err)
	}
	performed(t, log, 1) // the pbcopy only
}

func TestTextOnTheHomeScreenAsksNoPermission(t *testing.T) {
	// SpringBoard's pid is no app's.
	s, log := textStream(t, flutterApp, map[string]pasteAuth{flutterApp: pasteAllowed}, map[string]string{
		"root.json": `[{"type": "Application", "AXLabel": " ", "pid": 99, "children": []}]`,
	})
	res, err := s.Input(t.Context(), wire.HostScreenInput{Type: wire.InputText, Text: "ab"})
	if err != nil || res != (wire.HostScreenInputResult{Via: wire.InputViaKeys}) {
		t.Fatalf("text answered %+v, %v, want typed into no app", res, err)
	}
	if q := s.host.(*fakeHost).queried; len(q) != 0 {
		t.Fatalf("asked the permission of %q", q)
	}
	want := slices.Concat(lang2Key, []string{"key down 4", "key up 4", "key down 5", "key up 5"})
	if got := whats(performed(t, log, len(want))); !slices.Equal(got, want) {
		t.Fatalf("text performed %q, want %q", got, want)
	}
}
