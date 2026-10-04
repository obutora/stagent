package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

func TestUnwrappedReason(t *testing.T) {
	tty, noTTY := ptr(true), ptr(false)
	for _, c := range []struct {
		name string
		p    wire.HookEventParams
		want string
	}{
		{"claude in a terminal opened before the setup", wire.HookEventParams{Harness: wire.HarnessClaude, Entrypoint: "cli", ParentTTY: tty}, wire.UnwrappedOldTerminal},
		{"command claude in a prepared shell", wire.HookEventParams{Harness: wire.HarnessClaude, Entrypoint: "cli", ShellWrapper: true, ParentTTY: tty}, wire.UnwrappedBypassed},
		// The entry point decides for Claude Code: the extension's stdin is no terminal.
		{"VS Code extension", wire.HookEventParams{Harness: wire.HarnessClaude, Entrypoint: "claude-vscode", ParentTTY: noTTY}, wire.UnwrappedIDE},
		{"desktop app in a prepared session", wire.HookEventParams{Harness: wire.HarnessClaude, Entrypoint: "claude-desktop", ShellWrapper: true}, wire.UnwrappedIDE},
		{"claude -p", wire.HookEventParams{Harness: wire.HarnessClaude, Entrypoint: "sdk-cli", ParentTTY: tty, ParentBatch: true}, ""},
		{"Agent SDK", wire.HookEventParams{Harness: wire.HarnessClaude, Entrypoint: "sdk-ts", ShellWrapper: true}, ""},
		{"claude without an entry point, -p", wire.HookEventParams{Harness: wire.HarnessClaude, ParentTTY: tty, ParentBatch: true}, ""},
		{"claude without an entry point", wire.HookEventParams{Harness: wire.HarnessClaude, ParentTTY: tty}, wire.UnwrappedOldTerminal},
		{"codex in an old terminal", wire.HookEventParams{Harness: wire.HarnessCodex, ParentTTY: tty}, wire.UnwrappedOldTerminal},
		{"command codex", wire.HookEventParams{Harness: wire.HarnessCodex, ParentTTY: tty, ShellWrapper: true}, wire.UnwrappedBypassed},
		{"codex without a terminal (IDE, pipe)", wire.HookEventParams{Harness: wire.HarnessCodex, ParentTTY: noTTY, ShellWrapper: true}, ""},
		{"codex exec typed in a terminal", wire.HookEventParams{Harness: wire.HarnessCodex, ParentTTY: tty, ShellWrapper: true, ParentBatch: true}, ""},
		{"omp -p", wire.HookEventParams{Harness: wire.HarnessOmp, ParentTTY: tty, ParentBatch: true}, ""},
		{"omp, terminal unknown", wire.HookEventParams{Harness: wire.HarnessOmp}, wire.UnwrappedOldTerminal},
		{"not an agent harness", wire.HookEventParams{Harness: "other", ParentTTY: tty}, ""},
		// Codex tells how a conversation started (session_meta.originator).
		{"codex TUI in an old terminal", wire.HookEventParams{Harness: wire.HarnessCodex, Originator: "codex-tui", ParentTTY: tty}, wire.UnwrappedOldTerminal},
		{"codex exec", wire.HookEventParams{Harness: wire.HarnessCodex, Originator: "codex_exec", ParentTTY: tty, ShellWrapper: true}, ""},
		{"codex subagent thread", wire.HookEventParams{Harness: wire.HarnessCodex, Originator: "codex-tui", Subagent: true, ParentTTY: tty}, ""},
		{"Codex desktop app", wire.HookEventParams{Harness: wire.HarnessCodex, Originator: "Codex Desktop", ParentTTY: noTTY}, wire.UnwrappedIDE},
		{"unknown codex originator", wire.HookEventParams{Harness: wire.HarnessCodex, Originator: "codex_sdk_ts", ParentTTY: tty}, ""},
		// Elsewhere SSH and the ancestors do not count.
		{"claude over SSH (Linux)", wire.HookEventParams{Harness: wire.HarnessClaude, Entrypoint: "cli", ParentTTY: tty, ShellWrapper: true, SSH: true}, wire.UnwrappedBypassed},
	} {
		if got := unwrappedReason(c.p, false); got != c.want {
			t.Errorf("%s: reason %q, want %q", c.name, got, c.want)
		}
	}
}

// On Windows the terminal is unknown: SSH and the shell above the harness
// tell launches apart.
func TestUnwrappedReasonWindows(t *testing.T) {
	shell, noShell := ptr(true), ptr(false)
	for _, c := range []struct {
		name string
		p    wire.HookEventParams
		want string
	}{
		{"claude in an SSH PowerShell with the wrapper's profile", wire.HookEventParams{Harness: wire.HarnessClaude, Entrypoint: "cli", ShellWrapper: true, SSH: true, TerminalAncestor: shell}, wire.UnwrappedSSH},
		{"claude -p over SSH", wire.HookEventParams{Harness: wire.HarnessClaude, Entrypoint: "sdk-cli", SSH: true, TerminalAncestor: shell}, ""},
		{"codex exec over SSH", wire.HookEventParams{Harness: wire.HarnessCodex, Originator: "codex_exec", SSH: true, TerminalAncestor: shell}, ""},
		{"claude in a console opened before the setup", wire.HookEventParams{Harness: wire.HarnessClaude, Entrypoint: "cli", TerminalAncestor: shell}, wire.UnwrappedOldTerminal},
		{"claude started by an app", wire.HookEventParams{Harness: wire.HarnessClaude, Entrypoint: "cli", TerminalAncestor: noShell}, wire.UnwrappedNoTerminal},
		{"omp, ancestors unknown", wire.HookEventParams{Harness: wire.HarnessOmp}, wire.UnwrappedOldTerminal},
		{"command claude in a prepared console", wire.HookEventParams{Harness: wire.HarnessClaude, Entrypoint: "cli", ShellWrapper: true, TerminalAncestor: shell}, wire.UnwrappedBypassed},
		{"Codex desktop app", wire.HookEventParams{Harness: wire.HarnessCodex, Originator: "Codex Desktop", TerminalAncestor: noShell}, wire.UnwrappedIDE},
		{"codex TUI started by an app", wire.HookEventParams{Harness: wire.HarnessCodex, Originator: "codex-tui", TerminalAncestor: noShell}, wire.UnwrappedNoTerminal},
	} {
		if got := unwrappedReason(c.p, true); got != c.want {
			t.Errorf("%s: reason %q, want %q", c.name, got, c.want)
		}
	}
}

func unwrappedUpdate(t *testing.T, events chan *wire.Msg, what string, pred func([]wire.UnwrappedLaunch) bool) []wire.UnwrappedLaunch {
	t.Helper()
	var got []wire.UnwrappedLaunch
	await(t, events, what, func(m *wire.Msg) bool {
		if m.Method != wire.NotifyUnwrappedUpdated {
			return false
		}
		var p wire.UnwrappedUpdatedParams
		if err := json.Unmarshal(m.Params, &p); err != nil || p.Unwrapped == nil {
			t.Fatalf("unwrapped.updated %s: %v", m.Params, err)
		}
		got = p.Unwrapped
		return pred(got)
	})
	return got
}

func TestUnwrappedLaunches(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{UnwrappedTTL: 400 * time.Millisecond})
	watcher, events := e.client()
	var w wire.WatchResult
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, &w)
	if w.Unwrapped == nil || len(w.Unwrapped) != 0 {
		t.Fatalf("fresh watch unwrapped = %#v", w.Unwrapped)
	}
	hook := func(p wire.HookEventParams, payload string) {
		t.Helper()
		p.Payload = json.RawMessage(payload)
		_, done := e.hookAsync(p)
		if err := <-done; err != nil {
			t.Fatalf("hook %s: %v", payload, err)
		}
	}
	tty := ptr(true)
	stale := wire.HookEventParams{Harness: wire.HarnessClaude, Entrypoint: "cli", ParentTTY: tty}

	before := time.Now().UnixMilli()
	hook(stale, `{"hook_event_name":"SessionStart","session_id":"conv-old","cwd":"/home/u/api"}`)
	got := unwrappedUpdate(t, events, "first record", func(u []wire.UnwrappedLaunch) bool { return len(u) == 1 })
	if u := got[0]; u.ConversationID != "conv-old" || u.Harness != wire.HarnessClaude || u.Cwd != "/home/u/api" ||
		u.Reason != wire.UnwrappedOldTerminal || u.FirstSeenAt < before || u.LastActivityAt != u.FirstSeenAt {
		t.Fatalf("record %+v", u)
	}

	// Non-interactive runs and agents stagent started are not recorded.
	hook(wire.HookEventParams{Harness: wire.HarnessClaude, Entrypoint: "sdk-cli"}, `{"hook_event_name":"SessionStart","session_id":"conv-p"}`)
	hook(wire.HookEventParams{Harness: wire.HarnessCodex, ParentTTY: tty, ParentBatch: true}, `{"hook_event_name":"SessionStart","session_id":"conv-exec"}`)
	holder, _ := e.client()
	call(t, holder, wire.MethodHolderRegister, testSession(sid), nil)
	hook(wire.HookEventParams{Harness: wire.HarnessClaude, SessionID: sid}, `{"hook_event_name":"SessionStart","session_id":"conv-wrapped"}`)

	hook(wire.HookEventParams{Harness: wire.HarnessCodex, ParentTTY: tty, ShellWrapper: true}, `{"hook_event_name":"UserPromptSubmit","session_id":"conv-cmd","cwd":"/srv/web","prompt":"hi"}`)
	got = unwrappedUpdate(t, events, "second record", func(u []wire.UnwrappedLaunch) bool { return len(u) == 2 })
	if got[0].ConversationID != "conv-cmd" || got[0].Reason != wire.UnwrappedBypassed || got[1].ConversationID != "conv-old" {
		t.Fatalf("records %+v", got)
	}
	var st wire.DaemonStatus
	call(t, watcher, wire.MethodDaemonStatus, nil, &st)
	if len(st.Unwrapped) != 2 {
		t.Fatalf("daemon.status unwrapped = %+v", st.Unwrapped)
	}

	// SessionEnd forgets the conversation.
	hook(stale, `{"hook_event_name":"SessionEnd","session_id":"conv-old"}`)
	got = unwrappedUpdate(t, events, "SessionEnd", func(u []wire.UnwrappedLaunch) bool { return len(u) == 1 })
	if got[0].ConversationID != "conv-cmd" {
		t.Fatalf("after SessionEnd %+v", got)
	}

	// Hooks keep a record alive; it goes UnwrappedTTL after the last one.
	for range 3 {
		time.Sleep(200 * time.Millisecond)
		hook(wire.HookEventParams{Harness: wire.HarnessCodex, ParentTTY: tty, ShellWrapper: true}, `{"hook_event_name":"Stop","session_id":"conv-cmd"}`)
	}
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, &w)
	if len(w.Unwrapped) != 1 || w.Unwrapped[0].LastActivityAt <= got[0].LastActivityAt {
		t.Fatalf("record not kept alive by its hooks: %+v", w.Unwrapped)
	}
	unwrappedUpdate(t, events, "expiry", func(u []wire.UnwrappedLaunch) bool { return len(u) == 0 })

	// Records live in memory only: a restarted daemon has none.
	hook(stale, `{"hook_event_name":"SessionStart","session_id":"conv-again"}`)
	unwrappedUpdate(t, events, "record before restart", func(u []wire.UnwrappedLaunch) bool { return len(u) == 1 })
	e.stop()
	e2 := startDaemonAt(t, fastConfig(), Options{})
	c2, _ := e2.client()
	call(t, c2, wire.MethodWatch, wire.WatchParams{}, &w)
	if len(w.Unwrapped) != 0 {
		t.Fatalf("after restart unwrapped = %+v", w.Unwrapped)
	}
}
