// Package github answers the app's GitHub screen on hosts where its POSIX
// sh scripts cannot run (native Windows, ADR 0007): the working trees the
// user is in (github.repos), what gh reports for one of them
// (github.activity) and the state of single issues and pull requests
// (github.status). It runs the same git and gh commands as the app's
// scripts (lib/services/github_repos.dart); keep both in step.
package github

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Runner runs git, gh and ssh with one environment, so they are found and
// behave as in the user's terminals.
type Runner struct {
	Env []string
}

// Look returns the path of the program name on Env's PATH, "" when it is
// not there.
func (r Runner) Look(name string) string {
	exts := []string{""}
	if runtime.GOOS == "windows" {
		exts = nil
		pathext := r.getenv("PATHEXT")
		if pathext == "" {
			pathext = ".COM;.EXE;.BAT;.CMD"
		}
		for _, e := range filepath.SplitList(pathext) {
			if e != "" {
				exts = append(exts, strings.ToLower(e))
			}
		}
	}
	for _, dir := range filepath.SplitList(r.getenv("PATH")) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		for _, ext := range exts {
			p := filepath.Join(dir, name+ext)
			if isProgram(p) {
				return p
			}
		}
	}
	return ""
}

// isProgram: on Windows any file counts (winget is an app execution alias,
// a reparse point Stat cannot follow); elsewhere an executable one.
func isProgram(p string) bool {
	if runtime.GOOS == "windows" {
		st, err := os.Lstat(p)
		return err == nil && !st.IsDir()
	}
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0
}

// getenv returns the last value of key in Env (os/exec lets later entries
// win); names compare case-insensitively on Windows ("Path").
func (r Runner) getenv(key string) string {
	for i := len(r.Env) - 1; i >= 0; i-- {
		k, v, ok := strings.Cut(r.Env[i], "=")
		if ok && (k == key || runtime.GOOS == "windows" && strings.EqualFold(k, key)) {
			return v
		}
	}
	return ""
}

// run runs the program at path in dir and returns its stdout and stderr.
func (r Runner) run(ctx context.Context, dir, path string, args ...string) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = dir
	cmd.Env = r.Env
	cmd.WaitDelay = time.Second // do not hang on grandchildren holding the pipes
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	return out.Bytes(), errOut.Bytes(), err
}

// parallel runs fn(0) … fn(n-1) with at most limit at a time.
func parallel(n, limit int, fn func(i int)) {
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}()
	}
	wg.Wait()
}
