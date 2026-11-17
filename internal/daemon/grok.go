package daemon

// grokDaemon: Grok Build sends Claude Code's hooks (beside events of its
// own, of which stagent installs StopCancelled), with three differences.
//
//   - It has no PermissionRequest: its PreToolUse comes before it checks
//     the call's permission (its rules and the session's answers may allow
//     it without asking), and the permission_prompt notification comes
//     with its menu, which registers the approval named by the tool of that
//     PreToolUse. An answer that allows sends no hook until the tool
//     finished (PostToolUse), so the holder watches the screen for the
//     menu, as for Codex: an answer on the PC or from SSH Term (the row's
//     digit typed into the screen) closes it.
//   - A turn the user cut short (a menu turned down or dismissed) ends
//     with StopCancelled instead of Stop.
//   - It sends Stop again when it exits (reason shutdown), which the hook
//     drops (hook/grok.go).
var grokDaemon = daemonHarness{watchPrompt: true, checkHook: hookPreToolUse}
