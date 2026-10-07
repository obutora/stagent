package install

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/obutora/stagent/internal/wire"
)

// UninstallReport is the output of `stagent uninstall`.
type UninstallReport struct {
	Level     string    `json:"level"`
	Steps     []Step    `json:"steps"`
	Removed   []string  `json:"removed"`
	Failed    []Failure `json:"failed"`
	Remaining []Finding `json:"remaining"`
}

type Step struct {
	Action string `json:"action"`
	Target string `json:"target"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

type Failure struct {
	Path     string   `json:"path"`
	Reason   string   `json:"reason"`
	Sessions []string `json:"sessions"`
}

func (r *UninstallReport) step(action, target string, err error) bool {
	s := Step{Action: action, Target: target, OK: err == nil}
	if err != nil {
		s.Error = err.Error()
	}
	r.Steps = append(r.Steps, s)
	return err == nil
}

// uninstall runs the cumulative levels stop ⊂ unhook ⊂ purge. linger
// (purge only) also turns lingering off when stagent turned it on; purge
// always undoes stagent's .wslconfig edit (WSL を動かし続ける).
func (e *env) uninstall(levelName string, uploads, linger bool) *UninstallReport {
	level := levelNames[levelName]
	r := &UninstallReport{Level: levelName, Steps: []Step{}, Removed: []string{}, Failed: []Failure{}}
	// Live sessions, for naming what holds files a purge cannot delete.
	sessions, _ := e.daemon.Sessions()

	// stop: the login service, then a daemon started on demand.
	if target, done, err := e.stopService(); done {
		r.step("stop-service", target, err)
	}
	e.stopDaemon(r)

	if level >= levelUnhook {
		e.unhook(r)
	}
	if level >= levelPurge {
		if linger && e.m.LingerEnabled {
			r.step("disable-linger", lingerTarget, e.disableLinger())
		}
		e.revertWSL(r)
		e.purge(r, sessions, uploads)
	} else if e.haveM || e.dirty {
		if err := e.saveManifest(); err != nil {
			r.step("update-manifest", e.l.Manifest, err)
		}
	}

	r.Remaining = []Finding{}
	for _, a := range e.inventory(uploads) {
		if a.level <= level {
			r.Remaining = append(r.Remaining, a.Finding)
		}
	}
	return r
}

func (e *env) stopDaemon(r *UninstallReport) {
	st, err := e.daemon.Status()
	if err != nil {
		return // not running
	}
	r.step(e.shutdownDaemon(st))
}

// shutdownDaemon stops the running daemon st reported: daemon.shutdown
// (holders take it as deliberate and do not start another), then a kill
// when it has not exited after settle. It returns the step taken.
func (e *env) shutdownDaemon(st *wire.DaemonStatus) (action, target string, err error) {
	if err := e.daemon.Shutdown(); agentRefused(err) {
		return "stop-daemon", e.l.DaemonAddr, err // and no kill instead: a person runs this
	}
	if e.waitDaemonGone(st.PID) {
		return "stop-daemon", e.l.DaemonAddr, nil
	}
	if st.PID <= 0 {
		return "stop-daemon", e.l.DaemonAddr, errString("daemon did not exit")
	}
	err = e.kill(st.PID)
	if err == nil && !e.waitDaemonGone(st.PID) {
		err = errString("daemon still running after kill")
	}
	return "kill-daemon", "pid " + strconv.Itoa(st.PID), err
}

// unhook removes every integration element (tracked or orphaned).
func (e *env) unhook(r *UninstallReport) {
	targets := e.allTargets()
	// Plan everything first: Codex trust tables are located through the
	// hooks.json positions before hooks.json changes.
	var changes []*Change
	for _, t := range targets {
		c, err := e.removeChange(t)
		if err != nil {
			r.step("remove-entries", t.path, err)
			continue
		}
		if c != nil {
			changes = append(changes, c)
		}
	}
	sc, err := e.serviceRemoveChanges()
	if err != nil {
		r.step("remove-service", e.serviceKind(), err)
	}
	changes = append(changes, sc...)
	if tc, err := e.terminalRemoveChange(); err != nil {
		r.step("remove-entries", terminalTarget, err)
	} else if tc != nil {
		changes = append(changes, tc)
	}
	for _, c := range changes {
		action := "remove-entries"
		switch {
		case strings.HasPrefix(c.Summary, "restore"):
			action = "restore"
		case c.ID == "service" && strings.HasPrefix(c.Target, "schtasks:"):
			action = "delete-task"
		case c.ID == "service":
			action = "disable-service"
		case c.Action == "delete":
			action = "delete-file"
		case strings.HasPrefix(c.ID, "shell-"):
			action = "remove-block"
		}
		if r.step(action, c.Target, c.apply()) && c.Action == "delete" && !strings.HasPrefix(c.Target, "schtasks:") {
			r.Removed = append(r.Removed, c.Target)
		}
	}
	e.dropStaleRecords(targets)
}

// revertWSL undoes stagent's .wslconfig edit (see wslRemoveChange).
func (e *env) revertWSL(r *UninstallReport) {
	c, err := e.wslRemoveChange()
	switch {
	case err != nil:
		r.step("remove-entries", e.wslRecord().Path, err)
	case c != nil:
		action := "remove-entries"
		switch {
		case strings.HasPrefix(c.Summary, "restore"):
			action = "restore"
		case c.Action == "delete":
			action = "delete-file"
		}
		if r.step(action, c.Target, c.apply()) && c.Action == "delete" {
			r.Removed = append(r.Removed, c.Target)
		}
	}
}

// purge deletes the installation, relocated data, sockets and backups.
func (e *env) purge(r *UninstallReport, sessions []wire.Session, uploads bool) {
	if e.goos == "windows" && exists(e.l.Bin) {
		// A running exe (this process, live holders) cannot be deleted but
		// can be renamed; the rest of the tree can then go.
		if os.Remove(e.l.Bin) != nil {
			old := e.l.Bin + ".old-" + e.now().UTC().Format("20060102T150405Z")
			if err := os.Rename(e.l.Bin, old); err == nil {
				r.step("rename-binary", old, nil)
			}
		}
	}
	e.removeTree(r, e.l.Root, sessions)
	for _, d := range e.outsideDirs() {
		e.removeTree(r, d, sessions)
	}
	for _, b := range e.m.Backups {
		if !exists(b) {
			continue
		}
		if r.step("delete-file", b, os.Remove(b)) {
			r.Removed = append(r.Removed, b)
		}
	}
	if uploads {
		e.removeTree(r, e.l.UploadsDir, sessions)
	}
	// ~/.ssh-term itself, when nothing else is left in it.
	parent := filepath.Dir(e.l.Root)
	if os.Remove(parent) == nil {
		r.Removed = append(r.Removed, parent)
	}
	e.dirty = false // the manifest went with the tree
}

func (e *env) removeTree(r *UninstallReport, dir string, sessions []wire.Session) {
	if !exists(dir) {
		return
	}
	err := os.RemoveAll(dir)
	if r.step("delete-dir", dir, err) {
		r.Removed = append(r.Removed, dir)
		return
	}
	// Report what is left, naming the sessions that hold executables.
	var held []string
	for _, s := range sessions {
		if s.State != wire.StateExited {
			held = append(held, s.ID+" "+s.Harness+" "+s.Cwd)
		}
	}
	n := 0
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() || n >= 20 {
			return nil
		}
		n++
		f := Failure{Path: p, Reason: "could not be deleted", Sessions: []string{}}
		if name := strings.ToLower(d.Name()); strings.HasPrefix(name, "stagent") && (strings.HasSuffix(name, ".exe") || strings.Contains(name, ".old-")) {
			f.Reason = "in use by running stagent processes (this uninstall and live sessions); run purge again after they exit"
			f.Sessions = nonNil(held)
		}
		r.Failed = append(r.Failed, f)
		return nil
	})
	if n == 0 {
		r.Failed = append(r.Failed, Failure{Path: dir, Reason: err.Error(), Sessions: []string{}})
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// agentRefused reports whether err is the daemon refusing a request from a
// coding agent's process tree (ADR 0004): run by an agent, install and
// uninstall cannot stop the daemon.
func agentRefused(err error) bool {
	we, ok := errors.AsType[*wire.Error](err)
	return ok && we.Code == wire.ErrAgentRefused
}
