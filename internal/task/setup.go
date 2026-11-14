package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"slices"
	"time"
)

// A task's scripts.setup comes from its worktree: the branch's orca.yaml,
// a fork's PR included. It runs only when it is the source checkout's own
// or the user approved that content for the checkout (setup の承認, ADR
// 0023); otherwise preparation asks the app (task.await_setup).

// SetupHash names a scripts.setup content: the hex SHA-256 of its text.
func SetupHash(script string) string {
	sum := sha256.Sum256([]byte(script))
	return hex.EncodeToString(sum[:])
}

// SetupApprovals is the file of approved scripts.setup contents
// (paths.Layout.SetupApprovals), written by the daemon only (task.setup).
type SetupApprovals struct {
	Approved []SetupApproval `json:"approved"`
}

// SetupApproval is one content (Hash, SetupHash) approved for the source
// checkout Repo (canonical).
type SetupApproval struct {
	Repo string    `json:"repo"`
	Hash string    `json:"hash"`
	At   time.Time `json:"at"`
}

// LoadSetupApprovals reads the approvals file (none: no approvals).
func LoadSetupApprovals(path string) (SetupApprovals, error) {
	var a SetupApprovals
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return a, err
	}
	err = json.Unmarshal(b, &a)
	return a, err
}

// Has reports whether hash is approved for repo.
func (a SetupApprovals) Has(repo, hash string) bool {
	return slices.ContainsFunc(a.Approved, func(x SetupApproval) bool {
		return x.Hash == hash && SamePath(x.Repo, repo)
	})
}

// SetupApproved reports whether the approvals file at path ("": none)
// approves hash for repo; one that cannot be read approves nothing.
func SetupApproved(path, repo, hash string) bool {
	if path == "" {
		return false
	}
	a, err := LoadSetupApprovals(path)
	return err == nil && a.Has(repo, hash)
}
