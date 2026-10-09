package task

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Script is an orca.yaml script (scripts.setup, scripts.archive) to run
// the way Orca runs it.
type Script struct {
	Text string
	// Dir is the task's worktree: the working directory and
	// ORCA_WORKTREE_PATH (ORCA_WORKSPACE_NAME is its base name).
	Dir string
	// Root is the source checkout (元のチェックアウト): ORCA_ROOT_PATH.
	Root string
	// Env is the environment before the ORCA_* variables; nil: this
	// process's.
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// ScriptPlan is how a Script runs: Program with Args and, when Runner is
// set, the path of a file holding Runner (suffix RunnerExt) as the last
// argument; Env is its environment.
type ScriptPlan struct {
	Program   string
	Args      []string
	Runner    string
	RunnerExt string
	Env       []string
}

// posixShells are the login shells that run a POSIX script themselves;
// with another one (fish, …) /bin/sh runs it.
var posixShells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "mksh": true, "ash": true, "yash": true}

// PlanScript decides how s runs on goos. git is git's path, used on
// Windows only, to find Git for Windows' bash.exe next to it.
//
//   - Linux, macOS: the login shell (SHELL of s.Env when it is a POSIX
//     shell, else /bin/sh) as `<shell> -l -c`, with `set -e` first: the
//     first failing command fails the script.
//   - Windows: cmd.exe (%ComSpec%) runs a .cmd runner, as Orca's: every
//     line is `call`ed and the first one failing (errorlevel) ends it. A
//     script starting with #! runs in Git for Windows' bash.exe instead,
//     with `set -e` and the ORCA_* paths in /c/… form; without that bash
//     it is an error.
func PlanScript(goos string, s Script, git string) (ScriptPlan, error) {
	env := s.Env
	if env == nil {
		env = os.Environ()
	}
	orca := func(root, dir string) []string {
		return append(env[:len(env):len(env)], "ORCA_ROOT_PATH="+root, "ORCA_WORKTREE_PATH="+dir, "ORCA_WORKSPACE_NAME="+baseName(s.Dir))
	}
	if goos != "windows" {
		shell, _ := lookupEnv(env, "SHELL")
		if !posixShells[baseName(shell)] {
			shell = "/bin/sh"
		}
		return ScriptPlan{Program: shell, Args: []string{"-l", "-c", "set -e\n" + s.Text}, Env: orca(s.Root, s.Dir)}, nil
	}
	text := strings.ReplaceAll(s.Text, "\r\n", "\n")
	if strings.HasPrefix(strings.TrimLeft(text, " \t\n\ufeff"), "#!") {
		bash := gitBash(git)
		if bash == "" {
			return ScriptPlan{}, fmt.Errorf("the script starts with #! and needs Git for Windows' bash.exe, which is not next to git (%s)", cmp.Or(git, "git not found"))
		}
		return ScriptPlan{Program: bash, Runner: "set -e\n" + text, RunnerExt: ".sh", Env: orca(PosixPath(s.Root), PosixPath(s.Dir))}, nil
	}
	comspec, _ := lookupEnv(env, "ComSpec")
	return ScriptPlan{Program: cmp.Or(comspec, "cmd.exe"), Args: []string{"/d", "/s", "/c"}, Runner: cmdRunner(text), RunnerExt: ".cmd", Env: orca(s.Root, s.Dir)}, nil
}

// RunScript runs s (PlanScript for this OS) in s.Dir and waits for it. A
// script exiting non-zero is an error naming its exit code.
func RunScript(ctx context.Context, s Script) error {
	git := ""
	if runtime.GOOS == "windows" {
		git, _ = lookProgram(s.Env, "git")
	}
	p, err := PlanScript(runtime.GOOS, s, git)
	if err != nil {
		return err
	}
	file := ""
	if p.Runner != "" {
		f, err := os.CreateTemp("", "stagent-script-*"+p.RunnerExt)
		if err != nil {
			return err
		}
		file = f.Name()
		defer os.Remove(file)
		_, err = f.WriteString(p.Runner)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	cmd := scriptCommand(ctx, p, file)
	cmd.Dir, cmd.Env = s.Dir, p.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s.Stdin, s.Stdout, s.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() >= 0 {
			return fmt.Errorf("exited with code %d", ee.ExitCode())
		}
		return err
	}
	return nil
}

// cmdRunner is the .cmd file running script in cmd.exe like Orca's: each
// line is `call`ed (npm, pnpm … are batch files) and the first that sets
// errorlevel ends the runner with it. Blank lines and comments (`::`,
// `rem`) are left out.
func cmdRunner(script string) string {
	var b strings.Builder
	b.WriteString("@echo off\r\nsetlocal EnableExtensions DisableDelayedExpansion\r\n")
	for line := range strings.Lines(script) {
		line = strings.TrimSpace(line)
		low := strings.ToLower(line)
		if line == "" || strings.HasPrefix(line, "::") || low == "rem" || strings.HasPrefix(low, "rem ") {
			continue
		}
		b.WriteString("call " + line + "\r\nif errorlevel 1 exit /b %errorlevel%\r\n")
	}
	b.WriteString("exit /b 0\r\n")
	return b.String()
}

// gitBash finds Git for Windows' bash.exe from git's path (…\Git\cmd\
// git.exe, …\Git\bin\git.exe or …\Git\mingw64\bin\git.exe): bin\bash.exe
// or usr\bin\bash.exe under the installation. "" when there is none —
// System32's bash.exe is WSL's, not Git Bash.
func gitBash(git string) string {
	if git == "" {
		return ""
	}
	dir := filepath.Dir(git)
	for _, root := range []string{filepath.Dir(dir), filepath.Dir(filepath.Dir(dir))} {
		for _, p := range []string{filepath.Join(root, "bin", "bash.exe"), filepath.Join(root, "usr", "bin", "bash.exe")} {
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
				return p
			}
		}
	}
	return ""
}

// PosixPath writes a Windows path the way Git Bash does: C:\a\b is /c/a/b.
func PosixPath(p string) string {
	if len(p) >= 2 && p[1] == ':' {
		return "/" + strings.ToLower(p[:1]) + strings.ReplaceAll(p[2:], `\`, "/")
	}
	return strings.ReplaceAll(p, `\`, "/")
}

// baseName is the last element of a path written with / or \.
func baseName(p string) string {
	p = strings.TrimRight(p, `/\`)
	return p[strings.LastIndexAny(p, `/\`)+1:]
}
