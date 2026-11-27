// Command hostscreen-bench measures the host screen's traffic (#1974, not
// shipped). It plays the app's host screen tab against a local `stagent
// bridge` (stdin/stdout, no SSH) over an emulated link — a bandwidth and a
// one-way delay each way — and, per kind of target and scenario, records
// the bytes per second on the wire, frames per second, input to screen
// change latency and stagent's CPU. See README.md next to this file.
//
//	go run ./cmd/hostscreen-bench -out results.json -md results.md
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

type config struct {
	Kinds       []string      `json:"kinds"`
	Scenarios   []string      `json:"scenarios"`
	Transport   string        `json:"transport"`
	DownMbps    float64       `json:"down_mbps"`
	UpMbps      float64       `json:"up_mbps"`
	Delay       time.Duration `json:"one_way_delay_ns"`
	Duration    time.Duration `json:"duration_ns"`
	Load        time.Duration `json:"load_ns"`
	Settle      time.Duration `json:"settle_ns"`
	Tail        time.Duration `json:"tail_ns"`
	Repeat      int           `json:"repeat"`
	CDPPort     int           `json:"cdp_port"`
	Serial      string        `json:"android_serial,omitempty"`
	UDID        string        `json:"ios_udid,omitempty"`
	stagent     string
	out, md     string
	cacheDir    string
	keepLogs    bool
	frames      bool
	logDir      string
	stagentHome string
}

// Result is one scenario run on one kind of target.
type Result struct {
	Kind      string `json:"kind"`
	Scenario  string `json:"scenario"`
	Run       int    `json:"run"`
	Transport string `json:"transport"`
	Target    string `json:"target,omitempty"`
	Error     string `json:"error,omitempty"`
	Summary
	// StagentCPUPct: CPU of the bridge and its direct children over the
	// window, in % of one core (ps).
	StagentCPUPct float64 `json:"stagent_cpu_pct"`
	// StagentCPUTotalSec: CPU over the bridge's whole life, start to exit
	// (getrusage of the child).
	StagentCPUTotalSec float64 `json:"stagent_cpu_total_sec"`
	// Frames and InputsMs: every frame and input of the window (-frames).
	Frames   []FrameInfo `json:"frames,omitempty"`
	InputsMs []float64   `json:"inputs_ms,omitempty"`
}

type report struct {
	Date           time.Time `json:"date"`
	Host           string    `json:"host"`
	StagentVersion string    `json:"stagent_version"`
	Config         config    `json:"config"`
	Results        []Result  `json:"results"`
}

func main() {
	var c config
	var kinds, scens string
	fs := flag.NewFlagSet("hostscreen-bench", flag.ExitOnError)
	fs.StringVar(&kinds, "kinds", "browser,android,ios", "kinds of target")
	fs.StringVar(&scens, "scenarios", "still,scroll,text,video", "scenarios")
	fs.StringVar(&c.Transport, "transport", "json", "how frames travel: json (host_screen.frame) or binary (binary frame channel with tiles, #1974)")
	fs.Float64Var(&c.DownMbps, "down-mbps", 5, "stagent → app bandwidth, Mbit/s")
	fs.Float64Var(&c.UpMbps, "up-mbps", 1, "app → stagent bandwidth, Mbit/s")
	fs.DurationVar(&c.Delay, "delay", 40*time.Millisecond, "one-way delay each way (RTT = 2×)")
	fs.DurationVar(&c.Duration, "duration", 30*time.Second, "measurement window per scenario")
	fs.DurationVar(&c.Load, "load", 3*time.Second, "wait after opening the page, before the bridge starts")
	fs.DurationVar(&c.Settle, "settle", 3*time.Second, "wait after the first frame and the setup, before the window")
	fs.DurationVar(&c.Tail, "tail", 2*time.Second, "keep receiving after the window (for the last inputs' frames)")
	fs.IntVar(&c.Repeat, "repeat", 1, "runs of each scenario")
	fs.IntVar(&c.CDPPort, "cdp-port", 9222, "desktop Chrome's remote debugging port")
	fs.StringVar(&c.Serial, "android-serial", "", "emulator (default: the first connectable)")
	fs.StringVar(&c.UDID, "ios-udid", "", "simulator (default: the first booted)")
	fs.StringVar(&c.stagent, "stagent", "", "stagent binary (default: go build ./cmd/stagent)")
	fs.StringVar(&c.out, "out", "", "write the results as JSON here")
	fs.StringVar(&c.md, "md", "", "write the results table as Markdown here")
	fs.StringVar(&c.cacheDir, "cache", "", "where the video clip is made (default: user cache dir)")
	fs.BoolVar(&c.keepLogs, "keep-logs", false, "keep the bridges' stderr and STAGENT_HOME")
	fs.BoolVar(&c.frames, "frames", false, "list every frame and input of the windows in -out")
	fs.Parse(os.Args[1:])
	c.Kinds, c.Scenarios = strings.Split(kinds, ","), strings.Split(scens, ",")
	if _, ok := transports[c.Transport]; !ok {
		fmt.Fprintf(os.Stderr, "hostscreen-bench: unknown transport %q\n", c.Transport)
		os.Exit(2)
	}
	r, err := run(c)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hostscreen-bench:", err)
		os.Exit(1)
	}
	table := r.markdown()
	fmt.Print(table)
	if c.md != "" {
		if err := os.WriteFile(c.md, []byte(table), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "hostscreen-bench:", err)
			os.Exit(1)
		}
	}
	if c.out != "" {
		b, _ := json.MarshalIndent(r, "", "  ")
		if err := os.WriteFile(c.out, append(b, '\n'), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "hostscreen-bench:", err)
			os.Exit(1)
		}
	}
}

func run(c config) (*report, error) {
	home, err := os.MkdirTemp("", "hostscreen-bench-")
	if err != nil {
		return nil, err
	}
	if c.keepLogs {
		fmt.Fprintln(os.Stderr, "logs and STAGENT_HOME:", home)
	} else {
		defer os.RemoveAll(home)
	}
	c.logDir = home
	c.stagentHome = filepath.Join(home, "stagent-home")
	if c.stagent == "" {
		c.stagent = filepath.Join(home, "stagent")
		fmt.Fprintln(os.Stderr, "building stagent…")
		goBin, err := exec.LookPath("go")
		if err != nil {
			goBin = filepath.Join(runtime.GOROOT(), "bin", "go")
		}
		if out, err := exec.Command(goBin, "build", "-o", c.stagent, "github.com/obutora/stagent/cmd/stagent").CombinedOutput(); err != nil {
			return nil, fmt.Errorf("go build ./cmd/stagent (run from the stagent module or pass -stagent): %v\n%s", err, out)
		}
	}
	if c.stagent, err = filepath.Abs(c.stagent); err != nil {
		return nil, err
	}
	if c.cacheDir == "" {
		d, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		c.cacheDir = filepath.Join(d, "stagent-hostscreen-bench")
	}
	clip, err := ensureVideo(c.cacheDir)
	if err != nil {
		return nil, err
	}
	port, err := servePages(clip)
	if err != nil {
		return nil, err
	}
	hostName, _ := os.Hostname()
	r := &report{Date: time.Now(), Host: fmt.Sprintf("%s %s/%s", hostName, runtime.GOOS, runtime.GOARCH), Config: c}

	for _, kn := range c.Kinds {
		var k kind
		switch kn {
		case "browser":
			k = &browserKind{port: c.CDPPort, pagePort: port}
		case "android":
			k = &androidKind{serial: c.Serial, pagePort: port}
		case "ios":
			k = &iosKind{udid: c.UDID, pagePort: port}
		default:
			return nil, fmt.Errorf("unknown kind %q", kn)
		}
		for _, sn := range c.Scenarios {
			i := slices.IndexFunc(scenarios, func(s scenario) bool { return s.name == sn })
			if i < 0 {
				return nil, fmt.Errorf("unknown scenario %q", sn)
			}
			sc := scenarios[i]
			for n := 1; n <= c.Repeat; n++ {
				fmt.Fprintf(os.Stderr, "%s / %s (run %d)…\n", kn, sn, n)
				res := Result{Kind: kn, Scenario: sn, Run: n, Transport: c.Transport}
				if sn == "video" && clip == "" {
					res.Error = "ffmpeg not found: no clip to play"
				} else if err := runOne(c, k, sc, &res, r); err != nil {
					res.Error = err.Error()
				}
				if res.Error != "" {
					fmt.Fprintln(os.Stderr, "  error:", res.Error)
				}
				r.Results = append(r.Results, res)
			}
		}
	}
	return r, nil
}

// runOne shows sc's page on k, runs a fresh bridge over a fresh emulated
// link, waits for the first frame, sets up, then measures one window.
func runOne(c config, k kind, sc scenario, res *Result, r *report) error {
	ctx, cancel := context.WithTimeout(context.Background(), c.Load+c.Settle+c.Duration+c.Tail+90*time.Second)
	defer cancel()
	if err := k.open(ctx, sc.path); err != nil {
		return err
	}
	defer k.close()
	time.Sleep(c.Load)

	rec := NewRecorder()
	down := NewLink(c.DownMbps*1e6, c.Delay)
	down.OnDeliver = rec.Down
	up := NewLink(c.UpMbps*1e6, c.Delay)
	up.OnDeliver = rec.Up
	logf, err := os.Create(filepath.Join(c.logDir, fmt.Sprintf("%s-%s-%d.stderr", k.name(), sc.name, res.Run)))
	if err != nil {
		return err
	}
	defer logf.Close()
	conn := &Conn{
		stagent: c.stagent,
		env:     append(os.Environ(), "STAGENT_HOME="+c.stagentHome),
		down:    down, up: up, stderr: logf,
	}
	an := NewAnalyzer(rec)
	first := make(chan struct{})
	var gotFirst bool
	sink := func(f Frame) {
		an.Add(time.Now(), f)
		if !gotFirst {
			gotFirst = true
			close(first)
		}
	}
	t, err := transports[c.Transport](ctx, conn, sink)
	if err != nil {
		conn.Wait(5 * time.Second)
		an.Close()
		return err
	}
	// finish stops the stream, ends the channels and returns the bridge's
	// lifetime CPU.
	finish := func() time.Duration {
		sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
		t.Stop(sctx)
		scancel()
		t.Close()
		cpu := conn.Wait(10 * time.Second)
		an.Close()
		return cpu
	}
	r.StagentVersion = t.Version()

	l, err := t.List(ctx, k.listParams())
	if err != nil {
		finish()
		return fmt.Errorf("host_screen.list: %w", err)
	}
	target, err := k.pick(l)
	if err != nil {
		finish()
		return err
	}
	tb, _ := json.Marshal(target)
	res.Target = string(tb)
	if err := t.Start(ctx, target); err != nil {
		finish()
		return fmt.Errorf("host_screen.start: %w", err)
	}
	select {
	case <-first:
	case <-time.After(30 * time.Second):
		finish()
		return fmt.Errorf("no frame within 30 s")
	}
	e := &env{t: t, rec: rec, k: k}
	if sc.setup != nil {
		sc.setup(e)
	}
	time.Sleep(c.Settle)

	cpu0, cpuErr := cpuTime(conn.PIDs())
	from := time.Now()
	to := from.Add(c.Duration)
	sc.run(e, to)
	time.Sleep(time.Until(to))
	cpu1, cpuErr1 := cpuTime(conn.PIDs())
	time.Sleep(c.Tail)
	total := finish()

	res.Summary = rec.Summary(from, to)
	if c.frames {
		res.Frames, res.InputsMs = rec.Frames(from, to), rec.Inputs(from, to)
	}
	if cpuErr == nil && cpuErr1 == nil {
		res.StagentCPUPct = float64(cpu1-cpu0) / float64(c.Duration) * 100
	}
	res.StagentCPUTotalSec = total.Seconds()
	return nil
}

func (r *report) markdown() string {
	var b strings.Builder
	c := r.Config
	fmt.Fprintf(&b, "stagent %s, transport %s, link down %g Mbit/s up %g Mbit/s, one-way delay %v each way (RTT %v), %v per scenario, %s, %s\n\n",
		r.StagentVersion, c.Transport, c.DownMbps, c.UpMbps, c.Delay, 2*c.Delay, c.Duration, r.Host, r.Date.Format(time.RFC3339))
	b.WriteString("| kind | scenario | down kB/s | down Mbit/s | up kB/s | frames/s | full / tiles | kB/frame (JPEG) | q mean | image px | input→change p50 / p95 ms (n) | settle frame: q, px, kB, ms after (n full / n) | stagent CPU % | decode ms/frame |\n")
	b.WriteString("|---|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|\n")
	for _, x := range r.Results {
		if x.Error != "" {
			fmt.Fprintf(&b, "| %s | %s | — | — | — | — | — | — | — | — | — | — | — | — |\n", x.Kind, x.Scenario)
			continue
		}
		lat := "—"
		if l := x.Latency; l.Measured > 0 || l.Unsettled > 0 || l.NoChange > 0 {
			lat = fmt.Sprintf("%.0f / %.0f (%d", l.P50Ms, l.P95Ms, l.Measured)
			if l.Unsettled > 0 {
				lat += fmt.Sprintf(", %d unsettled", l.Unsettled)
			}
			if l.NoChange > 0 {
				lat += fmt.Sprintf(", %d unchanged", l.NoChange)
			}
			lat += ")"
		}
		px := "—"
		if x.ImageWidth > 0 {
			px = fmt.Sprintf("%d×%d", x.ImageWidth, x.ImageHeight)
		}
		settle := "—"
		if s := x.Settle; s.Count > 0 {
			q := fmt.Sprint(s.QualityMin)
			if s.QualityMax != s.QualityMin {
				q = fmt.Sprintf("%d–%d", s.QualityMin, s.QualityMax)
			}
			settle = fmt.Sprintf("q%s, %d×%d, %.1f, %.0f (%d / %d)", q, s.ImageWidth, s.ImageHeight, s.BytesMean/1000, s.DelayMsMean, s.Full, s.Count)
		}
		fmt.Fprintf(&b, "| %s | %s | %.1f | %.2f | %.2f | %.1f | %d / %d | %.1f | %.0f | %s | %s | %s | %.1f | %.1f |\n",
			x.Kind, x.Scenario, x.DownBytesPerSec/1000, x.DownBytesPerSec*8/1e6, x.UpBytesPerSec/1000,
			x.FramesPerSec, x.FullFrames, x.TileFrames, x.MeanFrameBytes/1000, x.QualityMean, px, lat, settle, x.StagentCPUPct, x.DecodeMsMean)
	}
	var errs []string
	for _, x := range r.Results {
		if x.Error != "" {
			errs = append(errs, fmt.Sprintf("- %s / %s: %s", x.Kind, x.Scenario, x.Error))
		}
	}
	if len(errs) > 0 {
		b.WriteString("\nNot measured:\n\n" + strings.Join(errs, "\n") + "\n")
	}
	return b.String()
}
