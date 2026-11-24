package transcript

import (
	"bytes"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

// What each fixture's entries hold beyond its chat rows: a tool output's
// text, a path a file-changing call touched (none: the fixture has no such
// call) and its thinking, never read.
var entryFixtureWant = map[string]struct {
	output, touched, thinking string
	images                    int
}{
	wire.HarnessClaude:      {"content 5", "/home/user/proj/main.go", "thinking 1", 1},
	wire.HarnessCodex:       {"output 1", "/home/user/proj/config.toml", "reasoning summary 1", 1},
	wire.HarnessOmp:         {"text 3", "src/file2.ts", "thinking 1", 3},
	wire.HarnessPi:          {"FAIL src/config.test.ts", "src/config.ts", "Run the tests first.", 1},
	wire.HarnessCopilot:     {"package config", "", "The user wants the test fixed.", 0},
	wire.HarnessQwen:        {"Output: hello-from-mock", "/home/user/proj/mock_write.txt", "Weighing the hunks first.", 0},
	wire.HarnessOpenCode:    {"Created file successfully: mock_write.txt", "mock_write.txt", "Let me look.", 0},
	wire.HarnessGrok:        {"exit: 0\nhello-from-mock", "/home/u/proj/mock_write.txt", "Reading the diff first.", 0},
	wire.HarnessAntigravity: {"HTTP/2 200", "/tmp/outside.txt", "Reading the headers.", 1},
}

func TestReadEntriesFixtures(t *testing.T) {
	for h, want := range entryFixtureWant {
		t.Run(h, func(t *testing.T) {
			es, err := ReadEntries(fixture(h), h, 0)
			if err != nil {
				t.Fatal(err)
			}
			// The chat view is the entries shaped in one place.
			if got := strip(messages(es)); !reflect.DeepEqual(got, fixtureWant[h]) {
				t.Fatalf("messages of the entries:\n got %v\nwant %v", got, fixtureWant[h])
			}
			var outputs, touched []string
			images := 0
			for _, e := range es {
				if strings.Contains(e.Text, want.thinking) || strings.Contains(e.Input, want.thinking) {
					t.Errorf("thinking in %+v", e)
				}
				if e.Role == RoleToolOutput {
					outputs = append(outputs, e.Text)
				}
				if e.Role == RoleTool && e.Input == "" && e.Text != "" && e.Ask == nil {
					t.Errorf("tool call without its input: %+v", e)
				}
				touched = append(touched, e.Touched...)
				images += len(e.Images)
			}
			if !slices.ContainsFunc(outputs, func(s string) bool { return strings.Contains(s, want.output) }) {
				t.Errorf("no tool output holding %q in %q", want.output, outputs)
			}
			if want.touched != "" && !slices.Contains(touched, want.touched) {
				t.Errorf("touched %q, want %q among them", touched, want.touched)
			}
			if want.touched == "" && len(touched) > 0 {
				t.Errorf("touched %q, want none", touched)
			}
			if images != want.images {
				t.Errorf("%d images, want %d", images, want.images)
			}
		})
	}
}

// Images come decoded when the transcript holds them, as a path when it
// names a file.
func TestReadEntriesImages(t *testing.T) {
	es, err := ReadEntries(fixture(wire.HarnessPi), wire.HarnessPi, 0)
	if err != nil {
		t.Fatal(err)
	}
	var img *Image
	for _, e := range es {
		if len(e.Images) > 0 {
			img = &e.Images[0]
		}
	}
	if img == nil || img.MediaType != "image/png" || !bytes.Equal(img.Data, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatalf("pi image %+v", img)
	}
	es, err = ReadEntries(fixture(wire.HarnessAntigravity), wire.HarnessAntigravity, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		for _, img := range e.Images {
			if e.Role != RoleToolOutput || img.Data != nil || !strings.HasSuffix(img.Path, "/logs/media/step_6_media_0.jpg") || img.MediaType != "image/jpeg" {
				t.Fatalf("antigravity image %+v of %+v", img, e)
			}
		}
	}
}

// ReadEntries reads up to end, complete records only.
func TestReadEntriesEnd(t *testing.T) {
	src, err := os.ReadFile(fixture(wire.HarnessOmp))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.SplitAfter(src, []byte("\n"))
	whole := 0
	for _, l := range lines[:5] { // the fifth record is the first prompt
		whole += len(l)
	}
	end := int64(whole + 10) // inside the sixth record
	es, err := ReadEntries(fixture(wire.HarnessOmp), wire.HarnessOmp, end)
	if err != nil {
		t.Fatal(err)
	}
	head, err := ReadEntries(fixture(wire.HarnessOmp), wire.HarnessOmp, int64(whole))
	if err != nil {
		t.Fatal(err)
	}
	if len(es) == 0 || !reflect.DeepEqual(es, head) {
		t.Fatalf("entries up to %d:\n got %+v\nwant %+v", end, es, head)
	}
}

func TestTouchedPaths(t *testing.T) {
	cases := []struct {
		name, input string
		want        []string
	}{
		{"Edit", `{"file_path":"/p/a.go","old_string":"x","new_string":"y"}`, []string{"/p/a.go"}},
		{"NotebookEdit", `{"notebook_path":"/p/n.ipynb"}`, []string{"/p/n.ipynb"}},
		{"write", `{"path":"src/b.ts","content":"x"}`, []string{"src/b.ts"}},
		{"create", `{"path":"/p/c.go","file_text":"x"}`, []string{"/p/c.go"}},
		{"replace", `{"file_path":"/p/d.go"}`, []string{"/p/d.go"}},
		{"edit_file", `{"Path":"/p/e.go","Target":"a","Replacement":"b"}`, []string{"/p/e.go"}},
		{"apply_patch", "*** Begin Patch\n*** Update File: a.go\n*** Move to: b.go\n*** Add File: c.go\n*** Delete File: d.go\n*** End Patch", []string{"a.go", "b.go", "c.go", "d.go"}},
		// A patch in a JSON string (OpenCode's patch, a shell heredoc,
		// Codex's arguments string).
		{"patch", `{"patchText":"*** Begin Patch\n*** Update File: x.go\n*** End Patch"}`, []string{"x.go"}},
		{"exec_command", `"{\"cmd\":\"apply_patch <<'EOF'\\n*** Begin Patch\\n*** Add File: y.go\\n*** End Patch\\nEOF\"}"`, []string{"y.go"}},
		{"Read", `{"file_path":"/p/a.go"}`, nil},
		{"Bash", `{"command":"ls"}`, nil},
	}
	for _, c := range cases {
		if got := touchedPaths(c.name, c.input); !reflect.DeepEqual(got, c.want) {
			t.Errorf("touchedPaths(%s, %s) = %q, want %q", c.name, c.input, got, c.want)
		}
	}
}
