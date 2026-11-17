package hook

// qwenHook: stagent's extension for Qwen Code (install/qwen.go) passes the
// event as an argument; the payloads are Claude Code's (snake_case
// session_id, transcript_path, notification_type, tool_name, …). How Qwen
// Code was started is read from its process (node and its script, then
// its arguments: harness.BatchArgs).
var qwenHook = hookHarness{}
