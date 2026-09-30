package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/obutora/stagent/internal/wire"
)

// Roots are the directories each harness keeps its conversations in.
type Roots struct {
	Claude string // <claude config dir>/projects
	Codex  string // codex home; rollouts under Codex/sessions
	Omp    string // ~/.omp/agent/sessions
}

// DefaultRoots resolves the roots for a home directory, honoring
// CLAUDE_CONFIG_DIR and CODEX_HOME like the harnesses do. getenv is the
// environment the harness runs with (os.Getenv for stagent's own).
func DefaultRoots(home string, getenv func(string) string) Roots {
	claude := getenv("CLAUDE_CONFIG_DIR")
	if claude == "" {
		claude = filepath.Join(home, ".claude")
	}
	codex := getenv("CODEX_HOME")
	if codex == "" {
		codex = filepath.Join(home, ".codex")
	}
	return Roots{
		Claude: filepath.Join(claude, "projects"),
		Codex:  codex,
		Omp:    filepath.Join(home, ".omp", "agent", "sessions"),
	}
}

func (r Roots) codexSessions() string { return filepath.Join(r.Codex, "sessions") }

// HarnessOf reports which harness's transcript directory holds path. Only
// .jsonl files under a known root qualify, so transcript requests cannot be
// pointed at arbitrary files.
func (r Roots) HarnessOf(path string) (string, bool) {
	if !filepath.IsAbs(path) || !strings.EqualFold(filepath.Ext(path), ".jsonl") {
		return "", false
	}
	path = filepath.Clean(path)
	for _, c := range []struct{ harness, root string }{
		{wire.HarnessClaude, r.Claude},
		{wire.HarnessCodex, r.codexSessions()},
		{wire.HarnessOmp, r.Omp},
	} {
		if c.root == "" {
			continue
		}
		rel, err := filepath.Rel(filepath.Clean(c.root), path)
		if err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
			return c.harness, true
		}
	}
	return "", false
}

// entry is the cached metadata of one transcript file.
type entry struct {
	Harness string `json:"harness"`
	ID      string `json:"id,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
	Mtime   int64  `json:"mtime"` // unix ns
	Size    int64  `json:"size"`
	// Skip marks automation / sub-agent runs that are never listed.
	Skip bool `json:"skip,omitempty"`
	// Scanned is how far the file was read (incremental rescans).
	Scanned int64 `json:"scanned,omitempty"`
	// Checked: the Claude entrypoint was seen (first occurrence decides).
	Checked bool `json:"checked,omitempty"`
	// Titles holds Claude title candidates by record type; Title is omp's.
	Titles map[string]string `json:"titles,omitempty"`
	Title  string            `json:"title,omitempty"`
	// Prompt is the first user prompt (Codex title fallback).
	Prompt string `json:"prompt,omitempty"`
}

type indexFile struct {
	Version int               `json:"version"`
	Entries map[string]*entry `json:"entries"`
}

const indexVersion = 1

// Index lists conversations of every harness, caching per-file metadata in
// a JSON file keyed by (path, mtime, size) so only changed files are read.
// Deleting the file only costs a rescan.
type Index struct {
	path  string
	roots Roots

	mu      sync.Mutex
	entries map[string]*entry
	dirty   bool

	// Codex thread names (session_index.jsonl), cached by mtime/size.
	threadNames     map[string]string
	threadNamesStat [2]int64
}

// OpenIndex loads the cache at path (missing or corrupt → empty).
func OpenIndex(path string, roots Roots) *Index {
	x := &Index{path: path, roots: roots, entries: map[string]*entry{}}
	if b, err := os.ReadFile(path); err == nil {
		var f indexFile
		if json.Unmarshal(b, &f) == nil && f.Version == indexVersion && f.Entries != nil {
			x.entries = f.Entries
		}
	}
	return x
}

// Roots returns the roots the index scans.
func (x *Index) Roots() Roots { return x.roots }

type fileStat struct {
	path  string
	mtime int64
	size  int64
}

// List returns the newest conversations of the given harnesses (all when
// empty), at most limit per harness, newest first.
func (x *Index) List(harnesses []string, limit int) ([]wire.Conversation, error) {
	if limit <= 0 {
		limit = DefaultLimit
	}
	if len(harnesses) == 0 {
		harnesses = []string{wire.HarnessClaude, wire.HarnessCodex, wire.HarnessOmp}
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	var out []wire.Conversation
	for _, h := range harnesses {
		files, err := x.enumerate(h)
		if err != nil {
			return nil, err
		}
		x.prune(h, files)
		sort.Slice(files, func(i, j int) bool { return files[i].mtime > files[j].mtime })
		var names map[string]string
		if h == wire.HarnessCodex {
			names = x.codexThreadNames()
		}
		n := 0
		for _, f := range files {
			if n >= limit {
				break
			}
			e := x.entries[f.path]
			if e == nil || e.Mtime != f.mtime || e.Size != f.size {
				e = x.refresh(h, f, e)
				x.entries[f.path] = e
				x.dirty = true
			}
			if e.Skip || e.ID == "" || e.Cwd == "" {
				continue
			}
			out = append(out, wire.Conversation{
				Harness:   h,
				ID:        e.ID,
				Cwd:       e.Cwd,
				Title:     e.title(names),
				UpdatedAt: f.mtime / 1e6,
				Path:      f.path,
			})
			n++
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	x.save()
	return out, nil
}

// Find returns the transcript path of a harness's conversation id.
func (x *Index) Find(harness, id string) (string, bool) {
	if id == "" {
		return "", false
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	for p, e := range x.entries {
		if e.Harness == harness && e.ID == id {
			if _, err := os.Stat(p); err == nil {
				return p, true
			}
		}
	}
	files, err := x.enumerate(harness)
	if err != nil {
		return "", false
	}
	for _, f := range files {
		base := filepath.Base(f.path)
		switch harness {
		case wire.HarnessClaude:
			if base == id+".jsonl" {
				return f.path, true
			}
		case wire.HarnessCodex:
			if strings.HasSuffix(base, "-"+id+".jsonl") {
				return f.path, true
			}
		case wire.HarnessOmp:
			if strings.HasSuffix(base, "_"+id+".jsonl") {
				return f.path, true
			}
		}
	}
	return "", false
}

// Conversation returns the metadata of one transcript file, re-reading it
// only when it changed since the last call. ok is false when the file is
// unreadable or is one List would skip (automation or sub-agent run, no id
// yet).
func (x *Index) Conversation(harness, path string) (c wire.Conversation, ok bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return wire.Conversation{}, false
	}
	f := fileStat{path, info.ModTime().UnixNano(), info.Size()}
	x.mu.Lock()
	defer x.mu.Unlock()
	e := x.entries[path]
	if e == nil || e.Harness != harness || e.Mtime != f.mtime || e.Size != f.size {
		e = x.refresh(harness, f, e)
		x.entries[path] = e
		x.dirty = true
		x.save()
	}
	if e.Skip || e.ID == "" {
		return wire.Conversation{}, false
	}
	var names map[string]string
	if harness == wire.HarnessCodex {
		names = x.codexThreadNames()
	}
	return wire.Conversation{
		Harness:   harness,
		ID:        e.ID,
		Cwd:       e.Cwd,
		Title:     e.title(names),
		UpdatedAt: f.mtime / 1e6,
		Path:      path,
	}, true
}

func (e *entry) title(codexNames map[string]string) string {
	switch e.Harness {
	case wire.HarnessClaude:
		for _, k := range []string{"custom-title", "ai-title", "summary", "last-prompt"} {
			if t := oneLine(e.Titles[k], maxSummary); t != "" {
				return t
			}
		}
	case wire.HarnessCodex:
		if t := oneLine(codexNames[e.ID], maxSummary); t != "" {
			return t
		}
		return oneLine(e.Prompt, maxSummary)
	case wire.HarnessOmp:
		return oneLine(e.Title, maxSummary)
	}
	return ""
}

func (x *Index) prune(harness string, files []fileStat) {
	live := make(map[string]bool, len(files))
	for _, f := range files {
		live[f.path] = true
	}
	for p, e := range x.entries {
		if e.Harness == harness && !live[p] {
			delete(x.entries, p)
			x.dirty = true
		}
	}
}

func (x *Index) save() {
	if !x.dirty || x.path == "" {
		return
	}
	b, err := json.Marshal(indexFile{Version: indexVersion, Entries: x.entries})
	if err != nil {
		return
	}
	if err := writeFileAtomic(x.path, b); err == nil {
		x.dirty = false
	}
}

// writeFileAtomic writes b to path via a temporary file and rename (0600).
func writeFileAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	cerr := f.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmp, 0o600)); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Enumeration

var claudeIDName = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\.jsonl$`)

// enumerate lists a harness's top-level conversation files. Sub-agent
// transcripts live one directory deeper and are not matched.
func (x *Index) enumerate(harness string) ([]fileStat, error) {
	switch harness {
	case wire.HarnessClaude:
		return listTwoLevels(x.roots.Claude, func(name string) bool { return claudeIDName.MatchString(name) })
	case wire.HarnessOmp:
		return listTwoLevels(x.roots.Omp, func(name string) bool { return strings.HasSuffix(name, ".jsonl") })
	case wire.HarnessCodex:
		var out []fileStat
		root := x.roots.codexSessions()
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if p == root {
					return fs.SkipAll
				}
				return nil
			}
			if d.IsDir() {
				if p != root && strings.Count(strings.TrimPrefix(p, root), string(filepath.Separator)) > 3 {
					return fs.SkipDir
				}
				return nil
			}
			name := d.Name()
			if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
				return nil
			}
			if info, err := d.Info(); err == nil {
				out = append(out, fileStat{p, info.ModTime().UnixNano(), info.Size()})
			}
			return nil
		})
		return out, err
	}
	return nil, nil
}

// listTwoLevels lists <root>/<dir>/<file> entries whose name matches.
func listTwoLevels(root string, match func(string) bool) ([]fileStat, error) {
	dirs, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []fileStat
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		dir := filepath.Join(root, d.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !match(f.Name()) {
				continue
			}
			if info, err := f.Info(); err == nil {
				out = append(out, fileStat{filepath.Join(dir, f.Name()), info.ModTime().UnixNano(), info.Size()})
			}
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Metadata extraction

// refresh (re)reads the metadata of f. An append-only grown file with a
// previous entry is read only from where the last scan stopped.
func (x *Index) refresh(harness string, f fileStat, prev *entry) *entry {
	grown := prev != nil && prev.Harness == harness && prev.Scanned > 0 &&
		f.size >= prev.Size && f.mtime >= prev.Mtime && prev.Scanned <= f.size
	var e *entry
	if grown {
		c := *prev
		e = &c
	} else {
		e = &entry{Harness: harness}
	}
	e.Mtime, e.Size = f.mtime, f.size
	switch harness {
	case wire.HarnessClaude:
		e.ID = strings.TrimSuffix(filepath.Base(f.path), ".jsonl")
		scanClaude(f.path, e)
	case wire.HarnessCodex:
		scanCodex(f.path, e)
	case wire.HarnessOmp:
		scanOmp(f.path, e)
	}
	return e
}

var (
	cwdKey        = []byte(`"cwd":"`)
	entrypointKey = []byte(`"entrypoint":"`)
	claudeTitle   = map[string]string{
		"custom-title": "customTitle", "ai-title": "aiTitle",
		"summary": "summary", "last-prompt": "lastPrompt",
	}
)

// scanClaude reads from e.Scanned: the first cwd, the first entrypoint
// (sdk-* runs are automation and skipped) and the latest title records.
func scanClaude(path string, e *entry) {
	forLines(path, e.Scanned, 0, func(line []byte, complete bool) bool {
		if e.Cwd == "" {
			if s, ok := stringAfter(line, cwdKey); ok {
				e.Cwd = s
			}
		}
		if !e.Checked {
			if s, ok := stringAfter(line, entrypointKey); ok {
				e.Checked = true
				if strings.HasPrefix(s, "sdk") {
					e.Skip = true
					return false
				}
			}
		}
		if complete && bytes.HasPrefix(line, []byte(`{"type":"`)) {
			var r map[string]json.RawMessage
			if t, ok := stringAfter(line, []byte(`{"type":"`)); ok {
				if field, want := claudeTitle[t]; want && json.Unmarshal(line, &r) == nil {
					if v, ok := jsonString(r[field]); ok && strings.TrimSpace(v) != "" {
						if e.Titles == nil {
							e.Titles = map[string]string{}
						}
						e.Titles[t] = clip(v, 1024)
					}
				}
			}
		}
		return true
	}, &e.Scanned)
}

type codexMeta struct {
	Type    string `json:"type"`
	Payload struct {
		ID         string          `json:"id"`
		Cwd        string          `json:"cwd"`
		Originator string          `json:"originator"`
		Source     json.RawMessage `json:"source"`
	} `json:"payload"`
}

// The first prompt is looked for within codexPromptScan bytes /
// codexPromptLines records of a rollout (developer instructions come first).
const (
	codexPromptScan  = 4 << 20
	codexPromptLines = 2000
)

// scanCodex reads the session_meta header (id, cwd; exec and sub-agent
// rollouts are skipped) and the first user prompt.
func scanCodex(path string, e *entry) {
	if e.Scanned == 0 {
		forLines(path, 0, 1, func(line []byte, complete bool) bool {
			var m codexMeta
			if !complete || json.Unmarshal(line, &m) != nil || m.Type != "session_meta" {
				return false
			}
			e.ID, e.Cwd = m.Payload.ID, m.Payload.Cwd
			src := bytes.TrimSpace(m.Payload.Source)
			if m.Payload.Originator == "codex_exec" || string(src) == `"exec"` || (len(src) > 0 && src[0] == '{') {
				e.Skip = true
			}
			return false
		}, nil)
		if e.ID == "" {
			// rollout-YYYY-MM-DDThh-mm-ss-<id>.jsonl
			base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
			if len(base) > len("rollout-2006-01-02T15-04-05-") {
				e.ID = base[len("rollout-2006-01-02T15-04-05-"):]
			}
		}
	}
	if e.Skip || e.Prompt != "" || e.Scanned >= codexPromptScan {
		return
	}
	forLines(path, e.Scanned, codexPromptLines, func(line []byte, complete bool) bool {
		if !complete {
			return true
		}
		for _, m := range parseCodex(line) {
			if m.Role == RoleUser {
				e.Prompt = clip(m.Text, 1024)
				return false
			}
		}
		return true
	}, &e.Scanned)
	if e.Scanned == 0 {
		e.Scanned = 1 // header read; keep the entry incremental
	}
}

// scanOmp reads the title record (line 1, rewritten in place) and the
// session header (line 2).
func scanOmp(path string, e *entry) {
	e.Title = ""
	forLines(path, 0, 2, func(line []byte, complete bool) bool {
		if !complete {
			return true
		}
		var r struct {
			Type  string `json:"type"`
			ID    string `json:"id"`
			Cwd   string `json:"cwd"`
			Title string `json:"title"`
		}
		if json.Unmarshal(line, &r) != nil {
			return true
		}
		switch r.Type {
		case "title":
			if r.Title != "" {
				e.Title = r.Title
			}
		case "session":
			e.ID, e.Cwd = r.ID, r.Cwd
			if e.Title == "" {
				e.Title = r.Title
			}
		}
		return true
	}, nil)
	if e.ID == "" {
		base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		if i := strings.IndexByte(base, '_'); i >= 0 {
			e.ID = base[i+1:]
		}
	}
	// Line 1 changes in place; never scan incrementally.
	e.Scanned = 0
}

// codexThreadNames maps Codex thread ids to their names from
// session_index.jsonl (last record wins), re-read when the file changes.
func (x *Index) codexThreadNames() map[string]string {
	p := filepath.Join(x.roots.Codex, "session_index.jsonl")
	st, err := os.Stat(p)
	if err != nil {
		return nil
	}
	key := [2]int64{st.ModTime().UnixNano(), st.Size()}
	if x.threadNames != nil && key == x.threadNamesStat {
		return x.threadNames
	}
	names := map[string]string{}
	forLines(p, 0, 0, func(line []byte, complete bool) bool {
		var r struct {
			ID         string `json:"id"`
			ThreadName string `json:"thread_name"`
		}
		if complete && json.Unmarshal(line, &r) == nil && r.ID != "" && r.ThreadName != "" {
			names[r.ID] = r.ThreadName
		}
		return true
	}, nil)
	x.threadNames, x.threadNamesStat = names, key
	return names
}

// forLines calls fn for each line of path from offset (at most maxLines
// when > 0). Lines longer than the reader buffer are passed truncated with
// complete=false. fn returns false to stop. When scanned is non-nil it
// receives the offset after the last complete line visited.
func forLines(path string, offset int64, maxLines int, fn func(line []byte, complete bool) bool, scanned *int64) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return
		}
	}
	r := bufio.NewReaderSize(f, 256<<10)
	pos := offset
	lines := 0
	for maxLines <= 0 || lines < maxLines {
		line, err := r.ReadSlice('\n')
		complete := err == nil
		n := int64(len(line))
		if errors.Is(err, bufio.ErrBufferFull) {
			// Long line: hand over its head, then skip the rest.
			head := line
			cont := fn(head, false)
			for errors.Is(err, bufio.ErrBufferFull) {
				line, err = r.ReadSlice('\n')
				n += int64(len(line))
			}
			if err != nil {
				return // unterminated tail: not scanned yet
			}
			pos += n
			if scanned != nil {
				*scanned = pos
			}
			lines++
			if !cont {
				return
			}
			continue
		}
		if err != nil {
			if len(bytes.TrimSpace(line)) > 0 && json.Valid(bytes.TrimSpace(line)) {
				fn(line, true)
			}
			return // unterminated tail: rescanned next time
		}
		pos += n
		cont := fn(bytes.TrimRight(line, "\r\n"), complete)
		if scanned != nil {
			*scanned = pos
		}
		lines++
		if !cont {
			return
		}
	}
}

// stringAfter decodes the JSON string value that starts right after key
// (which ends with the opening quote).
func stringAfter(line, key []byte) (string, bool) {
	i := bytes.Index(line, key)
	if i < 0 {
		return "", false
	}
	start := i + len(key) - 1 // at the opening quote
	j := start + 1
	for j < len(line) {
		switch line[j] {
		case '\\':
			j += 2
			continue
		case '"':
			var s string
			if json.Unmarshal(line[start:j+1], &s) != nil {
				return "", false
			}
			return s, true
		}
		j++
	}
	return "", false
}
