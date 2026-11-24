package relay

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/obutora/stagent/internal/transcript"
	"github.com/obutora/stagent/internal/wire"
)

// The relay document (リレーの文書, PROTOCOL.md "The relay document"):
// relay.md, a harness-neutral Markdown file of the conversation, and its
// images beside it.

// DocInput is what the relay document is written from.
type DocInput struct {
	Lang          string           // wire.LangJa → 日本語, anything else → English
	Instruction   string           // "" = the default first instruction of Lang
	Source        wire.RelaySource // the source as the manifest records it
	InPlace       bool             // the target works in the source's own place (nothing brought)
	SourceMayWork bool             // InPlace with source_agent keep: the source agent may still be at work there
	Entries       []transcript.Entry
	Question      *wire.Question  // Session.question when read (nil: none)
	Approvals     []wire.Approval // approvals pending when read
	Tree          *TreeState      // nil: cwd is in no git checkout
	Memo          *MemoDoc        // nil: no memo asked for
	Location      *time.Location  // for the times; nil = time.Local
}

// TreeState is what the document tells of the working tree.
type TreeState struct {
	BundleCommits      int      // commits carried in the bundle (0: none)
	Commits            []string // `git log --oneline base..head`, at most 50
	CommitsMore        int      // how many more there were
	Status             []string // `git status --porcelain=v1` lines, at most 200
	StatusMore         int
	StagedPatchBytes   int64 // 0: no staged patch
	UnstagedPatchBytes int64
	Untracked          []string // carried untracked paths ("/" separated, relative to the worktree root)
	Skipped            []SkippedEntry
	Submodules         []string // `git submodule status` lines (not carried)
	LFS                bool     // the repo uses Git LFS (objects not carried)
}

// SkippedEntry is an untracked entry not carried: Why is "socket", "fifo",
// "nested git checkout", "device", …
type SkippedEntry struct{ Path, Why string }

// MemoDoc is the hand-off memo: Text, or TimedOut when it did not come in
// time.
type MemoDoc struct {
	Text     string // the memo ("" with TimedOut)
	TimedOut bool
}

// DocResult tells what WriteDocument wrote.
type DocResult struct {
	Bytes      int64 // relay.md's size
	Messages   int   // messages under the full conversation
	Images     int   // image files written
	ImageBytes int64
}

// maxDocImage bounds an image the document carries; a larger one is named,
// not carried.
const maxDocImage = 20 << 20

// imageExt are the image files the document writes, by media type.
var imageExt = map[string]string{
	"image/png": "png", "image/jpeg": "jpg", "image/jpg": "jpg", "image/gif": "gif", "image/webp": "webp",
}

// WriteDocument writes dir/relay.md and dir/images/<nnnn>.<ext> (dirs
// 0700, files 0600 on unix whatever the umask). dir exists.
func WriteDocument(dir string, in DocInput) (DocResult, error) {
	d := &document{in: in, t: docTexts(in.Lang), loc: in.Location, dir: dir}
	if d.loc == nil {
		d.loc = time.Local
	}
	if err := d.writeImages(); err != nil {
		return DocResult{}, err
	}
	d.render()
	b := []byte(d.b.String())
	if err := writePrivate(filepath.Join(dir, DocumentFile), b); err != nil {
		return DocResult{}, err
	}
	d.res.Bytes = int64(len(b))
	return d.res, nil
}

type document struct {
	in  DocInput
	t   docText
	loc *time.Location
	dir string
	b   strings.Builder
	res DocResult
	// images[i] are the Markdown lines of entry i's images.
	images map[int][]string
}

// writeImages writes the entries' images to dir/images in order and keeps
// the line each is shown by: a link to its file, or its name when it is
// not carried.
func (d *document) writeImages() error {
	d.images = map[int][]string{}
	n := 0
	madeDir := false
	for i, e := range d.in.Entries {
		for _, img := range e.Images {
			n++
			data, why := d.imageData(img)
			ext := ""
			if why == "" {
				if ext = imageExtOf(img, data); ext == "" {
					why = d.t.imageUnknown
				}
			}
			if why != "" {
				name := fmt.Sprintf("image %d", n)
				if img.Path != "" {
					name += " (" + img.Path + ")"
				}
				d.images[i] = append(d.images[i], fmt.Sprintf(d.t.imageNotCarried, name, why))
				continue
			}
			if !madeDir {
				if err := MkdirPrivate(filepath.Join(d.dir, ImagesDir)); err != nil {
					return err
				}
				madeDir = true
			}
			file := fmt.Sprintf("%04d.%s", n, ext)
			if err := writePrivate(filepath.Join(d.dir, ImagesDir, file), data); err != nil {
				return err
			}
			d.res.Images++
			d.res.ImageBytes += int64(len(data))
			d.images[i] = append(d.images[i], fmt.Sprintf("![image %d](%s/%s)", n, ImagesDir, file))
		}
	}
	return nil
}

// imageData is an image's bytes, or why it is not carried.
func (d *document) imageData(img transcript.Image) ([]byte, string) {
	if len(img.Data) > 0 {
		if len(img.Data) > maxDocImage {
			return nil, d.t.imageTooLarge
		}
		return img.Data, ""
	}
	if img.Path == "" {
		return nil, d.t.imageMissing
	}
	st, err := os.Stat(img.Path)
	if err != nil || !st.Mode().IsRegular() {
		return nil, d.t.imageUnreadable
	}
	if st.Size() > maxDocImage {
		return nil, d.t.imageTooLarge
	}
	b, err := os.ReadFile(img.Path)
	if err != nil || len(b) > maxDocImage {
		return nil, d.t.imageUnreadable
	}
	return b, ""
}

// imageExtOf is the file extension of an image: of its media type, else
// of its bytes, else of its path; "" when it is none of png, jpg, gif and
// webp.
func imageExtOf(img transcript.Image, data []byte) string {
	if ext := imageExt[strings.ToLower(img.MediaType)]; ext != "" {
		return ext
	}
	if ext := imageExt[http.DetectContentType(data)]; ext != "" {
		return ext
	}
	switch ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(img.Path), ".")); ext {
	case "png", "gif", "webp":
		return ext
	case "jpg", "jpeg":
		return "jpg"
	}
	return ""
}

func (d *document) render() {
	t, in := d.t, d.in
	d.line("# " + t.title)
	d.line("")
	d.line(d.intro())
	d.line("")

	d.line("## " + t.firstInstruction)
	d.line("")
	instruction := in.Instruction
	if strings.TrimSpace(instruction) == "" {
		instruction = t.defaultInstruction
	}
	d.line(instruction)
	d.line("")

	d.line("## " + t.summary)
	d.line("")
	d.summaryFacts()
	d.line("")
	d.line("### " + t.firstRequest)
	d.line("")
	if i := d.firstRequest(); i >= 0 {
		d.message(i)
	} else {
		d.line(t.none)
	}
	d.line("")
	d.line("### " + t.lastTurns)
	d.line("")
	d.lastTurns()
	d.line("### " + t.touched)
	d.line("")
	d.touched()
	d.line("")
	d.line("### " + t.tree)
	d.line("")
	d.tree()

	if in.Memo != nil {
		d.line("## " + t.memo)
		d.line("")
		if in.Memo.TimedOut {
			d.line(t.memoTimedOut)
		} else {
			d.line(demoteHeadings(in.Memo.Text, memoHeadingShift))
		}
		d.line("")
	}

	d.line("## " + t.conversation)
	d.line("")
	d.conversation()

	d.notAnswered()
}

func (d *document) line(s string) {
	d.b.WriteString(s)
	d.b.WriteByte('\n')
}

// intro is the paragraph that tells the reading agent what it reads.
func (d *document) intro() string {
	t, in := d.t, d.in
	parts := []string{fmt.Sprintf(t.introSource, in.Source.Harness, in.Source.Host.Label)}
	switch {
	case in.InPlace:
		parts = append(parts, fmt.Sprintf(t.introInPlace, in.Source.Cwd))
		if in.SourceMayWork {
			parts = append(parts, t.introMayWork)
		}
	case in.Tree != nil:
		parts = append(parts, t.introBrought)
	default:
		parts = append(parts, t.introNoTree)
	}
	return strings.Join(append(parts, t.introOrder), t.sentenceSep)
}

// summaryFacts lists the source.
func (d *document) summaryFacts() {
	t, src := d.t, d.in.Source
	d.line(fmt.Sprintf("- %s: %s", t.harness, src.Harness))
	d.line(fmt.Sprintf("- %s: %s", t.host, src.Host.Label))
	d.line(fmt.Sprintf("- %s: %s", t.conversationID, src.ConversationID))
	if src.Title != "" {
		d.line(fmt.Sprintf("- %s: %s", t.titleLabel, src.Title))
	}
	if src.PreparedAt != 0 {
		d.line(fmt.Sprintf("- %s: %s", t.preparedAt, time.UnixMilli(src.PreparedAt).In(d.loc).Format(time.RFC3339)))
	}
	if src.State == wire.StateWorking {
		d.line("")
		d.line(t.wasWorking)
	}
}

// isRequest: a user message the user wrote (an answer to a question is
// not one).
func isRequest(e transcript.Entry) bool { return e.Role == transcript.RoleUser && e.Ask == nil }

func (d *document) firstRequest() int {
	for i, e := range d.in.Entries {
		if isRequest(e) {
			return i
		}
	}
	return -1
}

// message writes entry i's text verbatim and its images.
func (d *document) message(i int) {
	if s := d.in.Entries[i].Text; s != "" {
		d.line(s)
	}
	for _, l := range d.images[i] {
		d.line("")
		d.line(l)
	}
}

// lastTurns writes the last wire.RelayLastTurns turns (a user message and
// everything up to the next one): user and assistant text verbatim, each
// tool call as one line.
func (d *document) lastTurns() {
	es := d.in.Entries
	var starts []int
	for i, e := range es {
		if isRequest(e) {
			starts = append(starts, i)
		}
	}
	if len(starts) == 0 {
		d.line(d.t.none)
		d.line("")
		return
	}
	from := starts[max(0, len(starts)-wire.RelayLastTurns)]
	for i := from; i < len(es); i++ {
		e := es[i]
		switch e.Role {
		case transcript.RoleUser, transcript.RoleAssistant:
			if e.Text == "" && len(d.images[i]) == 0 {
				continue
			}
			d.line(fmt.Sprintf("**%s** %s", e.Role, d.when(e.Time)))
			d.line("")
			d.message(i)
			d.line("")
		case transcript.RoleTool:
			d.line(fmt.Sprintf("- %s `%s`: %s", e.Role, e.ToolName, oneLine(e.Text)))
			d.line("")
		case transcript.RoleSystem:
			d.line(fmt.Sprintf("- %s: %s", e.Role, oneLine(e.Text)))
			d.line("")
		}
	}
}

// touched writes the paths the file-changing tool calls named, in the
// order first touched, relative to the worktree's root when inside it.
func (d *document) touched() {
	var paths []string
	seen := map[string]bool{}
	for _, e := range d.in.Entries {
		if e.Role != transcript.RoleTool {
			continue
		}
		for _, p := range e.Touched {
			if p = d.relPath(p); !seen[p] {
				seen[p] = true
				paths = append(paths, p)
			}
		}
	}
	if len(paths) == 0 {
		d.line(d.t.none)
		return
	}
	for _, p := range paths {
		d.line("- `" + p + "`")
	}
}

// relPath is p (as a tool call wrote it, relative to the source's cwd when
// not absolute) relative to the worktree's root, "/" separated, when it is
// inside it; else p as written.
func (d *document) relPath(p string) string {
	src := d.in.Source
	if src.Cwd == "" || d.in.Tree == nil && src.Repo == "" {
		return p
	}
	cwd := filepath.Clean(src.Cwd)
	root := cwd
	if src.CwdRel != "" {
		rel := filepath.FromSlash(src.CwdRel)
		if !strings.HasSuffix(cwd, string(filepath.Separator)+rel) {
			return p
		}
		root = strings.TrimSuffix(cwd, string(filepath.Separator)+rel)
	}
	abs := filepath.FromSlash(p)
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(cwd, abs)
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return filepath.ToSlash(rel)
}

// tree writes the working tree's state: where it is, its commits and
// status, what was carried and what was not.
func (d *document) tree() {
	t, in, src := d.t, d.in, d.in.Source
	d.line(fmt.Sprintf("- cwd: `%s`", src.Cwd))
	ts := in.Tree
	if ts == nil {
		d.line("")
		d.line(t.noCheckout)
		d.line("")
		return
	}
	for _, kv := range [][2]string{
		{"repo", src.Repo}, {"cwd_rel", src.CwdRel}, {"remote_url", src.RemoteURL},
		{"branch", src.Branch}, {"head", src.Head}, {"base", src.Base},
	} {
		if kv[1] != "" {
			d.line(fmt.Sprintf("- %s: `%s`", kv[0], kv[1]))
		}
	}
	if src.Branch == "" {
		d.line("- branch: " + t.detached)
	}
	d.line("")
	if in.InPlace {
		d.line(t.inPlaceTree)
		d.line("")
	}

	d.line(fmt.Sprintf(t.commits, len(ts.Commits)))
	d.line("")
	if len(ts.Commits) > 0 {
		d.fenced(strings.Join(ts.Commits, "\n"))
		if ts.CommitsMore > 0 {
			d.line(fmt.Sprintf(t.more, ts.CommitsMore))
		}
		d.line("")
	}
	d.line(t.status)
	d.line("")
	if len(ts.Status) == 0 {
		d.line(t.clean)
	} else {
		d.fenced(strings.Join(ts.Status, "\n"))
		if ts.StatusMore > 0 {
			d.line(fmt.Sprintf(t.more, ts.StatusMore))
		}
	}
	d.line("")

	if in.InPlace {
		return
	}
	d.line(t.carried)
	d.line("")
	carried := false
	if ts.BundleCommits > 0 {
		d.line("- " + fmt.Sprintf(t.bundle, ts.BundleCommits))
		carried = true
	}
	if ts.StagedPatchBytes > 0 {
		d.line("- " + fmt.Sprintf(t.stagedPatch, ts.StagedPatchBytes))
		carried = true
	}
	if ts.UnstagedPatchBytes > 0 {
		d.line("- " + fmt.Sprintf(t.unstagedPatch, ts.UnstagedPatchBytes))
		carried = true
	}
	if len(ts.Untracked) > 0 {
		d.line("- " + fmt.Sprintf(t.untracked, len(ts.Untracked)))
		for _, p := range ts.Untracked {
			d.line("  - `" + p + "`")
		}
		carried = true
	}
	if !carried {
		d.line(t.none)
	}
	d.line("")
	d.line(t.notCarried)
	d.line("")
	notCarried := false
	for _, s := range ts.Skipped {
		d.line(fmt.Sprintf("- `%s` (%s)", s.Path, s.Why))
		notCarried = true
	}
	if len(ts.Submodules) > 0 {
		d.line("- " + t.submodules)
		d.line("")
		d.fenced(strings.Join(ts.Submodules, "\n"))
		notCarried = true
	}
	if ts.LFS {
		d.line("- " + t.lfs)
		notCarried = true
	}
	if !notCarried {
		d.line(t.none)
	}
	d.line("")
}

// section is one message of the full conversation: an entry, with the
// outputs of a tool call.
type section struct {
	i       int   // entry index
	outputs []int // a tool call's outputs (entry indexes)
}

// sections groups the entries: a tool output goes under its call (the
// latest one with its CallID still without an output, else the latest
// one with its CallID); one without a call is a section of its own.
func (d *document) sections() []section {
	var out []section
	byID := map[string][]int{} // CallID → section indexes
	for i, e := range d.in.Entries {
		if e.Role == transcript.RoleToolOutput && e.CallID != "" {
			if ss := byID[e.CallID]; len(ss) > 0 {
				at := ss[len(ss)-1]
				for j := len(ss) - 1; j >= 0; j-- {
					if len(out[ss[j]].outputs) == 0 {
						at = ss[j]
						break
					}
				}
				out[at].outputs = append(out[at].outputs, i)
				continue
			}
		}
		if e.Role == transcript.RoleTool && e.CallID != "" {
			byID[e.CallID] = append(byID[e.CallID], len(out))
		}
		out = append(out, section{i: i})
	}
	return out
}

// conversation writes every message in order.
func (d *document) conversation() {
	t := d.t
	for n, s := range d.sections() {
		e := d.in.Entries[s.i]
		role := e.Role
		switch e.Role {
		case transcript.RoleTool, transcript.RoleToolOutput:
			role = transcript.RoleTool
			name := e.ToolName
			if name == "" {
				name = "?"
			}
			role += " · " + name
		}
		head := fmt.Sprintf("### %d · %s", n+1, role)
		if e.Time != 0 {
			head += " · " + d.when(e.Time)
		}
		d.line(head)
		d.line("")
		d.res.Messages++
		switch e.Role {
		case transcript.RoleTool:
			if e.Text != "" {
				d.line(e.Text)
				d.line("")
			}
			if e.Input != "" {
				d.line(t.input)
				d.line("")
				d.fenced(cutToolText(e.Input))
				d.line("")
			}
			for _, o := range s.outputs {
				d.output(o)
			}
		case transcript.RoleToolOutput:
			d.output(s.i)
		default:
			d.message(s.i)
			d.line("")
		}
	}
	if d.res.Messages == 0 {
		d.line(t.none)
		d.line("")
	}
}

// output writes a tool output: its text fenced and cut, and its images.
func (d *document) output(i int) {
	e := d.in.Entries[i]
	d.line(d.t.output)
	d.line("")
	if e.Text != "" {
		d.fenced(cutToolText(e.Text))
		d.line("")
	}
	for _, l := range d.images[i] {
		d.line(l)
		d.line("")
	}
}

// notAnswered writes the approvals pending and the question waiting, when
// there are any.
func (d *document) notAnswered() {
	t, in := d.t, d.in
	q := in.Question
	if q == nil {
		q = d.waitingQuestion()
	}
	if len(in.Approvals) == 0 && q == nil {
		return
	}
	d.line("## " + t.notAnswered)
	d.line("")
	if len(in.Approvals) > 0 {
		d.line(t.approvals)
		d.line("")
		for _, a := range in.Approvals {
			d.line(fmt.Sprintf("- `%s`: %s", a.ToolName, oneLine(a.Summary)))
		}
		d.line("")
	}
	if q != nil {
		d.line(fmt.Sprintf(t.question, q.Tool))
		d.line("")
		for _, it := range q.Questions {
			label := it.Question
			if it.Header != "" {
				label = it.Header + ": " + label
			}
			if it.Multi {
				label += " " + t.multi
			}
			d.line("- " + label)
			for _, o := range it.Options {
				d.line("  - " + o)
			}
		}
		d.line("")
	}
}

// waitingQuestion is the question of a question row at the transcript's
// end (no answer row, nothing else after it), nil when there is none.
func (d *document) waitingQuestion() *wire.Question {
	es := d.in.Entries
	for i := len(es) - 1; i >= 0; i-- {
		e := es[i]
		if e.Role == transcript.RoleToolOutput {
			continue
		}
		if e.Ask == nil || e.Ask.Kind != wire.AskQuestion {
			return nil
		}
		return &wire.Question{Tool: e.ToolName, Questions: e.Ask.Questions}
	}
	return nil
}

// fenced writes s in a fenced block whose fence is longer than any run of
// backticks in s.
func (d *document) fenced(s string) {
	fence := strings.Repeat("`", max(3, longestRun(s, '`')+1))
	d.line(fence)
	d.b.WriteString(s)
	if !strings.HasSuffix(s, "\n") {
		d.b.WriteByte('\n')
	}
	d.line(fence)
}

// longestRun is the length of the longest run of c in s.
func longestRun(s string, c byte) int {
	best, run := 0, 0
	for i := range len(s) {
		if s[i] == c {
			run++
			best = max(best, run)
		} else {
			run = 0
		}
	}
	return best
}

// cutToolText cuts a tool call's input or output to wire.RelayToolTextMax:
// its first wire.RelayToolTextHead bytes and its last
// wire.RelayToolTextMax-wire.RelayToolTextHead, at UTF-8 boundaries, with
// the count of the bytes cut between.
func cutToolText(s string) string {
	if len(s) <= wire.RelayToolTextMax {
		return s
	}
	head := wire.RelayToolTextHead
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	tail := len(s) - (wire.RelayToolTextMax - wire.RelayToolTextHead)
	for tail < len(s) && !utf8.RuneStart(s[tail]) {
		tail++
	}
	return fmt.Sprintf("%s\n[… %d bytes cut …]\n%s", s[:head], tail-head, s[tail:])
}

// when is a time in the document's location, RFC 3339; "" for none.
func (d *document) when(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.UnixMilli(ms).In(d.loc).Format(time.RFC3339)
}

// oneLine collapses whitespace.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// docText is the document's wording in one language.
type docText struct {
	title, firstInstruction, summary, firstRequest, lastTurns, touched, tree string
	memo, conversation, notAnswered                                          string
	defaultInstruction                                                       string

	introSource, introInPlace, introMayWork, introBrought, introNoTree, introOrder string
	sentenceSep                                                                    string // between the sentences of a paragraph

	harness, host, conversationID, titleLabel, preparedAt, wasWorking string

	none, memoTimedOut, input, output string

	noCheckout, detached, inPlaceTree, commits, more, status, clean string
	carried, bundle, stagedPatch, unstagedPatch, untracked          string
	notCarried, submodules, lfs                                     string

	approvals, question, multi string

	imageNotCarried, imageTooLarge, imageUnreadable, imageMissing, imageUnknown string
}

func docTexts(lang string) docText {
	if lang == wire.LangJa {
		return docText{
			title: "リレーの文書", firstInstruction: "最初の指示", summary: "まとめ",
			firstRequest: "最初の依頼", lastTurns: "最後のやり取り", touched: "触ったファイル",
			tree: "作業ツリーの状態", memo: "引き継ぎのメモ", conversation: "会話の全文",
			notAnswered:        "答えていない承認・質問",
			defaultInstruction: "リレーの文書を読んで続けて",

			introSource:  "あなたは、別の agent（%s、ホスト %s）がしていた会話の続きを引き受けます。",
			sentenceSep:  "",
			introInPlace: "元の agent と同じ場所（`%s`）で作業します。作業ツリーは運んでいません。",
			introMayWork: "元の agent はまだそこで作業しているかもしれません。同じファイルを同時に変えないよう気をつけてください。",
			introBrought: "作業ツリー（まだどの remote にもない commit、commit していない変更、追跡していないファイル）はここに運ばれています。",
			introNoTree:  "元の作業場所は git のチェックアウトではなかったので、作業ツリーは運ばれていません。",
			introOrder:   "この文書には、まずまとめ、そのあとに会話の全文があります。",

			harness: "元のハーネス", host: "ホスト", conversationID: "会話 ID", titleLabel: "タイトル",
			preparedAt: "用意した時刻",
			wasWorking: "このとき元の agent は作業中でした。最後のターンは途中で切れているかもしれません。",

			none: "（なし）", memoTimedOut: "引き継ぎのメモを頼みましたが、時間内に届きませんでした。",
			input: "入力:", output: "出力:",

			noCheckout:  "git のチェックアウトではありません。運んだものはありません。",
			detached:    "（detached HEAD）",
			inPlaceTree: "元の場所で作業するので、作業ツリーからは何も運んでいません。",
			commits:     "まだどの remote にもない commit（%d 件、`git log --oneline base..head`）:",
			more:        "ほかに %d 行",
			status:      "`git status --porcelain=v1`:",
			clean:       "（変更なし）",
			carried:     "運んだもの:", bundle: "bundle の commit %d 件",
			stagedPatch: "staged の patch（%d バイト）", unstagedPatch: "unstaged の patch（%d バイト）",
			untracked:  "追跡していないファイル %d 件:",
			notCarried: "運んでいないもの:",
			submodules: "submodule の状態（submodule の中身は運んでいません）:",
			lfs:        "Git LFS のオブジェクト（このリポジトリは Git LFS を使っています）",

			approvals: "答えを待っている承認:",
			question:  "答えを待っている質問（%s）:",
			multi:     "（複数選べる）",

			imageNotCarried: "%s: 運んでいません（%s）",
			imageTooLarge:   "20 MiB を超える",
			imageUnreadable: "ファイルを読めない",
			imageMissing:    "transcript に中身がない",
			imageUnknown:    "形式が png, jpg, gif, webp のどれでもない",
		}
	}
	return docText{
		title: "Relay document", firstInstruction: "First instruction", summary: "Summary",
		firstRequest: "First request", lastTurns: "Last turns", touched: "Touched files",
		tree: "Working tree", memo: "Hand-off memo", conversation: "Conversation",
		notAnswered:        "Not answered",
		defaultInstruction: "Read the relay document and continue.",

		introSource:  "You are taking over a conversation another agent (%s, host %s) had.",
		sentenceSep:  " ",
		introInPlace: "You work in the same place as that agent (`%s`); no working tree was brought.",
		introMayWork: "That agent may still be at work there: take care not to change the same files at the same time.",
		introBrought: "Its working tree (the commits no remote has, the uncommitted changes, the untracked files) has been brought here.",
		introNoTree:  "Its working directory was in no git checkout, so no working tree was brought.",
		introOrder:   "This document has a summary first and the whole conversation after it.",

		harness: "Source harness", host: "Host", conversationID: "Conversation id", titleLabel: "Title",
		preparedAt: "Prepared at",
		wasWorking: "The source agent was working at that moment: its last turn may be cut off.",

		none: "(none)", memoTimedOut: "A hand-off memo was asked for and did not come in time.",
		input: "Input:", output: "Output:",

		noCheckout:  "Not a git checkout: nothing was carried.",
		detached:    "(detached HEAD)",
		inPlaceTree: "The work goes on in the source's own place: nothing of the working tree was carried.",
		commits:     "Commits no remote has (%d, `git log --oneline base..head`):",
		more:        "%d more lines",
		status:      "`git status --porcelain=v1`:",
		clean:       "(no changes)",
		carried:     "Carried:", bundle: "a bundle of %d commits",
		stagedPatch: "the staged patch (%d bytes)", unstagedPatch: "the unstaged patch (%d bytes)",
		untracked:  "%d untracked files:",
		notCarried: "Not carried:",
		submodules: "the submodules' state (their contents were not carried):",
		lfs:        "Git LFS objects (the repository uses Git LFS)",

		approvals: "Approvals waiting for an answer:",
		question:  "Question waiting for an answer (%s):",
		multi:     "(several may be chosen)",

		imageNotCarried: "%s: not carried (%s)",
		imageTooLarge:   "over 20 MiB",
		imageUnreadable: "its file cannot be read",
		imageMissing:    "the transcript holds no data of it",
		imageUnknown:    "not a png, jpg, gif or webp",
	}
}

// memoHeadingShift is how many levels the memo's headings go down in the
// document: under its "## " section they come out as "### " and deeper.
const memoHeadingShift = 2

// demoteHeadings moves the Markdown headings of text by levels down (at
// most to level 6): ATX headings ("## x") and setext ones (a line under
// "===" or "---", made ATX), leaving fenced code blocks as they are.
func demoteHeadings(text string, levels int) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	fence := ""   // the open code fence's marker, "" outside one
	para := false // the line before is paragraph text (a setext title)
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		if len(line)-len(trimmed) > 3 {
			out, para = append(out, line), false
			continue
		}
		switch {
		case fence != "":
			if strings.HasPrefix(trimmed, fence) && strings.Trim(strings.TrimSpace(trimmed), fence[:1]) == "" {
				fence = ""
			}
			out, para = append(out, line), false
		case openFence(trimmed) != "":
			fence = openFence(trimmed)
			out, para = append(out, line), false
		case atxLevel(trimmed) > 0:
			n := atxLevel(trimmed)
			out, para = append(out, strings.Repeat("#", min(n+levels, 6))+trimmed[n:]), false
		case para && setextLevel(trimmed) > 0:
			title := strings.TrimSpace(out[len(out)-1])
			out[len(out)-1] = strings.Repeat("#", min(setextLevel(trimmed)+levels, 6)) + " " + title
			para = false
		default:
			out, para = append(out, line), isParagraphLine(line)
		}
	}
	return strings.Join(out, "\n")
}

// openFence is the marker of a code fence line opens (``` or ~~~, three or
// more), "" when it opens none.
func openFence(trimmed string) string {
	for _, c := range []string{"`", "~"} {
		n := len(trimmed) - len(strings.TrimLeft(trimmed, c))
		if n >= 3 {
			return strings.Repeat(c, n)
		}
	}
	return ""
}

// atxLevel is the level of an ATX heading line (1–6 #, then a space or
// nothing), 0 when it is none.
func atxLevel(trimmed string) int {
	n := len(trimmed) - len(strings.TrimLeft(trimmed, "#"))
	if n == 0 || n > 6 || n < len(trimmed) && trimmed[n] != ' ' && trimmed[n] != '\t' {
		return 0
	}
	return n
}

// setextLevel is 1 for a "===" underline, 2 for "---", 0 otherwise.
func setextLevel(trimmed string) int {
	t := strings.TrimRight(trimmed, " \t")
	switch {
	case t == "":
		return 0
	case strings.Trim(t, "=") == "":
		return 1
	case strings.Trim(t, "-") == "":
		return 2
	}
	return 0
}

// isParagraphLine: a line a setext underline makes a heading of — not
// blank, not a heading or list item or quote already.
func isParagraphLine(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" || atxLevel(t) > 0 || strings.HasPrefix(t, ">") {
		return false
	}
	for _, p := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(t, p) {
			return false
		}
	}
	return true
}
