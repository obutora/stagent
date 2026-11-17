package daemon

// openCodeDaemon: stagent's plugin for OpenCode sends Claude Code's hooks
// (hook/opencode.go). OpenCode shows its permission menu with the
// permission.asked event the plugin sends as PermissionRequest, never
// waiting on the plugin: the hook registers the approval and returns at
// once (harness.Harness.HookHeldForPrompt is false). Its menu has no
// numbers and changes its text while the user moves through it, but
// OpenCode tells when it is answered, wherever that happened
// (permission.replied → PermissionReplied), so the holder does not watch
// the screen for it. Its question tool's form comes as the tool's
// PreToolUse and ends with its PostToolUse.
var openCodeDaemon = daemonHarness{}
