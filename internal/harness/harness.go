// Package harness holds the harness registry (registry.go), recognizes the
// processes of the coding agents it lists, and connections that come from
// their process trees (guard.go).
package harness

import (
	"strings"

	"github.com/obutora/stagent/internal/ptable"
)

// interpreters run agents distributed as scripts.
var interpreters = map[string]bool{"node": true, "nodejs": true, "bun": true, "deno": true}

// Interpreter reports whether n (a ptable.BaseName) is a script interpreter
// an agent may run under.
func Interpreter(n string) bool { return interpreters[n] }

// Of recognizes an agent by its process name, its argv[0] (native binaries,
// and scripts that set their process title) or the script an interpreter
// runs; "" for other processes.
func Of(name string, argv []string) string {
	if h := Name(ptable.BaseName(name)); h != "" {
		return h
	}
	if h := Name(ptable.Argv0(argv)); h != "" {
		return h
	}
	if !interpreters[ptable.Argv0(argv)] {
		return ""
	}
	for _, a := range argv[1:] {
		if strings.HasPrefix(a, "-") {
			continue
		}
		script := strings.ReplaceAll(a, `\`, "/")
		base := ptable.BaseName(script)
		for _, ext := range []string{".js", ".mjs", ".cjs", ".ts"} {
			base = strings.TrimSuffix(base, ext)
		}
		if h := Name(base); h != "" {
			return h
		}
		for _, h := range All {
			for _, pkg := range h.Packages {
				if strings.Contains(script, "/"+pkg+"/") {
					return h.ID
				}
			}
		}
		return ""
	}
	return ""
}
