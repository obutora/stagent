package paths

import (
	"path/filepath"
	"sort"
)

// ToolDirs lists the per-user and common install directories of CLI tools
// (npm/bun/pnpm globals, version managers, Homebrew, …) that are often only
// on PATH in interactive or login shells — not in the environment an SSH
// exec channel starts with. goos and getenv are parameters so callers can
// fake another platform in tests.
func ToolDirs(goos, home string, getenv func(string) string) []string {
	h := func(parts ...string) string { return filepath.Join(append([]string{home}, parts...)...) }
	dirs := []string{h(".local", "bin"), h(".bun", "bin"), h(".npm-global", "bin"), h(".volta", "bin"),
		h(".claude", "local"), h("bin"), h(".cargo", "bin"), h(".local", "share", "pnpm"),
		h(".asdf", "shims"), h(".local", "share", "mise", "shims")}
	globs := []string{h(".nvm", "versions", "node", "*", "bin"),
		h(".local", "share", "fnm", "node-versions", "*", "installation", "bin"),
		h(".fnm", "node-versions", "*", "installation", "bin"),
		h(".local", "share", "mise", "installs", "node", "*", "bin")}
	switch goos {
	case "windows":
		if a := getenv("APPDATA"); a != "" {
			dirs = append(dirs, filepath.Join(a, "npm"))
		}
		if a := getenv("LOCALAPPDATA"); a != "" {
			dirs = append(dirs, filepath.Join(a, "Microsoft", "WinGet", "Links"), filepath.Join(a, "pnpm"))
		}
		dirs = append(dirs, h("scoop", "shims"))
		globs = nil
	case "darwin":
		dirs = append(dirs, h("Library", "pnpm"), "/opt/homebrew/bin", "/usr/local/bin")
	default:
		dirs = append(dirs, "/usr/local/bin", "/home/linuxbrew/.linuxbrew/bin")
	}
	for _, g := range globs {
		m, _ := filepath.Glob(g)
		sort.Sort(sort.Reverse(sort.StringSlice(m))) // newest version first (roughly)
		dirs = append(dirs, m...)
	}
	return dirs
}
