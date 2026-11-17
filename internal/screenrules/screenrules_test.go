package screenrules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/obutora/stagent/internal/harness"
)

// Every harness of the registry has a built-in file.
func TestEveryHarnessHasBuiltinRules(t *testing.T) {
	for _, h := range harness.All {
		if _, err := builtinFS.ReadFile("builtin/" + h.ID + ".json"); err != nil {
			t.Errorf("harness %s has no internal/screenrules/builtin/%s.json", h.ID, h.ID)
		}
	}
}

func writeRules(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// An override replaces the members it names, keeps the others built-in,
// and is the origin of the rules it names only.
func TestOverrideIsLaidOverBuiltin(t *testing.T) {
	dir := t.TempDir()
	path := writeRules(t, dir, "codex.json", `{"question": {"header": "^\\s+Frage \\d+/\\d+"}}`)
	s := Load(dir)
	if len(s.Files) != 1 || s.Files[0].Err != nil {
		t.Fatalf("files = %+v", s.Files)
	}
	q := s.Harnesses["codex"].Question
	if !q.Header.MatchString("  Frage 1/2") || q.Header.MatchString("  Question 1/2") {
		t.Errorf("header = %s", q.Header)
	}
	if !q.Hint.MatchString("enter to submit all") || q.HintRows != 3 {
		t.Errorf("the members the override does not name changed: hint %s, hint_rows %d", q.Hint, q.HintRows)
	}
	if got := s.Origin("codex", "question"); got != path {
		t.Errorf("origin of codex.question = %q, want %q", got, path)
	}
	if got := s.Origin("codex", "menu"); got != "" {
		t.Errorf("origin of codex.menu = %q, want built-in", got)
	}
	if got := string(s.Rule("codex", "question")); !strings.Contains(got, `"header":"^\\s+Frage \\d+/\\d+"`) {
		t.Errorf("rule codex.question = %s", got)
	}
}

// null turns a rule off.
func TestOverrideNullRemovesRule(t *testing.T) {
	dir := t.TempDir()
	writeRules(t, dir, "codex.json", `{"question": null}`)
	s := Load(dir)
	if s.Files[0].Err != nil || s.Harnesses["codex"].Question != nil || s.Harnesses["codex"].Menu == nil {
		t.Fatalf("files %+v, codex %+v", s.Files, s.Harnesses["codex"])
	}
}

// A file that does not make valid rules is not used, and says why; the
// rules it would override stay built-in, other files still apply.
func TestBrokenOverrideKeepsBuiltin(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct{ file, content, err string }{
		{"claude.json", `{"menu": {"cursr": "x"}}`, `unknown field "cursr"`},
		{"codex.json", `{"question": {"header": "("}}`, "question.header: error parsing regexp"},
		{"common.json", `{"menu": {"hint_rows": 0}}`, "menu.hint_rows: must be 1 or more"},
		{"copilot.json", `{"menu": {"dialog_edge": "rule"}}`, `menu.dialog_edge: "rule" is not`},
		{"gemini.json", `{}`, `no rules are called "gemini"`},
		{"pi.json", `{} {}`, "after top-level value"},
		{"omp.json", `{"question": {"kind": "box"}}`, `question.kind: "box" is not`},
		{"qwen.json", `{"menu": {"ends": {"rows": 0}}}`, "menu.ends.rows: must be 1 or more"},
		{"qwen.json", `{"menu": {"ends": {"permission": false}}}`, "menu.permission_label: missing"},
		{"opencode.json", `{"dialogs": {"title_mark": ""}}`, "dialogs.title_mark: missing"},
		{"opencode.json", `{"dialogs": {"permission": {"row": null}}}`, "dialogs.permission.row: missing"},
		{"grok.json", `{"dialogs": {"question": {"kind": "list"}}}`, `dialogs.question.kind: "list" is not`},
		{"grok.json", `{"dialogs": {"permission": {"hint_place": "above"}}}`, `dialogs.permission.hint_place: "above" is not`},
		{"grok.json", `{"dialogs": {"trust": {"rows": ["("]}}}`, "dialogs.trust.rows[0]: error parsing regexp"},
	} {
		writeRules(t, dir, c.file, c.content)
		s := Load(dir)
		var got error
		for _, f := range s.Files {
			if filepath.Base(f.Path) == c.file {
				got = f.Err
			}
		}
		if got == nil || !strings.Contains(got.Error(), c.err) {
			t.Errorf("%s %s: error %v, want %q", c.file, c.content, got, c.err)
		}
		os.Remove(filepath.Join(dir, c.file))
	}

	writeRules(t, dir, "claude.json", `{"menu": {"cursor": ""}}`)
	writeRules(t, dir, "codex.json", `{"menu": {"cursor": ">"}}`)
	s := Load(dir)
	if c := s.Harnesses["claude"].Menu.Cursor; c != "❯" {
		t.Errorf("claude's cursor = %q after a broken override, want the built-in ❯", c)
	}
	if c := s.Harnesses["codex"].Menu.Cursor; c != ">" {
		t.Errorf("codex's cursor = %q, want the override's", c)
	}
	if s.Origin("claude", "menu") != "" {
		t.Error("a broken override is the origin of claude.menu")
	}
}

// An override of a dialog right of a bar replaces the members it names
// of that dialog only: the other dialogs and members stay built-in.
func TestOverrideOfDialogIsLaidOverBuiltin(t *testing.T) {
	dir := t.TempDir()
	path := writeRules(t, dir, "grok.json", `{"dialogs": {"permission": {"hint": "^Space:allow"}}}`)
	s := Load(dir)
	if len(s.Files) != 1 || s.Files[0].Err != nil {
		t.Fatalf("files = %+v", s.Files)
	}
	d := s.Harnesses["grok"].Dialogs
	if p := d.Permission; !p.Hint.MatchString("Space:allow") || p.Kind != DialogRadioRows || p.HintPlace != HintOutsideBar {
		t.Errorf("permission = %+v", p)
	}
	if d.Bar != "┃" || d.Question == nil || !d.Question.Hint.MatchString("↑/↓ navigate · y copy  Enter:submit") || d.Trust == nil {
		t.Errorf("the members the override does not name changed: %+v", d)
	}
	if got := s.Origin("grok", "dialogs"); got != path {
		t.Errorf("origin of grok.dialogs = %q, want %q", got, path)
	}
}
