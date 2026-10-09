// Package task holds what the daemon, the bridge and `stagent task` share
// about tasks (タスク): the canonical path of a source checkout (元のチェ
// ックアウト), which paths lie in a worktree, and running git.
package task

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// NewID returns a random 16-hex-char task id.
func NewID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// GitError is git exiting with an error: its first stderr line names why.
type GitError struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *GitError) Error() string {
	msg := e.Stderr
	if msg == "" {
		msg = e.Err.Error()
	}
	return "git " + strings.Join(e.Args, " ") + ": " + msg
}

func (e *GitError) Unwrap() error { return e.Err }

// Git runs git in dir with env (nil: this process's environment) and
// returns its standard output. git is looked up in env's PATH. On Windows
// every call gets -c core.longpaths=true. A git that cannot be found is an
// error wrapping exec.ErrNotFound; one that fails is a *GitError.
func Git(ctx context.Context, env []string, dir string, args ...string) ([]byte, error) {
	bin, err := lookProgram(env, "git")
	if err != nil {
		return nil, err
	}
	args = gitArgs(args)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		line, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n")
		return out, &GitError{Args: args, Stderr: strings.TrimSpace(line), Err: err}
	}
	return out, nil
}

// gitArgs adds -c core.longpaths=true on Windows: Git for Windows cannot
// handle paths over MAX_PATH without it, and the user's settings are not
// changed for that.
func gitArgs(args []string) []string {
	if runtime.GOOS == "windows" {
		return append([]string{"-c", "core.longpaths=true"}, args...)
	}
	return args
}

// lookProgram finds the program name in env's PATH (this process's when
// env is nil or has none).
func lookProgram(env []string, name string) (string, error) {
	path, ok := lookupEnv(env, "PATH")
	if env == nil || !ok {
		return exec.LookPath(name)
	}
	for _, dir := range filepath.SplitList(path) {
		if !filepath.IsAbs(dir) {
			continue
		}
		if p, err := exec.LookPath(filepath.Join(dir, name)); err == nil {
			return p, nil
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

// lookupEnv returns the last value of key in env (case-insensitively on
// Windows, where it is Path).
func lookupEnv(env []string, key string) (string, bool) {
	val, found := "", false
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok && (k == key || runtime.GOOS == "windows" && strings.EqualFold(k, key)) {
			val, found = v, true
		}
	}
	return val, found
}

// Canonical returns the canonical path (正の形) of the source checkout
// that dir lies in: `git rev-parse --show-toplevel`, symlinks resolved
// (CanonicalPath). A linked worktree is a checkout of its own: it is not
// mapped to its main worktree. Task.Repo and task.preview's repo are this
// value, as is the key of config.json's repos.
func Canonical(ctx context.Context, env []string, dir string) (string, error) {
	out, err := Git(ctx, env, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	top := strings.TrimSpace(string(out))
	if top == "" {
		return "", fmt.Errorf("%s is not in a git checkout", dir)
	}
	return CanonicalPath(filepath.FromSlash(top)), nil
}

// CanonicalPath makes p absolute and clean, resolves its symlinks when it
// exists and, on Windows, upper-cases its drive letter.
func CanonicalPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" && len(p) >= 2 && p[1] == ':' {
		p = strings.ToUpper(p[:1]) + p[1:]
	}
	return p
}

// SamePath reports whether two canonical paths name the same place
// (case-insensitively on Windows).
func SamePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// Within reports whether canonical path p is dir or lies inside it.
func Within(p, dir string) bool {
	if SamePath(p, dir) {
		return true
	}
	prefix := dir
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return len(p) > len(prefix) && SamePath(p[:len(prefix)], prefix)
}

// Worktrees lists the worktrees `git worktree list` knows for the
// checkout repo (its main worktree first), as canonical paths.
func Worktrees(ctx context.Context, env []string, repo string) ([]string, error) {
	ws, err := ListWorktrees(ctx, env, repo)
	if err != nil {
		return nil, err
	}
	paths := make([]string, len(ws))
	for i, w := range ws {
		paths[i] = w.Path
	}
	return paths, nil
}

// Worktree is one entry of `git worktree list`.
type Worktree struct {
	Path   string // canonical
	Branch string // short name; "" when detached (or bare)
}

// ListWorktrees lists the worktrees `git worktree list` knows for the
// checkout repo, its main worktree first.
func ListWorktrees(ctx context.Context, env []string, repo string) ([]Worktree, error) {
	// Not -z (git 2.36): older gits are still common on servers.
	out, err := Git(ctx, env, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var ws []Worktree
	for line := range strings.Lines(string(out)) {
		line = strings.TrimRight(line, "\r\n")
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			ws = append(ws, Worktree{Path: CanonicalPath(filepath.FromSlash(p))})
		} else if b, ok := strings.CutPrefix(line, "branch "); ok && len(ws) > 0 {
			ws[len(ws)-1].Branch = strings.TrimPrefix(b, "refs/heads/")
		}
	}
	return ws, nil
}

// Listed reports whether worktree (canonical) is a worktree of the
// checkout repo. An error means git could not answer: not found (wrapping
// exec.ErrNotFound) or failing, e.g. because repo is gone.
func Listed(ctx context.Context, env []string, repo, worktree string) (bool, error) {
	ws, err := Worktrees(ctx, env, repo)
	if err != nil {
		return false, err
	}
	for _, w := range ws {
		if SamePath(w, worktree) {
			return true, nil
		}
	}
	return false, nil
}

// IsDir reports whether p is an existing directory.
func IsDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
