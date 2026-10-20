package wire

import (
	"encoding/json"
	"maps"
	"reflect"
	"strings"
)

// ---------------------------------------------------------------------------
// Schedules (定期実行): a command the daemon runs at the times a schedule
// names, each run a detached holder session. The daemon keeps the
// schedules in <root>/state/schedules.json and each one's runs in
// <root>/state/schedule-runs/<id>.jsonl, and is the only writer of both.
//
// Later versions add members to Schedule, ScheduleWhen and ScheduleRun;
// the ones this version does not know are kept as they are (Extra) when
// the daemon writes them back.

// Schedule methods (see PROTOCOL.md). Served by the daemon (the bridge
// forwards them); Linux and macOS only (CapSchedules).
const (
	MethodScheduleList   = "schedule.list"   // {} → ScheduleListResult
	MethodScheduleCreate = "schedule.create" // ScheduleCreateParams → ScheduleEntry; refused to coding agents
	MethodScheduleRuns   = "schedule.runs"   // ScheduleRunsParams → ScheduleRunsResult
	// schedule.preview: what a schedule would run and when, without
	// saving it (ScheduleCreateParams → SchedulePreview).
	MethodSchedulePreview = "schedule.preview"
	// schedule.update, schedule.remove, schedule.pause, schedule.resume
	// and schedule.run_now change a schedule; all are refused to coding
	// agents.
	MethodScheduleUpdate = "schedule.update"  // ScheduleUpdateParams → ScheduleEntry
	MethodScheduleRemove = "schedule.remove"  // ScheduleRemoveParams → {}
	MethodSchedulePause  = "schedule.pause"   // ScheduleRef → ScheduleEntry
	MethodScheduleResume = "schedule.resume"  // ScheduleRef → ScheduleEntry
	MethodScheduleRunNow = "schedule.run_now" // ScheduleRef → ScheduleRun

	// NotifyScheduleChanged (daemon → app): a schedule or its newest run
	// changed, or it was removed (ScheduleChanged).
	NotifyScheduleChanged = "schedule.changed"
)

// CapSchedules: the daemon serves schedule.* (Linux and macOS hosts).
const CapSchedules = "schedules"

// SchedulesServedOn reports whether a host of goos (runtime.GOOS) serves
// schedules: Linux and macOS (a login shell and $SHELL -c), as the daemon
// serves them and the bridge announces CapSchedules.
func SchedulesServedOn(goos string) bool { return goos == "linux" || goos == "darwin" }

// ScheduleHarnesses are the coding agents an agent schedule can run
// (Schedule.Harness).
var ScheduleHarnesses = []string{HarnessClaude, HarnessCodex, HarnessOmp}

// ScheduleHarnessNames names ScheduleHarnesses as a sentence does
// ("claude, codex or omp").
func ScheduleHarnessNames() string {
	n := len(ScheduleHarnesses)
	return strings.Join(ScheduleHarnesses[:n-1], ", ") + " or " + ScheduleHarnesses[n-1]
}

// ScheduleSummaryMax bounds ScheduleTry.Summary (bytes).
const ScheduleSummaryMax = 8 << 10

// Schedule kinds.
const (
	ScheduleKindCommand = "command" // Schedule.Command through $SHELL -c
	// ScheduleKindAgent: a coding agent (Schedule.Harness) given
	// Schedule.Prompt, run non-interactively by `stagent schedule agent`.
	ScheduleKindAgent = "agent"
)

// Schedule.Permission values: how much an agent schedule's agent may do
// with nobody watching (#1083). Each maps to the harness's own flags
// (schedagent.Argv).
const (
	PermissionRead  = "read"  // read only (the default)
	PermissionWrite = "write" // change files in its directory
	PermissionAll   = "all"   // anything, without asking
)

// Schedule states.
const (
	ScheduleActive = "active"
	// SchedulePaused: its times pass without running it (schedule.pause);
	// schedule.resume runs it again from its next time on.
	SchedulePaused = "paused"
)

// ScheduleWhen.Every values: when a schedule runs, on the wall clock of
// its time zone (Schedule.TZ).
const (
	EveryDaily    = "daily"    // every day at Time
	EveryWeekdays = "weekdays" // Monday to Friday at Time
	EveryWeekly   = "weekly"   // on Days at Time
	EveryHours    = "hours"    // at Minute past every hour h of the day with h % Hours == 0
)

// EveryOnce: once, at Date Time (ScheduleWhen); after that the schedule
// has no next time and stays.
const EveryOnce = "once"

// EveryCron: at the times the cron expression Cron (ScheduleWhen) names,
// five fields: minute, hour, day of month, month, weekday.
const EveryCron = "cron"

// Schedule is one schedule (定め) as schedules.json keeps it.
type Schedule struct {
	ID   string `json:"id"` // 16 lowercase hex chars, given by the daemon
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Command is the shell string a command schedule runs ($SHELL -c).
	Command string       `json:"command,omitempty"`
	Cwd     string       `json:"cwd"` // absolute
	When    ScheduleWhen `json:"when"`
	// TZ is the IANA time zone (Asia/Tokyo) the times are on.
	TZ        string `json:"tz"`
	State     string `json:"state"`
	CreatedAt int64  `json:"created_at"` // unix ms
	// Harness (claude, codex, omp), Prompt and Permission (Permission*;
	// "" is read) of an agent schedule.
	Harness    string `json:"harness,omitempty"`
	Prompt     string `json:"prompt,omitempty"`
	Permission string `json:"permission,omitempty"`
	// TimeoutMin is a run's time limit in minutes (omitted: 60; 0: none).
	// At it the run is sent terminate, kill 10 seconds later, and is
	// recorded as RunTimedOut.
	TimeoutMin *int `json:"timeout_min,omitempty"`
	// Retries (0–3) is how many times a run that failed or timed out is
	// run again, RetryDelayMin (1–60, omitted: 5) minutes after it ended.
	Retries       int `json:"retries,omitempty"`
	RetryDelayMin int `json:"retry_delay_min,omitempty"`
	// StopAfterFailures (0: never) pauses the schedule when that many of
	// its times in a row failed, their retries included.
	StopAfterFailures int `json:"stop_after_failures,omitempty"`
	// FailedInRow (how many of its times in a row failed) and AutoStopped
	// (it was paused for StopAfterFailures) are the daemon's: create and
	// update ignore them, resume clears them.
	FailedInRow int  `json:"failed_in_row,omitempty"`
	AutoStopped bool `json:"auto_stopped,omitempty"`
	// LastAt (unix ms) is the newest of its times the daemon dealt with
	// (ran, ran late, skipped), or when the times before stopped counting
	// (created, updated, resumed). Times of When after it that have come
	// are missed: the daemon runs them once, late. The daemon's alone.
	LastAt int64 `json:"last_at,omitempty"`
	// Notify is which of its runs are notified (ScheduleNotify*; "" is
	// problems), with reason "schedule"; notify.reasons.schedule off
	// notifies none of any schedule.
	Notify string `json:"notify,omitempty"`
	// ProblemSent: a problem of it was notified and no success since (the
	// next success is notified as fixed). The daemon's, like FailedInRow.
	ProblemSent bool `json:"problem_sent,omitempty"`
	// Extra holds the members this version does not know.
	Extra map[string]json.RawMessage `json:"-"`
}

// Schedule.Notify values.
const (
	// ScheduleNotifyProblems: a problem (failed, timed out, denied,
	// undetermined or unjudged, skipped for an overlap, stopped after
	// failures); while problems go on, only the first and the stop, then
	// the first success as fixed. The default.
	ScheduleNotifyProblems = "problems"
	// ScheduleNotifyAlways: every time's last result.
	ScheduleNotifyAlways = "always"
	ScheduleNotifyOff    = "off"
)

// ValidScheduleNotify reports whether v is a Schedule.Notify ("" =
// default).
func ValidScheduleNotify(v string) bool {
	switch v {
	case "", ScheduleNotifyProblems, ScheduleNotifyAlways, ScheduleNotifyOff:
		return true
	}
	return false
}

// ScheduleWhen is when a schedule runs (ScheduleWhen.Every).
type ScheduleWhen struct {
	Every string `json:"every"`
	// Time is "HH:MM" (daily, weekdays, weekly).
	Time string `json:"time,omitempty"`
	// Days are the weekdays of a weekly schedule, 0 Sunday … 6 Saturday.
	Days []int `json:"days,omitempty"`
	// Date is "YYYY-MM-DD" (once).
	Date string `json:"date,omitempty"`
	// Hours (1–23) and Minute (0–59) of an hours schedule.
	Hours  int `json:"hours,omitempty"`
	Minute int `json:"minute,omitempty"`
	// Cron is a cron schedule's expression ("0 3 1 * *").
	Cron string `json:"cron,omitempty"`
	// Extra holds the members this version does not know.
	Extra map[string]json.RawMessage `json:"-"`
}

// Run triggers (a retry goes on in its time's line, ScheduleTry.Retry).
const (
	RunTriggerScheduled = "scheduled" // its time came
	RunTriggerNow       = "now"       // schedule.run_now
)

// RunTriggerLate: its time (the newest of the times missed while the host
// slept or the daemon was not running) came over a minute before it
// started.
const RunTriggerLate = "late"

// RunSkippedPaused: a once schedule's time passed while it was paused, so
// it did not run.
const RunSkippedPaused = "skipped_paused"

// Run results.
const (
	RunRunning   = "running"
	RunSucceeded = "succeeded" // a command run's exit code was 0
	// RunFailed: a non-zero exit code (ExitCode), or no exit code at all
	// (the run could not start, or its session was lost; Error says).
	RunFailed = "failed"
	// RunUndetermined: the run's end is not known (it ended while the
	// daemon was not running).
	RunUndetermined = "undetermined"
	// RunSkippedOverlap: its time came while the schedule's previous run
	// was still running, so it did not run.
	RunSkippedOverlap = "skipped_overlap"
	// RunDenied: an agent run went to its end, but some of what its agent
	// tried was refused (ScheduleRun.Denials).
	RunDenied = "denied"
	// RunTimedOut: still running at its schedule's time limit
	// (Schedule.TimeoutMin), so it was stopped.
	RunTimedOut = "timed_out"
)

// ScheduleRun is one time of a schedule (a time that came, or run_now):
// one line of its schedule-runs/<id>.jsonl. Its retries go on in the same
// line: the newest try is ScheduleTry (its members flat in the line), the
// ones before it are Tries.
type ScheduleRun struct {
	// RunID stays the same over the time's retries (the holder's --run).
	RunID       string `json:"run_id"`
	ScheduledAt int64  `json:"scheduled_at"` // unix ms: the time it was due
	Trigger     string `json:"trigger"`
	// Harness of an agent run.
	Harness string `json:"harness,omitempty"`
	// Cwd is the schedule's directory when the time started (absolute):
	// its runs and "continue this conversation" use it.
	Cwd string `json:"cwd,omitempty"`
	// Retries is the schedule's Retries when the time started (the m of
	// "retry n/m"; omitted: 0).
	Retries int `json:"retries,omitempty"`
	// TZOffset is the UTC offset in seconds (east positive) of the
	// schedule's tz at ScheduledAt; the app shows the run's times on that
	// wall clock.
	TZOffset int `json:"tz_offset"`
	// AutoStopped is how many of its times in a row had failed when this
	// one failing stopped the schedule (StopAfterFailures; omitted or 0:
	// it did not stop it).
	AutoStopped int `json:"auto_stopped,omitempty"`
	ScheduleTry
	// Tries are the tries before the newest, oldest first.
	Tries []ScheduleTry `json:"tries,omitempty"`
	// Extra holds the members this version does not know.
	Extra map[string]json.RawMessage `json:"-"`
}

// ScheduleTry is one try of a time of a schedule: the first one, or a
// retry of it.
type ScheduleTry struct {
	// Retry is which retry (1…) of ScheduleRun.Retries it is (0: the first
	// try).
	Retry     int    `json:"retry,omitempty"`
	StartedAt int64  `json:"started_at"`
	EndedAt   int64  `json:"ended_at,omitempty"`
	Result    string `json:"result"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	// Error says why a try failed without an exit code (one line).
	Error string `json:"error,omitempty"`
	// Summary is the end of a try's output as plain text, kept for when
	// the output is gone: a command run's last 50 lines, an agent run's
	// last reply; at most 8 KiB.
	Summary string `json:"summary,omitempty"`
	// The agent's conversation (ConversationID) and how many of its
	// operations were refused (Denials) of an agent run. Unjudged: its
	// output could not be read, so the result is the exit code's alone.
	ConversationID string `json:"conversation_id,omitempty"`
	Denials        int    `json:"denials,omitempty"`
	Unjudged       bool   `json:"unjudged,omitempty"`
	// OutsideGUI: the try's holder was started outside the GUI login
	// session (macOS with nobody logged in to the desktop: launchd refused
	// the job in gui/<uid>), where claude cannot read its login from the
	// keychain. AuthFailed: the try failed because its agent could not log
	// in.
	OutsideGUI bool `json:"outside_gui,omitempty"`
	AuthFailed bool `json:"auth_failed,omitempty"`
}

// HasSession reports whether a try of r ran in session sessionID.
func (r ScheduleRun) HasSession(sessionID string) bool {
	if r.SessionID == sessionID {
		return true
	}
	for _, t := range r.Tries {
		if t.SessionID == sessionID {
			return true
		}
	}
	return false
}

// ScheduleEntry is a schedule as schedule.list and schedule.create answer
// it.
type ScheduleEntry struct {
	Schedule Schedule `json:"schedule"`
	// NextAt is when it runs next, unix ms (omitted: never), NextOffset
	// the UTC offset in seconds of its tz then (omitted with NextAt).
	NextAt     int64 `json:"next_at,omitempty"`
	NextOffset *int  `json:"next_offset,omitempty"`
	// TZOffset is the UTC offset in seconds (east positive) of its tz now:
	// the app tells today from tomorrow on that wall clock.
	TZOffset int `json:"tz_offset"`
	// LastRun is its newest run (omitted: none yet).
	LastRun *ScheduleRun `json:"last_run,omitempty"`
	// RetryAt is when its newest run is run again (omitted: no retry
	// waits), unix ms.
	RetryAt int64 `json:"retry_at,omitempty"`
	// HostClock is the host's own clock at its next time, omitted when it
	// is its tz's.
	HostClock *HostClock `json:"host_clock,omitempty"`
}

// HostClock is the host's own clock at a time of a schedule (At, unix ms,
// its next) when it is not the clock of the schedule's tz: the host's zone
// (TZ: its IANA name, else its abbreviation, "UTC") and the UTC offsets in
// seconds of the host (Offset) and of the schedule's tz (TZOffset) then.
// The app says it once (#1085: "this host is on UTC; 9:00 is its 0:00").
type HostClock struct {
	TZ       string `json:"tz"`
	At       int64  `json:"at"`
	Offset   int    `json:"offset"`
	TZOffset int    `json:"tz_offset"`
}

type ScheduleListResult struct {
	Schedules []ScheduleEntry `json:"schedules"` // in the order they were created
}

// ScheduleCreateParams carries the schedule to create (schedule.create;
// id, state and created_at are the daemon's) or to preview.
type ScheduleCreateParams struct {
	Schedule Schedule `json:"schedule"`
}

type ScheduleRunsParams struct {
	ID string `json:"id"`
}

type ScheduleRunsResult struct {
	Runs []ScheduleRun `json:"runs"` // newest first
}

// ScheduleUpdateParams replaces what schedule ID runs and when
// (schedule.update): name, kind, command, cwd, when and tz as Schedule
// carries them; its id, state and created_at stay. The last update wins.
type ScheduleUpdateParams struct {
	ID       string   `json:"id"`
	Schedule Schedule `json:"schedule"`
}

// ScheduleRef names a schedule (schedule.pause, schedule.resume,
// schedule.run_now).
type ScheduleRef struct {
	ID string `json:"id"`
}

// ScheduleRemoveParams: StopRun stops the schedule's running run (its
// session) before the schedule and its runs are removed; without it the
// run goes on, no longer recorded.
type ScheduleRemoveParams struct {
	ID      string `json:"id"`
	StopRun bool   `json:"stop_run,omitempty"`
}

// ScheduleChanged is schedule.changed: Entry is the schedule as
// schedule.list has it now, omitted when it was removed.
type ScheduleChanged struct {
	ID    string         `json:"id"`
	Entry *ScheduleEntry `json:"entry,omitempty"`
}

// SchedulePreview is what a schedule would run and when (schedule.preview).
type SchedulePreview struct {
	Argv []string `json:"argv"`
	Cwd  string   `json:"cwd"`
	Next []int64  `json:"next"` // the next three times, unix ms
	// NextOffsets are the UTC offsets in seconds of the tz at each of Next
	// (as long as Next), TZOffset the one now.
	NextOffsets []int             `json:"next_offsets"`
	TZOffset    int               `json:"tz_offset"`
	Warnings    []ScheduleWarning `json:"warnings"`
	// CwdKind is what Cwd is (CwdKind*; omitted when it is not there);
	// TaskID the task whose worktree it is.
	CwdKind string `json:"cwd_kind,omitempty"`
	TaskID  string `json:"task_id,omitempty"`
	// HostClock is the host's own clock at the first of Next, omitted
	// when it is the schedule's tz's.
	HostClock *HostClock `json:"host_clock,omitempty"`
}

// Schedule warning codes.
const (
	// WarnCwdMissing: the directory is not there (schedule.create refuses
	// it).
	WarnCwdMissing = "cwd_missing"
	// WarnCodexOutsideGit: a codex schedule's directory is outside any Git
	// work tree; its runs pass --skip-git-repo-check.
	WarnCodexOutsideGit = "codex_outside_git"
	// WarnWritesCheckout: an agent schedule that may write runs in a
	// checkout the user works in, not a task's worktree.
	WarnWritesCheckout = "writes_checkout"
	// WarnClaudeGUILogin: a claude schedule on a macOS host without
	// ~/.claude/.credentials.json: claude reads its login from the
	// keychain, which its runs cannot while nobody is logged in to the
	// desktop.
	WarnClaudeGUILogin = "claude_gui_login"
)

// SchedulePreview.CwdKind values.
const (
	CwdKindTask      = "task"      // a task's worktree (not an in_place task)
	CwdKindCheckout  = "checkout"  // a Git work tree that is no task's
	CwdKindDirectory = "directory" // outside Git
)

type ScheduleWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// The JSON of the types with Extra: their own members, then the unknown
// ones as they were read.

type (
	scheduleJSON     Schedule
	scheduleWhenJSON ScheduleWhen
	scheduleRunJSON  ScheduleRun
)

func (s Schedule) MarshalJSON() ([]byte, error) {
	return marshalExtra(scheduleJSON(s), s.Extra)
}

func (s *Schedule) UnmarshalJSON(b []byte) error {
	return unmarshalExtra(b, (*scheduleJSON)(s), &s.Extra)
}

func (w ScheduleWhen) MarshalJSON() ([]byte, error) {
	return marshalExtra(scheduleWhenJSON(w), w.Extra)
}

func (w *ScheduleWhen) UnmarshalJSON(b []byte) error {
	return unmarshalExtra(b, (*scheduleWhenJSON)(w), &w.Extra)
}

func (r ScheduleRun) MarshalJSON() ([]byte, error) {
	return marshalExtra(scheduleRunJSON(r), r.Extra)
}

func (r *ScheduleRun) UnmarshalJSON(b []byte) error {
	return unmarshalExtra(b, (*scheduleRunJSON)(r), &r.Extra)
}

// marshalExtra encodes v, adding the members of extra it does not have.
func marshalExtra(v any, extra map[string]json.RawMessage) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil || len(extra) == 0 {
		return b, err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil, err
	}
	for k, raw := range extra {
		if _, ok := obj[k]; !ok {
			obj[k] = raw
		}
	}
	return json.Marshal(obj)
}

// unmarshalExtra decodes b into v and keeps the members v's type does not
// name in *extra (nil when there are none).
func unmarshalExtra(b []byte, v any, extra *map[string]json.RawMessage) error {
	if err := json.Unmarshal(b, v); err != nil {
		return err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	known := jsonNames(reflect.TypeOf(v).Elem())
	*extra = nil
	for k, raw := range obj {
		if known[k] {
			continue
		}
		if *extra == nil {
			*extra = map[string]json.RawMessage{}
		}
		(*extra)[k] = raw
	}
	return nil
}

// jsonNames are the member names t's fields encode as, those of an
// embedded struct (ScheduleRun's ScheduleTry) included.
func jsonNames(t reflect.Type) map[string]bool {
	names := map[string]bool{}
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch {
		case name == "-":
		case name == "" && f.Anonymous && f.Type.Kind() == reflect.Struct:
			maps.Copy(names, jsonNames(f.Type))
		case name == "":
			names[f.Name] = true
		default:
			names[name] = true
		}
	}
	return names
}
