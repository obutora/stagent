package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// writeUsageTree writes about mb MiB of Claude Code transcripts under
// home/.claude/projects for the usage scan: sessions of ~2 MiB spread over
// 40 projects and the last 60 days, each turn a prompt, a response
// streamed as three records with the same message.id:requestId (thinking,
// text, tool_use) carrying usage, and a tool result; every 50th turn has a
// large (~256 KiB) tool result. Every 10th session has a sub-agent run.
// It returns how many requests it wrote.
func writeUsageTree(home string, mb int) (requests int, err error) {
	const sessionBytes = 2 << 20
	root := filepath.Join(home, ".claude", "projects")
	want := int64(mb) << 20
	now := time.Now()
	var written int64
	text := strings.Repeat(filler, 6)
	result := strings.Repeat(`ok  \tpkg/worker\t0.01s\n`, 300)
	large := strings.Repeat(`line of a long build log that goes on\n`, 6700)
	for s := 0; written < want; s++ {
		id := uuid()
		cwd := fmt.Sprintf("/home/user/usage-proj%d", s%40)
		dir := filepath.Join(root, strings.ReplaceAll(cwd, "/", "-"))
		start := now.Add(-time.Duration(s%60)*24*time.Hour - time.Duration(s%24)*time.Hour)
		files := []string{filepath.Join(dir, id+".jsonl")}
		if s%10 == 0 {
			files = append(files, filepath.Join(dir, id, "subagents", "agent-"+id[:8]+".jsonl"))
		}
		for _, path := range files {
			n, reqs, err := writeUsageSession(path, id, cwd, start, sessionBytes, text, result, large)
			if err != nil {
				return requests, err
			}
			written += n
			requests += reqs
			mtime := start.Add(3 * time.Hour)
			if err := os.Chtimes(path, mtime, mtime); err != nil {
				return requests, err
			}
		}
	}
	return requests, nil
}

func writeUsageSession(path, id, cwd string, start time.Time, size int64, text, result, large string) (written int64, requests int, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, 0, err
	}
	f, err := os.Create(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	common := func(kind, ts string) string {
		return fmt.Sprintf(`"type":%q,"uuid":%q,"timestamp":%q,"userType":"external","entrypoint":"cli","cwd":%q,"sessionId":%q,"version":"2.1.270","gitBranch":"main"`,
			kind, uuid(), ts, cwd, id)
	}
	for t := 0; written < size; t++ {
		at := start.Add(time.Duration(t) * 20 * time.Second)
		ts := at.UTC().Format("2006-01-02T15:04:05.000Z")
		msg, req := "msg_"+uuid()[:24], "req_"+uuid()[:24]
		var lines []string
		lines = append(lines, fmt.Sprintf(`{"parentUuid":null,"isSidechain":false,"message":{"role":"user","content":"Fix the flaky test number %d in the worker pool"},%s}`, t, common("user", ts)))
		blocks := []string{
			fmt.Sprintf(`{"type":"thinking","thinking":%q,"signature":"c2ln"}`, text),
			fmt.Sprintf(`{"type":"text","text":%q}`, text),
			fmt.Sprintf(`{"type":"tool_use","id":"toolu_%s","name":"Bash","input":{"command":"go test ./... -run T%d"}}`, uuid()[:20], t),
		}
		for b, block := range blocks {
			out := 40 + 150*b + t%97
			lines = append(lines, fmt.Sprintf(`{"parentUuid":%q,"isSidechain":false,"message":{"model":"claude-opus-5-5","id":%q,"type":"message","role":"assistant","content":[%s],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":2,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":%d,"server_tool_use":{"web_search_requests":0,"web_fetch_requests":0},"service_tier":"standard","cache_creation":{"ephemeral_1h_input_tokens":%d,"ephemeral_5m_input_tokens":0},"inference_geo":"global"}},"requestId":%q,%s}`,
				uuid(), msg, block, 300+t%500, 20000+t*400, out, 300+t%500, req, common("assistant", ts)))
		}
		requests++
		res := result
		if t%50 == 49 {
			res = large
		}
		lines = append(lines, fmt.Sprintf(`{"parentUuid":%q,"isSidechain":false,"message":{"role":"user","content":[{"tool_use_id":"toolu_x","type":"tool_result","content":%q,"is_error":false}]},%s}`,
			uuid(), res, common("user", ts)))
		for _, l := range lines {
			n, err := w.WriteString(l + "\n")
			written += int64(n)
			if err != nil {
				return written, requests, err
			}
		}
	}
	return written, requests, w.Flush()
}

// measureUsage waits for the daemon's first usage scan, then times
// usage.get over the unchanged tree.
func measureUsage(res *results, call func(method string, params, result any) error, bridgeStart time.Time) error {
	p := wire.UsageGetParams{TZ: "Asia/Tokyo"}
	var r wire.UsageGetResult
	for {
		if err := call(wire.MethodUsageGet, p, &r); err != nil {
			return err
		}
		if !r.Scanning {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	res.usageScan = time.Since(bridgeStart)
	for range 10 {
		t0 := time.Now()
		if err := call(wire.MethodUsageGet, p, &r); err != nil {
			return err
		}
		res.usageWarm = append(res.usageWarm, time.Since(t0))
	}
	res.usageDays = len(r.Days)
	for _, d := range r.Days {
		res.usageCounted += d.Requests
	}
	return nil
}

// dirSize is the total size of the files below dir.
func dirSize(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}
