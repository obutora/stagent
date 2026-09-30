// Command stagent-bench is the load benchmark for stagent (not shipped).
//
// It creates an isolated installation (STAGENT_HOME in a temp dir), starts
// `stagent bridge` as the app would, spawns N detached sessions that each
// replay a TUI-like output stream (`stagent-bench replay`, a hidden helper
// mode of this binary), attaches to K of them in raw and in screen mode, and
// measures what the design targets in docs/agent-bridge-plan.md:
// daemon/holder memory (PSS) and CPU (Linux /proc), idle CPU, bytes sent to the app,
// attach-to-first-frame latency, output-to-client latency (timestamps are
// embedded in the stream) and conversations.list over 1,000 transcripts.
//
//	go run ./cmd/stagent-bench -n 50 -k 1 -speed 10
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/obutora/stagent/internal/daemonclient"
	"github.com/obutora/stagent/internal/ipc"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/version"
	"github.com/obutora/stagent/internal/wire"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "replay" {
		os.Exit(replayMain(os.Args[2:]))
	}
	os.Exit(benchMain(os.Args[1:]))
}

type config struct {
	n, k        int
	rate, speed float64
	fixture     string
	chunk       int
	cols, rows  int
	fps         int
	warmup      time.Duration
	window      time.Duration
	idleSettle  time.Duration
	convs       int
	stagent     string
	keep        bool
}

func benchMain(args []string) int {
	var c config
	fs := flag.NewFlagSet("stagent-bench", flag.ExitOnError)
	fs.IntVar(&c.n, "n", 50, "concurrent sessions")
	fs.IntVar(&c.k, "k", 1, "sessions attached in each attach phase")
	fs.Float64Var(&c.rate, "rate", 20, "replay frames per second at 1x")
	fs.Float64Var(&c.speed, "speed", 1, "replay speed (1 = realtime, 10 = 10x)")
	fs.StringVar(&c.fixture, "fixture", "", "replay recorded raw PTY bytes from this file (chunked, timing-less)")
	fs.IntVar(&c.chunk, "chunk", 2048, "fixture bytes per frame")
	fs.IntVar(&c.cols, "cols", 120, "session columns")
	fs.IntVar(&c.rows, "rows", 40, "session rows")
	fs.IntVar(&c.fps, "fps", 15, "screen mode frame cap")
	fs.DurationVar(&c.warmup, "warmup", 2*time.Second, "settle time after spawning")
	fs.DurationVar(&c.window, "window", 5*time.Second, "measurement window per phase")
	fs.DurationVar(&c.idleSettle, "idle-settle", 5*time.Second, "wait after output stops before the idle window (> idle_after_ms)")
	fs.IntVar(&c.convs, "conversations", 1000, "synthetic stored conversations for conversations.list")
	fs.StringVar(&c.stagent, "stagent", "", "stagent binary (default: go build ./cmd/stagent)")
	fs.BoolVar(&c.keep, "keep", false, "keep the temporary STAGENT_HOME")
	fs.Parse(args)
	if c.n < 1 || c.k < 0 || c.k > c.n || c.rate <= 0 || c.speed <= 0 {
		fmt.Fprintln(os.Stderr, "stagent-bench: need n ≥ 1, 0 ≤ k ≤ n, rate > 0, speed > 0")
		return 2
	}
	r, err := run(c)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stagent-bench:", err)
		return 1
	}
	if !r.print(os.Stdout) {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// App-side client

type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

type pipeRW struct {
	io.Reader
	io.WriteCloser
}

// recorder consumes notifications on the client's read goroutine.
type recorder struct {
	mu         sync.Mutex
	attaching  map[string]time.Time // attach sent, first frame not yet seen
	firstFrame []time.Duration
	lat        []time.Duration
	outBytes   int64
}

func (r *recorder) onNotify(m *wire.Msg) {
	if m.Method != wire.NotifyOutput {
		return
	}
	now := time.Now()
	var o wire.OutputParams
	if json.Unmarshal(m.Params, &o) != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outBytes += int64(len(o.Data))
	if t, ok := r.attaching[o.ID]; ok {
		r.firstFrame = append(r.firstFrame, now.Sub(t))
		delete(r.attaching, o.ID)
	}
	if !o.Reset {
		parseMarkers(o.Data, func(us int64) {
			r.lat = append(r.lat, now.Sub(time.UnixMicro(us)))
		})
	}
}

// resetWindow starts a new measurement window and returns the previous
// window's latencies and output bytes.
func (r *recorder) resetWindow() ([]time.Duration, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	lat, n := r.lat, r.outBytes
	r.lat, r.outBytes = nil, 0
	return lat, n
}

// ---------------------------------------------------------------------------
// Run

type phase struct {
	name     string
	appBps   float64 // bytes/s the bridge wrote to the app
	outBps   float64 // decoded terminal bytes/s in output notifications
	lat      []time.Duration
	daemon   usage
	holders  group
	holderOK bool
}

type session struct {
	id        string
	holderPID int
}

type results struct {
	c             config
	ptyBps        float64 // one session's replay output
	spawn         []time.Duration
	phases        []phase
	firstRaw      []time.Duration
	firstScreen   []time.Duration
	convCold      time.Duration
	convWarm      []time.Duration
	convCount     int
	daemonRSSMax  int64
	holderRSSMax  int64
	measuredProcs bool
}

func run(c config) (*results, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	home, err := os.MkdirTemp("", "stagent-bench-")
	if err != nil {
		return nil, err
	}
	if c.keep {
		fmt.Fprintln(os.Stderr, "STAGENT_HOME:", home)
	} else {
		defer os.RemoveAll(home)
	}
	if c.stagent == "" {
		c.stagent = filepath.Join(home, "stagent")
		fmt.Fprintln(os.Stderr, "building stagent…")
		goBin, err := exec.LookPath("go")
		if err != nil {
			goBin = filepath.Join(runtime.GOROOT(), "bin", "go")
		}
		out, err := exec.Command(goBin, "build", "-o", c.stagent, "github.com/obutora/stagent/cmd/stagent").CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("go build ./cmd/stagent (run from the stagent module or pass -stagent): %v\n%s", err, out)
		}
	}
	if c.stagent, err = filepath.Abs(c.stagent); err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "writing %d synthetic conversations…\n", c.convs)
	if err := writeConversations(home, c.convs); err != nil {
		return nil, err
	}
	for k, v := range map[string]string{paths.EnvHome: home, daemonclient.EnvExe: c.stagent, "HOME": home} {
		os.Setenv(k, v)
	}
	layout, err := paths.Resolve()
	if err != nil {
		return nil, err
	}

	res := &results{c: c, ptyBps: streamRate(c.fixture, c.chunk, c.rate, c.speed, c.cols, c.rows)}

	// The bridge, exactly as the app runs it.
	bridge := exec.Command(c.stagent, "bridge")
	stdin, err := bridge.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := bridge.StdoutPipe()
	if err != nil {
		return nil, err
	}
	logf, err := os.Create(filepath.Join(home, "bridge.stderr"))
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	bridge.Stderr = logf
	if err := bridge.Start(); err != nil {
		return nil, err
	}
	var appBytes atomic.Int64
	rec := &recorder{attaching: map[string]time.Time{}}
	app := rpc.NewClient(pipeRW{countingReader{stdout, &appBytes}, stdin}, rec.onNotify)

	var sessions []session
	defer func() {
		teardown(app, layout, sessions)
		stdin.Close()
		waitTimeout(bridge, 5*time.Second)
	}()

	call := func(method string, params, result any) error {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := app.Call(ctx, method, params, result); err != nil {
			return fmt.Errorf("%s: %w", method, err)
		}
		return nil
	}

	var hello wire.HelloResult
	if err := call(wire.MethodHello, wire.HelloParams{Protocol: version.Protocol, Client: "stagent-bench"}, &hello); err != nil {
		return nil, err
	}
	var watch wire.WatchResult
	if err := call(wire.MethodWatch, wire.WatchParams{}, &watch); err != nil {
		return nil, err
	}

	fmt.Fprintf(os.Stderr, "spawning %d sessions…\n", c.n)
	for i := range c.n {
		cmd := []string{self, "replay", "--rate", ftoa(c.rate), "--speed", ftoa(c.speed), "--seed", strconv.Itoa(i)}
		if c.fixture != "" {
			abs, _ := filepath.Abs(c.fixture)
			cmd = append(cmd, "--fixture", abs, "--chunk", strconv.Itoa(c.chunk))
		}
		var sp wire.SpawnResult
		t0 := time.Now()
		if err := call(wire.MethodSessionSpawn, wire.SpawnParams{Command: cmd, Cols: c.cols, Rows: c.rows}, &sp); err != nil {
			return nil, err
		}
		res.spawn = append(res.spawn, time.Since(t0))
		sessions = append(sessions, session{id: sp.Session.ID, holderPID: sp.Session.HolderPID})
	}
	daemonPID := daemonPID(layout)
	time.Sleep(c.warmup)

	measure := func(name string) phase {
		rec.resetWindow()
		b0 := appBytes.Load()
		d0 := sampleProc(daemonPID)
		h0 := make([]procSample, len(sessions))
		for i, s := range sessions {
			h0[i] = sampleProc(s.holderPID)
		}
		time.Sleep(c.window)
		d1 := sampleProc(daemonPID)
		hs := make([]usage, len(sessions))
		for i, s := range sessions {
			hs[i] = between(h0[i], sampleProc(s.holderPID))
		}
		sec := d1.at.Sub(d0.at).Seconds()
		lat, out := rec.resetWindow()
		p := phase{
			name:   name,
			appBps: float64(appBytes.Load()-b0) / sec,
			outBps: float64(out) / sec,
			lat:    lat,
			daemon: between(d0, d1),
		}
		p.holders, p.holderOK = aggregate(hs)
		if p.daemon.ok {
			res.measuredProcs = true
			res.daemonRSSMax = max(res.daemonRSSMax, p.daemon.rssKiB)
		}
		res.holderRSSMax = max(res.holderRSSMax, p.holders.rssMaxKiB)
		res.phases = append(res.phases, p)
		fmt.Fprintf(os.Stderr, "  %s done\n", name)
		return p
	}

	attachPhase := func(name, mode string) ([]time.Duration, error) {
		rec.mu.Lock()
		rec.firstFrame = nil
		rec.mu.Unlock()
		for _, s := range sessions[:c.k] {
			rec.mu.Lock()
			rec.attaching[s.id] = time.Now()
			rec.mu.Unlock()
			if err := call(wire.MethodSessionAttach, wire.AttachParams{ID: s.id, Mode: mode, FPS: c.fps}, nil); err != nil {
				return nil, err
			}
		}
		measure(name)
		for _, s := range sessions[:c.k] {
			if err := call(wire.MethodSessionDetach, wire.SessionRef{ID: s.id}, nil); err != nil {
				return nil, err
			}
		}
		rec.mu.Lock()
		defer rec.mu.Unlock()
		missing := len(rec.attaching)
		clear(rec.attaching)
		if missing > 0 {
			return nil, fmt.Errorf("%s: %d attached sessions never sent a first frame", name, missing)
		}
		return slices.Clone(rec.firstFrame), nil
	}

	fmt.Fprintln(os.Stderr, "measuring…")
	measure("unattached")
	if c.k > 0 {
		if res.firstRaw, err = attachPhase(fmt.Sprintf("raw ×%d", c.k), wire.AttachRaw); err != nil {
			return nil, err
		}
		if res.firstScreen, err = attachPhase(fmt.Sprintf("screen ×%d", c.k), wire.AttachScreen); err != nil {
			return nil, err
		}
	}
	for _, s := range sessions {
		if err := call(wire.MethodSessionInput, wire.InputParams{ID: s.id, Text: "quiet", Submit: true}, nil); err != nil {
			return nil, err
		}
	}
	time.Sleep(c.idleSettle)
	measure("idle")

	fmt.Fprintln(os.Stderr, "conversations.list…")
	params := wire.ConversationsListParams{Limit: c.convs}
	for i := range 6 {
		var cl wire.ConversationsListResult
		t0 := time.Now()
		if err := call(wire.MethodConversationsList, params, &cl); err != nil {
			return nil, err
		}
		d := time.Since(t0)
		if i == 0 {
			res.convCold = d
		} else {
			res.convWarm = append(res.convWarm, d)
		}
		res.convCount = len(cl.Conversations)
	}
	return res, nil
}

// teardown kills the sessions and stops the daemon of the isolated
// installation, then makes sure no holder survives.
func teardown(app *rpc.Client, l *paths.Layout, sessions []session) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range sessions {
		app.Call(ctx, wire.MethodSessionSignal, wire.SignalParams{ID: s.id, Signal: wire.SignalKill}, nil)
	}
	if conn, err := ipc.Dial(l.DaemonAddr, time.Second); err == nil {
		d := rpc.NewClient(conn, nil)
		d.Call(ctx, wire.MethodDaemonShutdown, nil, nil)
		d.Close()
	}
	time.Sleep(300 * time.Millisecond)
	for _, s := range sessions {
		if p, err := os.FindProcess(s.holderPID); err == nil && s.holderPID > 0 {
			p.Kill()
		}
	}
}

func daemonPID(l *paths.Layout) int {
	conn, err := ipc.Dial(l.DaemonAddr, time.Second)
	if err != nil {
		return 0
	}
	c := rpc.NewClient(conn, nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var st wire.DaemonStatus
	if err := c.Call(ctx, wire.MethodDaemonStatus, nil, &st); err != nil {
		return 0
	}
	return st.PID
}

func waitTimeout(cmd *exec.Cmd, d time.Duration) {
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		cmd.Process.Kill()
		<-done
	}
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// ---------------------------------------------------------------------------
// Report

func pct(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return -1
	}
	s := slices.Clone(ds)
	slices.Sort(s)
	i := int(p/100*float64(len(s)-1) + 0.5)
	return s[i]
}

func ms(d time.Duration) string {
	if d < 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f ms", float64(d)/float64(time.Millisecond))
}

func bps(b float64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.2f MiB/s", b/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB/s", b/(1<<10))
	}
	return fmt.Sprintf("%.0f B/s", b)
}

func mib(kib int64) string { return fmt.Sprintf("%.1f MiB", float64(kib)/1024) }

func cpu(u float64) string { return fmt.Sprintf("%.2f%%", u*100) }

func (r *results) phase(name string) *phase {
	for i := range r.phases {
		if r.phases[i].name == name {
			return &r.phases[i]
		}
	}
	return nil
}

// print writes the report and reports whether every target passed.
func (r *results) print(w io.Writer) bool {
	c := r.c
	src := fmt.Sprintf("synthetic TUI stream %g fps × %gx", c.rate, c.speed)
	if c.fixture != "" {
		src = fmt.Sprintf("fixture %s (%d B chunks, %g/s × %gx)", filepath.Base(c.fixture), c.chunk, c.rate, c.speed)
	}
	fmt.Fprintf(w, "\nstagent-bench — %d sessions (%dx%d), %d attached per attach phase, %s\n", c.n, c.cols, c.rows, c.k, src)
	fmt.Fprintf(w, "PTY output: %s per session, %s total; window %s per phase\n", bps(r.ptyBps), bps(r.ptyBps*float64(c.n)), c.window)
	fmt.Fprintf(w, "spawn latency: p50 %s, max %s\n\n", ms(pct(r.spawn, 50)), ms(pct(r.spawn, 100)))

	fmt.Fprintf(w, "%-12s %12s %12s %9s %10s %9s %12s %13s %12s %12s\n",
		"phase", "app B/s", "term B/s", "dmn CPU", "dmn PSS", "dmn wk/s", "holders CPU", "holder PSS", "hldr wk/s", "out→app p95")
	for _, p := range r.phases {
		dcpu, drss, dwk := "n/a", "n/a", "n/a"
		if p.daemon.ok {
			dcpu, drss, dwk = cpu(p.daemon.cpu), mib(p.daemon.rssKiB), fmt.Sprintf("%.1f", p.daemon.wakeups)
		}
		hcpu, hrss, hwk := "n/a", "n/a", "n/a"
		if p.holderOK {
			hcpu = cpu(p.holders.cpuSum)
			hrss = fmt.Sprintf("%s max", mib(p.holders.rssMaxKiB))
			hwk = fmt.Sprintf("%.1f avg", p.holders.wakeSum/float64(p.holders.n))
		}
		fmt.Fprintf(w, "%-12s %12s %12s %9s %10s %9s %12s %13s %12s %12s\n",
			p.name, bps(p.appBps), bps(p.outBps), dcpu, drss, dwk, hcpu, hrss, hwk, ms(pct(p.lat, 95)))
	}
	fmt.Fprintln(w)
	for _, p := range r.phases {
		if len(p.lat) > 0 {
			fmt.Fprintf(w, "output→app %-10s p50 %s  p95 %s  max %s  (%d samples)\n",
				p.name+":", ms(pct(p.lat, 50)), ms(pct(p.lat, 95)), ms(pct(p.lat, 100)), len(p.lat))
		}
	}
	fmt.Fprintf(w, "attach→first frame: raw p50 %s max %s; screen p50 %s max %s\n",
		ms(pct(r.firstRaw, 50)), ms(pct(r.firstRaw, 100)), ms(pct(r.firstScreen, 50)), ms(pct(r.firstScreen, 100)))
	fmt.Fprintf(w, "conversations.list (%d files): cold %s, cached p50 %s max %s, %d returned\n\n",
		c.convs, ms(r.convCold), ms(pct(r.convWarm, 50)), ms(pct(r.convWarm, 100)), r.convCount)

	type check struct {
		name, value, target string
		pass                bool
		na                  bool
	}
	var checks []check
	add := func(name, value, target string, pass, na bool) {
		checks = append(checks, check{name, value, target, pass, na})
	}
	add("daemon PSS (max)", mib(r.daemonRSSMax), "≤ 30 MB", float64(r.daemonRSSMax)/1024 <= 30, !r.measuredProcs)
	add("holder PSS (max)", mib(r.holderRSSMax), "≤ 15 MB", float64(r.holderRSSMax)/1024 <= 15, !r.measuredProcs)
	if idle := r.phase("idle"); idle != nil {
		add("idle CPU daemon", cpu(idle.daemon.cpu), "≈ 0 (≤ 0.1%)", idle.daemon.cpu <= 0.001, !idle.daemon.ok)
		add(fmt.Sprintf("idle CPU holders (sum of %d)", c.n), cpu(idle.holders.cpuSum), "≈ 0 (≤ 0.5%)", idle.holders.cpuSum <= 0.005, !idle.holderOK)
	}
	if c.k > 0 {
		first := max(pct(r.firstRaw, 100), pct(r.firstScreen, 100))
		add("attach → first frame (max)", ms(first), "≤ 300 ms", first <= 300*time.Millisecond, false)
		for _, name := range []string{fmt.Sprintf("raw ×%d", c.k), fmt.Sprintf("screen ×%d", c.k)} {
			p := r.phase(name)
			p95 := pct(p.lat, 95)
			add("output → app p95 ("+strings.Fields(name)[0]+")", ms(p95), "≤ 150 ms", p95 >= 0 && p95 <= 150*time.Millisecond, p95 < 0)
		}
	}
	warm := pct(r.convWarm, 50)
	add(fmt.Sprintf("conversations.list %d (cached)", c.convs), ms(warm), "≤ 200 ms", warm <= 200*time.Millisecond && r.convCount > 0, false)
	if un := r.phase("unattached"); un != nil {
		limit := max(1024, 0.01*r.ptyBps*float64(c.n))
		add("unattached app traffic", bps(un.appBps), "≤ "+bps(limit)+" (1% of PTY)", un.appBps <= limit, false)
	}

	fmt.Fprintf(w, "%-34s %14s %26s  %s\n", "target", "measured", "goal", "result")
	allPass := true
	for _, ch := range checks {
		result := "PASS"
		switch {
		case ch.na:
			result = "n/a"
		case !ch.pass:
			result = "FAIL"
			allPass = false
		}
		fmt.Fprintf(w, "%-34s %14s %26s  %s\n", ch.name, ch.value, ch.target, result)
	}
	if r.convCount < c.convs {
		fmt.Fprintf(w, "note: conversations.list returned %d of %d synthetic conversations\n", r.convCount, c.convs)
	}
	return allPass
}
