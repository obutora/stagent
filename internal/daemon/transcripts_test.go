package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/pathstest"
	"github.com/obutora/stagent/internal/transcript"
	"github.com/obutora/stagent/internal/wire"
)

// Any process of the user can report a hook, so the transcript path a
// session's hooks report is read only when it lies in the transcript
// directory of the session's harness: transcript.get and
// transcript.subscribe by session never read a file elsewhere (a key, a
// token file, anything that happens to be JSON lines), even through a ".."
// that starts below the root. A later report of a path inside the roots is
// followed as before.
func TestSessionTranscriptOnlyUnderRoots(t *testing.T) {
	for name, report := range map[string]func(home, root, file string) string{
		"outside": func(home, _, file string) string { return filepath.Join(home, "secret", file) },
		// Unclean, as a hook may send it: lexically below the root first.
		"traversal": func(_, root, file string) string { return root + "/../../secret/" + file },
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			pathstest.SetHome(t, home)
			roots := transcript.Roots{wire.HarnessClaude: filepath.Join(home, "claude", "projects"), wire.HarnessCodex: filepath.Join(home, "codex"), wire.HarnessOmp: filepath.Join(home, "omp")}
			line := func(text string) string {
				return `{"type":"user","cwd":"/home/u/proj","entrypoint":"cli","timestamp":"2026-09-01T00:00:00Z","message":{"role":"user","content":"` + text + `"}}` + "\n"
			}
			conv := "2a425a50-3f53-4e94-8acb-f2d54d242f33"
			// A readable Claude transcript, so only where it lies keeps it out.
			secret := filepath.Join(home, "secret", conv+".jsonl")
			os.MkdirAll(filepath.Dir(secret), 0o700)
			os.WriteFile(secret, []byte(line("secret")), 0o600)
			own := filepath.Join(roots[wire.HarnessClaude], "-home-u-proj", "3b536b61-4f64-4a05-9bf9-03e65e353f44.jsonl")
			os.MkdirAll(filepath.Dir(own), 0o700)
			os.WriteFile(own, []byte(line("own")), 0o600)

			e := startDaemonAt(t, fastConfig(), Options{Roots: roots, TranscriptPoll: 20 * time.Millisecond})
			c, notes := e.client()
			h, _ := e.client()
			call(t, h, wire.MethodHolderRegister, testSession(sid), nil)
			hook := func(path string) {
				t.Helper()
				pathJSON, _ := json.Marshal(path) // a Windows path has backslashes
				call(t, h, wire.MethodHookEvent, wire.HookEventParams{Harness: wire.HarnessClaude, Event: "UserPromptSubmit", SessionID: sid,
					Payload: json.RawMessage(`{"session_id":"` + conv + `","transcript_path":` + string(pathJSON) + `,"cwd":"/home/u/proj"}`)}, nil)
			}
			hook(report(home, roots[wire.HarnessClaude], filepath.Base(secret)))

			err := c.Call(context.Background(), wire.MethodTranscriptGet, wire.TranscriptGetParams{SessionID: sid}, nil)
			var we *wire.Error
			if !errors.As(err, &we) || we.Code != wire.ErrNotFound {
				t.Fatalf("transcript.get of a session whose reported path is outside the roots: %v", err)
			}

			call(t, c, wire.MethodTranscriptSubscribe, wire.TranscriptSubscribeParams{SessionID: sid}, nil)
			f, _ := os.OpenFile(secret, os.O_WRONLY|os.O_APPEND, 0)
			f.WriteString(line("more secret"))
			f.Close()
			time.Sleep(5 * 20 * time.Millisecond) // polls that would push the appended line
			hook(own)                             // the session's next report: its own transcript
			m := await(t, notes, "transcript", func(m *wire.Msg) bool { return m.Method == wire.NotifyTranscript })
			var tp wire.TranscriptParams
			json.Unmarshal(m.Params, &tp)
			if tp.Path != own || len(tp.Messages) != 1 || tp.Messages[0].Text != "own" {
				t.Fatalf("first transcript notification %+v, want only the session's own transcript", tp)
			}
		})
	}
}
