package install

import (
	"bytes"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/pscwd"
)

// Opt-in shell wrappers: functions named after the command of each harness
// of the registry that start the real command under `stagent run
// --handoff=auto --` when the shell is interactive with a terminal on stdin
// and stdout (the passthrough holder requires one), not already inside a
// stagent session, the binary exists and the arguments do not ask for a
// non-interactive run (the harness's harness.Batch, the rule of
// harness.BatchArgs). --handoff=auto keeps the program running as a
// detached session when the terminal goes away unless STAGENT_HANDOFF or
// config.json's disable_handoff say otherwise (decided by `stagent run` at
// every start). The block exports STAGENT_SHELL_WRAPPER=1 so hooks can tell
// a shell that has the wrapper.

const (
	blockBegin = "# >>> ssh-term stagent >>>"
	blockEnd   = "# <<< ssh-term stagent <<<"
)

const blockAbout = `# Added by SSH Term (stagent integrate --shell-wrapper): runs the coding
# agents wrapped below under a stagent PTY holder so SSH Term's Agent Mode
# can see them.
# Remove with: stagent integrate --apply --remove shell-wrapper`

// batchAbout introduces the batch function of every script.
const batchAbout = `# Non-interactive runs, asked for by the arguments the batch function
# matches for each command, are left alone.
`

// wrappedCommands are the commands of the registry's harnesses, in its
// order.
var wrappedCommands = func() []string {
	cs := make([]string, len(harness.All))
	for i, h := range harness.All {
		cs[i] = h.Command
	}
	return cs
}()

// The batch functions of the scripts tell whether the arguments of a
// wrapped command ask for a non-interactive run, by its harness.Batch:
// the first argument is one of Subcommands, any argument is one of Flags,
// or Mode is given (`Mode value` or `Mode=value`) with a value other than
// ModeInteractive. A harness with no Batch has no branch.

func hasBatch(b harness.Batch) bool {
	return len(b.Subcommands) > 0 || len(b.Flags) > 0 || b.Mode != ""
}

// quoteAll quotes each of ss with quote and joins them with sep.
func quoteAll(ss []string, quote func(string) string, sep string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = quote(s)
	}
	return strings.Join(q, sep)
}

// posixBatch is __stagent_batch for bash and zsh.
func posixBatch() string {
	var sb strings.Builder
	sb.WriteString(`__stagent_batch() {
  local __stagent_c="$1" __stagent_a __stagent_mode=
  shift
  case $__stagent_c in
`)
	for _, h := range harness.All {
		b := h.Batch
		if !hasBatch(b) {
			continue
		}
		sb.WriteString("    " + shellQuote(h.Command) + ")\n")
		if len(b.Subcommands) > 0 {
			sb.WriteString("      case ${1-} in " + quoteAll(b.Subcommands, shellQuote, "|") + ") return 0 ;; esac\n")
		}
		if len(b.Flags) > 0 || b.Mode != "" {
			sb.WriteString(`      for __stagent_a in "$@"; do` + "\n")
			if b.Mode != "" {
				sb.WriteString(`        if [ -n "$__stagent_mode" ]; then
          [ "$__stagent_a" != ` + shellQuote(b.ModeInteractive) + ` ] && return 0
          __stagent_mode=
          continue
        fi
`)
			}
			sb.WriteString("        case $__stagent_a in\n")
			if len(b.Flags) > 0 {
				sb.WriteString("          " + quoteAll(b.Flags, shellQuote, "|") + ") return 0 ;;\n")
			}
			if b.Mode != "" {
				sb.WriteString("          " + shellQuote(b.Mode) + ") __stagent_mode=1 ;;\n")
				sb.WriteString("          " + shellQuote(b.Mode+"=") + `*) [ "$__stagent_a" != ` + shellQuote(b.Mode+"="+b.ModeInteractive) + " ] && return 0 ;;\n")
			}
			sb.WriteString("        esac\n      done\n")
		}
		sb.WriteString("      ;;\n")
	}
	sb.WriteString("  esac\n  return 1\n}\n")
	return sb.String()
}

// fishBatch is __stagent_batch for fish. A quoted case pattern still
// matches with wildcards (quoting keeps fish from globbing it).
func fishBatch() string {
	var sb strings.Builder
	sb.WriteString(`function __stagent_batch
    set -l c $argv[1]
    set -e argv[1]
    switch "$c"
`)
	for _, h := range harness.All {
		b := h.Batch
		if !hasBatch(b) {
			continue
		}
		sb.WriteString("        case " + fishQuote(h.Command) + "\n")
		if len(b.Subcommands) > 0 {
			sb.WriteString("            if test (count $argv) -gt 0; and contains -- $argv[1] " + quoteAll(b.Subcommands, fishQuote, " ") + "\n" +
				"                return 0\n" +
				"            end\n")
		}
		if len(b.Flags) > 0 || b.Mode != "" {
			if b.Mode != "" {
				sb.WriteString("            set -l mode 0\n")
			}
			sb.WriteString("            for a in $argv\n")
			if b.Mode != "" {
				sb.WriteString("                if test $mode = 1\n" +
					"                    test \"$a\" != " + fishQuote(b.ModeInteractive) + "; and return 0\n" +
					"                    set mode 0\n" +
					"                    continue\n" +
					"                end\n")
			}
			sb.WriteString("                switch \"$a\"\n")
			if len(b.Flags) > 0 {
				sb.WriteString("                    case " + quoteAll(b.Flags, fishQuote, " ") + "\n" +
					"                        return 0\n")
			}
			if b.Mode != "" {
				sb.WriteString("                    case " + fishQuote(b.Mode) + "\n" +
					"                        set mode 1\n" +
					"                    case " + fishQuote(b.Mode+"=*") + "\n" +
					"                        test \"$a\" != " + fishQuote(b.Mode+"="+b.ModeInteractive) + "; and return 0\n")
			}
			sb.WriteString("                end\n" +
				"            end\n")
		}
	}
	sb.WriteString("    end\n    return 1\nend\n")
	return sb.String()
}

// powershellBatch is __StagentBatch for PowerShell; arguments compare case
// sensitively, as the other shells do.
func powershellBatch() string {
	var sb strings.Builder
	sb.WriteString(`function __StagentBatch {
  param([string]$Name, [object[]]$Rest)
  $a = @(foreach ($x in $Rest) { "$x" })
`)
	is := func(x string) func(string) string {
		return func(s string) string { return x + " -ceq " + psQuote(s) }
	}
	for _, h := range harness.All {
		b := h.Batch
		if !hasBatch(b) {
			continue
		}
		sb.WriteString("  if ($Name -eq " + psQuote(h.Command) + ") {\n")
		if len(b.Subcommands) > 0 {
			sb.WriteString("    if ($a.Count -gt 0 -and (" + quoteAll(b.Subcommands, is("$a[0]"), " -or ") + ")) { return $true }\n")
		}
		if len(b.Flags) > 0 || b.Mode != "" {
			sb.WriteString("    for ($i = 0; $i -lt $a.Count; $i++) {\n")
			if len(b.Flags) > 0 {
				sb.WriteString("      if (" + quoteAll(b.Flags, is("$a[$i]"), " -or ") + ") { return $true }\n")
			}
			if b.Mode != "" {
				sb.WriteString("      if (($a[$i] -ceq " + psQuote(b.Mode) + " -and $i + 1 -lt $a.Count -and $a[$i + 1] -cne " + psQuote(b.ModeInteractive) + ") -or " +
					"($a[$i].StartsWith(" + psQuote(b.Mode+"=") + ", [StringComparison]::Ordinal) -and $a[$i] -cne " + psQuote(b.Mode+"="+b.ModeInteractive) + ")) { return $true }\n")
			}
			sb.WriteString("    }\n")
		}
		sb.WriteString("  }\n")
	}
	sb.WriteString("  return $false\n}\n")
	return sb.String()
}

func posixBlock(bin string) string {
	var sb strings.Builder
	sb.WriteString(blockBegin + "\n" + blockAbout + "\n")
	sb.WriteString("export STAGENT_SHELL_WRAPPER=1\n" + batchAbout + posixBatch())
	sb.WriteString(`__stagent_wrap() {
  local __stagent_bin=` + shellQuote(bin) + `
  case $- in
    *i*) ;;
    *) command "$@"; return ;;
  esac
  # The passthrough holder needs a terminal on stdin and stdout.
  if [ -t 0 ] && [ -t 1 ] && [ -z "${STAGENT_SESSION_ID:-}" ] && [ -x "$__stagent_bin" ] && ! __stagent_batch "$@"; then
    "$__stagent_bin" run --handoff=auto -- "$@"
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
	sb.WriteString("set -gx STAGENT_SHELL_WRAPPER 1\n" + batchAbout + fishBatch())
	sb.WriteString(`function __stagent_wrap
    set -l bin ` + fishQuote(bin) + `
    if status is-interactive; and isatty stdin; and isatty stdout; and not set -q STAGENT_SESSION_ID; and test -x $bin; and not __stagent_batch $argv
        $bin run --handoff=auto -- $argv
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

// psBinExpr is the PowerShell expression for the stagent binary: built
// from $env:USERPROFILE when the binary lies under it, so the profile stays
// ASCII whatever the user name (Windows PowerShell 5.1 reads a profile
// without BOM in the ANSI code page); the literal path otherwise.
func psBinExpr(bin, userProfile string) string {
	if userProfile != "" {
		rel, err := filepath.Rel(userProfile, bin)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return "Join-Path $env:USERPROFILE " + psQuote(strings.ReplaceAll(rel, "/", `\`))
		}
	}
	return psQuote(bin)
}

func powershellBlock(binExpr string) string {
	var sb strings.Builder
	sb.WriteString(blockBegin + "\n" + blockAbout + "\n")
	sb.WriteString("$env:STAGENT_SHELL_WRAPPER = '1'\n" + batchAbout + powershellBatch())
	sb.WriteString(`function __StagentWrap {
  param([string]$Name, [object[]]$Rest)
  $bin = ` + binExpr + `
  $interactive = [Environment]::UserInteractive -and -not ([Environment]::GetCommandLineArgs() | Where-Object { $_ -like '-NonI*' }) -and -not [Console]::IsInputRedirected -and -not [Console]::IsOutputRedirected
  if ($interactive -and -not $env:STAGENT_SESSION_ID -and -not (__StagentBatch $Name $Rest) -and (Test-Path -LiteralPath $bin)) {
    & $bin run --handoff=auto -- $Name @Rest
  } else {
    $cmd = Get-Command -Name $Name -CommandType Application,ExternalScript -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($cmd) { & $cmd @Rest } else { Write-Error "${Name}: command not found" }
  }
}
`)
	sb.WriteString("# Keep the process's working directory at the shell's location, which\n" +
		"# Set-Location alone does not move, so SSH Term's GitHub screen sees it.\n" +
		pscwd.Hook + "\n")
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

// blockTarget wraps a shared rc/profile file. New files get no BOM; an
// existing file keeps its encoding (a UTF-16 one is not edited). For a
// PowerShell profile (psProfile), a block with non-ASCII characters in a
// file without UTF-8 BOM gets a note: Windows PowerShell 5.1 would read it
// in the ANSI code page.
func (e *env) blockTarget(id, path, block, defEOL string, psProfile bool) *target {
	return &target{
		id: id, path: path, mode: 0o644,
		identify: "lines between `" + blockBegin + "` and `" + blockEnd + "`",
		add: func(cur []byte) (addResult, error) {
			if bytes.HasPrefix(cur, []byte{0xff, 0xfe}) || bytes.HasPrefix(cur, []byte{0xfe, 0xff}) {
				return addResult{}, &contentError{codeUTF16Profile, path + " is UTF-16 encoded; add the wrapper manually"}
			}
			if psProfile && !isASCII(block) && !bytes.HasPrefix(cur, utf8BOM) {
				e.note(notePowerShellNoBOM, "{path} has no UTF-8 BOM and the wrapper names stagent by a path with non-ASCII characters ({bin}); Windows PowerShell 5.1 reads such a file in the ANSI code page and will not find stagent. Save the profile as UTF-8 with BOM, or install stagent under %USERPROFILE%.", "path", path, "bin", e.l.Bin)
			}
			var after []byte
			if cur == nil {
				after = []byte(withEOL(block, defEOL))
			} else {
				after = setBlock(cur, block, defEOL)
			}
			return addResult{after: after, summary: "add shell functions " + strings.Join(wrappedCommands, "/") + " that run under `stagent run`"}, nil
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
				return addResult{}, unmanagedFile(path + " exists and is not managed by stagent; left unchanged")
			}
			return addResult{after: []byte(fishFile(e.l.Bin)), summary: "add fish functions " + strings.Join(wrappedCommands, "/") + " that run under `stagent run`"}, nil
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

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// shellTargets returns every wrapper target of this OS (for removal and
// inspection).
func (e *env) shellTargets() []*target {
	if e.goos == "windows" {
		docs := e.documentsDir()
		b := powershellBlock(psBinExpr(e.l.Bin, e.getenv("USERPROFILE")))
		return []*target{
			e.blockTarget("shell-powershell", filepath.Join(docs, "WindowsPowerShell", "Microsoft.PowerShell_profile.ps1"), b, "\r\n", true),
			e.blockTarget("shell-pwsh", filepath.Join(docs, "PowerShell", "Microsoft.PowerShell_profile.ps1"), b, "\r\n", true),
		}
	}
	b := posixBlock(e.l.Bin)
	zdot := e.getenv("ZDOTDIR")
	if zdot == "" {
		zdot = e.l.Home
	}
	return []*target{
		e.blockTarget("shell-bash", e.home(".bashrc"), b, "\n", false),
		e.blockTarget("shell-bash-profile", e.home(".bash_profile"), b, "\n", false),
		e.blockTarget("shell-zsh", filepath.Join(zdot, ".zshrc"), b, "\n", false),
		e.fishTarget(),
	}
}

// loginShell is the base name of the login shell ($SHELL; on Windows sshd
// sets it to its DefaultShell, cmd.exe when unset), "" when unknown.
func (e *env) loginShell() string {
	s := e.getenv("SHELL")
	if e.goos == "windows" {
		s = s[strings.LastIndexAny(s, `\/`)+1:]
	}
	if s == "" {
		return ""
	}
	return filepath.Base(s)
}

// shellAddTargets picks the wrapper targets to install: rc files that exist
// or belong to the login shell.
func (e *env) shellAddTargets() []*target {
	login := e.loginShell()
	bash := false
	var out []*target
	for _, t := range e.shellTargets() {
		switch t.id {
		case "shell-bash":
			if exists(t.path) || login == "bash" {
				out = append(out, t)
				bash = true
			}
		case "shell-bash-profile":
			// A login bash (macOS Terminal opens one) reads ~/.bash_profile
			// and not ~/.bashrc; defining the functions twice is harmless.
			if bash && exists(t.path) {
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
