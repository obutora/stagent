package daemon

// piDaemon: Pi asks no approvals (it runs every tool call without asking),
// and its hooks tell how it was started only through the hook command
// (hook/pi.go).
var piDaemon = daemonHarness{}
