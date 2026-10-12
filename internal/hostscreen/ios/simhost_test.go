package ios

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// macTools skips a test without macOS's sqlite3 and plutil.
func macTools(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("sqlite3 and plutil are read from /usr/bin of macOS")
	}
}

func TestPasteAuthReadsTheSimulatorsTCCDatabase(t *testing.T) {
	macTools(t)
	data := t.TempDir()
	db := filepath.Join(data, "Library", "TCC", "TCC.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o700); err != nil {
		t.Fatal(err)
	}
	// TCC.db's access table, as a simulator's (iOS 27.0), with the rows
	// iOS adds.
	schema := `create table access (service text not null, client text not null, client_type integer not null, auth_value integer not null, auth_reason integer not null, auth_version integer not null, indirect_object_identifier_type integer, flags integer);
insert into access values ('kTCCServicePasteboard', 'com.obutora.sshtermxxx', 0, 2, 4, 2, 0, 0);
insert into access values ('kTCCServicePasteboard', 'com.apple.mobilesafari', 0, 3, 4, 2, 0, 0);
insert into access values ('kTCCServicePhotos', 'com.example.photos', 0, 2, 4, 2, 0, 0);`
	if out, err := exec.Command(sqlite3, db, schema).CombinedOutput(); err != nil {
		t.Fatalf("sqlite3: %v: %s", err, out)
	}
	before, _ := os.ReadFile(db)
	h := &simctlHost{sim: simulator{UDID: "U1", DataPath: data}, run: run}
	for bundle, want := range map[string]pasteAuth{
		"com.obutora.sshtermxxx": pasteAllowed,
		"com.apple.mobilesafari": pasteRefused,
		"com.example.photos":     pasteNoRow, // another service's row
		"com.example.new":        pasteNoRow,
		"x' or '1'='1":           pasteRefused, // no bundle id: not asked
	} {
		if got := h.pasteAuth(t.Context(), bundle); got != want {
			t.Errorf("pasteAuth(%q) = %v, want %v", bundle, got, want)
		}
	}
	if after, _ := os.ReadFile(db); !slices.Equal(before, after) {
		t.Fatal("pasteAuth changed TCC.db")
	}
	// A simulator whose TCC.db cannot be read allows nothing.
	none := &simctlHost{sim: simulator{UDID: "U1", DataPath: t.TempDir()}, run: run}
	if got := none.pasteAuth(t.Context(), "com.obutora.sshtermxxx"); got != pasteRefused {
		t.Fatalf("pasteAuth without TCC.db = %v, want refused", got)
	}
}

// plist writes a property list of json to path, binary as a simulator's.
func plist(t *testing.T, path, json string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(json), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(plutil, "-convert", "binary1", path).CombinedOutput(); err != nil {
		t.Fatalf("plutil: %v: %s", err, out)
	}
}

func TestPasteLabelsAreUIKitsInTheSimulatorsLanguageThenJapaneseAndEnglish(t *testing.T) {
	macTools(t)
	data, root := t.TempDir(), t.TempDir()
	uikit := filepath.Join(root, "System", "Library", "PrivateFrameworks", "UIKitCore.framework")
	plist(t, filepath.Join(uikit, "zh_CN.lproj", "Localizable.strings"), `{"Paste": "粘贴", "Select": "选择"}`)
	plist(t, filepath.Join(uikit, "ja.lproj", "Localizable.strings"), `{"Paste": "ペースト"}`)
	prefs := filepath.Join(data, "Library", "Preferences", ".GlobalPreferences.plist")
	const runtimeID = "com.apple.CoreSimulator.SimRuntime.iOS-27-0"
	runtimes := `{"runtimes": [{"identifier": "com.apple.CoreSimulator.SimRuntime.iOS-26-2", "runtimeRoot": "/nowhere"},
  {"identifier": "` + runtimeID + `", "runtimeRoot": "` + root + `"}]}`
	h := &simctlHost{sim: simulator{UDID: "U1", DataPath: data, RuntimeID: runtimeID}, xcrun: "xcrun",
		run: func(ctx context.Context, env []string, path string, args ...string) ([]byte, error) {
			if path == "xcrun" {
				if !slices.Equal(args, []string{"simctl", "list", "runtimes", "--json"}) {
					t.Errorf("xcrun %q", args)
				}
				return []byte(runtimes), nil
			}
			return run(ctx, env, path, args...)
		}}
	for _, c := range []struct {
		langs string
		want  []string
	}{
		{`["zh-Hans-CN", "en-US"]`, []string{"粘贴", pasteJa, pasteEn}},
		{`["ja-JP"]`, []string{pasteJa, pasteEn}},
		{`["de-DE"]`, []string{pasteJa, pasteEn}}, // no de.lproj
		{`[]`, []string{pasteJa, pasteEn}},
	} {
		plist(t, prefs, `{"AppleLanguages": `+c.langs+`}`)
		if got := h.pasteLabels(t.Context()); !slices.Equal(got, c.want) {
			t.Errorf("AppleLanguages %s: pasteLabels = %q, want %q", c.langs, got, c.want)
		}
	}
}

func TestLprojsTryTheWholeLanguageThenItsRegionThenItself(t *testing.T) {
	for lang, want := range map[string][]string{
		"ja-JP":      {"ja_JP", "ja"},
		"en":         {"en"},
		"zh-Hans-CN": {"zh_Hans_CN", "zh_CN", "zh"},
		"":           nil,
	} {
		if got := lprojs(lang); !slices.Equal(got, want) {
			t.Errorf("lprojs(%q) = %q, want %q", lang, got, want)
		}
	}
}
