package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// writeConversations creates n synthetic stored conversations under home,
// split across the Claude Code, Codex and omp layouts, with distinct
// mtimes. Each file holds a realistic mix of records (~15 KiB).
func writeConversations(home string, n int) error {
	base := time.Now().Add(-time.Duration(n) * time.Minute)
	for i := range n {
		mtime := base.Add(time.Duration(i) * time.Minute)
		ts := mtime.UTC().Format("2006-01-02T15:04:05.000Z")
		id := uuid()
		cwd := fmt.Sprintf("/home/user/proj%d", i%40)
		var path string
		var lines []any
		switch i % 10 {
		case 0, 1, 2, 3: // claude
			path = filepath.Join(home, ".claude", "projects", strings.ReplaceAll(cwd, "/", "-"), id+".jsonl")
			lines = claudeLines(id, cwd, ts, i)
		case 4, 5, 6: // codex
			day := mtime.UTC()
			path = filepath.Join(home, ".codex", "sessions", day.Format("2006"), day.Format("01"), day.Format("02"),
				"rollout-"+day.Format("2006-01-02T15-04-05")+"-"+id+".jsonl")
			lines = codexLines(id, cwd, ts, i)
		default: // omp
			path = filepath.Join(home, ".omp", "agent", "sessions", "-"+strings.ReplaceAll(cwd, "/", "-")+"-",
				mtime.UTC().Format("2006-01-02T15-04-05-000Z")+"_"+id+".jsonl")
			lines = ompLines(id, cwd, ts, i)
		}
		if err := writeJSONL(path, lines); err != nil {
			return err
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			return err
		}
	}
	return nil
}

func writeJSONL(path string, lines []any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var b strings.Builder
	for _, l := range lines {
		j, err := json.Marshal(l)
		if err != nil {
			return err
		}
		b.Write(j)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

const filler = "The change updates the handler, adds tests for the boundary cases and keeps the public API unchanged. "

func claudeLines(id, cwd, ts string, i int) []any {
	common := func(kind string, msg any) map[string]any {
		return map[string]any{
			"parentUuid": uuid(), "isSidechain": false, "type": kind, "message": msg,
			"uuid": uuid(), "timestamp": ts, "userType": "external", "entrypoint": "cli",
			"cwd": cwd, "sessionId": id, "version": "2.1.270", "gitBranch": "main",
		}
	}
	lines := []any{
		map[string]any{"type": "mode", "mode": "normal", "sessionId": id},
		common("user", map[string]any{"role": "user", "content": fmt.Sprintf("Fix the flaky test number %d in the worker pool", i)}),
	}
	for t := range 12 {
		lines = append(lines,
			common("assistant", map[string]any{"role": "assistant", "model": "claude-fable-5-1", "content": []any{
				map[string]any{"type": "text", "text": strings.Repeat(filler, 4)},
				map[string]any{"type": "tool_use", "id": uuid(), "name": "Bash", "input": map[string]any{"command": fmt.Sprintf("go test ./... -run T%d", t)}},
			}}),
			common("user", map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": uuid(), "content": strings.Repeat("ok  \tpkg\t0.01s\n", 8)},
			}}),
		)
	}
	return lines
}

func codexLines(id, cwd, ts string, i int) []any {
	item := func(ord int, kind string, payload any) map[string]any {
		return map[string]any{"timestamp": ts, "ordinal": ord, "type": kind, "payload": payload}
	}
	lines := []any{
		item(0, "session_meta", map[string]any{"session_id": id, "id": id, "timestamp": ts, "cwd": cwd,
			"originator": "codex-tui", "cli_version": "0.154.0", "source": "cli"}),
		item(1, "response_item", map[string]any{"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": fmt.Sprintf("Refactor module %d to remove the global state", i)}}}),
	}
	for t := range 12 {
		lines = append(lines,
			item(2+2*t, "response_item", map[string]any{"type": "message", "role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": strings.Repeat(filler, 4)}}}),
			item(3+2*t, "response_item", map[string]any{"type": "function_call", "name": "shell",
				"arguments": fmt.Sprintf(`{"command":["go","test","./...","-run","T%d"]}`, t), "call_id": uuid()}),
		)
	}
	return lines
}

func ompLines(id, cwd, ts string, i int) []any {
	title := fmt.Sprintf("Investigate latency regression %d", i)
	lines := []any{
		map[string]any{"type": "title", "v": 1, "title": title, "source": "auto", "updatedAt": ts, "pad": strings.Repeat(" ", 40)},
		map[string]any{"type": "session", "version": 3, "id": id, "timestamp": ts, "cwd": cwd, "title": title, "titleSource": "auto"},
		map[string]any{"type": "message", "id": uuid()[:8], "timestamp": ts, "message": map[string]any{
			"role": "user", "content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("Why did request %d get slower?", i)}}}},
	}
	for range 12 {
		lines = append(lines, map[string]any{"type": "message", "id": uuid()[:8], "timestamp": ts, "message": map[string]any{
			"role": "assistant", "model": "claude-opus-5-5", "content": []any{
				map[string]any{"type": "text", "text": strings.Repeat(filler, 4)},
				map[string]any{"type": "toolCall", "id": uuid(), "name": "bash", "arguments": map[string]any{"command": "go test ./..."}},
			}}})
	}
	return lines
}

func uuid() string {
	var b [16]byte
	rand.Read(b[:])
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
