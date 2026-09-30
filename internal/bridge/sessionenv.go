package bridge

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// envMarker precedes the environment printed by `stagent __env`, so output
// of rc files that run before it is skipped.
const envMarker = "\x00__STAGENT_ENV__\x00"

// EnvDump is `stagent __env`: print the process environment after
// envMarker, NUL separated. The bridge runs it through the user's login
// shell to learn the environment their terminal sessions get.
func EnvDump() int {
	var b bytes.Buffer
	b.WriteString(envMarker)
	for _, kv := range os.Environ() {
		b.WriteString(kv)
		b.WriteByte(0)
	}
	os.Stdout.Write(b.Bytes())
	return 0
}

// parseEnvDump extracts the environment from EnvDump output.
func parseEnvDump(out []byte) ([]string, bool) {
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

// mergeLoginEnv starts from the login shell's environment (PATH and
// exports from profile/rc files) and keeps variables of the bridge's own
// environment that the login shell did not set (SSH_*, anything the sshd
// session added).
func mergeLoginEnv(current, login []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, kv := range login {
		k, _, _ := strings.Cut(kv, "=")
		if shellOnlyVars[k] || k == "STAGENT_SESSION_ID" {
			continue
		}
		seen[envKey(k)] = true
		out = append(out, kv)
	}
	for _, kv := range current {
		k, _, _ := strings.Cut(kv, "=")
		if !seen[envKey(k)] {
			out = append(out, kv)
		}
	}
	return out
}

// envKey normalizes a variable name for comparison (case-insensitive on
// Windows).
func envKey(k string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(k)
	}
	return k
}

// envGet returns the last value of key in env.
func envGet(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && envKey(k) == envKey(key) {
			v = val
		}
	}
	return v
}

// ensureOnPath makes argv0 resolvable with env's PATH: when it is a bare
// name that PATH does not contain, the first tool directory (npm/bun/…
// globals, version manager shims) that has it is prepended to PATH. The
// directory also typically holds the interpreter such scripts need (bun for
// `#!/usr/bin/env bun`). env is returned unchanged when nothing is found.
func ensureOnPath(env []string, argv0, home string) []string {
	if argv0 == "" || strings.ContainsAny(argv0, `/\`) {
		return env
	}
	pathVar := envGet(env, "PATH")
	if lookIn(filepath.SplitList(pathVar), argv0) != "" {
		return env
	}
	dirs := toolDirs(home, func(k string) string { return envGet(env, k) })
	dir := lookIn(dirs, argv0)
	if dir == "" {
		return env
	}
	newPath := dir
	if pathVar != "" {
		newPath += string(os.PathListSeparator) + pathVar
	}
	return append(env, "PATH="+newPath)
}

// lookIn returns the first directory in dirs holding an executable name.
func lookIn(dirs []string, name string) string {
	exts := []string{""}
	if runtime.GOOS == "windows" {
		exts = []string{".exe", ".cmd", ".bat", ".ps1", ""}
	}
	for _, d := range dirs {
		if d == "" {
			continue
		}
		for _, ext := range exts {
			st, err := os.Stat(filepath.Join(d, name+ext))
			if err == nil && !st.IsDir() && (runtime.GOOS == "windows" || st.Mode()&0o111 != 0) {
				return d
			}
		}
	}
	return ""
}
