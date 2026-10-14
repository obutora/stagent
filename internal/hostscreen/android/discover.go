package android

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/obutora/stagent/internal/hostscreen"
	"github.com/obutora/stagent/internal/proc"
	"github.com/obutora/stagent/internal/wire"
)

// host is what discovery reads of the host: the OS, the environment the
// user's sessions get, the home directory, whether it is WSL and which
// processes run. Replaceable in tests, so every OS's locations are checked
// on any.
type host struct {
	goos  string
	env   []string
	home  string
	uid   int            // for /run/user/<uid> on Linux
	wsl   bool           // the Windows side's emulators are not seen (hostscreen.WSLNote)
	alive func(int) bool // whether a process runs: proc.Alive (signal 0; a process handle on Windows)
}

func (h host) getenv(key string) string {
	v := ""
	for _, kv := range h.env {
		k, val, ok := strings.Cut(kv, "=")
		if ok && (k == key || h.goos == "windows" && strings.EqualFold(k, key)) {
			v = val
		}
	}
	return v
}

// adbPath finds adb: on the PATH of the user's environment, then in
// $ANDROID_HOME and $ANDROID_SDK_ROOT, then in the SDK location Android
// Studio installs to on each OS. "" when there is none.
func (h host) adbPath() string {
	name, sep := "adb", ":"
	if h.goos == "windows" {
		name, sep = "adb.exe", ";"
	}
	var dirs []string
	if p := h.getenv("PATH"); p != "" {
		dirs = append(dirs, strings.Split(p, sep)...)
	}
	for _, sdk := range h.sdkRoots() {
		dirs = append(dirs, filepath.Join(sdk, "platform-tools"))
	}
	for _, d := range dirs {
		if d == "" || !filepath.IsAbs(d) {
			continue
		}
		p := filepath.Join(d, name)
		st, err := os.Stat(p)
		// The executable bit is the real file system's (none on Windows).
		if err == nil && !st.IsDir() && (runtime.GOOS == "windows" || st.Mode()&0o111 != 0) {
			return p
		}
	}
	return ""
}

// sdkRoots are the Android SDK directories to look for adb in, after PATH.
func (h host) sdkRoots() []string {
	var roots []string
	for _, k := range []string{"ANDROID_HOME", "ANDROID_SDK_ROOT"} {
		if v := h.getenv(k); v != "" {
			roots = append(roots, v)
		}
	}
	switch h.goos {
	case "darwin":
		roots = append(roots, filepath.Join(h.home, "Library", "Android", "sdk"))
	case "windows":
		roots = append(roots, filepath.Join(h.localAppData(), "Android", "Sdk"))
	default:
		roots = append(roots, filepath.Join(h.home, "Android", "Sdk"))
	}
	return roots
}

func (h host) localAppData() string {
	if v := h.getenv("LOCALAPPDATA"); v != "" {
		return v
	}
	return filepath.Join(h.home, "AppData", "Local")
}

// runningDirs are where a running emulator advertises itself
// (avd/running/pid_<pid>.ini), as the emulator picks it
// (ConfigDirs::getDiscoveryDirectory): under $XDG_RUNTIME_DIR on Linux,
// ~/Library/Caches/TemporaryItems on macOS and %LOCALAPPDATA%\Temp on
// Windows. On Linux without $XDG_RUNTIME_DIR (a session without logind,
// su) the emulator takes /run/user/<uid> when it exists, else its user
// directory (emulatorUserDir): both are read (#705).
func (h host) runningDirs() []string {
	var roots []string
	switch h.goos {
	case "darwin":
		roots = []string{filepath.Join(h.home, "Library", "Caches", "TemporaryItems")}
	case "windows":
		roots = []string{filepath.Join(h.localAppData(), "Temp")}
	default:
		if x := h.getenv("XDG_RUNTIME_DIR"); x != "" {
			roots = []string{x}
		} else {
			roots = []string{filepath.Join("/run/user", strconv.Itoa(h.uid)), h.emulatorUserDir()}
		}
	}
	dirs := make([]string, len(roots))
	for i, r := range roots {
		dirs[i] = filepath.Join(r, "avd", "running")
	}
	return dirs
}

// emulatorUserDir is the emulator's user directory
// (ConfigDirs::getUserDirectory): $ANDROID_EMULATOR_HOME, else
// $ANDROID_PREFS_ROOT or the older $ANDROID_SDK_HOME (its .android when
// that is a directory), else ~/.android.
func (h host) emulatorUserDir() string {
	if v := h.getenv("ANDROID_EMULATOR_HOME"); v != "" {
		return v
	}
	for _, k := range []string{"ANDROID_PREFS_ROOT", "ANDROID_SDK_HOME"} {
		if v := h.getenv(k); v != "" {
			if st, err := os.Stat(filepath.Join(v, ".android")); err == nil && st.IsDir() {
				return filepath.Join(v, ".android")
			}
			return v
		}
	}
	return filepath.Join(h.home, ".android")
}

// emulator is one discovery file: the emulator's console port (its adb
// serial is emulator-<port>), AVD name and gRPC endpoint, if any.
type emulator struct {
	serialPort int
	avd        string
	grpcPort   int    // 0: no gRPC
	token      string // Bearer token of the gRPC port, if it asks for one
}

func (e emulator) serial() string { return "emulator-" + strconv.Itoa(e.serialPort) }

// parseDiscovery reads one pid_<pid>.ini (key=value lines).
func parseDiscovery(data []byte) (emulator, bool) {
	var e emulator
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimRight(sc.Text(), "\r"), "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "port.serial":
			e.serialPort, _ = strconv.Atoi(strings.TrimSpace(v))
		case "avd.name":
			e.avd = strings.TrimSpace(v)
		case "grpc.port":
			e.grpcPort, _ = strconv.Atoi(strings.TrimSpace(v))
		case "grpc.token":
			e.token = strings.TrimSpace(v)
		}
	}
	if e.grpcPort < 1 || e.grpcPort > 65535 {
		e.grpcPort = 0
	}
	return e, e.serialPort > 0
}

// readRunning reads the discovery files of the running dirs by serial. A
// missing dir means no emulator advertises itself there. An emulator that
// stops leaves its pid_<pid>.ini behind: a file whose pid does not run
// is left as it is and not read (#679).
func (h host) readRunning() map[string]emulator {
	out := map[string]emulator{}
	for _, dir := range h.runningDirs() {
		ents, _ := os.ReadDir(dir)
		for _, ent := range ents {
			name := ent.Name()
			if ent.IsDir() || !strings.HasPrefix(name, "pid_") || !strings.HasSuffix(name, ".ini") {
				continue
			}
			if pid, err := strconv.Atoi(name[len("pid_") : len(name)-len(".ini")]); err != nil || !h.alive(pid) {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				continue
			}
			if e, ok := parseDiscovery(data); ok {
				out[e.serial()] = e
			}
		}
	}
	return out
}

// emulatorSerials returns the emulators of `adb devices -l` output, in
// its order. Physical devices (USB serials, ip:port, mDNS names) are not
// emulators. adb also takes any process listening on an odd port of
// 5555-5585 for emulator-<port-1>, offline: one offline without a
// discovery file in running is not an emulator. Offline with a file
// (booting) and device without one (-port only) are.
func emulatorSerials(out []byte, running map[string]emulator) []string {
	var serials []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 || !isEmulatorSerial(f[0]) {
			continue
		}
		if _, ok := running[f[0]]; !ok && f[1] == "offline" {
			continue
		}
		serials = append(serials, f[0])
	}
	return serials
}

func isEmulatorSerial(s string) bool {
	n, ok := strings.CutPrefix(s, "emulator-")
	if !ok {
		return false
	}
	p, err := strconv.Atoi(n)
	return err == nil && p > 0 && strconv.Itoa(p) == n
}

// items matches adb's emulators with the discovery files by serial; one
// without a file or without a gRPC port cannot be shown (no_grpc). Items
// are ordered by console port.
func items(serials []string, running map[string]emulator) []wire.HostScreenEmulatorItem {
	out := make([]wire.HostScreenEmulatorItem, 0, len(serials))
	for _, s := range serials {
		if slices.ContainsFunc(out, func(it wire.HostScreenEmulatorItem) bool { return it.Serial == s }) {
			continue
		}
		e, ok := running[s]
		it := wire.HostScreenEmulatorItem{Serial: s, AVD: e.avd, Connectable: ok && e.grpcPort > 0}
		if !it.Connectable {
			it.Reason = wire.HostScreenNoGRPC
		}
		out = append(out, it)
	}
	slices.SortStableFunc(out, func(a, b wire.HostScreenEmulatorItem) int {
		return consolePort(a.Serial) - consolePort(b.Serial)
	})
	return out
}

func consolePort(serial string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(serial, "emulator-"))
	return n
}

// avdName reads the answer of `adb -s <serial> emu avd name`: the name,
// then OK (or KO and an error). "" when it has none.
func avdName(out []byte) string {
	line, _, _ := strings.Cut(string(out), "\n")
	line = strings.TrimSpace(line)
	if line == "OK" || strings.HasPrefix(line, "KO") {
		return ""
	}
	return line
}

// thisHost is the host stagent runs on, with env as its environment.
func thisHost(env []string) host {
	home, _ := os.UserHomeDir()
	return host{goos: runtime.GOOS, env: env, home: home, uid: os.Getuid(), wsl: hostscreen.IsWSL(), alive: proc.Alive}
}
