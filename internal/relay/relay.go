// Package relay builds and takes a relay's package (リレー, PROTOCOL.md
// "Relay", ssh-term ADR 0004 and 0005): on the relay source the
// conversation's document and the working tree's parts (a git bundle of
// what no remote has, the uncommitted changes, the untracked files), packed
// into one gzip tar for another host; on the relay target the package
// checked and unpacked, and the agent's argv. The bridge runs the methods;
// `stagent task run --relay` applies the parts (task.RelayPrep).
package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/obutora/stagent/internal/fsutil"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/task"
	"github.com/obutora/stagent/internal/wire"
)

// ManifestVersion is the only manifest.json version there is.
const ManifestVersion = 1

// Manifest is a package's manifest.json.
type Manifest struct {
	Version int              `json:"version"`
	RelayID string           `json:"relay_id"`
	Source  wire.RelaySource `json:"source"`
	Parts   wire.RelayParts  `json:"parts"`
	Memo    wire.RelayMemo   `json:"memo"`
}

// Members of a package and files of a relay's directories.
const (
	ManifestFile = "manifest.json"
	DocumentFile = "relay.md"
	MemoFile     = "memo.md"
	ImagesDir    = "images"
	TreeDir      = "tree"
	PackageFile  = "package.tar.gz"
	// startedFile, in a target's relay directory, records what relay.start
	// made (Started).
	startedFile = "started.json"
	// receiveFile records the package being received (relay.receive).
	receiveFile = "receive.json"
)

// Keep is how long a relay's package and working tree parts stay
// without relay.discard.
const Keep = 7 * 24 * time.Hour

// OutDir is relay id's directory on its source: the package, or within
// one host its parts as files.
func OutDir(l *paths.Layout, id string) string { return filepath.Join(l.RelayOut, id) }

// InDir is relay id's directory on its target: the received package, the
// unpacked tree parts and the document.
func InDir(l *paths.Layout, id string) string { return filepath.Join(l.RelayIn, id) }

// chmodOK: modes are set whatever the umask; on Windows files get the ACL
// they inherit from the state directory instead.
const chmodOK = runtime.GOOS != "windows"

// maxPackage bounds a package (too_large).
const maxPackage = wire.RelayPackageMax

// MkdirPrivate makes dir (and its parents) owner-only.
func MkdirPrivate(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if !chmodOK {
		return nil
	}
	return os.Chmod(dir, 0o700)
}

// writePrivate writes b to path, mode 0600.
func writePrivate(path string, b []byte) error {
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	if !chmodOK {
		return nil
	}
	return os.Chmod(path, 0o600)
}

// ReadManifest reads dir's manifest.json, refusing another version.
func ReadManifest(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", ManifestFile, err)
	}
	if m.Version != ManifestVersion {
		return nil, fmt.Errorf("%s: version %d, not %d", ManifestFile, m.Version, ManifestVersion)
	}
	return &m, nil
}

// WriteManifest writes m to dir's manifest.json.
func WriteManifest(dir string, m *Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(dir, ManifestFile), b)
}

// Started is what relay.start made on its target, for busy, relay.abort
// and the 7 days of the tree parts.
type Started struct {
	TaskID    string `json:"task_id,omitempty"`
	SessionID string `json:"session_id"`
	// Branch is the branch the relay made (deleted by relay.abort), Repo
	// its checkout.
	Branch string `json:"branch,omitempty"`
	Repo   string `json:"repo,omitempty"`
	At     int64  `json:"at"` // unix ms
}

// ReadStarted is what relay.start recorded in dir, nil when nothing.
func ReadStarted(dir string) *Started {
	b, err := os.ReadFile(filepath.Join(dir, startedFile))
	if err != nil {
		return nil
	}
	var s Started
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	return &s
}

// WriteStarted records s in dir.
func WriteStarted(dir string, s Started) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(dir, startedFile), b)
}

// ForgetStarted removes what relay.start recorded in dir.
func ForgetStarted(dir string) error {
	err := os.Remove(filepath.Join(dir, startedFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Discard removes what relay id left on this host but a started relay's
// document: its source directory, and on the target the package and the
// tree parts — the whole directory of a relay never started. Idempotent.
func Discard(l *paths.Layout, id string) error {
	errs := []error{os.RemoveAll(OutDir(l, id))}
	in := InDir(l, id)
	if ReadStarted(in) == nil && !exists(filepath.Join(in, DocumentFile)) {
		errs = append(errs, os.RemoveAll(in))
	} else {
		errs = append(errs, removeFile(filepath.Join(in, PackageFile)), removeFile(filepath.Join(in, receiveFile)),
			os.RemoveAll(filepath.Join(in, TreeDir)))
	}
	return errors.Join(errs...)
}

// Undo removes what relay.start left in relay id's target directory but
// the received package: the document, the tree parts and the record of the
// start (relay.abort).
func Undo(l *paths.Layout, id string) error {
	in := InDir(l, id)
	ents, err := os.ReadDir(in)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range ents {
		if e.Name() == PackageFile || e.Name() == receiveFile {
			continue
		}
		errs = append(errs, os.RemoveAll(filepath.Join(in, e.Name())))
	}
	return errors.Join(errs...)
}

// Sweep removes what has passed its time (Keep): a source's directory 7
// days after relay.prepare, a received package 7 days after its last
// relay.write (with its directory, when the relay never started), the tree
// parts 7 days after relay.start. Documents stay.
func Sweep(l *paths.Layout, now time.Time) {
	old := func(path string) bool {
		st, err := os.Stat(path)
		return err == nil && now.Sub(st.ModTime()) > Keep
	}
	if ents, err := os.ReadDir(l.RelayOut); err == nil {
		for _, e := range ents {
			if dir := filepath.Join(l.RelayOut, e.Name()); old(dir) {
				os.RemoveAll(dir)
			}
		}
	}
	ents, err := os.ReadDir(l.RelayIn)
	if err != nil {
		return
	}
	for _, e := range ents {
		dir := filepath.Join(l.RelayIn, e.Name())
		started := filepath.Join(dir, startedFile)
		if !exists(started) && !exists(filepath.Join(dir, DocumentFile)) {
			if pkg := filepath.Join(dir, PackageFile); old(pkg) || !exists(pkg) && old(dir) {
				os.RemoveAll(dir)
			}
			continue
		}
		if pkg := filepath.Join(dir, PackageFile); old(pkg) {
			removeFile(pkg)
			removeFile(filepath.Join(dir, receiveFile))
		}
		if old(started) {
			os.RemoveAll(filepath.Join(dir, TreeDir))
		}
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func removeFile(path string) error {
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// StageError prefixes err with the relay stage it failed at.
type StageError struct {
	Stage string
	Err   error
}

func (e *StageError) Error() string { return e.Stage + ": " + e.Err.Error() }
func (e *StageError) Unwrap() error { return e.Err }

// Staged wraps err (nil: nil) as failing stage.
func Staged(stage string, err error) error {
	if err == nil {
		return nil
	}
	return &StageError{Stage: stage, Err: err}
}

// gitErr is git's error without the command line task.GitError puts first.
func gitErr(err error) error {
	var ge *task.GitError
	if errors.As(err, &ge) {
		if ge.Stderr != "" {
			return errors.New(ge.Stderr)
		}
		return ge.Err
	}
	return err
}
