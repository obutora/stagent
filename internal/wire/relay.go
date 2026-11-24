package wire

import (
	"crypto/rand"
	"encoding/hex"
)

// ---------------------------------------------------------------------------
// Relay (リレー, ssh-term ADR 0004 and 0005): a conversation's content and its
// working tree handed to a new conversation of another agent — another
// harness, model or host. The relay source's stagent (リレー元) builds a
// package (the relay document, リレーの文書, its images, the hand-off memo
// and the working tree: a git bundle of the commits no remote has, the
// uncommitted changes, the untracked files); the client reads it in chunks
// and writes it to the relay target's stagent (リレー先), which unpacks it,
// prepares the place to work and starts the agent. No host talks to
// another. See PROTOCOL.md "Relay". Every OS (CapRelay).

// Relay methods.
const (
	// Served by the bridge (queue "relay"), refused to coding agents'
	// process trees (ADR 0004): build the package of a conversation
	// (RelayPrepareParams → RelayPrepareResult), with relay.progress
	// notifications while it runs.
	MethodRelayPrepare = "relay.prepare"
	// Served by the bridge (queue "relay"), refused to coding agents'
	// process trees: one chunk of a prepared package (RelayReadParams →
	// RelayReadResult).
	MethodRelayRead = "relay.read"
	// Served by the bridge at once (no queue), refused to coding agents'
	// process trees: remove what a relay left on this host but a started
	// relay's document (RelayRef → {}); cancels a relay.prepare of it
	// still running. Idempotent.
	MethodRelayDiscard = "relay.discard"
	// Served by the bridge (queue "relay"), refused to coding agents'
	// process trees: get ready to take a package, or say how much of it
	// is here already (RelayReceiveParams → RelayReceiveResult).
	MethodRelayReceive = "relay.receive"
	// Served by the bridge (queue "relay"), refused to coding agents'
	// process trees: write one chunk of a package (RelayWriteParams →
	// RelayWriteResult).
	MethodRelayWrite = "relay.write"
	// Served by the bridge (queue "relay"; its session through the spawn
	// queue), refused to coding agents' process trees: unpack the
	// package, prepare the place to work and start the agent
	// (RelayStartParams → RelayStartResult), with relay.progress
	// notifications while it runs.
	MethodRelayStart = "relay.start"
	// Served by the bridge (queue "relay"), refused to coding agents'
	// process trees: undo a relay.start that failed (RelayRef → {}).
	MethodRelayAbort = "relay.abort"
	// Served by the daemon (the bridge forwards it; the bridge's
	// relay.start calls it too), refused to coding agents' process trees:
	// record one end of a relay's trace (RelayRecordParams → {}).
	MethodRelayRecord = "relay.record"
	// MethodRelayForget (bridge → daemon only, never forwarded from the app;
	// refused to coding agents' process trees): forget the trace this daemon
	// recorded for a relay (RelayRef → {}); relay.abort calls it. Unknown: {}.
	MethodRelayForget = "relay.forget"

	// NotifyRelayProgress (bridge → app, on the connection that sent the
	// request): a stage of relay.prepare or relay.start began or moved
	// on (RelayProgressParams).
	NotifyRelayProgress = "relay.progress"
)

// CapRelay: the bridge serves relay.* and the daemon relay.record; Session
// and Conversation carry relay (every OS, since 0.26.0).
const CapRelay = "relay"

// Where the relay target is (RelayPrepareParams.Target).
const (
	// RelayTargetSameHost: the target is this host; relay.start takes the
	// package on the same host, nothing is read or written.
	RelayTargetSameHost = "same_host"
	// RelayTargetOtherHost: the client carries the package to another
	// host's stagent.
	RelayTargetOtherHost = "other_host"
)

// What becomes of the source agent (RelayPrepareParams.SourceAgent). The
// client ends it itself (session.signal hangup) once the target is up.
const (
	RelaySourceKeep = "keep" // keeps running
	RelaySourceEnd  = "end"  // will be ended
)

// Stages of relay.prepare (relay source) and relay.start (relay target):
// relay.progress names them, and an error of either method starts with the
// stage it failed at and ": ". The stages a relay target's task goes
// through after its session started are Task.Stage (TaskStage*, with
// TaskStageRelay).
const (
	RelayStageTranscript = "transcript" // reading the conversation, writing the document
	RelayStageMemo       = "memo"       // waiting for the source agent's hand-off memo
	RelayStageBundle     = "bundle"     // git bundle of the commits no remote has
	RelayStageDiff       = "diff"       // the uncommitted changes (staged, unstaged)
	RelayStageUntracked  = "untracked"  // the untracked files
	RelayStagePack       = "pack"       // writing the package

	RelayStageReceive = "receive" // relay.receive / relay.write
	RelayStageUnpack  = "unpack"  // checking and unpacking the package
	RelayStageRepo    = "repo"    // finding the source checkout
	RelayStageClone   = "clone"   // cloning the remote (RelayStartParams.Clone)
	RelayStageTask    = "task"    // registering the task, starting its session
	RelayStageSpawn   = "spawn"   // starting the agent's session (no task)
)

// Memo states (RelayMemo.Status).
const (
	RelayMemoIncluded = "included" // the memo is in the package
	RelayMemoTimeout  = "timeout"  // the agent did not finish within the limit; not included
	RelayMemoSkipped  = "skipped"  // not asked for (RelayMemo.Reason)
)

// Why no memo was asked for (RelayMemo.Reason).
const (
	RelayMemoNotLive = "not_live" // the conversation has no live session
	RelayMemoBusy    = "busy"     // the agent was not idle: working, an approval or a question
)

// Roles of a relay's end (RelayRecordParams.Role): what the conversation
// on the recording host is.
const (
	RelayRoleSource = "source" // it was relayed to Peer
	RelayRoleTarget = "target" // it was relayed from Peer
)

// Limits of a relay (PROTOCOL.md "Relay").
const (
	// RelayPackageMax bounds a package (relay.prepare: too_large).
	RelayPackageMax = 512 << 20
	// RelayChunkDefault and RelayChunkMax bound one relay.read chunk;
	// relay.write takes chunks up to RelayChunkMax.
	RelayChunkDefault = 1 << 20
	RelayChunkMax     = 4 << 20
	// RelayToolTextMax bounds one tool call's input and one tool output
	// in the document: longer ones keep their first RelayToolTextHead
	// bytes and their last RelayToolTextMax-RelayToolTextHead.
	RelayToolTextMax  = 8 << 10
	RelayToolTextHead = 6 << 10
	// RelayLastTurns is how many of the last turns the document's summary
	// repeats.
	RelayLastTurns = 3
	// RelayMemoTimeoutDefault and RelayMemoTimeoutMax bound the wait for
	// the hand-off memo, in ms.
	RelayMemoTimeoutDefault = 300_000
	RelayMemoTimeoutMax     = 900_000
)

// Environment variables of a relay target's agent session.
const (
	// EnvRelayDocument is the absolute path of the relay document.
	EnvRelayDocument = "STAGENT_RELAY_DOCUMENT"
	// EnvRelayID is the relay's id.
	EnvRelayID = "STAGENT_RELAY_ID"
)

// RelayRef names a relay: RelayID is 32 lowercase hex chars, made by the
// source's relay.prepare and the same on both hosts.
type RelayRef struct {
	RelayID string `json:"relay_id"`
}

// RelayHost names a host as the client knows it: HostID is its hello
// host_id (empty when it sent none), Label the name the client shows for
// it (its saved connection's).
type RelayHost struct {
	HostID string `json:"host_id,omitempty"`
	Label  string `json:"label"`
}

// RelayPrepareParams is relay.prepare on the relay source. The
// conversation is the live session SessionID's, else Harness's
// ConversationID. SourceHost is this host as the client names it (the
// document and the target's trace show Label). Memo asks the source agent
// for a hand-off memo, waiting MemoTimeoutMS at most (default
// RelayMemoTimeoutDefault, max RelayMemoTimeoutMax). Instruction is the
// target agent's first instruction ("" = the default one in Lang). Lang is
// the document's language (LangJa or LangEn; others and "" are LangEn).
type RelayPrepareParams struct {
	SessionID      string    `json:"session_id,omitempty"`
	Harness        string    `json:"harness,omitempty"`
	ConversationID string    `json:"conversation_id,omitempty"`
	Target         string    `json:"target"`       // RelayTarget*
	SourceAgent    string    `json:"source_agent"` // RelaySource*
	SourceHost     RelayHost `json:"source_host"`
	Memo           bool      `json:"memo,omitempty"`
	MemoTimeoutMS  int       `json:"memo_timeout_ms,omitempty"`
	Instruction    string    `json:"instruction,omitempty"`
	Lang           string    `json:"lang,omitempty"`
}

// RelayErrorData is the data of a relay.prepare error once the relay's id
// was made: the client sends relay.discard with it.
type RelayErrorData struct {
	RelayID string `json:"relay_id"`
}

// RelayPrepareResult describes the package. Size and SHA256 (hex) are the
// package file's, for relay.read and relay.receive; both are absent for a
// same_host relay, whose package is never read. Parts tells what it holds.
type RelayPrepareResult struct {
	RelayID string      `json:"relay_id"`
	Size    int64       `json:"size,omitempty"`
	SHA256  string      `json:"sha256,omitempty"`
	Source  RelaySource `json:"source"`
	Parts   RelayParts  `json:"parts"`
	Memo    RelayMemo   `json:"memo"`
}

// RelaySource is the relay source as the package's manifest records it.
// Repo is the source checkout (元のチェックアウト) of the repository Cwd is
// in: its main working tree (the first of `git worktree list`; the
// worktree Cwd is in when the main one is bare), canonical; "" when Cwd is
// no git checkout, and then the git members are empty. CwdRel is Cwd
// relative to the root of the worktree it is in ("" at the root, "/"
// separated). Remote names the remote RemoteURL is (the branch's upstream
// remote, else origin, else the first one; both "" without a remote).
// Branch is "" on a detached HEAD. Head is HEAD's commit; Base the newest
// commit of HEAD's history a remote-tracking ref has (Head when nothing is
// unpushed, "" when no remote has any of it). TaskID is the session's task
// on the source host.
type RelaySource struct {
	Host           RelayHost `json:"host"`
	Harness        string    `json:"harness"`
	ConversationID string    `json:"conversation_id"`
	SessionID      string    `json:"session_id,omitempty"`
	Title          string    `json:"title,omitempty"`
	State          string    `json:"state,omitempty"` // the session's state when the conversation was read
	Cwd            string    `json:"cwd"`
	Repo           string    `json:"repo,omitempty"`
	CwdRel         string    `json:"cwd_rel,omitempty"`
	Remote         string    `json:"remote,omitempty"`
	RemoteURL      string    `json:"remote_url,omitempty"`
	Branch         string    `json:"branch,omitempty"`
	Head           string    `json:"head,omitempty"`
	Base           string    `json:"base,omitempty"`
	TaskID         string    `json:"task_id,omitempty"`
	Target         string    `json:"target"`       // RelayTarget*
	SourceAgent    string    `json:"source_agent"` // RelaySource*
	PreparedAt     int64     `json:"prepared_at"`  // unix ms
}

// RelayParts is what a package holds, for the client to show: the
// document's bytes and messages, its images, the bundle's commits and
// bytes (0: nothing unpushed, or same_host), the patches' bytes and the
// untracked files' count and bytes.
type RelayParts struct {
	DocumentBytes  int64 `json:"document_bytes"`
	Messages       int   `json:"messages"`
	Images         int   `json:"images"`
	ImageBytes     int64 `json:"image_bytes"`
	BundleCommits  int   `json:"bundle_commits"`
	BundleBytes    int64 `json:"bundle_bytes"`
	PatchBytes     int64 `json:"patch_bytes"`
	UntrackedFiles int   `json:"untracked_files"`
	UntrackedBytes int64 `json:"untracked_bytes"`
}

// RelayMemo says what came of the hand-off memo: Status RelayMemo*, Reason
// (skipped) RelayMemoNotLive or RelayMemoBusy. Not asked for: Status "".
type RelayMemo struct {
	Status string `json:"status,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// RelayReadParams reads MaxBytes (default RelayChunkDefault, at most
// RelayChunkMax) of a prepared package from Offset.
type RelayReadParams struct {
	RelayID  string `json:"relay_id"`
	Offset   int64  `json:"offset"`
	MaxBytes int    `json:"max_bytes,omitempty"`
}

// RelayReadResult: EOF is set when Data reaches the package's end.
type RelayReadResult struct {
	Data []byte `json:"data"`
	EOF  bool   `json:"eof,omitempty"`
}

// RelayReceiveParams starts (or continues) taking a package of Size bytes
// whose SHA-256 is SHA256 (hex), as relay.prepare answered.
type RelayReceiveParams struct {
	RelayID string `json:"relay_id"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
}

// RelayReceiveResult: Received is how many bytes are here already; the
// next relay.write starts there.
type RelayReceiveResult struct {
	Received int64 `json:"received"`
}

// RelayWriteParams writes Data at Offset, which must be the bytes received
// so far.
type RelayWriteParams struct {
	RelayID string `json:"relay_id"`
	Offset  int64  `json:"offset"`
	Data    []byte `json:"data"`
}

// RelayWriteResult: Received is the bytes received so far.
type RelayWriteResult struct {
	Received int64 `json:"received"`
}

// RelayStartParams is relay.start on the relay target. Harness is the
// agent to start, Model its model ("" = the harness's default). Repo is
// the source checkout to work from ("" = the one whose remote is the
// package's RemoteURL); Clone, sent after repo_missing, clones the remote
// first. Name and Branch are the new task's (default from the package's
// branch). Cwd is where the agent starts when the package has no git
// checkout ("" = home). TargetHost is this host as the client names it,
// for the source's trace.
type RelayStartParams struct {
	RelayID    string            `json:"relay_id"`
	Harness    string            `json:"harness"`
	Model      string            `json:"model,omitempty"`
	Repo       string            `json:"repo,omitempty"`
	Clone      *RelayClone       `json:"clone,omitempty"`
	Name       string            `json:"name,omitempty"`
	Branch     string            `json:"branch,omitempty"`
	Cwd        string            `json:"cwd,omitempty"`
	TargetHost RelayHost         `json:"target_host"`
	Cols       int               `json:"cols"`
	Rows       int               `json:"rows"`
	Env        map[string]string `json:"env,omitempty"`
}

// RelayClone clones the package's RemoteURL into Dir ("" = the default
// place) before the task is made.
type RelayClone struct {
	Dir string `json:"dir,omitempty"`
}

// RelayStartResult: Task is the task made for the relay (absent when the
// agent works in the source's own place or in Cwd), Session the agent's
// session (a task's prepared session), Document the relay document's
// absolute path, Cloned the clone made (Clone).
type RelayStartResult struct {
	Task     *Task   `json:"task,omitempty"`
	Session  Session `json:"session"`
	Document string  `json:"document"`
	Cloned   string  `json:"cloned,omitempty"`
}

// RelayProgressParams is the relay.progress notification: Stage (RelayStage*)
// began; Done and Total (bytes or items, both 0 when not counted) say how
// far it is.
type RelayProgressParams struct {
	RelayID string `json:"relay_id"`
	Stage   string `json:"stage"`
	Done    int64  `json:"done,omitempty"`
	Total   int64  `json:"total,omitempty"`
}

// RelayEnd is a conversation at one end of a relay: its host (Peer only),
// harness, conversation id (empty until known) and session id.
type RelayEnd struct {
	Host           *RelayHost `json:"host,omitempty"`
	Harness        string     `json:"harness"`
	ConversationID string     `json:"conversation_id,omitempty"`
	SessionID      string     `json:"session_id,omitempty"`
}

// RelayRecordParams records one end of relay RelayID on this host's daemon:
// Local is the conversation here, which is the relay's Role, Peer the
// conversation at the other end. Recording the same RelayID and Role again
// fills in what was empty (a conversation id learned later).
type RelayRecordParams struct {
	RelayID string   `json:"relay_id"`
	Role    string   `json:"role"` // RelayRole*
	Local   RelayEnd `json:"local"`
	Peer    RelayEnd `json:"peer"`
}

// RelayLink is the conversation at the other end of a relay, as Session and
// Conversation carry it.
type RelayLink struct {
	RelayID        string    `json:"relay_id"`
	At             int64     `json:"at"` // unix ms, when it was recorded
	Host           RelayHost `json:"host"`
	Harness        string    `json:"harness"`
	ConversationID string    `json:"conversation_id,omitempty"`
	SessionID      string    `json:"session_id,omitempty"`
}

// RelayLinks is a conversation's relay trace, one step each way: Source
// the conversation it was relayed from, Target the one it was last
// relayed to.
type RelayLinks struct {
	Source *RelayLink `json:"source,omitempty"`
	Target *RelayLink `json:"target,omitempty"`
}

// NewRelayID returns a random relay id (RelayRef.RelayID).
func NewRelayID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// ValidRelayID accepts the 32 lowercase hex chars of NewRelayID, which also
// keeps ids from escaping the relay directories.
func ValidRelayID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range []byte(id) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
