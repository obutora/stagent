// Package loginenv learns the environment of the user's login shell, which
// the sessions stagent starts begin from: the bridge's (session.spawn,
// task.create) and the daemon's (schedule runs).
package loginenv

import (
	"bytes"
	"os"
	"runtime"
	"strings"
	"time"
)

// TryTimeout bounds one try of Capture; rc files that hang (waiting for
// input, network) must not block session.spawn or a schedule run.
const TryTimeout = 5 * time.Second

// envMarker precedes the environment printed by `stagent __env`, so output
// of rc files that run before it is skipped.
const envMarker = "\x00__STAGENT_ENV__\x00"

// Dump is `stagent __env`: print the process environment after envMarker,
// NUL separated. Capture runs it through the user's login shell to learn
// the environment their terminal sessions get.
func Dump() int {
	var b bytes.Buffer
	b.WriteString(envMarker)
	for _, kv := range os.Environ() {
		b.WriteString(kv)
		b.WriteByte(0)
	}
	os.Stdout.Write(b.Bytes())
	return 0
}

// parseDump extracts the environment from Dump output.
func parseDump(out []byte) ([]string, bool) {
	i := bytes.LastIndex(out, []byte(envMarker))
	if i < 0 {
		return nil, false
	}
	var env []string
	for _, kv := range bytes.Split(out[i+len(envMarker):], []byte{0}) {
		if k, _, ok := bytes.Cut(kv, []byte("=")); ok && len(k) > 0 {
			env = append(env, string(kv))
		}
	}
	return env, len(env) > 0
}

// shellOnlyVars describe the capturing shell itself, not the environment a
// new session should inherit.
var shellOnlyVars = map[string]bool{
	"PWD": true, "OLDPWD": true, "SHLVL": true, "_": true, "TERM": true,
	"COLUMNS": true, "LINES": true, "TMUX": true, "TMUX_PANE": true,
	"STY": true, "ZELLIJ": true, "ZELLIJ_SESSION_NAME": true,
}

// Merge starts from the login shell's environment (PATH and exports from
// profile/rc files) and keeps variables of the process's own environment
// that the login shell did not set (SSH_*, anything the sshd session
// added).
func Merge(current, login []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, kv := range login {
		k, _, _ := strings.Cut(kv, "=")
		if shellOnlyVars[k] || k == "STAGENT_SESSION_ID" {
			continue
		}
		seen[Key(k)] = true
		out = append(out, kv)
	}
	for _, kv := range current {
		k, _, _ := strings.Cut(kv, "=")
		if !seen[Key(k)] {
			out = append(out, kv)
		}
	}
	return out
}

// Key normalizes a variable name for comparison (case-insensitive on
// Windows).
func Key(k string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(k)
	}
	return k
}

// Get returns the last value of key in env.
func Get(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && Key(k) == Key(key) {
			v = val
		}
	}
	return v
}
