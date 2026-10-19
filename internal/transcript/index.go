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
	"sort"
	"strings"
	"sync"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/wire"
)

// Roots are the directories each harness keeps its conversations in, by
// harness id (harness.Harness.TranscriptRoot).
type Roots map[string]string

// DefaultRoots resolves the roots for a home directory as the harnesses
// do (CLAUDE_CONFIG_DIR, CODEX_HOME). getenv is the environment the
// harness runs with (os.Getenv for stagent's own).
func DefaultRoots(home string, getenv func(string) string) Roots {
	r := make(Roots, len(harness.All))
	for _, h := range harness.All {
		r[h.ID] = h.TranscriptRoot(home, getenv)
	}
	return r
}

// HarnessOf reports which harness's transcript directory holds path. Only
// .jsonl files under a known root qualify, so transcript requests cannot be
// pointed at arbitrary files.
func (r Roots) HarnessOf(path string) (string, bool) {
	if !filepath.IsAbs(path) || !strings.EqualFold(filepath.Ext(path), ".jsonl") {
		return "", false
	}
	path = filepath.Clean(path)
	for _, h := range harness.All {
		root := r[h.ID]
		if root == "" {
			continue
		}
		rel, err := filepath.Rel(filepath.Clean(formats[h.ID].dir(root)), path)
		if err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
			return h.ID, true
		}
	}
	return "", false
}

// format is how the conversation files of one harness are laid out and
// read. Each harness of harness.All has one in formats, in its own file.
type format struct {
	// dir is the directory below the harness's root that holds its
	// transcripts.
	dir func(root string) string
	// enumerate lists the top-level conversation files in dir. Sub-agent
	// transcripts are not matched.
	enumerate func(dir string) ([]fileStat, error)
	// named reports whether base is the file name of conversation id.
	named func(base, id string) bool
	// scan reads the metadata of path into e, from e.Scanned where the
	// harness's files are append-only.
	scan func(path string, e *entry)
	// titler returns how entries are titled, read once per listing.
	titler func(x *Index) func(*entry) string
	// parse normalizes one record (see ParseLine).
	parse func(line []byte) []wire.Message
}

var formats = map[string]format{
	wire.HarnessClaude: claudeFormat,
	wire.HarnessCodex:  codexFormat,
	wire.HarnessOmp:    ompFormat,
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
		harnesses = harness.IDs()
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	var out []wire.Conversation
	for _, h := range harnesses {
		hf, ok := formats[h]
		if !ok {
			continue
		}
		files, err := x.enumerate(h)
		if err != nil {
			return nil, err
		}
		x.prune(h, files)
		sort.Slice(files, func(i, j int) bool { return files[i].mtime > files[j].mtime })
		title := hf.titler(x)
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
				Title:     title(e),
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
	hf, ok := formats[harness]
	if !ok || id == "" {
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
		if hf.named(filepath.Base(f.path), id) {
			return f.path, true
		}
	}
	return "", false
}

// Conversation returns the metadata of one transcript file, re-reading it
// only when it changed since the last call. ok is false when the file is
// unreadable or is one List would skip (automation or sub-agent run, no id
// yet).
func (x *Index) Conversation(harness, path string) (c wire.Conversation, ok bool) {
	hf, known := formats[harness]
	info, err := os.Stat(path)
	if !known || err != nil || info.IsDir() {
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
	return wire.Conversation{
		Harness:   harness,
		ID:        e.ID,
		Cwd:       e.Cwd,
		Title:     hf.titler(x)(e),
		UpdatedAt: f.mtime / 1e6,
		Path:      path,
	}, true
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

// enumerate lists a harness's top-level conversation files.
func (x *Index) enumerate(harness string) ([]fileStat, error) {
	hf, ok := formats[harness]
	if !ok {
		return nil, nil
	}
	return hf.enumerate(hf.dir(x.roots[harness]))
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
	if hf, ok := formats[harness]; ok {
		hf.scan(f.path, e)
	}
	return e
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
