package browser

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/obutora/stagent/internal/ptable"
	"github.com/obutora/stagent/internal/wire"
)

const (
	// probeWait bounds one HTTP request to a CDP port.
	probeWait = 2 * time.Second
	// maxJSON bounds a /json/* answer.
	maxJSON = 4 << 20
)

// httpClient talks to CDP ports on 127.0.0.1 only: no proxy, no redirect
// to anywhere else.
var httpClient = &http.Client{
	Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// candidate is a browser process that opened a CDP port, or (port 0) one
// started with --remote-debugging-pipe only.
type candidate struct {
	pid      int
	port     int
	tool     *string
	headless bool
}

// candidates finds owner's Chromium-family browsers in s started with
// --remote-debugging-port or --remote-debugging-pipe (not their child
// processes) and the port each listens on: N of --remote-debugging-port=N,
// or for 0 the first line of DevToolsActivePort in its --user-data-dir;
// port 0 for a browser with the pipe only. Ports are not verified here.
func candidates(s *ptable.Snapshot, owner string) []candidate {
	var out []candidate
	seen := map[int]bool{}
	for _, pid := range s.PIDs() {
		argv := s.Argv(pid)
		if len(argv) == 0 || !isChromium(argv[0], s.Get(pid).Name) {
			continue
		}
		sw := switches(argv[1:])
		if _, child := sw["type"]; child {
			continue
		}
		portArg, ok := sw["remote-debugging-port"]
		_, pipe := sw["remote-debugging-pipe"]
		if !ok && !pipe {
			continue
		}
		pipeOnly := !ok
		port := 0
		if !pipeOnly {
			var err error
			port, err = strconv.Atoi(portArg)
			if err != nil || port < 0 || port > 65535 {
				continue
			}
		}
		// The owner costs a system call per process: asked last.
		if owner == "" || s.Owner(pid) != owner {
			continue
		}
		dir := sw["user-data-dir"]
		if dir != "" && !filepath.IsAbs(dir) {
			if cwd := s.Cwd(pid); cwd != "" {
				dir = filepath.Join(cwd, dir)
			}
		}
		if port == 0 && !pipeOnly {
			if port = activePort(dir); port == 0 {
				continue
			}
		}
		if port != 0 {
			if seen[port] {
				continue
			}
			seen[port] = true
		}
		_, headless := sw["headless"]
		out = append(out, candidate{
			pid:      pid,
			port:     port,
			tool:     toolOf(dir),
			headless: headless || strings.Contains(strings.ToLower(filepath.Base(argv[0])), "headless"),
		})
	}
	return out
}

// isChromium tells a Chromium-family browser by its executable (argv[0])
// or process name: Chrome, Chrome for Testing, chrome-headless-shell,
// Chromium, Edge, Brave.
func isChromium(arg0, name string) bool {
	for _, n := range []string{baseName(arg0), strings.ToLower(name)} {
		n = strings.TrimSuffix(n, ".exe")
		for _, m := range []string{"chrome", "chromium", "headless_shell", "msedge", "microsoft edge", "brave"} {
			if strings.Contains(n, m) {
				return true
			}
		}
	}
	return false
}

// baseName is the lowercased last element of a path of either OS.
func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		p = p[i+1:]
	}
	return strings.ToLower(p)
}

// switches parses Chromium command-line switches: "--name=value" or
// "--name" (value ""), "-name" too, up to a lone "--". Names compare
// lowercased, as Chromium does on Windows.
func switches(args []string) map[string]string {
	sw := map[string]string{}
	for _, a := range args {
		if a == "--" {
			break
		}
		var rest string
		switch {
		case strings.HasPrefix(a, "--"):
			rest = a[2:]
		case strings.HasPrefix(a, "-"):
			rest = a[1:]
		case runtime.GOOS == "windows" && strings.HasPrefix(a, "/"):
			rest = a[1:]
		default:
			continue
		}
		name, value, _ := strings.Cut(rest, "=")
		sw[strings.ToLower(name)] = value
	}
	return sw
}

// activePort reads the port Chrome wrote for --remote-debugging-port=0:
// line 1 of <user-data-dir>/DevToolsActivePort (0 when unknown).
func activePort(userDataDir string) int {
	if userDataDir == "" {
		return 0
	}
	f, err := os.Open(filepath.Join(userDataDir, "DevToolsActivePort"))
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 4096))
	if !sc.Scan() {
		return 0
	}
	port, err := strconv.Atoi(strings.TrimSpace(sc.Text()))
	if err != nil || port < 1 || port > 65535 {
		return 0
	}
	return port
}

// Profile path markers of the tools that drive a browser.
var toolMarkers = []struct{ marker, tool string }{
	{"agent-browser-chrome", wire.ToolAgentBrowser},
	{"ms-playwright-mcp", wire.ToolPlaywrightMCP},
	{"chrome-devtools-mcp", wire.ToolChromeDevToolsMCP},
	{"playwright_chromiumdev_profile", wire.ToolPlaywright},
}

// toolOf names the tool by the browser's profile path, nil when none.
func toolOf(userDataDir string) *string {
	p := strings.ToLower(userDataDir)
	for _, m := range toolMarkers {
		if strings.Contains(p, m.marker) {
			return &m.tool
		}
	}
	return nil
}

// cdpTarget is one entry of /json/list.
type cdpTarget struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// getJSON GETs http://127.0.0.1:<port><path> into v.
func getJSON(ctx context.Context, port int, path string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, probeWait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", path, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxJSON)).Decode(v)
}

// isCDP tells whether port answers /json/version as a CDP endpoint.
func isCDP(ctx context.Context, port int) bool {
	var v struct {
		Browser              string `json:"Browser"`
		ProtocolVersion      string `json:"Protocol-Version"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if getJSON(ctx, port, "/json/version", &v) != nil {
		return false
	}
	return v.ProtocolVersion != "" || v.WebSocketDebuggerURL != ""
}

// targets returns /json/list of port.
func targets(ctx context.Context, port int) ([]cdpTarget, error) {
	var list []cdpTarget
	err := getJSON(ctx, port, "/json/list", &list)
	return list, err
}

// discover lists the browsers of candidates whose port answers as CDP,
// with their page targets, and those with the pipe only (not connectable),
// in the candidates' (pid) order; then the ports of manual that answer as
// CDP and are no candidate's, in manual's order. A candidate on a port of
// manual is listed once, as manual.
func discover(ctx context.Context, cands []candidate, manual []int) []wire.HostScreenBrowserItem {
	type row struct {
		c      candidate
		found  bool // c is a process of the table
		manual bool
	}
	rows := make([]row, 0, len(cands)+len(manual))
	for _, c := range cands {
		rows = append(rows, row{c: c, found: true, manual: c.port != 0 && slices.Contains(manual, c.port)})
	}
	for _, p := range manual {
		if !slices.ContainsFunc(cands, func(c candidate) bool { return c.port == p }) {
			rows = append(rows, row{c: candidate{port: p}, manual: true})
		}
	}
	items := make([]*wire.HostScreenBrowserItem, len(rows))
	var wg sync.WaitGroup
	for i, r := range rows {
		wg.Go(func() {
			headless := r.c.headless
			if r.c.port == 0 {
				items[i] = &wire.HostScreenBrowserItem{
					PID: r.c.pid, Tool: r.c.tool, Headless: &headless, Reason: wire.HostScreenPipeOnly, Pages: []wire.HostScreenPage{},
				}
				return
			}
			if !isCDP(ctx, r.c.port) {
				return
			}
			pages := []wire.HostScreenPage{}
			list, _ := targets(ctx, r.c.port)
			for _, t := range list {
				if t.Type == "page" {
					pages = append(pages, wire.HostScreenPage{TargetID: t.ID, Title: t.Title, URL: t.URL})
				}
			}
			it := &wire.HostScreenBrowserItem{Port: r.c.port, Manual: r.manual, Connectable: true, Pages: pages}
			if r.found {
				it.PID, it.Tool, it.Headless = r.c.pid, r.c.tool, &headless
			}
			items[i] = it
		})
	}
	wg.Wait()
	out := []wire.HostScreenBrowserItem{}
	for _, it := range items {
		if it != nil {
			out = append(out, *it)
		}
	}
	return out
}
