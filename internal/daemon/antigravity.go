package daemon

// antigravityDaemon: Antigravity CLI's hooks, as stagent's hook rewrites
// them (hook/antigravity.go), are SessionStart, UserPromptSubmit (the
// first model call of a turn), PreToolUse, PostToolUse, Stop and
// StopCancelled. It has no PermissionRequest and no Notification, and
// several things happen on its screen alone.
//
//   - Its PreToolUse comes before it checks the call's permission (its
//     rules, the session's "always allow" answers and Auto mode may allow
//     it without asking), and nothing tells when its permission menu
//     shows: the check starts a watch of the screen with shown, and the
//     menu's coming registers the approval, named by the check's tool
//     (menuOnScreen, prompt.go). An answer sends no hook either (an
//     allowed call's PostToolUse comes when it finished, if at all): the
//     holder reports the menu gone, wherever it was answered.
//   - A menu turned down or dismissed (Esc) and an Esc while it works end
//     the turn with no hook at all, no Stop: the session's output goes
//     quiet, and the holder's idle ends the turn's working
//     (turnEndsUnheard). Its spinner runs for as long as it works, so its
//     output never goes quiet amid a turn.
//   - Its question tool, ask_question, comes as the tool's PreToolUse and
//     ends with its PostToolUse, also when the user skips it (Esc).
var antigravityDaemon = daemonHarness{
	watchPrompt:     true,
	checkHook:       hookPreToolUse,
	menuOnScreen:    true,
	turnEndsUnheard: true,
}
