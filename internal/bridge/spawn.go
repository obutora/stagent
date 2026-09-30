package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/daemonclient"
	"github.com/obutora/stagent/internal/proc"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

const (
	defaultCols = 80
	defaultRows = 24
	maxTermDim  = 1000
)

func (b *Bridge) spawn(m *wire.Msg) {
	var p wire.SpawnParams
	if err := rpc.Decode(m, &p); err != nil {
		b.replyResult(m.ID, nil, err)
		return
	}
	res, err := b.doSpawn(p)
	b.replyResult(m.ID, res, err)
}

// doSpawn starts a detached holder for p.Command and returns its session
// once the holder answers session.info.
func (b *Bridge) doSpawn(p wire.SpawnParams) (*wire.SpawnResult, error) {
	if len(p.Command) == 0 || p.Command[0] == "" {
		return nil, wire.Errorf(wire.ErrBadRequest, "session.spawn: command is empty")
	}
	cols, rows := p.Cols, p.Rows
	if cols <= 0 {
		cols = defaultCols
	}
	if rows <= 0 {
		rows = defaultRows
	}
	if cols > maxTermDim || rows > maxTermDim {
		return nil, wire.Errorf(wire.ErrBadRequest, "session.spawn: size %dx%d too large", cols, rows)
	}
	cwd := b.expandHome(p.Cwd)
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		return nil, wire.Errorf(wire.ErrBadRequest, "session.spawn: cwd %q is not a directory", cwd)
	}
	env := ensureOnPath(b.sessionEnv(), p.Command[0], b.l.Home)
	keys := make([]string, 0, len(p.Env))
	for k := range p.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			return nil, wire.Errorf(wire.ErrBadRequest, "session.spawn: invalid env name %q", k)
		}
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		env = append(env, k+"="+p.Env[k]) // later entries win in os/exec
	}

	exe, err := daemonclient.Exe()
	if err != nil {
		return nil, err
	}
	if err := b.l.EnsureDirs(); err != nil {
		return nil, err
	}
	id := wire.NewSessionID()
	args := append([]string{
		"run", "--detached", "--id", id,
		"--cols", strconv.Itoa(cols), "--rows", strconv.Itoa(rows),
		"--cwd", cwd, "--",
	}, p.Command...)
	logPath := filepath.Join(b.l.LogDir, id+".log")
	pid, err := proc.SpawnDetached(exe, args, b.l.Home, env, logPath)
	if err != nil {
		return nil, fmt.Errorf("start holder: %w", err)
	}
	s, err := b.awaitHolder(id, pid, logPath)
	if err != nil {
		return nil, err
	}
	return &wire.SpawnResult{Session: *s}, nil
}

// sessionEnv is the environment detached sessions start from: the login
// shell's environment merged over the bridge's own (see captureLoginEnv),
// captured once per bridge. When capturing fails the bridge's own
// environment is used.
func (b *Bridge) sessionEnv() []string {
	b.envOnce.Do(func() {
		b.baseEnv = os.Environ()
		if login, err := b.loginEnv(); err == nil {
			b.baseEnv = mergeLoginEnv(b.baseEnv, login)
		}
	})
	return slices.Clone(b.baseEnv)
}

// awaitHolder waits until the new holder answers session.info and keeps
// that connection for later session requests. pid is 0 when the platform
// could not report it (Windows Task Scheduler fallback).
func (b *Bridge) awaitHolder(id string, pid int, logPath string) (*wire.Session, error) {
	ctx, cancel := context.WithTimeout(b.ctx, b.spawnWait)
	defer cancel()
	for {
		if conn, err := b.dialHolder(id, 300*time.Millisecond); err == nil {
			c := rpc.NewClient(conn, b.relay)
			var s wire.Session
			if err := c.Call(ctx, wire.MethodSessionInfo, wire.SessionRef{ID: id}, &s); err == nil {
				if _, err := b.adoptHolder(id, c); err != nil {
					return nil, err
				}
				return &s, nil
			}
			c.Close()
		}
		if pid > 0 && !proc.Alive(pid) {
			return nil, wire.Errorf(wire.ErrInternal, "holder exited during start%s", logTail(logPath))
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, wire.Errorf(wire.ErrUnavailable, "holder did not answer within %s%s", b.spawnWait, logTail(logPath))
			}
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// expandHome resolves "", "~" and "~/..." against the home directory.
func (b *Bridge) expandHome(p string) string {
	switch {
	case p == "" || p == "~":
		return b.l.Home
	case strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`):
		return filepath.Join(b.l.Home, p[2:])
	}
	return p
}

// logTail returns the end of a holder's stderr log for error messages.
func logTail(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	const n = 1024
	if st, err := f.Stat(); err == nil && st.Size() > n {
		f.Seek(-n, io.SeekEnd)
	}
	b, _ := io.ReadAll(f)
	s := strings.TrimSpace(string(b))
	if s == "" {
		return ""
	}
	return ": " + s
}
