// Package eventlog is the daemon's append-only event log: one JSON event per
// line in segment files (events.log, events.log.1, … newest first), a
// monotonically increasing seq that survives restarts, and an in-memory ring
// of the latest events that answers `since` queries without touching disk.
//
// Retention drops whole old segments by age and by count. Nothing is fsynced
// per event; Close syncs the active segment.
package eventlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// Options configures a Log. Zero fields take the defaults.
type Options struct {
	// MaxEvents bounds the in-memory ring and the retained events.
	MaxEvents int
	// MaxAge drops events (and whole segments) older than this.
	MaxAge time.Duration
	// SegmentBytes rotates the active segment once it would exceed this size.
	SegmentBytes int64
	// SegmentAge rotates the active segment once its first event is older
	// than this, so age-based retention works on quiet hosts too.
	SegmentAge time.Duration
	// Now is the clock (tests).
	Now func() time.Time
}

const (
	defaultMaxEvents    = 10000
	defaultMaxAge       = 7 * 24 * time.Hour
	defaultSegmentBytes = 1 << 20
	defaultSegmentAge   = 24 * time.Hour
	// maxSegmentRead bounds how much of one segment Open reads; segments are
	// rotated at SegmentBytes so anything larger is foreign or corrupt.
	maxSegmentRead = 64 << 20
)

func (o Options) withDefaults() Options {
	if o.MaxEvents <= 0 {
		o.MaxEvents = defaultMaxEvents
	}
	if o.MaxAge <= 0 {
		o.MaxAge = defaultMaxAge
	}
	if o.SegmentBytes <= 0 {
		o.SegmentBytes = defaultSegmentBytes
	}
	if o.SegmentAge <= 0 {
		o.SegmentAge = defaultSegmentAge
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// segment is the metadata of one segment file. segs[0] is the active one.
type segment struct {
	count       int
	first, last int64 // event times, unix ms
	size        int64
}

// Log is safe for concurrent use.
type Log struct {
	mu   sync.Mutex
	base string
	opts Options
	f    *os.File
	segs []segment
	seq  int64
	ring ring
}

// Open loads (or creates) the log at base. It reads segments newest first
// until MaxEvents events are in memory, restores seq from the newest event
// and deletes segments beyond retention.
func Open(base string, opts Options) (*Log, error) {
	opts = opts.withDefaults()
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		return nil, err
	}
	l := &Log{base: base, opts: opts}
	l.ring.init(opts.MaxEvents)

	nums, err := segmentNumbers(base)
	if err != nil {
		return nil, err
	}
	// Renumber gaps away so segs[i] always lives at l.segPath(i).
	for i, n := range nums {
		if n != i {
			if err := os.Rename(l.numPath(n), l.segPath(i)); err != nil {
				return nil, err
			}
		}
	}
	var newestFirst []wire.Event // accumulated newest → oldest
	for i := range nums {
		evs, size, err := readSegment(l.segPath(i))
		if err != nil {
			return nil, err
		}
		seg := segment{count: len(evs), size: size}
		if len(evs) > 0 {
			seg.first, seg.last = evs[0].Time, evs[len(evs)-1].Time
			if l.seq == 0 {
				l.seq = evs[len(evs)-1].Seq
			}
		}
		l.segs = append(l.segs, seg)
		for j := len(evs) - 1; j >= 0 && len(newestFirst) < opts.MaxEvents; j-- {
			newestFirst = append(newestFirst, evs[j])
		}
		if len(newestFirst) >= opts.MaxEvents {
			// Older segments are beyond the count limit; retention deletes them.
			for k := i + 1; k < len(nums); k++ {
				l.segs = append(l.segs, segment{})
			}
			break
		}
	}
	if len(l.segs) == 0 {
		l.segs = []segment{{}}
	}
	for j := len(newestFirst) - 1; j >= 0; j-- {
		l.ring.push(newestFirst[j])
	}
	l.ring.pruneOlder(l.cutoff())

	f, err := os.OpenFile(l.segPath(0), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	l.f = f
	if err := terminateTornLine(f, &l.segs[0]); err != nil {
		f.Close()
		return nil, err
	}
	if err := l.retain(); err != nil {
		f.Close()
		return nil, err
	}
	return l, nil
}

// terminateTornLine ends a partially written last line (crash mid-write) so
// the next event starts on its own line.
func terminateTornLine(f *os.File, seg *segment) error {
	if seg.size == 0 {
		return nil
	}
	r, err := os.Open(f.Name())
	if err != nil {
		return err
	}
	defer r.Close()
	var last [1]byte
	if _, err := r.ReadAt(last[:], seg.size-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	n, err := f.Write([]byte{'\n'})
	seg.size += int64(n)
	return err
}

// Append assigns the next seq (and Time when zero) and writes ev.
// On a write error the event is still assigned and kept in memory.
func (l *Log) Append(ev wire.Event) (wire.Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.opts.Now()
	if ev.Time == 0 {
		ev.Time = now.UnixMilli()
	}
	l.seq++
	ev.Seq = l.seq
	l.ring.push(ev)
	l.ring.pruneOlder(l.cutoff())

	line, err := json.Marshal(ev)
	if err != nil {
		return ev, err
	}
	line = append(line, '\n')
	if l.f == nil {
		return ev, errors.New("eventlog: closed")
	}
	act := &l.segs[0]
	var rotErr error
	rotated := false
	if act.count > 0 && (act.size+int64(len(line)) > l.opts.SegmentBytes ||
		now.UnixMilli()-act.first > l.opts.SegmentAge.Milliseconds()) {
		rotErr = l.rotate()
		if l.f == nil {
			return ev, rotErr
		}
		rotated = rotErr == nil
		act = &l.segs[0]
	}
	n, err := l.f.Write(line)
	act.size += int64(n)
	if err != nil {
		return ev, errors.Join(rotErr, err)
	}
	if act.count == 0 {
		act.first = ev.Time
	}
	act.count++
	act.last = ev.Time
	if rotated {
		// After the write, so the new active segment carries seq and the
		// previous one can go if it is already past retention.
		rotErr = l.retain()
	}
	return ev, rotErr
}

// Since returns the retained events with seq > since, oldest first, and
// whether some events after since were already dropped by retention. since
// 0 returns nothing. A since beyond the latest seq (the log was reset)
// reports truncated with no events.
func (l *Log) Since(since int64) (events []wire.Event, truncated bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ring.pruneOlder(l.cutoff())
	if since <= 0 || since == l.seq {
		return nil, false
	}
	if since > l.seq {
		return nil, true
	}
	if l.ring.n == 0 {
		return nil, true
	}
	oldest := l.ring.at(0).Seq
	truncated = since < oldest-1
	i := sort.Search(l.ring.n, func(i int) bool { return l.ring.at(i).Seq > since })
	events = make([]wire.Event, 0, l.ring.n-i)
	for ; i < l.ring.n; i++ {
		events = append(events, l.ring.at(i))
	}
	return events, truncated
}

// Any reports whether a retained event satisfies pred, scanning newest
// first without copying the ring.
func (l *Log) Any(pred func(*wire.Event) bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := l.ring.n - 1; i >= 0; i-- {
		if pred(&l.ring.buf[(l.ring.start+i)%len(l.ring.buf)]) {
			return true
		}
	}
	return false
}

// Seq is the latest assigned seq (0 for an empty log).
func (l *Log) Seq() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
}

// SetRetention changes the count and age limits (config.set). The ring
// shrinks or grows (growth only fills with future events).
func (l *Log) SetRetention(maxEvents int, maxAge time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if maxEvents <= 0 {
		maxEvents = defaultMaxEvents
	}
	if maxAge <= 0 {
		maxAge = defaultMaxAge
	}
	l.opts.MaxEvents, l.opts.MaxAge = maxEvents, maxAge
	l.ring.resize(maxEvents)
	l.ring.pruneOlder(l.cutoff())
	return l.retain()
}

// Close syncs and closes the active segment.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := errors.Join(l.f.Sync(), l.f.Close())
	l.f = nil
	return err
}

func (l *Log) cutoff() int64 { return l.opts.Now().Add(-l.opts.MaxAge).UnixMilli() }

func (l *Log) segPath(i int) string { return l.numPath(i) }

func (l *Log) numPath(n int) string {
	if n == 0 {
		return l.base
	}
	return l.base + "." + strconv.Itoa(n)
}

// rotate shifts every segment one number up and starts a new active one.
// On failure it reopens whatever is at the base path so logging continues.
func (l *Log) rotate() (err error) {
	defer func() {
		if err != nil && l.f == nil {
			if f, oerr := os.OpenFile(l.segPath(0), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); oerr == nil {
				l.f = f
			}
		}
	}()
	if err := l.f.Close(); err != nil {
		return err
	}
	l.f = nil
	for i := len(l.segs) - 1; i >= 0; i-- {
		if err := os.Rename(l.segPath(i), l.segPath(i+1)); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(l.segPath(0), os.O_CREATE|os.O_WRONLY|os.O_APPEND|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	l.f = f
	l.segs = append([]segment{{}}, l.segs...)
	return nil
}

// retain deletes the oldest segments that are entirely beyond MaxEvents
// (counting newer segments) or entirely older than MaxAge. The active
// segment and the newest segment holding events (which carries seq across
// restarts) are never deleted.
func (l *Log) retain() error {
	cutoff := l.cutoff()
	keep := len(l.segs)
	cum := 0
	haveEvents := false
	for i, s := range l.segs {
		protected := i == 0 || !haveEvents
		if !protected && (cum >= l.opts.MaxEvents || (s.count > 0 && s.last < cutoff) || s.count == 0) {
			keep = i
			break
		}
		cum += s.count
		if s.count > 0 {
			haveEvents = true
		}
	}
	var errs []error
	for i := len(l.segs) - 1; i >= keep; i-- {
		if err := os.Remove(l.segPath(i)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	l.segs = l.segs[:keep]
	return errors.Join(errs...)
}

// segmentNumbers lists the existing segment numbers, ascending (0 = base).
func segmentNumbers(base string) ([]int, error) {
	entries, err := os.ReadDir(filepath.Dir(base))
	if err != nil {
		return nil, err
	}
	name := filepath.Base(base)
	var nums []int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch n := e.Name(); {
		case n == name:
			nums = append(nums, 0)
		case strings.HasPrefix(n, name+"."):
			if k, err := strconv.Atoi(n[len(name)+1:]); err == nil && k > 0 {
				nums = append(nums, k)
			}
		}
	}
	sort.Ints(nums)
	if len(nums) > 0 && nums[0] != 0 {
		// No active segment: start a fresh one in front of the rotated ones.
		nums = append([]int{0}, nums...)
		f, err := os.OpenFile(base, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		f.Close()
	}
	return nums, nil
}

// readSegment parses one segment. Torn or foreign lines are skipped.
func readSegment(path string) ([]wire.Event, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	r := bufio.NewReaderSize(io.LimitReader(f, maxSegmentRead), 64<<10)
	var evs []wire.Event
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var ev wire.Event
			if json.Unmarshal(line, &ev) == nil && ev.Seq > 0 &&
				(len(evs) == 0 || ev.Seq > evs[len(evs)-1].Seq) {
				evs = append(evs, ev)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, 0, fmt.Errorf("eventlog: read %s: %w", path, err)
		}
	}
	return evs, st.Size(), nil
}

// ring is a fixed-capacity circular buffer of events, oldest first.
type ring struct {
	buf   []wire.Event
	start int
	n     int
}

func (r *ring) init(capacity int) { r.buf = make([]wire.Event, capacity); r.start, r.n = 0, 0 }

func (r *ring) at(i int) wire.Event { return r.buf[(r.start+i)%len(r.buf)] }

func (r *ring) push(ev wire.Event) {
	if r.n < len(r.buf) {
		r.buf[(r.start+r.n)%len(r.buf)] = ev
		r.n++
		return
	}
	r.buf[r.start] = ev
	r.start = (r.start + 1) % len(r.buf)
}

func (r *ring) pop() {
	r.buf[r.start] = wire.Event{}
	r.start = (r.start + 1) % len(r.buf)
	r.n--
}

func (r *ring) pruneOlder(cutoff int64) {
	for r.n > 0 && r.at(0).Time < cutoff {
		r.pop()
	}
}

func (r *ring) resize(capacity int) {
	if capacity == len(r.buf) {
		return
	}
	keep := min(r.n, capacity)
	nb := make([]wire.Event, capacity)
	for i := 0; i < keep; i++ {
		nb[i] = r.at(r.n - keep + i)
	}
	r.buf, r.start, r.n = nb, 0, keep
}
