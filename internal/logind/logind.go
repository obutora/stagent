// Package logind asks systemd-logind whether processes a user leaves
// running survive the end of the login session they were started from, and
// moves stagent's long-lived processes where they do.
//
// With KillUserProcesses=yes (logind.conf) logind kills every process of a
// session's scope when the session ends, which would take holders started
// from an SSH or tty login with it. Processes in a scope of the user's
// service manager (what `systemd-run --user --scope` creates) are out of the
// session; they still end with the service manager at the user's last
// logout unless lingering is enabled. Terminals of a graphical session
// already run under the service manager, so they need lingering too.
package logind

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// probeTimeout bounds one busctl call; logind may be slow or absent.
const probeTimeout = 2 * time.Second

// moveWait bounds the wait for the cgroup to change after the scope was
// asked for.
const moveWait = time.Second

// KillUserProcessesCmd reads logind's KillUserProcesses setting; the output
// is parsed with ParseBusctlBool.
var KillUserProcessesCmd = []string{
	"busctl", "get-property", "org.freedesktop.login1", "/org/freedesktop/login1",
	"org.freedesktop.login1.Manager", "KillUserProcesses",
}

// LingerCmd reads whether lingering is enabled for uid; the output is
// parsed with ParseBusctlBool. It fails when the user has no logind user
// object (never logged in, no lingering).
func LingerCmd(uid int) []string {
	return []string{
		"busctl", "get-property", "org.freedesktop.login1",
		"/org/freedesktop/login1/user/_" + strconv.Itoa(uid),
		"org.freedesktop.login1.User", "Linger",
	}
}

// StartScopeCmd asks the user's service manager for a new transient scope
// unit holding pid: the D-Bus call `systemd-run --user --scope` makes.
func StartScopeCmd(unit string, pid int) []string {
	return []string{
		"busctl", "--user", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
		"org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))",
		unit, "fail", "3",
		"PIDs", "au", "1", strconv.Itoa(pid),
		"CollectMode", "s", "inactive-or-failed",
		"Description", "s", "stagent (SSH Term)",
		"0",
	}
}

// ParseBusctlBool parses a boolean property as printed by busctl
// get-property ("b true"). ok is false for any other output.
func ParseBusctlBool(out string) (v, ok bool) {
	switch strings.TrimSpace(out) {
	case "b true":
		return true, true
	case "b false":
		return false, true
	}
	return false, false
}

// Facts are logind's settings that decide whether a user's processes
// outlive logout. A *Known field false means logind could not be asked.
type Facts struct {
	Kill, KillKnown     bool // KillUserProcesses
	Linger, LingerKnown bool
}

// WantScope reports whether a process should move into a scope of the
// user's service manager: it leaves a login session that would kill it
// (KillUserProcesses=yes), or lingering keeps the service manager past every
// logout. With KillUserProcesses=no and no lingering the login session
// outlives the last logout while the service manager does not, so moving
// would make things worse.
func (f Facts) WantScope() bool {
	return f.KillKnown && f.Kill || f.LingerKnown && f.Linger
}

// Placement is where a process runs as far as logout is concerned.
type Placement int

const (
	PlaceUnknown Placement = iota
	// PlaceSession: the scope of a login session (session-N.scope).
	PlaceSession
	// PlaceManager: under the user's service manager (user@UID.service).
	PlaceManager
	// PlaceOther: outside the user's slice (a system service, a container
	// without systemd); logout does not end it.
	PlaceOther
)

// SystemdCgroup returns the cgroup path systemd tracks units by from the
// content of /proc/<pid>/cgroup: the name=systemd hierarchy of cgroup v1,
// else the unified (v2) one. "" when neither is there.
func SystemdCgroup(procCgroup string) string {
	unified := ""
	for _, line := range strings.Split(procCgroup, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 {
			continue
		}
		if parts[1] == "name=systemd" {
			return parts[2]
		}
		if parts[0] == "0" && parts[1] == "" {
			unified = parts[2]
		}
	}
	return unified
}

// Place classifies a cgroup path (SystemdCgroup).
func Place(cgroup string) Placement {
	if cgroup == "" {
		return PlaceUnknown
	}
	inUserSlice := false
	for _, seg := range strings.Split(cgroup, "/") {
		switch {
		case strings.HasPrefix(seg, "user@") && strings.HasSuffix(seg, ".service"):
			return PlaceManager
		case strings.HasPrefix(seg, "session-") && strings.HasSuffix(seg, ".scope"):
			return PlaceSession
		case strings.HasPrefix(seg, "user-") && strings.HasSuffix(seg, ".slice"):
			inUserSlice = true
		}
	}
	if inUserSlice {
		return PlaceUnknown
	}
	return PlaceOther
}

// inService reports whether cgroup is a service unit's own: moving such a
// process out (the daemon run by stagent.service) would leave the unit
// empty while its main process still runs.
func inService(cgroup string) bool {
	leaf := cgroup[strings.LastIndexByte(cgroup, '/')+1:]
	return strings.HasSuffix(leaf, ".service")
}

// Survives reports whether a process placed at p outlives the user's
// logout (the last one, for the service manager); known is false when the
// placement or the logind setting it depends on is unknown.
func (f Facts) Survives(p Placement) (survives, known bool) {
	switch p {
	case PlaceSession:
		return !f.Kill, f.KillKnown
	case PlaceManager:
		return f.Linger, f.LingerKnown
	case PlaceOther:
		return true, true
	}
	return false, false
}

// Result is what Escape found and did.
type Result struct {
	Facts
	// Cgroup is the process's systemd cgroup after Escape ("" unknown).
	Cgroup string
	// Moved: the process is in the new scope.
	Moved bool
}

// SurvivesLogout reports whether the process outlives logout where it is
// now; known is false off Linux and when it cannot be told.
func (r Result) SurvivesLogout() (survives, known bool) {
	return r.Facts.Survives(Place(r.Cgroup))
}

// run is replaced in tests.
var run = func(argv []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
	return string(out), err
}

// readCgroup is replaced in tests.
var readCgroup = func() string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	return SystemdCgroup(string(b))
}

func readBool(argv []string) (v, ok bool) {
	out, err := run(argv)
	if err != nil {
		return false, false
	}
	return ParseBusctlBool(out)
}

// ReadFacts asks logind for KillUserProcesses and uid's lingering. Both are
// unknown off Linux.
func ReadFacts(uid int) Facts {
	var f Facts
	if runtime.GOOS != "linux" {
		return f
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		f.Linger, f.LingerKnown = readBool(LingerCmd(uid))
	}()
	f.Kill, f.KillKnown = readBool(KillUserProcessesCmd)
	wg.Wait()
	return f
}

// Escape is the first thing `stagent run` and `stagent daemon` do on Linux:
// when Facts.WantScope, the process moves itself into a new transient scope
// of the user's service manager (unit "<prefix>-<pid>-<random>.scope") and
// waits until its cgroup has changed, so that everything it starts later
// (the agent, the daemon) starts there too. A process already in a service
// unit stays. Failing to move is not an error: the process runs on where it
// is, and Result says where that is.
func Escape(prefix string) Result {
	if runtime.GOOS != "linux" {
		return Result{}
	}
	r := Result{Facts: ReadFacts(os.Getuid()), Cgroup: readCgroup()}
	if !r.WantScope() || r.Cgroup == "" || inService(r.Cgroup) {
		return r
	}
	var b [4]byte
	rand.Read(b[:])
	unit := prefix + "-" + strconv.Itoa(os.Getpid()) + "-" + hex.EncodeToString(b[:]) + ".scope"
	if _, err := run(StartScopeCmd(unit, os.Getpid())); err != nil {
		return r
	}
	before := r.Cgroup
	deadline := time.Now().Add(moveWait)
	for {
		if cg := readCgroup(); cg != before && cg != "" {
			r.Cgroup, r.Moved = cg, true
			return r
		}
		if time.Now().After(deadline) {
			return r
		}
		time.Sleep(5 * time.Millisecond)
	}
}
