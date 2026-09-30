// Package paths resolves where stagent keeps its binary, state, sockets and
// scrollback on the current host. Every other package takes a *Layout rather
// than computing paths itself, so tests and the load bench can isolate a whole
// installation by setting STAGENT_HOME.
package paths

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// EnvHome overrides the home directory. Tests and the bench point it at a
// temporary directory; it also namespaces the IPC endpoints so an isolated
// installation never talks to the real daemon.
const EnvHome = "STAGENT_HOME"

// Layout is the resolved file-system layout of one installation.
type Layout struct {
	Home       string // user home (or STAGENT_HOME)
	Root       string // <Home>/.ssh-term/agent
	BinDir     string // <Root>/bin
	Bin        string // <BinDir>/stagent[.exe]
	Manifest   string // <Root>/manifest.json
	Config     string // <Root>/config.json
	StateDir   string // <Root>/state
	EventLog   string // <StateDir>/events.log (segments get a numeric suffix)
	Index      string // <StateDir>/index.json — conversation list cache
	DataDir    string // per-session scrollback segments; local disk even when Home is on NFS
	LogDir     string // <Root>/log — stderr of detached processes
	RunDir     string // sockets (unix) / lock and pid files (all OSes), 0700
	DaemonAddr string // IPC address of the daemon
	UploadsDir string // <Home>/.ssh-term/uploads — files sent from the app
	// DataOnNetworkFS reports that Home is on a network file system and
	// DataDir was moved to local disk.
	DataOnNetworkFS bool

	pipePrefix string // windows: `\\.\pipe\stagent-<sid>[-<hash>]`
}

// Resolve computes the layout for the current user. It does not create
// anything; call EnsureDirs for that.
func Resolve() (*Layout, error) {
	home, isolated := os.Getenv(EnvHome), true
	if home == "" {
		isolated = false
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home: %w", err)
		}
		home = h
	}
	return resolve(home, isolated)
}

func resolve(home string, isolated bool) (*Layout, error) {
	root := filepath.Join(home, ".ssh-term", "agent")
	exe := "stagent"
	if runtime.GOOS == "windows" {
		exe = "stagent.exe"
	}
	l := &Layout{
		Home:       home,
		Root:       root,
		BinDir:     filepath.Join(root, "bin"),
		Bin:        filepath.Join(root, "bin", exe),
		Manifest:   filepath.Join(root, "manifest.json"),
		Config:     filepath.Join(root, "config.json"),
		StateDir:   filepath.Join(root, "state"),
		LogDir:     filepath.Join(root, "log"),
		UploadsDir: filepath.Join(home, ".ssh-term", "uploads"),
	}
	l.EventLog = filepath.Join(l.StateDir, "events.log")
	l.Index = filepath.Join(l.StateDir, "index.json")
	l.DataDir = filepath.Join(l.StateDir, "sessions")
	if !isolated && isNetworkFS(home) {
		l.DataOnNetworkFS = true
		l.DataDir = filepath.Join(localDataBase(), fmt.Sprintf("stagent-%s", userTag()), "sessions")
	}
	if err := l.resolveIPC(isolated); err != nil {
		return nil, err
	}
	return l, nil
}

// HolderAddr is the IPC address a session holder listens on.
func (l *Layout) HolderAddr(sessionID string) string {
	if runtime.GOOS == "windows" {
		return l.pipePrefix + "-" + sessionID
	}
	return filepath.Join(l.RunDir, "s", sessionID+".sock")
}

// HolderSocketDir is the directory holding holder sockets on unix (empty on
// Windows, where holders use named pipes). The daemon scans it on start.
func (l *Layout) HolderSocketDir() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	return filepath.Join(l.RunDir, "s")
}

// SessionDataDir is where one session's scrollback segments live.
func (l *Layout) SessionDataDir(sessionID string) string {
	return filepath.Join(l.DataDir, sessionID)
}

// EnsureDirs creates every directory of the layout with owner-only access.
func (l *Layout) EnsureDirs() error {
	dirs := []string{l.Root, l.BinDir, l.StateDir, l.DataDir, l.LogDir, l.RunDir}
	if d := l.HolderSocketDir(); d != "" {
		dirs = append(dirs, d)
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		// MkdirAll leaves pre-existing directories alone; tighten them.
		if runtime.GOOS != "windows" {
			if err := os.Chmod(d, 0o700); err != nil {
				return err
			}
		}
	}
	return nil
}

// homeTag is a short stable hash of the home directory, used to keep the
// IPC endpoints of isolated (STAGENT_HOME) installations apart.
func homeTag(home string) string {
	sum := sha256.Sum256([]byte(home))
	return hex.EncodeToString(sum[:4])
}
