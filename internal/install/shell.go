package install

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"time"
)

// Opt-in shell wrappers: functions named claude / codex / omp that start the
// real command under `stagent run --` when the shell is interactive with a
// terminal on stdin and stdout (the passthrough holder requires one), not
// already inside a stagent session, and the binary exists. With
// STAGENT_HANDOFF=1 they add --handoff, so that the program keeps running
// as a detached session when the terminal goes away.

const (
	blockBegin = "# >>> ssh-term stagent >>>"
	blockEnd   = "# <<< ssh-term stagent <<<"
)

const blockAbout = `# Added by SSH Term (stagent integrate --shell-wrapper): runs claude, codex
# and omp under a stagent PTY holder so SSH Term's Agent Mode can see them.
# Remove with: stagent integrate --apply --remove shell-wrapper`

var wrappedCommands = []string{"claude", "codex", "omp"}

func posixBlock(bin string) string {
	var sb strings.Builder
	sb.WriteString(blockBegin + "\n" + blockAbout + "\n")
	sb.WriteString(`__stagent_wrap() {
  local __stagent_bin=` + shellQuote(bin) + `
  case $- in
    *i*) ;;
    *) command "$@"; return ;;
  esac
  # The passthrough holder needs a terminal on stdin and stdout.
  if [ -t 0 ] && [ -t 1 ] && [ -z "${STAGENT_SESSION_ID:-}" ] && [ -x "$__stagent_bin" ]; then
    if [ "${STAGENT_HANDOFF:-}" = 1 ]; then
      "$__stagent_bin" run --handoff -- "$@"
    else
      "$__stagent_bin" run -- "$@"
    fi
  else
    command "$@"
  fi
}
`)
	for _, c := range wrappedCommands {
		// The `function` keyword keeps an alias of the same name from being
		// expanded in the definition.
		sb.WriteString("function " + c + " { __stagent_wrap " + c + ` "$@"; }` + "\n")
	}
	sb.WriteString(blockEnd + "\n")
	return sb.String()
}

func fishQuote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

func fishFile(bin string) string {
	var sb strings.Builder
	sb.WriteString(blockBegin + "\n" + blockAbout + "\n")
	sb.WriteString(`function __stagent_wrap
    set -l bin ` + fishQuote(bin) + `
    if status is-interactive; and isatty stdin; and isatty stdout; and not set -q STAGENT_SESSION_ID; and test -x $bin
        if test "$STAGENT_HANDOFF" = 1
            $bin run --handoff -- $argv
        else
            $bin run -- $argv
        end
    else
        command $argv
    end
end
`)
	for _, c := range wrappedCommands {
		sb.WriteString("function " + c + " --wraps " + c + "\n    __stagent_wrap " + c + " $argv\nend\n")
	}
	sb.WriteString(blockEnd + "\n")
	return sb.String()
}

func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func powershellBlock(bin string) string {
	var sb strings.Builder
	sb.WriteString(blockBegin + "\n" + blockAbout + "\n")
	sb.WriteString(`function __StagentWrap {
  param([string]$Name, [object[]]$Rest)
  $bin = ` + psQuote(bin) + `
  $interactive = [Environment]::UserInteractive -and -not ([Environment]::GetCommandLineArgs() | Where-Object { $_ -like '-NonI*' }) -and -not [Console]::IsInputRedirected -and -not [Console]::IsOutputRedirected
  if ($interactive -and -not $env:STAGENT_SESSION_ID -and (Test-Path -LiteralPath $bin)) {
    if ($env:STAGENT_HANDOFF -eq '1') { & $bin run --handoff -- $Name @Rest } else { & $bin run -- $Name @Rest }
  } else {
    $cmd = Get-Command -Name $Name -CommandType Application,ExternalScript -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($cmd) { & $cmd @Rest } else { Write-Error "${Name}: command not found" }
  }
}
`)
	for _, c := range wrappedCommands {
		sb.WriteString("function " + c + " { __StagentWrap '" + c + "' $args }\n")
	}
	sb.WriteString(blockEnd + "\n")
	return sb.String()
}

// withEOL converts a "\n" text to the given line ending.
func withEOL(s, eol string) string {
	if eol == "\n" {
		return s
	}
	return strings.ReplaceAll(s, "\n", eol)
}

func detectEOL(b []byte, def string) string {
	if bytes.Contains(b, []byte("\r\n")) {
		return "\r\n"
	}
	if len(b) > 0 {
		return "\n"
	}
	return def
}

// findBlock returns the byte range of our marker block, from the start of
// the begin line to after the end line's terminator.
func findBlock(s string) (int, int, bool) {
	b := lineStartIndex(s, blockBegin)
	if b < 0 {
		return 0, 0, false
	}
	rel := lineStartIndex(s[b:], blockEnd)
	if rel < 0 {
		return 0, 0, false
	}
	e := b + rel + len(blockEnd)
	if strings.HasPrefix(s[e:], "\r\n") {
		e += 2
	} else if strings.HasPrefix(s[e:], "\n") {
		e++
	}
	return b, e, true
}

// lineStartIndex finds marker at the start of a line.
func lineStartIndex(s, marker string) int {
	off := 0
	for {
		i := strings.Index(s[off:], marker)
		if i < 0 {
			return -1
		}
		i += off
		if i == 0 || s[i-1] == '\n' {
			return i
		}
		off = i + 1
	}
}

// setBlock replaces our block in cur or appends it after one blank line.
func setBlock(cur []byte, block, defEOL string) []byte {
	eol := detectEOL(cur, defEOL)
	block = withEOL(block, eol)
	s := string(cur)
	if b, e, ok := findBlock(s); ok {
		return []byte(s[:b] + block + s[e:])
	}
	if s != "" {
		if !strings.HasSuffix(s, "\n") {
			s += eol
		}
		s += eol
	}
	return []byte(s + block)
}

// removeBlock takes our block out together with the one blank line setBlock
// put before it.
func removeBlock(cur []byte) []byte {
	s := string(cur)
	b, e, ok := findBlock(s)
	if !ok {
		return cur
	}
	before, after := s[:b], s[e:]
	switch {
	case strings.HasSuffix(before, "\r\n\r\n"):
		before = before[:len(before)-2]
	case strings.HasSuffix(before, "\n\n"):
		before = before[:len(before)-1]
	}
	return []byte(before + after)
}

func hasBlock(b []byte) bool {
	_, _, ok := findBlock(string(b))
	return ok
}

var utf8BOM = []byte("\xef\xbb\xbf")

// blockTarget wraps a shared rc/profile file.
func (e *env) blockTarget(id, path, block, defEOL string, newFilePrefix []byte) *target {
	return &target{
		id: id, path: path, mode: 0o644,
		identify: "lines between `" + blockBegin + "` and `" + blockEnd + "`",
		add: func(cur []byte) (addResult, error) {
			if bytes.HasPrefix(cur, []byte{0xff, 0xfe}) || bytes.HasPrefix(cur, []byte{0xfe, 0xff}) {
				return addResult{}, errors.New(path + " is UTF-16 encoded; add the wrapper manually")
			}
			var after []byte
			if cur == nil {
				after = append(append([]byte(nil), newFilePrefix...), withEOL(block, defEOL)...)
			} else {
				after = setBlock(cur, block, defEOL)
			}
			return addResult{after: after, summary: "add shell functions claude/codex/omp that run under `stagent run`"}, nil
		},
		remove: func(cur []byte, _ *ConfigEntry) ([]byte, error) { return removeBlock(cur), nil },
		present: func(cur []byte) string {
			if hasBlock(cur) {
				return "shell wrapper block"
			}
			return ""
		},
	}
}

func (e *env) fishTarget() *target {
	path := e.home(".config", "fish", "conf.d", "ssh-term-stagent.fish")
	return &target{
		id: "shell-fish", path: path, owned: true, mode: 0o644,
		identify: "whole file (" + blockBegin + ")",
		add: func(cur []byte) (addResult, error) {
			if cur != nil && !hasBlock(cur) {
				return addResult{}, errors.New(path + " exists and is not managed by stagent; left unchanged")
			}
			return addResult{after: []byte(fishFile(e.l.Bin)), summary: "add fish functions claude/codex/omp that run under `stagent run`"}, nil
		},
		remove: func(cur []byte, _ *ConfigEntry) ([]byte, error) {
			if !hasBlock(cur) {
				return cur, nil
			}
			return nil, nil
		},
		present: func(cur []byte) string {
			if hasBlock(cur) {
				return "shell wrapper file"
			}
			return ""
		},
	}
}

// documentsDir is the Windows Documents folder (it may be redirected, e.g.
// to OneDrive).
func (e *env) documentsDir() string {
	if e.docs != "" {
		return e.docs
	}
	e.docs = filepath.Join(e.l.Home, "Documents")
	out, err := e.run.Run(10*time.Second, "powershell", "-NoProfile", "-NonInteractive", "-Command", "[Environment]::GetFolderPath('MyDocuments')")
	if d := strings.TrimSpace(out); err == nil && d != "" && !strings.ContainsAny(d, "\r\n") {
		e.docs = d
	}
	return e.docs
}

// shellTargets returns every wrapper target of this OS (for removal and
// inspection).
func (e *env) shellTargets() []*target {
	if e.goos == "windows" {
		docs := e.documentsDir()
		b := powershellBlock(e.l.Bin)
		return []*target{
			e.blockTarget("shell-powershell", filepath.Join(docs, "WindowsPowerShell", "Microsoft.PowerShell_profile.ps1"), b, "\r\n", utf8BOM),
			e.blockTarget("shell-pwsh", filepath.Join(docs, "PowerShell", "Microsoft.PowerShell_profile.ps1"), b, "\r\n", utf8BOM),
		}
	}
	b := posixBlock(e.l.Bin)
	zdot := e.getenv("ZDOTDIR")
	if zdot == "" {
		zdot = e.l.Home
	}
	return []*target{
		e.blockTarget("shell-bash", e.home(".bashrc"), b, "\n", nil),
		e.blockTarget("shell-zsh", filepath.Join(zdot, ".zshrc"), b, "\n", nil),
		e.fishTarget(),
	}
}

// shellAddTargets picks the wrapper targets to install: rc files that exist
// or belong to the login shell.
func (e *env) shellAddTargets() []*target {
	login := filepath.Base(e.getenv("SHELL"))
	var out []*target
	for _, t := range e.shellTargets() {
		switch t.id {
		case "shell-bash":
			if exists(t.path) || login == "bash" {
				out = append(out, t)
			}
		case "shell-zsh":
			if exists(t.path) || login == "zsh" {
				out = append(out, t)
			}
		case "shell-fish":
			if login == "fish" || exists(e.home(".config", "fish")) {
				out = append(out, t)
			}
		case "shell-powershell":
			out = append(out, t)
		case "shell-pwsh":
			if _, err := e.lookPath("pwsh"); err == nil || exists(filepath.Dir(t.path)) {
				out = append(out, t)
			}
		}
	}
	return out
}
