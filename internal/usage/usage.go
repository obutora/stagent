// Package usage counts the tokens the coding agents spent on this host
// (使用量) from their transcripts. A Scanner reads only what was added
// since its last scan and keeps what it counted in usage-index.json, so a
// restarted daemon does not read the transcripts again.
//
// How a request is counted: one model request is one dedupe key (Claude:
// message.id:requestId). The same key seen again in the same file (a
// streamed response written as one line per content block, or a file
// rewritten in place) keeps the largest of each count; seen in another
// file (a resumed or forked conversation copies earlier lines into its new
// file) it stays counted for the file it was first seen in. The last line
// of a file counts once its newline is written.
package usage

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/obutora/stagent/internal/fsutil"
	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/transcript"
	"github.com/obutora/stagent/internal/wire"
)

// tokens are the counts of one request or bucket: input, output, cache
// read, cache write (5 minutes), cache write (1 hour), and the cost the
// agent wrote for it in millionths of a US dollar (omp, pi).
type tokens [6]int64

// costMicro is the index of the agent's cost in tokens.
const costMicro = 5

// record is one model request read from a transcript line.
type record struct {
	key     string
	at      int64 // unix ms
	model   string
	project string
	session string
	tokens  tokens
}

// reader is how the usage of one harness is read.
type reader struct {
	// files lists the transcript files below the harness's root.
	files func(root string) ([]string, error)
	// parse reads the request one complete line records, if any. st is
	// what the earlier lines of the file said; parse updates it.
	parse func(st *lineState, line []byte) (record, bool)
}

// lineState is what the earlier lines of a file tell about its later ones
// (codex: the session and project of session_meta, the model of the last
// turn_context, the last total_token_usage, the newest rate_limits). It is
// kept with the file, so a scan reads only what was appended.
type lineState struct {
	Session string `json:"session,omitempty"`
	Project string `json:"project,omitempty"`
	Model   string `json:"model,omitempty"`
	// Total is the last total_token_usage: input, cached input, output.
	Total [3]int64 `json:"total,omitzero"`
	// Limits is the newest event's limit windows (codex rate_limits).
	Limits *RateLimits `json:"limits,omitempty"`
}

// readers is the table of how each harness of harness.All is read; a nil
// entry is a harness whose usage is not counted. Copilot CLI writes the
// output tokens of a reply but not its input, so it is not counted.
var readers = map[string]*reader{
	wire.HarnessClaude:  claudeReader,
	wire.HarnessCodex:   codexReader,
	wire.HarnessOmp:     ompReader,
	wire.HarnessPi:      ompReader,
	wire.HarnessCopilot: nil,
}

// Counted lists, in the order of harness.All, the harnesses whose usage is
// counted.
func Counted() []string {
	var ids []string
	for _, h := range harness.All {
		if readers[h.ID] != nil {
			ids = append(ids, h.ID)
		}
	}
	return ids
}

const (
	// indexVersion is bumped whenever how usage is counted changes, so
	// every transcript is read again.
	indexVersion = 4
	// slotSeconds is the time resolution counts are kept at. Every time
	// zone's offset is a multiple of 15 minutes, so the calendar days of
	// any zone are made of whole slots.
	slotSeconds = 15 * 60
	// maxLine: a longer line (a large tool result) is skipped.
	maxLine = 8 << 20
	// saveEvery bounds how often the index is written while transcripts
	// keep growing.
	saveEvery = time.Minute
)

// Stats describes one scan.
type Stats struct {
	Files   int   // files read
	Read    int64 // bytes read
	Changed bool  // new usage was counted, or the first scan finished
}

// bucketKey groups the requests of one file by slot, model, project,
// session (indexes into the file's tables) and tier: how many thresholds
// of wire.UsageLongContextThresholds their prompt was over.
type bucketKey struct {
	slot                    int64
	model, project, session int32
	tier                    int32
}

// tierOf is how many thresholds of wire.UsageLongContextThresholds the
// prompt of a request with counts t was over. The prompt is the input,
// cache reads and cache writes.
func tierOf(t *[6]uint32) int32 {
	prompt := int64(t[0]) + int64(t[2]) + int64(t[3]) + int64(t[4])
	var n int32
	for _, over := range wire.UsageLongContextThresholds {
		if prompt > over {
			n++
		}
	}
	return n
}

type bucket struct {
	t        tokens
	requests int64
}

// span is the time of the first and last request of a session (unix ms).
type span struct{ first, last int64 }

// file is what was counted from one transcript file.
type file struct {
	path, harness string
	id            string    // fileID
	size, mtime   int64     // as last seen (mtime: unix ns)
	offset        int64     // how far it was read: the end of its last complete line
	state         lineState // what the lines up to offset said
	// gone: the file was not found by the last scan. Its counts stay;
	// the keys it holds are dropped.
	gone bool

	models, projects, sessions []string
	spans                      []span // by session index
	buckets                    map[bucketKey]*bucket
}

// intern returns the index of s in *tab, appending it when missing. The
// tables of a file have a few entries.
func intern(tab *[]string, s string) int32 {
	for i, v := range *tab {
		if v == s {
			return int32(i)
		}
	}
	*tab = append(*tab, s)
	return int32(len(*tab) - 1)
}

// keyState is a counted request: the file it is counted for and the
// largest counts seen for it.
type keyState struct {
	f *file
	t [6]uint32
}

// Scanner counts usage. Scan is called by one goroutine at a time;
// Report may be called concurrently and never waits for file reads.
type Scanner struct {
	path  string
	roots transcript.Roots
	// BeforeFile, when set, is called before each file a scan reads.
	BeforeFile func(path string)

	// done (a scan finished since Open) and scannedAt (unix ms) are read
	// without mu: the daemon asks for them under its own lock.
	done      atomic.Bool
	scannedAt atomic.Int64

	// mu guards files and keys against Report; only Scan (and save, which
	// Scan calls) changes them, so Scan reads them without it.
	mu      sync.Mutex
	files   map[string]*file
	keys    map[uint64]keyState
	dirty   bool
	savedAt time.Time
}

// Open loads the index at path (missing, corrupt or of another version:
// empty) for the transcripts under roots.
func Open(path string, roots transcript.Roots) *Scanner {
	s := &Scanner{path: path, roots: roots, files: map[string]*file{}, keys: map[uint64]keyState{}}
	if b, err := os.ReadFile(path); err == nil {
		s.load(b)
	}
	return s
}

// Scan reads what the transcripts gained since the last scan. It returns
// early, keeping what it read so far, when stop is closed.
func (s *Scanner) Scan(stop <-chan struct{}) (Stats, error) {
	var st Stats
	type candidate struct {
		path, harness string
		r             *reader
		info          os.FileInfo
	}
	var found []candidate
	listed := map[string]bool{}
	var listErr error
	for _, h := range harness.All {
		r, root := readers[h.ID], s.roots[h.ID]
		if r == nil || root == "" {
			continue
		}
		paths, err := r.files(root)
		if err != nil {
			listErr = errors.Join(listErr, err)
		}
		for _, p := range paths {
			info, err := os.Stat(p)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			listed[p] = true
			found = append(found, candidate{p, h.ID, r, info})
		}
	}
	// Oldest first: a conversation is read before the ones resumed from
	// it, which copy its lines.
	sort.SliceStable(found, func(i, j int) bool {
		if a, b := found[i].info.ModTime(), found[j].info.ModTime(); !a.Equal(b) {
			return a.Before(b)
		}
		return found[i].path < found[j].path
	})

	for _, c := range found {
		if stopped(stop) {
			s.save(true)
			return st, nil
		}
		prev := s.files[c.path] // only Scan changes files: read without the lock
		id, size, mtime := fileID(c.info), c.info.Size(), c.info.ModTime().UnixNano()
		if prev != nil && !prev.gone && prev.id == id && prev.size == size && prev.mtime == mtime {
			continue
		}
		reread := prev == nil || prev.id != id || size < prev.size || prev.harness != c.harness
		from, state := int64(0), lineState{}
		if !reread {
			from, state = prev.offset, prev.state
		}
		if s.BeforeFile != nil {
			s.BeforeFile(c.path)
		}
		// Each chunk of lines is counted as it is read, its end kept as the
		// file's offset: a long file never sits in memory whole, and a stop
		// keeps what was read. The size and mtime are those seen only once
		// the file was read to its end, so a file left halfway is read on
		// from its offset by the next scan.
		var f *file
		merge := func(recs []record, end int64, whole bool) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if f == nil {
				f = prev
				if reread {
					if prev != nil {
						s.dropKeysLocked(map[*file]bool{prev: true})
					}
					f = &file{path: c.path, harness: c.harness, buckets: map[bucketKey]*bucket{}}
					s.files[c.path] = f
				}
			}
			f.id, f.offset, f.state, f.gone = id, end, state, false
			if whole {
				f.size, f.mtime = size, mtime
			}
			if s.mergeLocked(f, recs) {
				st.Changed = true
			}
			s.dirty = true
		}
		n, err := readRecords(c.path, from, c.r.parse, &state, stop, merge)
		st.Read += n
		st.Files++
		if errors.Is(err, errStopped) {
			s.save(true)
			return st, nil
		}
		// Any other error: unreadable now, read on from what was counted
		// by the next scan.
	}

	s.mu.Lock()
	if listErr == nil {
		gone := map[*file]bool{}
		for p, f := range s.files {
			if !f.gone && !listed[p] {
				f.gone = true
				gone[f] = true
			}
		}
		if len(gone) > 0 {
			s.dropKeysLocked(gone)
			s.dirty = true
		}
	}
	first := !s.done.Load()
	if first {
		st.Changed = true
	}
	s.mu.Unlock()
	// After the counts: a reader seeing done sees them too.
	s.scannedAt.Store(time.Now().UnixMilli())
	s.done.Store(true)
	s.save(first)
	return st, listErr
}

// RateLimits is the newest limit windows any transcript of harness h wrote
// (codex), by the time of the event; ok is false when none has any.
func (s *Scanner) RateLimits(h string) (rl RateLimits, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *file
	for _, f := range s.files {
		l := f.state.Limits
		if f.harness != h || l == nil {
			continue
		}
		if best == nil || l.At > best.state.Limits.At || l.At == best.state.Limits.At && f.path > best.path {
			best = f
		}
	}
	if best == nil {
		return RateLimits{}, false
	}
	return *best.state.Limits, true
}

func stopped(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

// mergeLocked counts recs, read from f, and reports whether any count
// grew.
func (s *Scanner) mergeLocked(f *file, recs []record) bool {
	changed := false
	for i := range recs {
		r := &recs[i]
		h := hashKey(r.key)
		ks, seen := s.keys[h]
		if seen && ks.f != f {
			continue // counted for the file it was first seen in
		}
		var delta tokens
		grew := false
		for k, v := range r.tokens {
			n := uint32(min(max(v, 0), 1<<32-1))
			if n > ks.t[k] {
				delta[k] = int64(n - ks.t[k])
				ks.t[k] = n
				grew = true
			}
		}
		if seen && !grew {
			continue
		}
		ks.f = f
		s.keys[h] = ks
		si := intern(&f.sessions, r.session)
		if int(si) == len(f.spans) {
			f.spans = append(f.spans, span{r.at, r.at})
		} else {
			f.spans[si].first = min(f.spans[si].first, r.at)
			f.spans[si].last = max(f.spans[si].last, r.at)
		}
		// A request streamed in several lines keeps its prompt, so its
		// lines land in the same tier.
		k := bucketKey{
			slot:    floorDiv(r.at, slotSeconds*1000),
			model:   intern(&f.models, r.model),
			project: intern(&f.projects, r.project),
			session: si,
			tier:    tierOf(&ks.t),
		}
		b := f.buckets[k]
		if b == nil {
			b = &bucket{}
			f.buckets[k] = b
		}
		for j := range delta {
			b.t[j] += delta[j]
		}
		if !seen {
			b.requests++
		}
		changed = true
	}
	return changed
}

// dropKeysLocked forgets the requests counted for the given files.
func (s *Scanner) dropKeysLocked(files map[*file]bool) {
	for h, ks := range s.keys {
		if files[ks.f] {
			delete(s.keys, h)
		}
	}
}

// hashKey is the 64-bit FNV-1a hash of a dedupe key.
func hashKey(k string) uint64 {
	h := uint64(14695981039346656037)
	for i := range len(k) {
		h ^= uint64(k[i])
		h *= 1099511628211
	}
	return h
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

var errStopped = errors.New("usage: scan stopped")

// chunkLines is how many lines readRecords reads between two merges, and
// between two looks at stop.
const chunkLines = 4096

// readRecords reads the complete lines of path from offset and gives the
// requests they record to merge every chunkLines lines and at the end
// (whole: the end of the file was reached), with the offset after the last
// line read; recs is reused once merge returns. A line still being written
// (no newline yet) is left for the next scan. st is the file's state at
// offset; the lines read update it, so at each merge it is the state at
// end. It returns how many bytes it read, and errStopped when stop closed
// (what was merged stays merged).
func readRecords(path string, offset int64, parse func(*lineState, []byte) (record, bool), st *lineState, stop <-chan struct{}, merge func(recs []record, end int64, whole bool)) (read int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return 0, err
		}
	}
	br := bufio.NewReaderSize(f, 64<<10)
	end := offset
	var recs []record
	var long []byte // a line longer than the reader's buffer
	var lineLen int64
	skip := false // the line is longer than maxLine
	for lines := 1; ; {
		chunk, rerr := br.ReadSlice('\n')
		read += int64(len(chunk))
		lineLen += int64(len(chunk))
		if rerr == bufio.ErrBufferFull {
			if !skip && len(long)+len(chunk) > maxLine {
				skip, long = true, nil
			}
			if !skip {
				long = append(long, chunk...)
			}
			continue
		}
		if rerr == io.EOF {
			merge(recs, end, true)
			return read, nil
		}
		if rerr != nil {
			return read, rerr
		}
		end += lineLen
		line := chunk
		if long != nil {
			line = append(long, chunk...)
		}
		if !skip {
			if r, ok := parse(st, line); ok {
				recs = append(recs, r)
			}
		}
		long, lineLen, skip = nil, 0, false
		if lines%chunkLines == 0 {
			merge(recs, end, false)
			recs = recs[:0]
			if stopped(stop) {
				return read, errStopped
			}
		}
		lines++
	}
}

// ---------------------------------------------------------------------------
// Report

// Report sums the usage counted so far by the calendar days of p.TZ.
func (s *Scanner) Report(p wire.UsageGetParams) (wire.UsageGetResult, error) {
	loc := time.UTC
	if p.TZ != "" {
		l, err := time.LoadLocation(p.TZ)
		if err != nil {
			return wire.UsageGetResult{}, wire.Errorf(wire.ErrBadRequest, "usage.get: tz %q: %v", p.TZ, err)
		}
		loc = l
	}
	fromDay, toDay := int64(-1<<62), int64(1<<62)
	for _, d := range []struct {
		v   string
		out *int64
	}{{p.From, &fromDay}, {p.To, &toDay}} {
		if d.v == "" {
			continue
		}
		t, err := time.Parse(time.DateOnly, d.v)
		if err != nil {
			return wire.UsageGetResult{}, wire.Errorf(wire.ErrBadRequest, "usage.get: day %q is not YYYY-MM-DD", d.v)
		}
		*d.out = floorDiv(t.Unix(), 86400)
	}

	type rowKey struct {
		day                     int64
		harness, model, project string
	}
	type sessKey struct{ harness, id string }
	type sessAcc struct {
		t           wire.UsageTokens
		first, last int64
		models      map[string]bool
		projects    map[string]int64
	}
	// rowAcc is a day's row; long[i] is its part over threshold i of
	// wire.UsageLongContextThresholds.
	type rowAcc struct {
		t    wire.UsageTokens
		long [len(wire.UsageLongContextThresholds)]wire.UsageTokens
	}
	rows := map[rowKey]*rowAcc{}
	sessions := map[sessKey]*sessAcc{}

	s.mu.Lock()
	res := wire.UsageGetResult{Scanning: !s.done.Load(), ScannedAt: s.scannedAt.Load(), Counted: Counted()}
	for _, f := range s.files {
		for k, b := range f.buckets {
			sec := k.slot * slotSeconds
			_, off := time.Unix(sec, 0).In(loc).Zone()
			day := floorDiv(sec+int64(off), 86400)
			if day < fromDay || day > toDay {
				continue
			}
			t := wire.UsageTokens{
				Input: b.t[0], Output: b.t[1], CacheRead: b.t[2], CacheWrite5m: b.t[3], CacheWrite1h: b.t[4],
				Requests: b.requests, AgentCost: float64(b.t[costMicro]) / 1e6,
			}
			model, project, session := f.models[k.model], f.projects[k.project], f.sessions[k.session]
			rk := rowKey{day, f.harness, model, project}
			r := rows[rk]
			if r == nil {
				r = &rowAcc{}
				rows[rk] = r
			}
			addTokens(&r.t, t)
			for i := range min(int(k.tier), len(r.long)) {
				addTokens(&r.long[i], t)
			}
			if session == "" {
				continue
			}
			sk := sessKey{f.harness, session}
			a := sessions[sk]
			if a == nil {
				a = &sessAcc{first: f.spans[k.session].first, last: f.spans[k.session].last,
					models: map[string]bool{}, projects: map[string]int64{}}
				sessions[sk] = a
			}
			addTokens(&a.t, t)
			a.first = min(a.first, f.spans[k.session].first)
			a.last = max(a.last, f.spans[k.session].last)
			a.models[model] = true
			a.projects[project] += t.Total()
		}
	}
	s.mu.Unlock()

	res.Days = make([]wire.UsageDay, 0, len(rows))
	for k, r := range rows {
		d := wire.UsageDay{
			Date: time.Unix(k.day*86400, 0).UTC().Format(time.DateOnly), Harness: k.harness,
			Model: k.model, Project: k.project, UsageTokens: r.t,
		}
		for i, t := range r.long {
			if t.Requests > 0 || t.Total() > 0 {
				d.LongContext = append(d.LongContext, wire.UsageLongContext{Over: wire.UsageLongContextThresholds[i], UsageTokens: t})
			}
		}
		res.Days = append(res.Days, d)
	}
	sort.Slice(res.Days, func(i, j int) bool {
		a, b := res.Days[i], res.Days[j]
		if a.Date != b.Date {
			return a.Date < b.Date
		}
		if a.Harness != b.Harness {
			return a.Harness < b.Harness
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		return a.Project < b.Project
	})

	res.Sessions = make([]wire.UsageSession, 0, min(len(sessions), wire.UsageTopSessions))
	all := make([]wire.UsageSession, 0, len(sessions))
	for k, a := range sessions {
		u := wire.UsageSession{Harness: k.harness, SessionID: k.id, First: a.first, Last: a.last,
			UsageTokens: a.t, Total: a.t.Total()}
		for m := range a.models {
			u.Models = append(u.Models, m)
		}
		sort.Strings(u.Models)
		var most int64 = -1
		for p, n := range a.projects {
			if n > most || (n == most && p < u.Project) {
				u.Project, most = p, n
			}
		}
		all = append(all, u)
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.Total != b.Total {
			return a.Total > b.Total
		}
		if a.Last != b.Last {
			return a.Last > b.Last
		}
		return a.SessionID < b.SessionID
	})
	res.Sessions = append(res.Sessions, all[:min(len(all), wire.UsageTopSessions)]...)
	return res, nil
}

func addTokens(dst *wire.UsageTokens, t wire.UsageTokens) {
	dst.Input += t.Input
	dst.Output += t.Output
	dst.CacheRead += t.CacheRead
	dst.CacheWrite5m += t.CacheWrite5m
	dst.CacheWrite1h += t.CacheWrite1h
	dst.Requests += t.Requests
	dst.AgentCost += t.AgentCost
}

// ScannedAt reports whether a scan finished since Open and when the last
// one did (unix ms). It never waits for a scan.
func (s *Scanner) ScannedAt() (done bool, at int64) {
	return s.done.Load(), s.scannedAt.Load()
}

// ---------------------------------------------------------------------------
// usage-index.json

type indexFile struct {
	Version   int        `json:"version"`
	ScannedAt int64      `json:"scanned_at,omitempty"`
	Files     []fileJSON `json:"files"`
	// Keys are the counted requests, keyRecordSize bytes each (little
	// endian): the FNV-1a 64 hash of the dedupe key, the index of its
	// file in Files (uint32) and its six largest counts (uint32, see
	// tokens).
	Keys []byte `json:"keys"`
}

const keyRecordSize = 8 + 4 + len(tokens{})*4

type fileJSON struct {
	Path    string `json:"path"`
	Harness string `json:"harness"`
	ID      string `json:"id,omitempty"`
	Size    int64  `json:"size"`
	Mtime   int64  `json:"mtime"`
	Offset  int64  `json:"offset"`
	// State is what the lines up to Offset said (codex).
	State    lineState `json:"state,omitzero"`
	Gone     bool      `json:"gone,omitempty"`
	Models   []string  `json:"models"`
	Projects []string  `json:"projects"`
	Sessions []string  `json:"sessions"`
	// Spans: [first, last] request time of each session (unix ms).
	Spans [][2]int64 `json:"spans"`
	// Buckets: [slot, model, project, session, input, output, cache_read,
	// cache_write_5m, cache_write_1h, requests, agent cost in millionths
	// of a dollar, tier]; slot is unix time / slotSeconds, model/project/
	// session index the tables above, tier is bucketKey.tier.
	Buckets [][12]int64 `json:"buckets"`
}

// load restores the index from b, leaving the scanner empty when b is not
// a valid index of this version.
func (s *Scanner) load(b []byte) {
	var x indexFile
	if json.Unmarshal(b, &x) != nil || x.Version != indexVersion || len(x.Keys)%keyRecordSize != 0 {
		return
	}
	files := make(map[string]*file, len(x.Files))
	byIndex := make([]*file, len(x.Files))
	for i, j := range x.Files {
		if len(j.Spans) != len(j.Sessions) {
			return
		}
		f := &file{path: j.Path, harness: j.Harness, id: j.ID, size: j.Size, mtime: j.Mtime, offset: j.Offset,
			state: j.State,
			gone:  j.Gone, models: j.Models, projects: j.Projects, sessions: j.Sessions,
			spans: make([]span, len(j.Spans)), buckets: make(map[bucketKey]*bucket, len(j.Buckets))}
		for k, sp := range j.Spans {
			f.spans[k] = span{sp[0], sp[1]}
		}
		for _, v := range j.Buckets {
			if v[1] < 0 || v[1] >= int64(len(f.models)) || v[2] < 0 || v[2] >= int64(len(f.projects)) ||
				v[3] < 0 || v[3] >= int64(len(f.sessions)) || v[11] < 0 || v[11] > int64(len(wire.UsageLongContextThresholds)) {
				return
			}
			f.buckets[bucketKey{v[0], int32(v[1]), int32(v[2]), int32(v[3]), int32(v[11])}] = &bucket{
				t: tokens{v[4], v[5], v[6], v[7], v[8], v[10]}, requests: v[9]}
		}
		files[f.path] = f
		byIndex[i] = f
	}
	keys := make(map[uint64]keyState, len(x.Keys)/keyRecordSize)
	for b := x.Keys; len(b) > 0; b = b[keyRecordSize:] {
		fi := binary.LittleEndian.Uint32(b[8:])
		if int(fi) >= len(byIndex) {
			return
		}
		ks := keyState{f: byIndex[fi]}
		for k := range ks.t {
			ks.t[k] = binary.LittleEndian.Uint32(b[12+4*k:])
		}
		keys[binary.LittleEndian.Uint64(b)] = ks
	}
	s.files, s.keys = files, keys
	s.scannedAt.Store(x.ScannedAt)
}

// save writes the index when it changed: at once when now is set, else
// at most every saveEvery. Only Scan calls it, and only Scan changes files
// and keys, so it reads them without mu: Report never waits for the
// marshal or the write.
func (s *Scanner) save(now bool) {
	s.mu.Lock()
	if !s.dirty || s.path == "" || (!now && time.Since(s.savedAt) < saveEvery) {
		s.mu.Unlock()
		return
	}
	s.dirty = false
	s.savedAt = time.Now()
	s.mu.Unlock()
	x := indexFile{Version: indexVersion, ScannedAt: s.scannedAt.Load(), Files: make([]fileJSON, 0, len(s.files))}
	index := make(map[*file]uint32, len(s.files))
	paths := make([]string, 0, len(s.files))
	for p := range s.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		f := s.files[p]
		index[f] = uint32(len(x.Files))
		j := fileJSON{Path: f.path, Harness: f.harness, ID: f.id, Size: f.size, Mtime: f.mtime, Offset: f.offset,
			State: f.state, Gone: f.gone, Models: f.models, Projects: f.projects, Sessions: f.sessions,
			Spans: make([][2]int64, len(f.spans)), Buckets: make([][12]int64, 0, len(f.buckets))}
		for k, sp := range f.spans {
			j.Spans[k] = [2]int64{sp.first, sp.last}
		}
		for k, b := range f.buckets {
			j.Buckets = append(j.Buckets, [12]int64{k.slot, int64(k.model), int64(k.project), int64(k.session),
				b.t[0], b.t[1], b.t[2], b.t[3], b.t[4], b.requests, b.t[costMicro], int64(k.tier)})
		}
		x.Files = append(x.Files, j)
	}
	x.Keys = make([]byte, 0, len(s.keys)*keyRecordSize)
	var rec [keyRecordSize]byte
	for h, ks := range s.keys {
		binary.LittleEndian.PutUint64(rec[:], h)
		binary.LittleEndian.PutUint32(rec[8:], index[ks.f])
		for k, v := range ks.t {
			binary.LittleEndian.PutUint32(rec[12+4*k:], v)
		}
		x.Keys = append(x.Keys, rec[:]...)
	}
	b, err := json.Marshal(x)
	if err == nil {
		err = fsutil.WriteFileAtomic(s.path, b)
	}
	if err != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
	}
}
