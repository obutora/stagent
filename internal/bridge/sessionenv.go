package bridge

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/obutora/stagent/internal/loginenv"
)

// ensureOnPath makes argv0 resolvable with env's PATH: when it is a bare
// name that PATH does not contain, the first tool directory (npm/bun/…
// globals, version manager shims) that has it is prepended to PATH. The
// directory also typically holds the interpreter such scripts need (bun for
// `#!/usr/bin/env bun`). env is returned unchanged when nothing is found.
func ensureOnPath(env []string, argv0, home string) []string {
	if argv0 == "" || strings.ContainsAny(argv0, `/\`) {
		return env
	}
	pathVar := loginenv.Get(env, "PATH")
	if lookIn(filepath.SplitList(pathVar), argv0) != "" {
		return env
	}
	dirs := toolDirs(home, func(k string) string { return loginenv.Get(env, k) })
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
