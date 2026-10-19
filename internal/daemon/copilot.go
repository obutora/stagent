package daemon

// copilotDaemon: Copilot CLI runs its PermissionRequest hook before its
// own permission checks (rules, approvals "for the session", auto-allow)
// and shows its menu only once the hook returned. The hook is answered at
// once and decides nothing; the permission_prompt notification Copilot CLI
// sends with the menu registers the approval, and the holder watches the
// screen for the menu, as for Codex: an answer on the PC or from SSH Term
// (the option's key typed into the screen) closes it.
var copilotDaemon = daemonHarness{watchPrompt: true, checksFirst: true}
