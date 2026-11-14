package task

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The time limits of the git and gh commands of preparing and removing a
// task (片付け) that can hang — on the network, on a credential or
// permission prompt nobody sees, on a huge checkout — as Orca has them. A
// command over its limit fails its stage (fetch is skipped) instead of
// holding the task for good. scripts.setup and scripts.archive have none:
// they run on a terminal the user sees and can ^C.
const (
	// limitNetwork: git fetch, gh issue develop, gh pr checkout and gh
	// pr view (is the branch's pull request merged).
	limitNetwork = 120 * time.Second
	// limitWorktreeAdd: git worktree add, which checks out every file.
	limitWorktreeAdd = 180 * time.Second
	// limitCheck: git status before a removal.
	limitCheck = 30 * time.Second
)

// EnvGitTimeoutScale multiplies those limits by a positive number (5: five
// times as long) for a repository too large for them (LFS). Set in the
// login shell's environment, it reaches the bridge and the task's
// sessions.
const EnvGitTimeoutScale = "STAGENT_GIT_TIMEOUT_SCALE"

// LimitError is a git or gh command stopped at its time limit.
type LimitError struct {
	Limit time.Duration
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("timed out after %s (%s lengthens the limit)", e.Limit, EnvGitTimeoutScale)
}

// withLimit limits ctx to d scaled by env's EnvGitTimeoutScale (env nil:
// this process's). When the limit ends it, context.Cause is a *LimitError.
func withLimit(ctx context.Context, env []string, d time.Duration) (context.Context, context.CancelFunc) {
	if env == nil {
		env = os.Environ()
	}
	if v, ok := lookupEnv(env, EnvGitTimeoutScale); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 {
			d = time.Duration(min(float64(d)*f, float64(24*time.Hour))).Round(time.Millisecond)
		}
	}
	return context.WithTimeoutCause(ctx, d, &LimitError{Limit: d})
}

// limitOf is the *LimitError that ended ctx, or nil.
func limitOf(ctx context.Context) *LimitError {
	var le *LimitError
	if errors.As(context.Cause(ctx), &le) {
		return le
	}
	return nil
}

// waitDelay is how long a git or gh is given to exit once its context
// ended (it is killed then), and to close its output once it exited (a
// child it left may hold it).
const waitDelay = 10 * time.Second

// command is exec.CommandContext for git and gh: when ctx ends the program
// is asked to stop (SIGTERM; killed on Windows), so that a stopped git
// worktree add removes what it made, and killed waitDelay later.
func command(ctx context.Context, bin string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Cancel = func() error {
		if runtime.GOOS == "windows" {
			return cmd.Process.Kill()
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = waitDelay
	return cmd
}

// exited is the error of a command's Run or Output: none when it exited
// successfully though a child it left kept its output open.
func exited(err error) error {
	if errors.Is(err, exec.ErrWaitDelay) {
		return nil
	}
	return err
}

// lockRetries are the waits before running a git command again that
// failed on a lock file (*.lock: index.lock, a ref's) another git in the
// same repository held, as Orca waits. A lock still there after the last
// is the command's failure.
var lockRetries = []time.Duration{50 * time.Millisecond, 150 * time.Millisecond, 400 * time.Millisecond}

// lockFailure matches git's stderr when it could not take a lock: the
// lock file is quoted ('…/index.lock', in every language), except for the
// config file's and packed-refs'.
var lockFailure = regexp.MustCompile(`\.lock'|could not lock|lock for the packed-ref file`)

// failureLine is the line of a failed command's stderr that says why: the
// one naming the lock it could not take, else the last (git follows a lock
// failure with advice).
func failureLine(stderr string) string {
	for line := range strings.Lines(stderr) {
		if lockFailure.MatchString(line) {
			return strings.TrimSpace(line)
		}
	}
	return lastLine(stderr)
}

// retryLocked runs attempt, which returns the command's stderr, again
// after each of lockRetries while it fails on a lock and ctx lasts.
// waiting is told each wait first.
func retryLocked(ctx context.Context, attempt func() (stderr string, err error), waiting func(time.Duration)) error {
	for i := 0; ; i++ {
		stderr, err := attempt()
		if err == nil || i == len(lockRetries) || !lockFailure.MatchString(stderr) || ctx.Err() != nil {
			return err
		}
		if waiting != nil {
			waiting(lockRetries[i])
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(lockRetries[i]):
		}
	}
}
