// Package screenrules holds the rules the holder reads coding agents'
// screens with (internal/holder/prompt.go): how each harness draws its
// menus and question UIs — its cursor, the patterns of its key hints and
// headers, how many rows to look at, and which of the holder's ways of
// finding a dialog's top edge, frame or question UI applies. What can
// only be written as code (walking the rows, the box characters) stays in
// the holder; the rules pick it by name.
//
// The rules stagent ships are embedded, one file per harness and one
// shared by all (builtin/<name>.json). A host overrides them with a file
// of the same name in ~/.ssh-term/agent/screen-rules
// (paths.Layout.ScreenRules), so a screen that changed can be followed
// without a new stagent release; a holder reads them when it starts. An
// override is laid over its built-in file: the members it names replace
// the built-in ones (an object member by member), null turns a rule off.
// A file that cannot be read or does not make valid rules is not used —
// those rules stay built-in — and Set.Files says why (stagent doctor,
// stagent explain, the holder's log).
package screenrules

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/wire"
)

//go:embed builtin/*.json
var builtinFS embed.FS

// Common is the name of the file of the rules every harness shares.
const Common = "common"

// Ways of finding the top edge of a harness's dialog (Menu.DialogEdge):
// the nearest line above the options that is one.
const (
	// EdgeSolidRule: a solid rule (─━═▔) starting in the first column.
	EdgeSolidRule = "solid_rule"
	// EdgeFirstColumn: a line with text in its first column.
	EdgeFirstColumn = "first_column"
	// EdgeBoxTop: a line whose text starts with ╭.
	EdgeBoxTop = "box_top"
	// EdgeShallowIndent: a line with text indented two columns at most.
	EdgeShallowIndent = "shallow_indent"
)

// FrameBox is Menu.Frame of a harness that draws its menus inside a
// `│ … │` box: they are read there only, a wrapped label continuing at
// the column of its number.
const FrameBox = "box"

// WrapFirstColumn is Menu.Wrap of a harness that wraps a long option's
// label to the first column of the lines under it, not under the label
// (Antigravity CLI): such a line right under an option continues its
// label.
const WrapFirstColumn = "first_column"

// UnnumberedTitledDialog is Unnumbered.Kind of a menu without numbers
// that only a dialog of a given title has: options in one column, one
// under the menu's cursor, a key hint below them.
const UnnumberedTitledDialog = "titled_dialog"

// Kinds of question UIs (Question.Kind).
const (
	// QuestionHintAndHeader: a key hint among the last rows of the screen
	// and, above it, a header with nothing between starting in the first
	// column; or, when the header scrolled away, indented rows reaching
	// the top of the screen.
	QuestionHintAndHeader = "hint_and_header"
	// QuestionBottomBox: a box at the bottom of the screen whose top edge
	// matches a title; or, when that edge scrolled away, whose rows reach
	// the top of the screen with an option under a cursor.
	QuestionBottomBox = "bottom_box"
)

// Ways of finding the top edge of a dialog drawn right of a bar
// (Dialogs.DialogEdge): the nearest line above the options that is one.
const (
	// EdgeOutsideBar: a line that is not in the bar.
	EdgeOutsideBar = "outside_bar"
	// EdgeTitleMark: a line holding Dialogs.TitleMark.
	EdgeTitleMark = "title_mark"
)

// Kinds of dialogs drawn right of a bar (Dialog.Kind).
const (
	// DialogChoiceRow: all options on one row matching Row, two or more
	// spaces apart, without numbers or a cursor; Hint on one of the
	// HintRows non-blank lines under it.
	DialogChoiceRow = "choice_row"
	// DialogNumberedRows: options numbered `1. label` (a description
	// indented under each) without a cursor, right above the lowest line
	// matching Hint.
	DialogNumberedRows = "numbered_rows"
	// DialogRadioRows: consecutive rows in the bar numbered from 1 with a
	// radio mark after the number (`2 (●) label`; z, the row to type an
	// answer in, last), ● on the highlighted one; Hint on one of the
	// HintRows non-blank lines under them.
	DialogRadioRows = "radio_rows"
	// DialogTitledRows: under a line reading Title, consecutive lines
	// matching Rows one by one; an option's label is its text up to two
	// spaces.
	DialogTitledRows = "titled_rows"
)

// Where a dialog's key hint is (Dialog.HintPlace): anywhere ("").
const (
	// HintInBar: in the bar.
	HintInBar = "bar"
	// HintOutsideBar: not in the bar (below it).
	HintOutsideBar = "outside_bar"
)

// Regexp is a regular expression of a rules file, its source in JSON;
// validating the rules compiles it.
type Regexp struct {
	*regexp.Regexp
	src string
}

func (r *Regexp) UnmarshalJSON(b []byte) error {
	var s *string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	r.Regexp, r.src = nil, ""
	if s != nil {
		r.src = *s
	}
	return nil
}

func (r *Regexp) MarshalJSON() ([]byte, error) {
	if r == nil || r.src == "" {
		return []byte("null"), nil
	}
	return json.Marshal(r.src)
}

// compile compiles the pattern of field (missing: an error).
func (r *Regexp) compile(field string) error {
	if r == nil || r.src == "" {
		return errors.New(field + ": missing")
	}
	re, err := regexp.Compile(r.src)
	if err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	r.Regexp = re
	return nil
}

// CommonRules are the rules of every harness's menus (common.json).
type CommonRules struct {
	Menu *CommonMenu `json:"menu"`
}

// CommonMenu: a menu has a key hint (Hint) within HintRows rows under its
// options, or nothing but blanks below them; a transcript entry there
// (TranscriptMark) makes the list part of the conversation.
type CommonMenu struct {
	Hint           *Regexp `json:"hint"`
	HintRows       int     `json:"hint_rows"`
	TranscriptMark *Regexp `json:"transcript_mark"`
}

// Rules are the rules of one harness (<harness>.json); a harness that
// draws no menu or question UI of its own has none.
type Rules struct {
	Menu       *Menu       `json:"menu,omitempty"`
	Unnumbered *Unnumbered `json:"unnumbered,omitempty"`
	Question   *Question   `json:"question,omitempty"`
	Dialogs    *Dialogs    `json:"dialogs,omitempty"`
}

// Menu is how the harness draws its numbered menus: Cursor marks the
// highlighted option, DialogEdge (Edge*) finds the dialog's top edge (the
// text from there to the options identifies a permission prompt), Frame
// ("" or FrameBox) says whether they are in a box, Wrap ("" or
// WrapFirstColumn) where a long option's label wraps to ("": indented
// under it), and a menu whose first option (number 1) matches
// PermissionLabel is a permission prompt. Ends tells a menu with no key
// hint below it; with Ends.Permission, such a menu is a permission prompt
// too, and PermissionLabel may be left out.
type Menu struct {
	Cursor          string  `json:"cursor"`
	DialogEdge      string  `json:"dialog_edge"`
	Frame           string  `json:"frame,omitempty"`
	Wrap            string  `json:"wrap,omitempty"`
	PermissionLabel *Regexp `json:"permission_label,omitempty"`
	Ends            *Ends   `json:"ends,omitempty"`
}

// Ends tells a numbered menu of the harness that has no key hint below
// it by its own end: the label of its last option matches LastLabel, and
// within Rows rows under that option there is nothing but blanks before a
// line matching Below (a spinner) or the bottom of the screen. ScrollBar
// is the scroll bar the harness draws in the last column, left out of
// both. With Permission, the first option (number 1) of such a menu
// allows the call whatever its label (a translated one): the menu is a
// permission prompt.
type Ends struct {
	LastLabel  *Regexp `json:"last_label"`
	Below      *Regexp `json:"below"`
	Rows       int     `json:"rows"`
	ScrollBar  string  `json:"scroll_bar,omitempty"`
	Permission bool    `json:"permission,omitempty"`
}

// Unnumbered is a menu of the harness without numbers (Kind
// UnnumberedTitledDialog), recognized in the dialog Title only: the first
// line under its top edge (Menu.DialogEdge), its options under
// Menu.Cursor.
type Unnumbered struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
}

// Question is the UI of a question the harness asked through its question
// tool, for a harness that draws it as no menu of its own (Kind
// Question*). QuestionHintAndHeader uses Header, Hint and HintRows (the
// rows from the bottom the hint may be on); QuestionBottomBox uses Title
// and Cursor.
type Question struct {
	Kind     string  `json:"kind"`
	Header   *Regexp `json:"header,omitempty"`
	Hint     *Regexp `json:"hint,omitempty"`
	HintRows int     `json:"hint_rows,omitempty"`
	Title    *Regexp `json:"title,omitempty"`
	Cursor   string  `json:"cursor,omitempty"`
}

// Dialogs are the menus of a harness that draws its dialogs right of a
// bar (Bar, `┃`) and tells them by their rows and key hints rather than
// by a cursor: its permission dialog (Permission), its question tool's
// (Question, the harness's question UI) and its folder trust prompt
// (Trust), each read the way its Kind (Dialog*) names; the first of them
// the screen shows is the harness's menu. DialogEdge (EdgeOutsideBar, or
// EdgeTitleMark with TitleMark) finds a dialog's top edge. ScrollBar is
// the scroll bar the harness draws in the last column, left out.
type Dialogs struct {
	Bar        string  `json:"bar"`
	ScrollBar  string  `json:"scroll_bar,omitempty"`
	DialogEdge string  `json:"dialog_edge"`
	TitleMark  string  `json:"title_mark,omitempty"`
	Permission *Dialog `json:"permission,omitempty"`
	Question   *Dialog `json:"question,omitempty"`
	Trust      *Dialog `json:"trust,omitempty"`
}

// Dialog is one dialog of Dialogs, read the way Kind (Dialog*) names:
// DialogChoiceRow uses Row, Hint and HintRows; DialogNumberedRows uses
// Hint; DialogRadioRows uses Hint and HintRows; DialogTitledRows uses
// Title and Rows. HintPlace (Hint*) says where the hint is.
type Dialog struct {
	Kind      string    `json:"kind"`
	Row       *Regexp   `json:"row,omitempty"`
	Hint      *Regexp   `json:"hint,omitempty"`
	HintRows  int       `json:"hint_rows,omitempty"`
	HintPlace string    `json:"hint_place,omitempty"`
	Title     string    `json:"title,omitempty"`
	Rows      []*Regexp `json:"rows,omitempty"`
}

// Set is the rules in force: built-in, with the overrides of a directory
// laid over them.
type Set struct {
	// Dir is the override directory read ("" for Builtin).
	Dir       string
	Common    CommonRules
	Harnesses map[string]Rules
	// Files are the override files found in Dir, by name; Err is why one
	// is not used.
	Files []File
	// origin holds, by rule id ("<name>.<rule>"), the override file that
	// set the rule.
	origin map[string]string
}

// File is one override file.
type File struct {
	Path string
	// Name is the file's name without .json: Common or a harness id.
	Name string
	// Err is why the file is not used; nil when it is.
	Err error
}

// Builtin returns the rules stagent ships.
func Builtin() *Set {
	s := &Set{Harnesses: map[string]Rules{}, origin: map[string]string{}}
	var err error
	if s.Common, err = layered(Common, nil, (*CommonRules).validate); err != nil {
		panic("screenrules: builtin/common.json: " + err.Error())
	}
	for _, h := range harness.All {
		r, err := layered(h.ID, nil, (*Rules).validate)
		if err != nil {
			panic("screenrules: builtin/" + h.ID + ".json: " + err.Error())
		}
		s.Harnesses[h.ID] = r
	}
	return s
}

// Load returns the built-in rules with the overrides in dir laid over
// them. A missing dir has none.
func Load(dir string) *Set {
	s := Builtin()
	s.Dir = dir
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.Files = append(s.Files, File{Path: dir, Err: err})
		}
		return s
	}
	for _, e := range entries {
		base, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() {
			continue
		}
		f := File{Path: filepath.Join(dir, e.Name()), Name: base}
		f.Err = s.apply(f)
		s.Files = append(s.Files, f)
	}
	return s
}

// FileReport lists the override files as `stagent doctor` and `stagent
// explain` report them.
func (s *Set) FileReport() []wire.ScreenRulesFile {
	out := []wire.ScreenRulesFile{}
	for _, f := range s.Files {
		r := wire.ScreenRulesFile{Path: f.Path, Name: f.Name, Used: f.Err == nil}
		if f.Err != nil {
			r.Error = f.Err.Error()
		}
		out = append(out, r)
	}
	return out
}

// apply lays override file f over the rules, unless it is broken.
func (s *Set) apply(f File) error {
	b, err := os.ReadFile(f.Path)
	if err != nil {
		return err
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(b, &members); err != nil || members == nil {
		if err == nil {
			err = errors.New("not a JSON object")
		}
		return err
	}
	switch _, known := s.Harnesses[f.Name]; {
	case f.Name == Common:
		c, err := layered(Common, b, (*CommonRules).validate)
		if err != nil {
			return err
		}
		s.Common = c
	case known:
		r, err := layered(f.Name, b, (*Rules).validate)
		if err != nil {
			return err
		}
		s.Harnesses[f.Name] = r
	default:
		names := []string{Common}
		for _, h := range harness.All {
			names = append(names, h.ID)
		}
		return fmt.Errorf("no rules are called %q (the files are %s.json)", f.Name, strings.Join(names, ".json, "))
	}
	for rule := range members {
		s.origin[f.Name+"."+rule] = f.Path
	}
	return nil
}

// Origin returns the override file that set rule (menu, unnumbered,
// question, dialogs) of name (Common or a harness id), "" when it is
// built-in.
func (s *Set) Origin(name, rule string) string {
	return s.origin[name+"."+rule]
}

// Rule returns rule of name as its file has it, null when there is none.
func (s *Set) Rule(name, rule string) json.RawMessage {
	var v any
	if name == Common {
		v = s.Common
	} else {
		v = s.Harnesses[name]
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	var members map[string]json.RawMessage
	if enc.Encode(v) != nil || json.Unmarshal(buf.Bytes(), &members) != nil || members[rule] == nil {
		return json.RawMessage("null")
	}
	return members[rule]
}

func builtinFile(name string) []byte {
	b, err := builtinFS.ReadFile("builtin/" + name + ".json")
	if err != nil {
		return []byte("{}")
	}
	return b
}

// layered decodes the built-in file of name and, over it, override (nil
// for none), and validates the result.
func layered[T any](name string, override []byte, validate func(*T) error) (T, error) {
	var v T
	if err := overlay(&v, builtinFile(name)); err != nil {
		return v, err
	}
	if override != nil {
		if err := overlay(&v, override); err != nil {
			return v, err
		}
	}
	return v, validate(&v)
}

// overlay decodes the JSON object b over v: its members replace v's,
// member by member for an object, null clears one. Unknown members are an
// error.
func overlay[T any](v *T, b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// validate checks the rules and compiles their patterns.
func (c *CommonRules) validate() error {
	m := c.Menu
	switch {
	case m == nil:
		return errors.New("menu: missing")
	case m.HintRows < 1:
		return errors.New("menu.hint_rows: must be 1 or more")
	}
	if err := m.Hint.compile("menu.hint"); err != nil {
		return err
	}
	return m.TranscriptMark.compile("menu.transcript_mark")
}

// validate checks the rules and compiles their patterns.
func (r *Rules) validate() error {
	if m := r.Menu; m != nil {
		switch {
		case m.Cursor == "":
			return errors.New("menu.cursor: missing")
		case m.DialogEdge != EdgeSolidRule && m.DialogEdge != EdgeFirstColumn && m.DialogEdge != EdgeBoxTop && m.DialogEdge != EdgeShallowIndent:
			return fmt.Errorf("menu.dialog_edge: %q is not %s, %s, %s or %s", m.DialogEdge, EdgeSolidRule, EdgeFirstColumn, EdgeBoxTop, EdgeShallowIndent)
		case m.Frame != "" && m.Frame != FrameBox:
			return fmt.Errorf("menu.frame: %q is not %s", m.Frame, FrameBox)
		case m.Wrap != "" && m.Wrap != WrapFirstColumn:
			return fmt.Errorf("menu.wrap: %q is not %s", m.Wrap, WrapFirstColumn)
		case m.Wrap != "" && (m.Frame != "" || m.Ends != nil):
			return errors.New("menu.wrap: not with frame or ends")
		}
		if m.PermissionLabel != nil || m.Ends == nil || !m.Ends.Permission {
			if err := m.PermissionLabel.compile("menu.permission_label"); err != nil {
				return err
			}
		}
		if e := m.Ends; e != nil {
			if e.Rows < 1 {
				return errors.New("menu.ends.rows: must be 1 or more")
			}
			if err := e.LastLabel.compile("menu.ends.last_label"); err != nil {
				return err
			}
			if err := e.Below.compile("menu.ends.below"); err != nil {
				return err
			}
		}
	}
	if u := r.Unnumbered; u != nil {
		switch {
		case r.Menu == nil:
			return errors.New("unnumbered: needs menu (its cursor and dialog_edge)")
		case u.Kind != UnnumberedTitledDialog:
			return fmt.Errorf("unnumbered.kind: %q is not %s", u.Kind, UnnumberedTitledDialog)
		case u.Title == "":
			return errors.New("unnumbered.title: missing")
		}
	}
	if q := r.Question; q != nil {
		switch q.Kind {
		case QuestionHintAndHeader:
			if err := q.Header.compile("question.header"); err != nil {
				return err
			}
			if err := q.Hint.compile("question.hint"); err != nil {
				return err
			}
			if q.HintRows < 1 {
				return errors.New("question.hint_rows: must be 1 or more")
			}
		case QuestionBottomBox:
			if err := q.Title.compile("question.title"); err != nil {
				return err
			}
			if q.Cursor == "" {
				return errors.New("question.cursor: missing")
			}
		default:
			return fmt.Errorf("question.kind: %q is not %s or %s", q.Kind, QuestionHintAndHeader, QuestionBottomBox)
		}
	}
	if d := r.Dialogs; d != nil {
		if err := d.validate(r.Question != nil); err != nil {
			return err
		}
	}
	return nil
}

// validate checks the dialogs and compiles their patterns; question: the
// harness has a question rule, which a question dialog would contradict.
func (d *Dialogs) validate(question bool) error {
	switch {
	case d.Bar == "":
		return errors.New("dialogs.bar: missing")
	case d.DialogEdge != EdgeOutsideBar && d.DialogEdge != EdgeTitleMark:
		return fmt.Errorf("dialogs.dialog_edge: %q is not %s or %s", d.DialogEdge, EdgeOutsideBar, EdgeTitleMark)
	case d.DialogEdge == EdgeTitleMark && d.TitleMark == "":
		return errors.New("dialogs.title_mark: missing")
	case question && d.Question != nil:
		return errors.New("dialogs.question: the harness has a question rule already")
	}
	for _, c := range []struct {
		name string
		d    *Dialog
	}{{"permission", d.Permission}, {"question", d.Question}, {"trust", d.Trust}} {
		if c.d != nil {
			if err := c.d.validate("dialogs." + c.name); err != nil {
				return err
			}
		}
	}
	return nil
}

// validate checks dialog field and compiles its patterns.
func (d *Dialog) validate(field string) error {
	if d.HintPlace != "" && d.HintPlace != HintInBar && d.HintPlace != HintOutsideBar {
		return fmt.Errorf("%s.hint_place: %q is not %s or %s", field, d.HintPlace, HintInBar, HintOutsideBar)
	}
	switch d.Kind {
	case DialogChoiceRow, DialogRadioRows:
		if d.Kind == DialogChoiceRow {
			if err := d.Row.compile(field + ".row"); err != nil {
				return err
			}
		}
		if d.HintRows < 1 {
			return errors.New(field + ".hint_rows: must be 1 or more")
		}
		return d.Hint.compile(field + ".hint")
	case DialogNumberedRows:
		return d.Hint.compile(field + ".hint")
	case DialogTitledRows:
		if d.Title == "" {
			return errors.New(field + ".title: missing")
		}
		if len(d.Rows) == 0 {
			return errors.New(field + ".rows: missing")
		}
		for i, row := range d.Rows {
			if err := row.compile(fmt.Sprintf("%s.rows[%d]", field, i)); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("%s.kind: %q is not %s, %s, %s or %s", field, d.Kind, DialogChoiceRow, DialogNumberedRows, DialogRadioRows, DialogTitledRows)
}
