package install

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
)

// Change is one entry of `integrate` output (and of the removals unhook
// performs).
type Change struct {
	ID     string `json:"id"`
	Target string `json:"target"`
	// Action is create | modify | delete, or skip for a target that could
	// not be planned (Error says why).
	Action  string `json:"action"`
	Summary string `json:"summary"`
	Diff    string `json:"diff"`
	Error   string `json:"error,omitempty"`
	// ErrorCode classifies Error for the app (errorCode).
	ErrorCode string `json:"error_code,omitempty"`

	apply func() error // nil for skip
}

// Error codes of Change.ErrorCode (PROTOCOL.md).
const (
	codeNotWritable   = "not_writable"
	codeReadOnlyFS    = "read_only_fs"
	codeUTF16Profile  = "utf16_profile"
	codeUnmanagedFile = "unmanaged_file"
	codeIOError       = "io_error"
	// codeLingerDenied: polkit refused `loginctl enable-linger`
	// (set-self-linger); an administrator has to enable it.
	codeLingerDenied = "linger_denied"
)

// contentError is a file whose content stagent does not edit: a UTF-16
// profile, or a file that is not ours or not parsable.
type contentError struct{ code, msg string }

func (e *contentError) Error() string { return e.msg }

func unmanagedFile(msg string) error { return &contentError{codeUnmanagedFile, msg} }

// targetError is a failure to plan one target.
type targetError struct {
	id, path string
	err      error
}

func (e *targetError) Error() string { return e.err.Error() }
func (e *targetError) Unwrap() error { return e.err }

// errorCode classifies an error of planning or applying a change.
func errorCode(err error) string {
	var ce *contentError
	var de *deniedError
	switch {
	case errors.As(err, &ce):
		return ce.code
	case errors.As(err, &de):
		return codeLingerDenied
	case errors.Is(err, fs.ErrPermission):
		return codeNotWritable
	case errors.Is(err, syscall.EROFS):
		return codeReadOnlyFS
	}
	return codeIOError
}

// skipChange reports a target that could not be planned.
func skipChange(id string, err error) *Change {
	c := &Change{ID: id, Action: "skip", Summary: "left unchanged", Error: err.Error(), ErrorCode: errorCode(err)}
	if te := (*targetError)(nil); errors.As(err, &te) {
		c.ID, c.Target = te.id, te.path
	}
	return c
}

// target is one integration point: a file stagent adds elements to (or
// owns entirely), with the operations to add, remove and detect them.
type target struct {
	id       string
	path     string
	owned    bool // the whole file is ours
	identify string
	mode     fs.FileMode

	// add returns the file content with our elements in place.
	add func(cur []byte) (addResult, error)
	// remove returns the content without our elements (element-wise; nil
	// deletes the file). rec is the manifest record, if any.
	remove func(cur []byte, rec *ConfigEntry) ([]byte, error)
	// present describes our elements in cur ("" = none).
	present func(cur []byte) string
	// afterRemove runs after a successful removal write (e.g. reload a
	// service manager); nil if not needed.
	afterRemove func() error
}

type addResult struct {
	after      []byte
	containers []string
	edits      []TOMLEdit
	summary    string
}

// addChange plans adding t's elements. nil when the file already has them.
func (e *env) addChange(t *target) (*Change, error) {
	cur, err := readOptional(t.path)
	if err != nil {
		return nil, &targetError{t.id, t.path, err}
	}
	res, err := t.add(cur)
	if err != nil {
		return nil, &targetError{t.id, t.path, err}
	}
	if cur != nil && bytes.Equal(res.after, cur) {
		return nil, nil
	}
	c := &Change{ID: t.id, Target: t.path, Action: "modify", Summary: res.summary, Diff: unifiedDiff(t.path, cur, res.after)}
	if cur == nil {
		c.Action = "create"
	}
	c.apply = func() error { return e.writeAdd(t, cur, res) }
	return c, nil
}

func (e *env) writeAdd(t *target, before []byte, res addResult) error {
	now := e.now()
	rec := e.m.config(t.id, t.path)
	if rec == nil {
		rec = &ConfigEntry{ID: t.id, Path: t.path, Owned: t.owned, Created: before == nil, Identify: t.identify}
		if before != nil {
			rec.SHA256Before = sha256Hex(before)
			// A file already holding our elements without a record (an
			// earlier version wrote them, or the manifest was lost) has no
			// pre-stagent content to back up: removal takes them out.
			if !t.owned && t.present(before) == "" {
				b := backupName(resolveTarget(t.path), now)
				if err := os.WriteFile(b, before, 0o600); err != nil {
					return err
				}
				rec.Backup = b
				e.m.Backups = addUnique(e.m.Backups, b)
			}
		}
		e.m.Configs = append(e.m.Configs, rec)
	} else if before == nil {
		// Deleted by someone since our last edit: we create it anew.
		rec.Created, rec.Backup, rec.SHA256Before = true, "", ""
	} else if sha256Hex(before) != rec.SHA256After {
		// Edited by someone since our last write: the backup no longer
		// restores a state that contains those edits.
		rec.Backup = ""
	}
	rec.CreatedDirs = addUnique(rec.CreatedDirs, missingDirs(filepath.Dir(resolveTarget(t.path)))...)
	if err := atomicWrite(t.path, res.after, t.mode); err != nil {
		return err
	}
	rec.SHA256After = sha256Hex(res.after)
	rec.CreatedContainers = addUnique(rec.CreatedContainers, res.containers...)
	for _, ed := range res.edits {
		known := false
		for _, x := range rec.TOMLEdits {
			if x.Kind == ed.Kind {
				known = true
			}
		}
		if !known {
			rec.TOMLEdits = append(rec.TOMLEdits, ed)
		}
	}
	rec.ModifiedAt = now.UnixMilli()
	e.dirty = true
	return nil
}

// removeChange plans taking t's elements out. When the file is exactly what
// we last wrote, it is restored from the backup, which is then deleted (or
// the file is deleted if we created it, with the directories we created for
// it once empty); otherwise our elements are removed one by one. nil when
// there is nothing of ours in the file.
func (e *env) removeChange(t *target) (*Change, error) {
	cur, err := readOptional(t.path)
	if err != nil {
		return nil, &targetError{t.id, t.path, err}
	}
	if cur == nil {
		return nil, nil
	}
	rec := e.m.config(t.id, t.path)
	var after []byte
	summary, decided, restored := "", false, ""
	if rec != nil && sha256Hex(cur) == rec.SHA256After {
		switch {
		case rec.Created || rec.Owned:
			after, summary, decided = nil, "delete the file stagent created", true
		case rec.Backup != "" && fileSHA256(rec.Backup) == rec.SHA256Before:
			b, err := os.ReadFile(rec.Backup)
			if err == nil {
				after, summary, decided, restored = b, "restore the pre-stagent content from "+rec.Backup+" and delete the backup", true, rec.Backup
			}
		}
	}
	if !decided {
		if t.present(cur) == "" {
			return nil, nil
		}
		after, err = t.remove(cur, rec)
		if err != nil {
			return nil, &targetError{t.id, t.path, err}
		}
		summary = "remove stagent's elements (" + t.identify + ")"
		if after == nil {
			summary = "delete the file (managed by stagent)"
		}
	}
	if after != nil && bytes.Equal(after, cur) {
		return nil, nil
	}
	c := &Change{ID: t.id, Target: t.path, Action: "modify", Summary: summary, Diff: unifiedDiff(t.path, cur, after)}
	if after == nil {
		c.Action = "delete"
	}
	c.apply = func() error {
		if after == nil {
			if err := os.Remove(resolveTarget(t.path)); err != nil && !os.IsNotExist(err) {
				return err
			}
		} else if err := atomicWrite(t.path, after, t.mode); err != nil {
			return err
		}
		if rec != nil {
			if after == nil {
				removeEmptyDirs(rec.CreatedDirs)
			}
			if restored != "" {
				e.dropBackup(restored)
			}
			e.m.dropConfig(rec)
			e.dirty = true
		}
		if t.afterRemove != nil {
			return t.afterRemove()
		}
		return nil
	}
	return c, nil
}

// dropBackup deletes a backup whose content is back in its file. One that
// cannot be deleted stays listed for purge.
func (e *env) dropBackup(b string) {
	if err := os.Remove(b); err == nil || errors.Is(err, fs.ErrNotExist) {
		e.m.Backups = slices.DeleteFunc(e.m.Backups, func(x string) bool { return x == b })
		e.dirty = true
	}
}

// dropStaleRecords forgets manifest records of targets that no longer hold
// any element of ours (removed by hand, or reverted).
func (e *env) dropStaleRecords(ts []*target) {
	for _, t := range ts {
		rec := e.m.config(t.id, t.path)
		if rec == nil {
			continue
		}
		cur, err := readOptional(t.path)
		if err != nil {
			continue
		}
		if cur == nil || t.present(cur) == "" {
			if cur == nil {
				removeEmptyDirs(rec.CreatedDirs)
			}
			e.m.dropConfig(rec)
			e.dirty = true
		}
	}
}
