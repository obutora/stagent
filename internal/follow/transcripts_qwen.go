package follow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/obutora/stagent/internal/wire"
)

// Qwen Code keeps a conversation in <runtime dir>/projects/<project's
// path, every character but [A-Za-z0-9] a dash>/chats/<id>.jsonl, appended
// to without keeping it open, and beside it <id>.runtime.json, the pid of
// the process that runs it and when that process started (rewritten by a
// resume, left behind at the end). It is found by a file of a chats
// directory it has open, else by the runtime sidecar of one of its
// processes in its directory's project (or a parent's: the project is the
// one it was started for), else by the conversation its command line
// resumes, else as the newest conversation of its directory.
var qwenFinder = finder{
	matcher:    qwenMatcher,
	breadcrumb: qwenRuntime,
	fromArgv:   (*transcripts).qwenFromArgv,
}

// qwenMatcher recognizes the files of a chats directory below the root and
// returns that conversation's file.
func qwenMatcher(a agentFacts) func(string) (string, bool) {
	root := newSpelledDir(a.roots[wire.HarnessQwen])
	return func(p string) (string, bool) {
		p, ok := root.below(p, 3)
		if !ok || filepath.Base(filepath.Dir(p)) != "chats" {
			return "", false
		}
		id, _, _ := strings.Cut(filepath.Base(p), ".")
		conv := filepath.Join(filepath.Dir(p), id+".jsonl")
		return conv, isFile(conv)
	}
}

// qwenProjectDir is the name Qwen Code gives the directory of a project at
// path (its sanitizeCwd, which works on UTF-16 code units and lowercases
// the path on Windows first).
func qwenProjectDir(path string) string {
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	var b strings.Builder
	for _, r := range path {
		switch {
		case r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'):
			b.WriteRune(r)
		case utf16.RuneLen(r) == 2:
			b.WriteString("--")
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// qwenRuntime returns the conversation whose runtime sidecar names one of
// the agent's processes, started when it did; ok with path "" when the
// conversation is not written yet.
func qwenRuntime(a agentFacts, _ func(string) (string, bool)) (string, bool) {
	root := a.roots[wire.HarnessQwen]
	if root == "" || a.cwd == "" {
		return "", false
	}
	for dir := filepath.Clean(a.cwd); ; dir = filepath.Dir(dir) {
		chats := filepath.Join(root, qwenProjectDir(dir), "chats")
		sidecars, _ := filepath.Glob(filepath.Join(chats, "*.runtime.json"))
		for _, s := range sidecars {
			b, err := os.ReadFile(s)
			if err != nil {
				continue
			}
			var r struct {
				PID       int     `json:"pid"`
				SessionID string  `json:"session_id"`
				StartedAt float64 `json:"started_at"`
			}
			if json.Unmarshal(b, &r) != nil || !slices.Contains(a.pids, r.PID) {
				continue
			}
			started := time.UnixMilli(int64(r.StartedAt * 1000))
			if d := started.Sub(a.start); d < -startSlack || d > time.Minute {
				continue // a process of the same pid before this one
			}
			conv := filepath.Join(chats, strings.TrimSuffix(filepath.Base(s), ".runtime.json")+".jsonl")
			if !isFile(conv) {
				return "", true
			}
			return conv, true
		}
		if parent := filepath.Dir(dir); parent == dir {
			return "", false
		}
	}
}

// qwenResume reads the conversation a Qwen Code command line resumes:
// `--resume[=]<id>`, `-r <id>`, `--session-id[=]<id>`; resumed without an
// id for the picker or with -c / --continue.
func qwenResume(argv []string) (id string, resumed bool) {
	for i, a := range argv {
		switch {
		case a == "--resume" || a == "-r" || a == "--session-id":
			if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
				return argv[i+1], true
			}
			return "", true
		case strings.HasPrefix(a, "--resume="):
			return strings.TrimPrefix(a, "--resume="), true
		case strings.HasPrefix(a, "--session-id="):
			return strings.TrimPrefix(a, "--session-id="), true
		case a == "--continue" || a == "-c":
			return "", true
		}
	}
	return "", false
}

// qwenFromArgv returns the conversation the agent's command line resumes
// (or names with --session-id, once it is written).
func (t *transcripts) qwenFromArgv(a agentFacts) (path string, fresh bool) {
	for _, pid := range a.pids {
		argv := a.argv(pid)
		if len(argv) == 0 {
			continue
		}
		if id, resumed := qwenResume(argv[1:]); resumed {
			return t.cachedArgv(instance{pid, a.start.UnixNano()}, func() string {
				if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
					return ""
				}
				p, _ := t.index(a.roots).Find(wire.HarnessQwen, id)
				return p
			}), false
		}
	}
	return "", true
}
