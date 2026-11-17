package holder

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/ipc"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/screenrules"
	"github.com/obutora/stagent/internal/wire"
)

// `stagent explain`: which screen rule matched a screen, as the holder
// reads it — a live session's (session.explain, the holder's own rules)
// or a saved one's (the rules in force on the host now).

const explainUsage = `usage: stagent explain (--session ID | --file SCREEN) [--harness H] [--json]

Reads a screen with the screen rules as a session's holder does and shows
which rule matched each check (menu, permission, question), the lines it
matched and whether the rule is built in or from an override file in
~/.ssh-term/agent/screen-rules.

`

// explain makes the checks the holder makes on lines for a session of
// harnessID: a menu of any harness (a chat message is refused on it), the
// permission menu of a prompt watch and the question UI of a question
// watch. For harnessID "" (no harness in particular) that last one is the
// question UI of each harness that draws its own, the menus being read
// already.
func (r *screenRules) explain(lines []string, harnessID string) wire.ScreenExplain {
	e := wire.ScreenExplain{Harness: harnessID, RulesDir: r.set.Dir, Files: r.set.FileReport()}
	e.Checks = append(e.Checks, r.checkMenu(lines), r.checkPermission(lines, harnessID))
	if harnessID != "" {
		return addCheck(e, r.checkQuestion(lines, harnessID))
	}
	for _, h := range harness.All {
		if r.styles[h.ID].question != nil {
			e = addCheck(e, r.checkQuestion(lines, h.ID))
		}
	}
	return e
}

func addCheck(e wire.ScreenExplain, c wire.ScreenCheck) wire.ScreenExplain {
	e.Checks = append(e.Checks, c)
	return e
}

func (r *screenRules) checkMenu(lines []string) wire.ScreenCheck {
	c := wire.ScreenCheck{Check: wire.CheckMenu, Rules: []wire.ScreenRuleUse{}, Lines: []wire.ScreenLine{}}
	if m, ok := r.findMenu(lines, ""); ok {
		r.menuMatched(&c, lines, m)
	}
	return c
}

func (r *screenRules) checkPermission(lines []string, harnessID string) wire.ScreenCheck {
	c := wire.ScreenCheck{Check: wire.CheckPermission, Harness: harnessID, Rules: []wire.ScreenRuleUse{}, Lines: []wire.ScreenLine{}}
	if m, ok := r.findMenu(lines, harnessID); ok && m.permission(lines) {
		r.menuMatched(&c, lines, m)
		c.Signature = menuSignature(lines, m.options[0].start, m.edge())
	} else {
		c.Rules = r.triedRules(r.styles[harnessID])
	}
	return c
}

func (r *screenRules) checkQuestion(lines []string, harnessID string) wire.ScreenCheck {
	c := wire.ScreenCheck{Check: wire.CheckQuestion, Harness: harnessID, Rules: []wire.ScreenRuleUse{}, Lines: []wire.ScreenLine{}}
	if shown := r.styles[harnessID].question; shown != nil {
		c.Rules = append(c.Rules, r.ruleUse(harnessID, r.styles[harnessID].questionRule))
		if at, ok := shown(lines); ok {
			c.Matched = true
			for _, j := range at {
				c.Lines = append(c.Lines, screenLine(lines, j))
			}
		}
		return c
	}
	if m, ok := r.findMenu(lines, harnessID); ok {
		r.menuMatched(&c, lines, m)
	} else {
		c.Rules = r.triedRules(r.styles[harnessID])
	}
	return c
}

// menuMatched fills c with menu m: its rules and its options' lines.
func (r *screenRules) menuMatched(c *wire.ScreenCheck, lines []string, m foundMenu) {
	c.Matched = true
	c.Rules = r.menuRules(m)
	for _, it := range m.options {
		for j := it.start; j <= it.end; j++ {
			if n := len(c.Lines); n > 0 && c.Lines[n-1].Line == j+1 {
				continue // options on one row
			}
			c.Lines = append(c.Lines, screenLine(lines, j))
		}
	}
}

// menuRules are the rules menu m is read with: its harness's dialogs for
// one of its dialogs right of a bar, else its menu (and its menu without
// numbers) and the common ones.
func (r *screenRules) menuRules(m foundMenu) []wire.ScreenRuleUse {
	s := m.style
	if m.dialog != "" {
		return []wire.ScreenRuleUse{r.ruleUse(s.id, "dialogs")}
	}
	var out []wire.ScreenRuleUse
	if s.id != "" {
		out = append(out, r.ruleUse(s.id, "menu"))
		if m.unnumbered {
			out = append(out, r.ruleUse(s.id, "unnumbered"))
		}
	}
	return append(out, r.ruleUse(screenrules.Common, "menu"))
}

// triedRules are the rules a menu of style s was looked for with, when
// none matched; none for a harness without menus of its own (the other
// harnesses' were tried).
func (r *screenRules) triedRules(s menuStyle) []wire.ScreenRuleUse {
	out := []wire.ScreenRuleUse{}
	if s.cursor != "" {
		out = r.menuRules(foundMenu{style: s})
	}
	if s.dialogs != nil {
		out = append(out, r.ruleUse(s.id, "dialogs"))
	}
	return out
}

func (r *screenRules) ruleUse(name, rule string) wire.ScreenRuleUse {
	u := wire.ScreenRuleUse{ID: name + "." + rule, Origin: "builtin", Rule: r.set.Rule(name, rule)}
	if f := r.set.Origin(name, rule); f != "" {
		u.Origin, u.File = "override", f
	}
	return u
}

func screenLine(lines []string, j int) wire.ScreenLine {
	return wire.ScreenLine{Line: j + 1, Text: strings.TrimRight(lines[j], " ")}
}

// explainHarness is the harness a session's screen is read as: asked, or
// else the session's; "" for a program that is no harness.
func (h *Holder) explainHarness(asked string) string {
	if asked == "" {
		asked = h.session().Harness
	}
	if _, ok := h.rules.styles[asked]; !ok {
		return ""
	}
	return asked
}

// Explain is the `stagent explain` entry point.
func Explain(args []string) int {
	var session, file, harnessID string
	var asJSON bool
	fs := flag.NewFlagSet("stagent explain", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), explainUsage)
		fs.PrintDefaults()
	}
	fs.StringVar(&session, "session", "", "read the screen of session `ID` (stagent ls), with the rules its holder started with")
	fs.StringVar(&file, "file", "", "read the screen saved in `SCREEN` (plain text, a row per line, e.g. tmux capture-pane -p; - for stdin), with the rules in force now")
	fs.StringVar(&harnessID, "harness", "", "read the screen as harness `H` draws it (default: the session's; for a file, no harness in particular)")
	fs.BoolVar(&asJSON, "json", false, "print the result as JSON")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() > 0 || (session == "") == (file == "") {
		fs.Usage()
		return ExitUsage
	}
	if harnessID != "" && !slices.ContainsFunc(harness.All, func(h harness.Harness) bool { return h.ID == harnessID }) {
		fmt.Fprintf(os.Stderr, "stagent explain: no harness %q\n", harnessID)
		return ExitUsage
	}
	if session != "" && !validID(session) {
		fmt.Fprintf(os.Stderr, "stagent explain: invalid session id %q (want 16 lowercase hex characters)\n", session)
		return ExitUsage
	}
	l, err := paths.Resolve()
	if err != nil {
		fmt.Fprintf(os.Stderr, "stagent explain: %v\n", err)
		return ExitFailure
	}
	var e wire.ScreenExplain
	var what string
	if session != "" {
		what = "session " + session
		e, err = explainSession(l, session, harnessID)
	} else {
		what = "file " + file
		var lines []string
		if lines, err = readScreen(file); err == nil {
			e = newScreenRules(screenrules.Load(l.ScreenRules)).explain(lines, harnessID)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "stagent explain: %v\n", err)
		return ExitFailure
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(e); err != nil {
			return ExitFailure
		}
		return 0
	}
	writeExplain(os.Stdout, what, e)
	return 0
}

// explainSession asks the holder of session id to explain its screen.
func explainSession(l *paths.Layout, id, harnessID string) (wire.ScreenExplain, error) {
	var e wire.ScreenExplain
	conn, err := ipc.Dial(l.HolderAddr(id), 2*time.Second)
	if err != nil {
		if oe := l.ForeignOwned(); oe != nil {
			return e, oe
		}
		return e, fmt.Errorf("no holder answers for session %s (stagent ls lists the live ones): %w", id, err)
	}
	c := rpc.NewClient(conn, nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = c.Call(ctx, wire.MethodSessionExplain, wire.ScreenExplainParams{ID: id, Harness: harnessID}, &e)
	if we, ok := errors.AsType[*wire.Error](err); ok {
		if we.Code == wire.ErrUnknownMethod {
			return e, fmt.Errorf("the holder of session %s is from a stagent before explain; start the session again, or save its screen and use --file", id)
		}
		return e, errors.New(we.Message)
	}
	return e, err
}

// readScreen reads a saved screen: a row per line.
func readScreen(file string) ([]string, error) {
	var r io.Reader = os.Stdin
	if file != "-" {
		f, err := os.Open(file)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	var lines []string
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		lines = append(lines, strings.TrimSuffix(sc.Text(), "\r"))
	}
	return lines, sc.Err()
}

// writeExplain prints e for a person.
func writeExplain(w io.Writer, what string, e wire.ScreenExplain) {
	as := e.Harness
	if as == "" {
		as = "no harness in particular"
	}
	fmt.Fprintf(w, "screen: %s, read as %s\n", what, as)
	if len(e.Files) == 0 {
		fmt.Fprintf(w, "rules: built-in (no overrides in %s)\n", e.RulesDir)
	} else {
		fmt.Fprintf(w, "rules: built-in, with the overrides in %s:\n", e.RulesDir)
		for _, f := range e.Files {
			if f.Used {
				fmt.Fprintf(w, "  %s: used\n", f.Path)
			} else {
				fmt.Fprintf(w, "  %s: not used, the built-in rules apply: %s\n", f.Path, f.Error)
			}
		}
	}
	for _, c := range e.Checks {
		name := c.Check + " (any harness)"
		if c.Harness != "" {
			name = c.Check + " (" + c.Harness + ")"
		}
		result, verb := "no match", "tried"
		if c.Matched {
			result, verb = "match", "rule"
		}
		fmt.Fprintf(w, "\n%s: %s\n", name, result)
		for _, u := range c.Rules {
			origin := "built-in"
			if u.Origin == "override" {
				origin = "override " + u.File
			}
			fmt.Fprintf(w, "  %s %s (%s): %s\n", verb, u.ID, origin, u.Rule)
		}
		for _, ln := range c.Lines {
			fmt.Fprintf(w, "  line %d: %s\n", ln.Line, ln.Text)
		}
		if c.Signature != "" {
			fmt.Fprintf(w, "  signature: %s\n", c.Signature)
		}
	}
}
