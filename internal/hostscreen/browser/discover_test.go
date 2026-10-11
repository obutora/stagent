package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/obutora/stagent/internal/ptable"
	"github.com/obutora/stagent/internal/wire"
)

// procSource serves the argv, owner and cwd of fake processes.
type procSource struct {
	argv  map[int][]string
	owner map[int]string
	cwd   map[int]string
}

func (s procSource) Argv(pid int) ([]string, error)  { return s.argv[pid], nil }
func (s procSource) Exe(int) (string, error)         { return "", errors.ErrUnsupported }
func (s procSource) Env(int) ([]string, error)       { return nil, errors.ErrUnsupported }
func (s procSource) OpenFiles(int) ([]string, error) { return nil, errors.ErrUnsupported }
func (s procSource) Cwd(pid int) (string, error)     { return s.cwd[pid], nil }
func (s procSource) Owner(pid int) (string, error) {
	if o, ok := s.owner[pid]; ok {
		return o, nil
	}
	return "", errors.New("unknown")
}

// cdpServer is an HTTP CDP endpoint with one page and one worker; it
// returns its port.
func cdpServer(t *testing.T, mux *http.ServeMux) int {
	t.Helper()
	if mux == nil {
		mux = http.NewServeMux()
	}
	var port int
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"Browser":"Chrome/150.0","Protocol-Version":"1.3","webSocketDebuggerUrl":"ws://127.0.0.1:%d/devtools/browser/b"}`, port)
	})
	mux.HandleFunc("/json/list", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `[{"id":"P1","type":"page","title":"Example","url":"https://example.com/","webSocketDebuggerUrl":"ws://127.0.0.1:%d/devtools/page/P1"},
			{"id":"W1","type":"service_worker","title":"sw","url":"https://example.com/sw.js"}]`, port)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	port = portOf(t, srv.URL)
	return port
}

func portOf(t *testing.T, raw string) int {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestListShowsOwnChromiumWithAVerifiedPort(t *testing.T) {
	const me, other = "1000", "1001"
	fixed := cdpServer(t, nil)
	active := cdpServer(t, nil)
	others := cdpServer(t, nil)
	notCDP := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(notCDP.Close)

	// --remote-debugging-port=0 writes the port to DevToolsActivePort.
	base := t.TempDir()
	mcpProfile := filepath.Join(base, "ms-playwright-mcp", "mcp-chrome-1")
	if err := os.MkdirAll(mcpProfile, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mcpProfile, "DevToolsActivePort"), fmt.Appendf(nil, "%d\n/devtools/browser/b\n", active), 0o644); err != nil {
		t.Fatal(err)
	}
	noFile := filepath.Join(base, "no-port-yet")
	agentProfile := filepath.Join(base, "agent-browser-chrome-x")
	chrome := filepath.Join(base, "chrome")
	cft := filepath.Join(base, "Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing")
	port := func(p int) string { return "--remote-debugging-port=" + strconv.Itoa(p) }

	procs := []ptable.Proc{
		{PID: 10, PPID: 1, Name: "chrome"},
		{PID: 11, PPID: 10, Name: "chrome"},
		{PID: 20, PPID: 1, Name: "Google Chrome for Testing"},
		{PID: 30, PPID: 1, Name: "chrome"},
		{PID: 40, PPID: 1, Name: "chrome"},
		{PID: 50, PPID: 1, Name: "chrome"},
		{PID: 60, PPID: 1, Name: "node"},
		{PID: 70, PPID: 1, Name: "chrome"},
	}
	src := procSource{
		argv: map[int][]string{
			10: {chrome, "--headless=new", port(fixed), "--user-data-dir=" + agentProfile, "about:blank"},
			11: {chrome, "--type=renderer", port(fixed)}, // a child
			// relative to its working directory
			20: {cft, "--remote-debugging-port=0", "--user-data-dir=" + filepath.Join("ms-playwright-mcp", "mcp-chrome-1")},
			30: {chrome, "--user-data-dir=" + agentProfile}, // no debugging port
			40: {chrome, port(others)},                      // another user's
			50: {chrome, port(portOf(t, notCDP.URL))},       // not CDP
			60: {"node", port(fixed)},                       // not Chromium
			70: {chrome, "--remote-debugging-port=0", "--user-data-dir=" + noFile},
		},
		owner: map[int]string{10: me, 11: me, 20: me, 30: me, 40: other, 50: me, 60: me, 70: me},
		cwd:   map[int]string{20: base},
	}
	k := &Kind{procs: func() (*ptable.Snapshot, string, error) { return ptable.New(procs, src), me, nil }}
	sec, err := k.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(sec)
	want := fmt.Sprintf(`{"items":[`+
		`{"pid":10,"tool":"agent-browser","headless":true,"port":%d,"connectable":true,"pages":[{"target_id":"P1","title":"Example","url":"https://example.com/"}]},`+
		`{"pid":20,"tool":"playwright-mcp","headless":false,"port":%d,"connectable":true,"pages":[{"target_id":"P1","title":"Example","url":"https://example.com/"}]}`+
		`]}`, fixed, active)
	if string(got) != want {
		t.Fatalf("list:\n got %s\nwant %s", got, want)
	}
}

func TestListWithoutBrowsersIsEmpty(t *testing.T) {
	k := &Kind{procs: func() (*ptable.Snapshot, string, error) {
		return ptable.New([]ptable.Proc{{PID: 1, Name: "init"}}, procSource{}), "1000", nil
	}}
	sec, err := k.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := json.Marshal(sec); string(got) != `{"items":[]}` {
		t.Fatalf("list = %s", got)
	}
}

func TestToolIsToldByTheProfilePath(t *testing.T) {
	for dir, want := range map[string]string{
		"/Users/me/.agent-browser/agent-browser-chrome-abc":                    wire.ToolAgentBrowser,
		"/Users/me/Library/Caches/ms-playwright-mcp/mcp-chrome-1":              wire.ToolPlaywrightMCP,
		"/home/me/.cache/chrome-devtools-mcp/chrome-profile-stable":            wire.ToolChromeDevToolsMCP,
		`C:\Users\me\AppData\Local\Temp\playwright_chromiumdev_profile-AbC123`: wire.ToolPlaywright,
		`C:\Users\me\AppData\Local\Google\Chrome\User Data`:                    "",
		"": "",
	} {
		got := toolOf(dir)
		if (got == nil) != (want == "") || got != nil && *got != want {
			t.Errorf("toolOf(%q) = %v, want %q", dir, got, want)
		}
	}
}

func TestListShowsPipeOnlyBrowsersAsNotConnectable(t *testing.T) {
	const me, other = "1000", "1001"
	fixed := cdpServer(t, nil)
	base := t.TempDir()
	devtools := filepath.Join(base, "chrome-devtools-mcp", "chrome-profile-stable")
	pwProfile := filepath.Join(base, "playwright_chromiumdev_profile-AbC")
	chrome := filepath.Join(base, "chrome")
	shell := filepath.Join(base, "chrome-headless-shell")

	procs := []ptable.Proc{
		{PID: 10, PPID: 1, Name: "chrome"},
		{PID: 11, PPID: 10, Name: "chrome"},
		{PID: 20, PPID: 1, Name: "chrome-headless-shell"},
		{PID: 30, PPID: 1, Name: "chrome"},
		{PID: 40, PPID: 1, Name: "chrome"},
		{PID: 50, PPID: 1, Name: "chrome"},
	}
	src := procSource{
		argv: map[int][]string{
			10: {chrome, "--remote-debugging-pipe", "--user-data-dir=" + devtools},
			11: {chrome, "--type=renderer", "--remote-debugging-pipe"}, // a child
			20: {shell, "--remote-debugging-pipe", "--user-data-dir=" + pwProfile},
			30: {chrome, "--remote-debugging-pipe"}, // another user's
			// the pipe and a port: connectable on the port
			40: {chrome, "--remote-debugging-pipe", "--remote-debugging-port=" + strconv.Itoa(fixed)},
			50: {chrome, "--remote-debugging-pipe", "--headless"},
		},
		owner: map[int]string{10: me, 11: me, 20: me, 30: other, 40: me, 50: me},
	}
	k := &Kind{procs: func() (*ptable.Snapshot, string, error) { return ptable.New(procs, src), me, nil }}
	sec, err := k.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(sec)
	want := fmt.Sprintf(`{"items":[`+
		`{"pid":10,"tool":"chrome-devtools-mcp","headless":false,"connectable":false,"reason":"pipe_only","pages":[]},`+
		`{"pid":20,"tool":"playwright","headless":true,"connectable":false,"reason":"pipe_only","pages":[]},`+
		`{"pid":40,"tool":null,"headless":false,"port":%d,"connectable":true,"pages":[{"target_id":"P1","title":"Example","url":"https://example.com/"}]},`+
		`{"pid":50,"tool":null,"headless":true,"connectable":false,"reason":"pipe_only","pages":[]}`+
		`]}`, fixed)
	if string(got) != want {
		t.Fatalf("list:\n got %s\nwant %s", got, want)
	}
}

func TestListPortsAddsVerifiedManualPortsMergingDiscoveredOnes(t *testing.T) {
	const me = "1000"
	found := cdpServer(t, nil)
	docker := cdpServer(t, nil) // no process: a container's published port
	notCDP := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(notCDP.Close)
	closed := httptest.NewServer(http.NotFoundHandler())
	closedPort := portOf(t, closed.URL)
	closed.Close()

	agentProfile := filepath.Join(t.TempDir(), "agent-browser-chrome-x")
	procs := []ptable.Proc{{PID: 10, PPID: 1, Name: "chrome"}}
	src := procSource{
		argv:  map[int][]string{10: {"chrome", "--remote-debugging-port=" + strconv.Itoa(found), "--user-data-dir=" + agentProfile}},
		owner: map[int]string{10: me},
	}
	k := &Kind{procs: func() (*ptable.Snapshot, string, error) { return ptable.New(procs, src), me, nil }}
	sec, err := k.ListPorts(t.Context(), []int{portOf(t, notCDP.URL), docker, closedPort, found})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(sec)
	page := `"pages":[{"target_id":"P1","title":"Example","url":"https://example.com/"}]`
	want := fmt.Sprintf(`{"items":[`+
		`{"pid":10,"tool":"agent-browser","headless":false,"port":%d,"manual":true,"connectable":true,%s},`+
		`{"tool":null,"port":%d,"manual":true,"connectable":true,%s}`+
		`]}`, found, page, docker, page)
	if string(got) != want {
		t.Fatalf("list:\n got %s\nwant %s", got, want)
	}
}
