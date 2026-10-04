package install

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// WSL stops a distribution's instance instanceIdleTimeout ms (15000 by
// default) after the last Windows-side client (wsl.exe, bash.exe) exits;
// processes inside WSL do not count, so detached sessions and kept shells
// end with the instance some 30 s after the SSH connection or the PC's
// terminal closes. `[general] instanceIdleTimeout=-1` in the Windows user's
// .wslconfig keeps it running until WSL or Windows itself stops. It applies
// to every distribution of the user and takes effect when WSL starts again.
// `integrate --wsl-keep-running` writes that one line through the drive
// mount (/mnt/c) and records it as a ConfigEntry; only a recorded edit is
// undone, by `integrate --remove wsl-keep-running` and `uninstall --level
// purge`. A line the user wrote is never touched.

const (
	// wslKeepRunningID is the Change.ID, --remove name and ConfigEntry.ID.
	wslKeepRunningID = "wsl-keep-running"
	// wslIdleEditKind is the TOMLEdit.Kind recording the line.
	wslIdleEditKind = "wsl_instance_idle_timeout"
	// wslIdleLine is the line stagent writes.
	wslIdleLine = "instanceIdleTimeout=-1"
	// wslDefaultIdleTimeout is WSL's instanceIdleTimeout when unset (ms).
	wslDefaultIdleTimeout = 15000
	// wslConfigWindowsPath names the file for people (skip changes).
	wslConfigWindowsPath = `%USERPROFILE%\.wslconfig`
	// wslShutdownTimeout bounds `wsl.exe --shutdown` when it does not take
	// this process down with it.
	wslShutdownTimeout = time.Minute
)

// WSLReport is doctor's view of WSL (persistence.wsl; null off WSL).
type WSLReport struct {
	// Distro is $WSL_DISTRO_NAME ("" when unset).
	Distro string `json:"distro"`
	// ConfigPath is the Linux path of the user's .wslconfig, whether or not
	// it exists; null when the Windows profile cannot be reached.
	ConfigPath *string `json:"config_path"`
	// InstanceIdleTimeout is [general] instanceIdleTimeout in ms, WSL's
	// default when the file or the key is absent or not an integer; null
	// when the file cannot be read.
	InstanceIdleTimeout *int64 `json:"instance_idle_timeout"`
	// NetworkingMode is [wsl2] networkingMode (lower case), "nat" when
	// absent; null when the file cannot be read.
	NetworkingMode *string `json:"networking_mode"`
	// KeepRunningNeeded: WSL stops this distribution when idle
	// (InstanceIdleTimeout >= 0). It follows the file, which WSL reads
	// only when it starts. Null when InstanceIdleTimeout is.
	KeepRunningNeeded *bool `json:"keep_running_needed"`
	// KeepRunningByStagent: the manifest records that `integrate
	// --wsl-keep-running` wrote the line, and the file still has it.
	KeepRunningByStagent bool `json:"keep_running_by_stagent"`
}

// isWSL reports whether this Linux runs on WSL's kernel.
func (e *env) isWSL() bool {
	return e.goos == "linux" && e.kernelRelease != nil &&
		strings.Contains(strings.ToLower(e.kernelRelease()), "microsoft")
}

// readKernelRelease is the kernel release (/proc/sys/kernel/osrelease).
func readKernelRelease() string {
	b, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	return string(b)
}

// windowsExe locates a program of Windows' System32 through PATH (WSL
// appends Windows' PATH), else at the default drive mount.
func (e *env) windowsExe(name string) string {
	if p, err := e.lookPath(name); err == nil {
		return p
	}
	return "/mnt/c/Windows/System32/" + name
}

// wslConfigPath is the Linux path of %USERPROFILE%\.wslconfig, asked from
// cmd.exe through interop and converted with wslpath. Cached per run.
func (e *env) wslConfigPath() (string, error) {
	if e.wslConfig == nil {
		p, err := e.findWSLConfig()
		e.wslConfig = &wslConfigLookup{p, err}
	}
	return e.wslConfig.path, e.wslConfig.err
}

type wslConfigLookup struct {
	path string
	err  error
}

func (e *env) findWSLConfig() (string, error) {
	unreachable := func(why string) error {
		return &contentError{codeWSLConfigUnreachable, "cannot reach the Windows user profile holding .wslconfig: " + why}
	}
	// cmd.exe started from a directory of the Linux file system warns on
	// stderr that UNC paths are not supported; the profile is the line
	// that looks like a Windows path.
	out, err := e.run.Run(cmdTimeout, e.windowsExe("cmd.exe"), "/d", "/c", "echo", "%USERPROFILE%")
	if err != nil {
		return "", unreachable("cmd.exe: " + firstLine(out, err))
	}
	win := ""
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if len(l) >= 3 && l[1] == ':' && l[2] == '\\' && isASCIILetter(l[0]) {
			win = l
		}
	}
	if win == "" {
		return "", unreachable("cmd.exe did not print %USERPROFILE%")
	}
	out, err = e.run.Run(cmdTimeout, "wslpath", "-u", win)
	p := strings.TrimSpace(out)
	if err != nil || !strings.HasPrefix(p, "/") || strings.Contains(p, "\n") {
		return "", unreachable("wslpath " + win + ": " + firstLine(out, err))
	}
	return strings.TrimSuffix(p, "/") + "/.wslconfig", nil
}

func isASCIILetter(c byte) bool { return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' }

// firstLine is a command's first output line, or its error.
func firstLine(out string, err error) string {
	if l, _, _ := strings.Cut(strings.TrimSpace(out), "\n"); l != "" {
		return strings.TrimSpace(l)
	}
	if err != nil {
		return err.Error()
	}
	return "no output"
}

// wslTarget is the .wslconfig at path as an integration target.
func (e *env) wslTarget(path string) *target {
	return &target{
		id: wslKeepRunningID, path: path, identify: wslIdleLine + " in [general]", mode: 0o644,
		add: wslAdd, remove: wslRemove, present: wslPresent,
	}
}

// wslRecord is the manifest's record of stagent's .wslconfig edit.
func (e *env) wslRecord() *ConfigEntry {
	for _, c := range e.m.Configs {
		if c.ID == wslKeepRunningID {
			return c
		}
	}
	return nil
}

// wslAddChange plans `[general] instanceIdleTimeout=-1`; nil when the file
// keeps WSL running already.
func (e *env) wslAddChange() (*Change, error) {
	if !e.isWSL() {
		return nil, errors.New("keeping WSL running (.wslconfig) applies to Linux running in WSL only")
	}
	p, err := e.wslConfigPath()
	if err != nil {
		return nil, &targetError{wslKeepRunningID, wslConfigWindowsPath, err}
	}
	c, err := e.addChange(e.wslTarget(p))
	if c != nil {
		e.note(noteWSLRestart, "WSL reads .wslconfig when it starts: the change applies once WSL is restarted (`wsl --shutdown`, which stops every distribution) or the PC is.")
	}
	return c, err
}

// wslRemoveChange plans undoing stagent's .wslconfig edit; nil when the
// manifest records none, or the file no longer has the line (the record is
// dropped then).
func (e *env) wslRemoveChange() (*Change, error) {
	rec := e.wslRecord()
	if rec == nil {
		return nil, nil
	}
	cur, err := readOptional(rec.Path)
	if err != nil {
		return nil, &targetError{wslKeepRunningID, rec.Path, err}
	}
	if cur == nil || wslPresent(cur) == "" {
		e.m.dropConfig(rec)
		e.dirty = true
		return nil, nil
	}
	c, err := e.removeChange(e.wslTarget(rec.Path))
	if c != nil {
		e.note(noteWSLRestart, "WSL reads .wslconfig when it starts: the change applies once WSL is restarted (`wsl --shutdown`, which stops every distribution) or the PC is.")
	}
	return c, err
}

// wslReport is doctor's WSL item; nil off WSL.
func (e *env) wslReport() *WSLReport {
	if !e.isWSL() {
		return nil
	}
	r := &WSLReport{Distro: e.getenv("WSL_DISTRO_NAME")}
	rec := e.wslRecord()
	r.KeepRunningByStagent = rec != nil
	p, err := e.wslConfigPath()
	if err != nil {
		return r
	}
	r.ConfigPath = &p
	cur, err := readOptional(p)
	if err != nil || isUTF16(cur) {
		return r
	}
	d := parseINI(cur)
	idle := int64(wslDefaultIdleTimeout)
	if i := d.lastKey("general", "instanceidletimeout"); i >= 0 {
		if v, err := strconv.ParseInt(d.lines[i].value, 10, 64); err == nil {
			idle = v
		}
	}
	mode := "nat"
	if i := d.lastKey("wsl2", "networkingmode"); i >= 0 && d.lines[i].value != "" {
		mode = strings.ToLower(d.lines[i].value)
	}
	needed := idle >= 0
	r.InstanceIdleTimeout, r.NetworkingMode, r.KeepRunningNeeded = &idle, &mode, &needed
	if rec != nil && rec.Path == p {
		r.KeepRunningByStagent = wslPresent(cur) != ""
	}
	return r
}

// wslProblem is doctor's problems line when WSL stops this distribution
// when idle.
func wslProblem(w *WSLReport) string {
	return "WSL shuts this distribution down " + strconv.FormatInt(*w.InstanceIdleTimeout, 10) +
		" ms after the last Windows terminal or SSH connection using it closes (.wslconfig instanceIdleTimeout), ending detached sessions and kept shells; run `stagent integrate --apply --wsl-keep-running` and restart WSL to keep it running"
}

// wslShutdown runs `wsl.exe --shutdown`: every distribution of the user
// stops, this one and this process included, so it normally does not
// return.
func (e *env) wslShutdown() error {
	if !e.isWSL() {
		return errors.New("not running in WSL")
	}
	out, err := e.run.Run(wslShutdownTimeout, e.windowsExe("wsl.exe"), "--shutdown")
	if err != nil {
		return errors.New("wsl.exe --shutdown: " + firstLine(decodeWSLOutput(out), err))
	}
	return nil
}

// decodeWSLOutput turns wsl.exe's UTF-16LE output into a string.
func decodeWSLOutput(s string) string {
	b := []byte(s)
	if len(b) < 2 || len(b)%2 != 0 || !strings.Contains(s, "\x00") {
		return s
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
	}
	return strings.TrimPrefix(string(utf16.Decode(u)), "\ufeff")
}

// --- .wslconfig editing ---------------------------------------------------
//
// .wslconfig is an INI file. Only the lines stagent adds, changes or removes
// are touched; every other byte is kept. Sections and keys are matched
// without regard to case.

type iniLine struct {
	raw     string // including its line terminator
	section string // lower-case name of the section the line is in
	header  bool   // the line is the section's [header]
	key     string // lower-case key of a key=value line
	value   string // its value, trimmed
}

func (l iniLine) text() string {
	return strings.TrimSuffix(strings.TrimSuffix(l.raw, "\n"), "\r")
}

type iniDoc struct {
	lines []iniLine
	eol   string
}

func parseINI(b []byte) *iniDoc {
	d := &iniDoc{eol: "\r\n"}
	for _, raw := range splitLines(b) {
		d.lines = append(d.lines, iniLine{raw: raw})
	}
	if len(d.lines) > 0 && strings.HasSuffix(d.lines[0].raw, "\n") && !strings.HasSuffix(d.lines[0].raw, "\r\n") {
		d.eol = "\n"
	}
	d.classify()
	return d
}

func (d *iniDoc) classify() {
	section := ""
	for i := range d.lines {
		l := &d.lines[i]
		t := strings.TrimSpace(strings.TrimPrefix(l.text(), "\ufeff"))
		l.header, l.key, l.value = false, "", ""
		switch {
		case t == "" || t[0] == '#' || t[0] == ';':
		case t[0] == '[' && strings.HasSuffix(t, "]"):
			section = strings.ToLower(strings.TrimSpace(t[1 : len(t)-1]))
			l.header = true
		default:
			if k, v, ok := strings.Cut(t, "="); ok {
				l.key, l.value = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
			}
		}
		l.section = section
	}
}

func (d *iniDoc) bytes() []byte {
	var sb strings.Builder
	for _, l := range d.lines {
		sb.WriteString(l.raw)
	}
	return []byte(sb.String())
}

// lastKey is the line of key in section (the last one wins); -1 if none.
func (d *iniDoc) lastKey(section, key string) int {
	at := -1
	for i, l := range d.lines {
		if l.section == section && l.key == key {
			at = i
		}
	}
	return at
}

// header is the line of section's first header; -1 if none.
func (d *iniDoc) header(section string) int {
	for i, l := range d.lines {
		if l.header && l.section == section {
			return i
		}
	}
	return -1
}

func (d *iniDoc) insert(at int, texts ...string) {
	if at > 0 && at == len(d.lines) && !strings.HasSuffix(d.lines[at-1].raw, "\n") {
		d.lines[at-1].raw += d.eol
	}
	add := make([]iniLine, len(texts))
	for i, t := range texts {
		add[i] = iniLine{raw: t + d.eol}
	}
	d.lines = append(d.lines[:at], append(add, d.lines[at:]...)...)
	d.classify()
}

func (d *iniDoc) remove(from, to int) {
	d.lines = append(d.lines[:from], d.lines[to:]...)
	d.classify()
}

// replace swaps the text of line i, keeping its terminator.
func (d *iniDoc) replace(i int, text string) {
	d.lines[i].raw = text + d.lines[i].raw[len(d.lines[i].text()):]
	d.classify()
}

func isUTF16(b []byte) bool {
	return len(b) >= 2 && (b[0] == 0xff && b[1] == 0xfe || b[0] == 0xfe && b[1] == 0xff)
}

var errWSLConfigUTF16 = unmanagedFile(".wslconfig is saved as UTF-16; stagent edits UTF-8 only")

// keepsRunning reports whether an instanceIdleTimeout value turns the
// idle shutdown off (negative).
func keepsRunning(value string) bool {
	v, err := strconv.ParseInt(value, 10, 64)
	return err == nil && v < 0
}

func wslPresent(cur []byte) string {
	if isUTF16(cur) {
		return ""
	}
	d := parseINI(cur)
	if i := d.lastKey("general", "instanceidletimeout"); i >= 0 && keepsRunning(d.lines[i].value) {
		return wslIdleLine + " in [general]"
	}
	return ""
}

// wslAdd makes [general] instanceIdleTimeout negative with one line: the
// value of an existing line is changed, else the line goes right under the
// [general] header, else [general] and the line are appended.
func wslAdd(cur []byte) (addResult, error) {
	if isUTF16(cur) {
		return addResult{}, errWSLConfigUTF16
	}
	d := parseINI(cur)
	edit := TOMLEdit{Kind: wslIdleEditKind, Action: "added"}
	const summary = "keep WSL running after the last Windows terminal or SSH connection closes (" + wslIdleLine + " under [general]; every distribution, once WSL restarts)"
	switch i, h := d.lastKey("general", "instanceidletimeout"), d.header("general"); {
	case i >= 0 && keepsRunning(d.lines[i].value):
		return addResult{after: cur}, nil
	case i >= 0:
		edit.Action, edit.PreviousLine = "changed", d.lines[i].text()
		d.replace(i, wslIdleLine)
	case h >= 0:
		d.insert(h+1, wslIdleLine)
	default:
		var texts []string
		if n := len(d.lines); n > 0 && strings.TrimSpace(d.lines[n-1].text()) != "" {
			texts = append(texts, "")
			edit.AddedBlank = true
		}
		d.insert(len(d.lines), append(texts, "[general]", wslIdleLine)...)
		edit.AddedHeader = true
	}
	return addResult{after: d.bytes(), edits: []TOMLEdit{edit}, summary: summary}, nil
}

// wslRemove undoes wslAdd as rec records it: the line stagent added goes
// (with the [general] header and blank line it added, once the section is
// empty), a line it changed gets its previous text back. A line that no
// longer turns the idle shutdown off is the user's and stays. nil deletes
// a file stagent created that has nothing else left.
func wslRemove(cur []byte, rec *ConfigEntry) ([]byte, error) {
	if isUTF16(cur) {
		return nil, errWSLConfigUTF16
	}
	d := parseINI(cur)
	i := d.lastKey("general", "instanceidletimeout")
	if i < 0 || !keepsRunning(d.lines[i].value) {
		return cur, nil
	}
	edit := TOMLEdit{Kind: wslIdleEditKind, Action: "added"}
	if rec != nil {
		for _, x := range rec.TOMLEdits {
			if x.Kind == wslIdleEditKind {
				edit = x
			}
		}
	}
	if edit.Action == "changed" && edit.PreviousLine != "" {
		d.replace(i, edit.PreviousLine)
		return d.bytes(), nil
	}
	d.remove(i, i+1)
	if edit.AddedHeader {
		h := i - 1
		for h >= 0 && !d.lines[h].header {
			h--
		}
		end := h + 1
		for end < len(d.lines) && !d.lines[end].header {
			end++
		}
		empty := h >= 0 && d.lines[h].section == "general"
		for j := h + 1; empty && j < end; j++ {
			empty = strings.TrimSpace(d.lines[j].text()) == ""
		}
		if empty {
			// Blank lines after the header separate what the user wrote
			// next; only the header and the blank line before it are ours.
			d.remove(h, h+1)
			if edit.AddedBlank && h > 0 && strings.TrimSpace(d.lines[h-1].text()) == "" {
				d.remove(h-1, h)
			}
		}
	}
	if len(d.lines) == 0 && rec != nil && rec.Created {
		return nil, nil
	}
	return d.bytes(), nil
}
