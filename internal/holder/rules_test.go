package holder

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/screenrules"
	"github.com/obutora/stagent/internal/wire"
)

// A holder reads the screen with the overrides on the host when it
// starts: Codex's question with another header ("Frage 1/2"), which the
// built-in rules do not see, is watched and reported gone once an
// override's header matches it, and session.explain names that override.
func TestHolderReadsOverriddenRules(t *testing.T) {
	l := isolate(t)
	ansi := bytes.Replace(readFixture(t, "codex_question_60x30.ansi"), []byte("Question 1/2"), []byte("Frage 1/2"), 1)
	text := strings.Replace(strings.Join(fixtureText(t, "codex_question_60x30"), "\n"), "Question 1/2", "Frage 1/2", 1)
	if questionShown(strings.Split(text, "\n"), wire.HarnessCodex) {
		t.Fatal("the built-in rules read Frage 1/2 as Codex's question")
	}
	fixture := filepath.Join(t.TempDir(), "frage.ansi")
	if err := os.WriteFile(fixture, ansi, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(l.ScreenRules, 0o700); err != nil {
		t.Fatal(err)
	}
	override := filepath.Join(l.ScreenRules, "codex.json")
	if err := os.WriteFile(override, []byte(`{"question": {"header": "^\\s+Frage \\d+/\\d+"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	d := startFakeDaemon(t, l)
	id, done := startDetached(t, l, 60, 30, "sh", "-c",
		`cat "$0"; read a; printf '\033[2J\033[H Question dismissed\r\n'; read b`, fixture)
	from := 0
	d.waitFor(t, &from, "holder.register", func(m *wire.Msg) bool { return m.Method == wire.MethodHolderRegister })
	d.notifyHolders(t, wire.MethodHolderQuestionWatch, wire.HolderPromptWatchParams{ID: id, Gen: 3, On: true, Harness: wire.HarnessCodex})
	time.Sleep(4 * promptEvalEvery) // the question is up

	c := dialHolder(t, l, id)
	var e wire.ScreenExplain
	if err := c.call(t, wire.MethodSessionExplain, wire.ScreenExplainParams{ID: id, Harness: wire.HarnessCodex}, &e); err != nil {
		t.Fatal(err)
	}
	var q *wire.ScreenCheck
	for i := range e.Checks {
		if e.Checks[i].Check == wire.CheckQuestion {
			q = &e.Checks[i]
		}
	}
	if q == nil || !q.Matched || len(q.Rules) != 1 || q.Rules[0].ID != "codex.question" ||
		q.Rules[0].Origin != "override" || q.Rules[0].File != override ||
		len(q.Lines) != 2 || !strings.Contains(q.Lines[0].Text, "Frage 1/2") {
		b, _ := json.Marshal(e)
		t.Fatalf("session.explain = %s", b)
	}

	c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Submit: true}, nil) // answered
	m := d.waitFor(t, &from, "holder.question_gone", func(m *wire.Msg) bool {
		return m.Method == wire.MethodHolderQuestionGone
	})
	var p wire.HolderPromptGoneParams
	json.Unmarshal(m.Params, &p)
	if p != (wire.HolderPromptGoneParams{ID: id, Gen: 3}) {
		t.Fatalf("holder.question_gone %+v, want watch 3", p)
	}
	c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Submit: true}, nil)
	waitExit(t, done)
}

// A broken override is not used: the holder reads with the built-in rules
// and explain says why the file is not used.
func TestExplainWithBrokenOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "codex.json"), []byte(`{"question": {"header": "("}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newScreenRules(screenrules.Load(dir)).explain(fixtureText(t, "codex_question_60x30"), wire.HarnessCodex)
	if len(e.Files) != 1 || e.Files[0].Used || !strings.Contains(e.Files[0].Error, "question.header") {
		t.Errorf("files = %+v", e.Files)
	}
	q := e.Checks[len(e.Checks)-1]
	if q.Check != wire.CheckQuestion || !q.Matched || q.Rules[0].Origin != "builtin" {
		t.Errorf("question check = %+v", q)
	}
}

// explain names the rules the harnesses added since the screen rules read
// their menus with: Qwen Code's menu told by its own end, OpenCode's and
// Grok Build's dialogs right of a bar, their question dialogs too.
func TestExplainNamesDialogRules(t *testing.T) {
	for _, c := range []struct {
		fixture, harness, check, rule string
		lines                         int
	}{
		{"qwen_approval_shell_60x30", wire.HarnessQwen, wire.CheckPermission, "qwen.menu", 4},
		{"opencode_approval_shell_60x30", wire.HarnessOpenCode, wire.CheckPermission, "opencode.dialogs", 1},
		{"opencode_ask_60x30", wire.HarnessOpenCode, wire.CheckQuestion, "opencode.dialogs", 4},
		{"grok_approval_shell_60x30", wire.HarnessGrok, wire.CheckPermission, "grok.dialogs", 4},
		{"grok_ask_60x30", wire.HarnessGrok, wire.CheckQuestion, "grok.dialogs", 4},
	} {
		e := builtinRules.explain(fixtureText(t, c.fixture), c.harness)
		var got *wire.ScreenCheck
		for i := range e.Checks {
			if e.Checks[i].Check == c.check {
				got = &e.Checks[i]
			}
		}
		if got == nil || !got.Matched || len(got.Rules) == 0 || got.Rules[0].ID != c.rule || got.Rules[0].Origin != "builtin" || len(got.Lines) != c.lines {
			b, _ := json.Marshal(got)
			t.Errorf("%s: %s check = %s, want %s matched on %d lines", c.fixture, c.check, b, c.rule, c.lines)
		}
	}
}

// An override of Grok Build's permission dialog applies: with another key
// hint, its permission menu is no longer one.
func TestOverriddenDialogRules(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "grok.json"), []byte(`{"dialogs": {"permission": {"hint": "^Space:allow"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newScreenRules(screenrules.Load(dir))
	lines := fixtureText(t, "grok_approval_shell_60x30")
	if _, ok := builtinRules.permissionMenu(lines, wire.HarnessGrok); !ok {
		t.Fatal("the built-in rules do not read Grok Build's permission menu")
	}
	if _, ok := r.permissionMenu(lines, wire.HarnessGrok); ok {
		t.Error("an override of the permission dialog's hint did not apply")
	}
	if !r.questionShown(fixtureText(t, "grok_ask_60x30"), wire.HarnessGrok) {
		t.Error("the question dialog the override does not name is no longer read")
	}
}
