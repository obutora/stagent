//go:build !windows

package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

func agentSchedule(cwd, harness, permission, prompt string) wire.Schedule {
	return wire.Schedule{Name: "review", Kind: wire.ScheduleKindAgent, Harness: harness, Prompt: prompt,
		Permission: permission, Cwd: cwd, When: wire.ScheduleWhen{Every: wire.EveryDaily, Time: "09:00"}, TZ: "Asia/Tokyo"}
}

// gitDir makes dir a Git work tree (a .git directory) and returns it.
func gitDir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func (e *scheduleEnv) preview(s wire.Schedule) wire.SchedulePreview {
	e.t.Helper()
	var p wire.SchedulePreview
	call(e.t, e.c, wire.MethodSchedulePreview, wire.ScheduleCreateParams{Schedule: s}, &p)
	return p
}

func warningCodes(p wire.SchedulePreview) []string {
	var codes []string
	for _, w := range p.Warnings {
		codes = append(codes, w.Code)
	}
	return codes
}

// An agent schedule is saved with its harness, prompt and permission (read
// when none is given), and nothing else of a command schedule; what is
// not one of the three agents, an empty prompt or an unknown permission
// is refused.
func TestAgentScheduleCreate(t *testing.T) {
	e := startScheduleDaemon(t, time.Now(), nil, nil)
	cwd := t.TempDir()
	s := agentSchedule(cwd, wire.HarnessClaude, "", "README の誤字を探して")
	s.Command = "rm -rf /"
	entry := e.create(s)
	got := entry.Schedule
	if got.Kind != wire.ScheduleKindAgent || got.Harness != wire.HarnessClaude || got.Prompt != "README の誤字を探して" ||
		got.Permission != wire.PermissionRead || got.Command != "" || entry.NextAt == 0 {
		t.Errorf("created %+v (next %d)", got, entry.NextAt)
	}
	for name, s := range map[string]wire.Schedule{
		"pi":           agentSchedule(cwd, wire.HarnessPi, "", "hi"),
		"no harness":   agentSchedule(cwd, "", "", "hi"),
		"empty prompt": agentSchedule(cwd, wire.HarnessCodex, "", "  \n"),
		"NUL":          agentSchedule(cwd, wire.HarnessCodex, "", "a\x00b"),
		"permission":   agentSchedule(cwd, wire.HarnessOmp, "root", "hi"),
	} {
		err := e.c.Call(t.Context(), wire.MethodScheduleCreate, wire.ScheduleCreateParams{Schedule: s}, nil)
		if errCode(err) != wire.ErrBadRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// schedule.preview shows the agent's command line, what the directory is
// (a task's worktree, a checkout, outside Git) and the warnings that
// apply: codex outside Git, and writing in a checkout that is no task's.
func TestAgentSchedulePreview(t *testing.T) {
	e := startScheduleDaemon(t, time.Now(), nil, nil)
	base := t.TempDir()
	repo := gitDir(t, filepath.Join(base, "repo"))
	worktree := gitDir(t, filepath.Join(base, "repo-7"))
	outside := filepath.Join(base, "plain")
	os.MkdirAll(outside, 0o755)
	c, _ := e.client()
	tk := registerTask(t, c, issueTask(repo, 7, "00000000000000a1")).Task

	p := e.preview(agentSchedule(repo, wire.HarnessClaude, wire.PermissionWrite, "fix the typo"))
	want := []string{"claude", "-p", "--output-format", "json", "--permission-prompts", "none", "--permission-mode", "acceptEdits", "--", "fix the typo"}
	if !slices.Equal(p.Argv, want) || p.CwdKind != wire.CwdKindCheckout || p.TaskID != "" ||
		!slices.Equal(warningCodes(p), []string{wire.WarnWritesCheckout}) {
		t.Errorf("claude write in a checkout: %+v", p)
	}
	if p := e.preview(agentSchedule(repo, wire.HarnessClaude, wire.PermissionRead, "x")); len(p.Warnings) != 0 || p.CwdKind != wire.CwdKindCheckout {
		t.Errorf("claude read in a checkout: %+v", p)
	}

	p = e.preview(agentSchedule(filepath.Join(worktree), wire.HarnessCodex, wire.PermissionAll, "x"))
	want = []string{"codex", "exec", "--json", "--dangerously-bypass-approvals-and-sandbox", "--", "x"}
	if !slices.Equal(p.Argv, want) || p.CwdKind != wire.CwdKindTask || p.TaskID != tk.ID || len(p.Warnings) != 0 {
		t.Errorf("codex all in a task: %+v", p)
	}

	p = e.preview(agentSchedule(outside, wire.HarnessCodex, wire.PermissionWrite, "x"))
	want = []string{"codex", "exec", "--json", "--sandbox", "workspace-write", "--skip-git-repo-check", "--", "x"}
	if !slices.Equal(p.Argv, want) || p.CwdKind != wire.CwdKindDirectory || !slices.Equal(warningCodes(p), []string{wire.WarnCodexOutsideGit}) {
		t.Errorf("codex outside Git: %+v", p)
	}

	p = e.preview(agentSchedule(filepath.Join(base, "gone"), wire.HarnessOmp, wire.PermissionRead, "x"))
	if p.CwdKind != "" || !slices.Equal(warningCodes(p), []string{wire.WarnCwdMissing}) {
		t.Errorf("missing cwd: %+v", p)
	}
}

// A directory in a task's worktree that lies in another task's worktree
// (nested) is the inner task's: the innermost worktree holding it, as a
// session's task is.
func TestSchedulePreviewNestedWorktreeIsTheInnerTasks(t *testing.T) {
	e := startScheduleDaemon(t, time.Now(), nil, nil)
	base := t.TempDir()
	repo := gitDir(t, filepath.Join(base, "repo"))
	outerWT := gitDir(t, filepath.Join(base, "repo-7"))
	innerWT := gitDir(t, filepath.Join(outerWT, "wt", "repo-8"))
	sub := filepath.Join(innerWT, "src")
	os.MkdirAll(sub, 0o755)
	c, _ := e.client()
	outer := registerTask(t, c, issueTask(repo, 7, "00000000000000a1")).Task
	ip := issueTask(repo, 8, "00000000000000a2")
	ip.Worktree, ip.Branch = innerWT, "8-other"
	inner := registerTask(t, c, ip).Task

	for _, c := range []struct{ cwd, task string }{{sub, inner.ID}, {innerWT, inner.ID}, {outerWT, outer.ID}} {
		if p := e.preview(commandSchedule(c.cwd, "true", dailyNine())); p.CwdKind != wire.CwdKindTask || p.TaskID != c.task {
			t.Errorf("%s: %s %s, want task %s", c.cwd, p.CwdKind, p.TaskID, c.task)
		}
	}
}

// An agent schedule is changed as it is created (what a command schedule
// uses goes, read when no permission is given), and 今すぐ動かす runs its
// agent under the wrapper like its time does.
func TestAgentScheduleUpdateAndRunNow(t *testing.T) {
	e := startScheduleDaemon(t, time.Now(), nil, nil)
	cwd := t.TempDir()
	id := e.create(agentSchedule(cwd, wire.HarnessClaude, wire.PermissionAll, "look")).Schedule.ID

	s := agentSchedule(cwd, wire.HarnessOmp, "", "look again")
	s.Command = "rm -rf /"
	var en wire.ScheduleEntry
	call(t, e.c, wire.MethodScheduleUpdate, wire.ScheduleUpdateParams{ID: id, Schedule: s}, &en)
	if got := en.Schedule; got.Harness != wire.HarnessOmp || got.Prompt != "look again" || got.Permission != wire.PermissionRead || got.Command != "" {
		t.Errorf("updated %+v", got)
	}

	var run wire.ScheduleRun
	call(t, e.c, wire.MethodScheduleRunNow, wire.ScheduleRef{ID: id}, &run)
	cmd := e.awaitSpawn().command()
	if run.Trigger != wire.RunTriggerNow || run.Harness != wire.HarnessOmp {
		t.Errorf("run_now %+v", run)
	}
	if i := slices.Index(cmd, "--"); len(cmd) < 3 || cmd[1] != "schedule" || cmd[2] != "agent" || i < 0 || cmd[len(cmd)-1] != "look again" {
		t.Errorf("command %q", cmd)
	}
}

// A run starts the agent under `stagent schedule agent`, which leaves its
// judgement in the file the daemon names; the run records it with the
// conversation and reply. Without that file the exit code decides, and
// the run says it could not be judged.
func TestAgentScheduleRunRecordsTheOutcome(t *testing.T) {
	start := time.Date(2026, 10, 19, 23, 59, 0, 0, time.UTC)
	e := startScheduleDaemon(t, start, nil, nil)
	cwd := t.TempDir()
	id := e.create(agentSchedule(cwd, wire.HarnessClaude, wire.PermissionRead, "write hello.txt")).Schedule.ID

	nine := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	e.clock.Set(nine.Add(time.Second))
	sp := e.awaitSpawn()
	cmd := sp.command()
	i := slices.Index(cmd, "--")
	if len(cmd) < 4 || cmd[1] != "schedule" || cmd[2] != "agent" || i < 0 || cmd[i-2] != "--result" {
		t.Fatalf("command %q", cmd)
	}
	result := cmd[i-1]
	wantAgent := []string{"claude", "-p", "--output-format", "json", "--permission-prompts", "none", "--permission-mode", "manual", "--", "write hello.txt"}
	if !slices.Equal(cmd[i+1:], wantAgent) {
		t.Errorf("agent %q", cmd[i+1:])
	}
	if run := e.awaitRun(id, func(wire.ScheduleRun) bool { return true }); run.Harness != wire.HarnessClaude || run.Result != wire.RunRunning {
		t.Errorf("started run %+v", run)
	}

	b, _ := json.Marshal(map[string]any{"result": "denied", "denials": 2, "conversation_id": "b0d56a6d", "summary": "書けませんでした"})
	if err := os.WriteFile(result, b, 0o600); err != nil {
		t.Fatal(err)
	}
	h := e.holderOf(sp)
	h.Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: sp.flag("id"), ExitCode: 0})
	run := e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.Result != wire.RunRunning })
	if run.Result != wire.RunDenied || run.Denials != 2 || run.ConversationID != "b0d56a6d" || run.Summary != "書けませんでした" ||
		run.Harness != wire.HarnessClaude || run.ExitCode == nil || *run.ExitCode != 0 || run.Unjudged {
		t.Errorf("ended run %+v", run)
	}
	if _, err := os.Stat(result); !os.IsNotExist(err) {
		t.Errorf("outcome file left: %v", err)
	}

	// The next day the wrapper leaves nothing (it was killed).
	e.clock.Set(nine.Add(24*time.Hour + time.Second))
	sp = e.awaitSpawn()
	h = e.holderOf(sp)
	h.Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: sp.flag("id"), ExitCode: 3})
	run = e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.RunID == sp.flag("run") && r.Result != wire.RunRunning })
	if run.Result != wire.RunFailed || !run.Unjudged || run.ExitCode == nil || *run.ExitCode != 3 {
		t.Errorf("run without an outcome %+v", run)
	}
}

// An agent run killed at its time limit leaves no outcome; it timed out,
// which is all its record says (not also unjudged).
func TestAgentScheduleTimedOutIsNotUnjudged(t *testing.T) {
	e := startScheduleDaemon(t, tokyoNine.Add(-time.Minute), nil, nil)
	s := agentSchedule(t.TempDir(), wire.HarnessClaude, wire.PermissionRead, "look")
	s.TimeoutMin = new(15)
	id := e.create(s).Schedule.ID
	sp, h := e.startRunning(tokyoNine.Add(time.Second))
	signalHolder(t, e, sp, h, func(sig string) (int, bool) { return 137, sig == wire.SignalKill })
	e.clock.Set(tokyoNine.Add(15*time.Minute + 2*time.Second))
	time.Sleep(50 * time.Millisecond)
	e.clock.Set(tokyoNine.Add(15*time.Minute + 13*time.Second))
	run := e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.Result != wire.RunRunning })
	if run.Result != wire.RunTimedOut || run.Unjudged {
		t.Errorf("run %+v", run)
	}
}

// A schedule run's session handles no approvals: claude's PermissionRequest
// hook returns at once (claude -p then refuses the tool), nothing is
// registered, and it raises no needs_approval, waiting_input or
// turn_complete notification.
func TestScheduleRunSessionHandlesNoApprovals(t *testing.T) {
	e := startScheduleDaemon(t, time.Now(), nil, nil)
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)

	const sid, other = "00000000000000b1", "00000000000000b2"
	s := testSession(sid)
	s.Command = []string{"/usr/bin/stagent", "schedule", "agent", "--result", "/tmp/r.json", "--", "claude", "-p"}
	s.State, s.ScheduleID, s.RunID = wire.StateWorking, "0123456789abcdef", "fedcba9876543210"
	e.register(s)
	e.register(testSession(other))

	_, reply := e.hookAsync(wire.HookEventParams{
		Harness: wire.HarnessClaude, SessionID: sid,
		Payload: json.RawMessage(`{"hook_event_name":"PermissionRequest","session_id":"conv-1","cwd":"/home/u/proj","tool_name":"Write","tool_input":{"file_path":"/home/u/proj/a.txt"}}`),
	})
	select {
	case err := <-reply:
		if err != nil {
			t.Fatalf("PermissionRequest: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("PermissionRequest hook held")
	}
	e.hook(wire.HookEventParams{Harness: wire.HarnessClaude, SessionID: sid,
		Payload: json.RawMessage(`{"hook_event_name":"Notification","session_id":"conv-1","notification_type":"idle_prompt","message":"waiting"}`)})
	e.hook(wire.HookEventParams{Harness: wire.HarnessClaude, SessionID: sid,
		Payload: json.RawMessage(`{"hook_event_name":"Stop","session_id":"conv-1","last_assistant_message":"done"}`)})
	var list wire.ApprovalsListResult
	call(t, e.c, wire.MethodApprovalsList, nil, &list)
	if len(list.Approvals) != 0 {
		t.Errorf("approvals %+v", list.Approvals)
	}

	// Another session's turn_complete still comes, after anything of the
	// run's would have (the state debounce is 30 ms).
	time.Sleep(150 * time.Millisecond)
	e.hook(wire.HookEventParams{Harness: wire.HarnessClaude, SessionID: other,
		Payload: json.RawMessage(`{"hook_event_name":"Stop","session_id":"conv-2"}`)})
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-events:
			if m.Method != wire.NotifyEvent {
				continue
			}
			var ev wire.Event
			json.Unmarshal(m.Params, &ev)
			if ev.Kind == wire.EventApprovalRequested {
				t.Fatalf("approval requested: %s", ev.Data)
			}
			if ev.Kind != wire.EventNotification {
				continue
			}
			if ev.SessionID == sid {
				t.Fatalf("notification of the run: %s", ev.Data)
			}
			if ev.SessionID == other {
				return
			}
		case <-deadline:
			t.Fatal("no turn_complete of the other session")
		}
	}
}
