package android

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

// adbOutput is `adb devices -l` with two emulators, one booting, a USB
// phone, a phone over Wi-Fi (ip:port and mDNS) and the server's start
// lines.
const adbOutput = "* daemon not running; starting now at tcp:5037\n" +
	"* daemon started successfully\n" +
	"List of devices attached\n" +
	"emulator-5556          offline transport_id:7\n" +
	"192.168.100.19:42283   device product:alioth_global model:M2012K11AG device:alioth transport_id:5\n" +
	"R58M12345AB            device usb:1-1 product:beyond1 model:SM_G973F device:beyond1 transport_id:3\n" +
	"adb-R58M-abc._adb-tls-connect._tcp device product:p model:m device:d transport_id:9\n" +
	"emulator-5554          device product:sdk_gphone64_arm64 model:sdk_gphone64_arm64 device:emu64a transport_id:4\n" +
	"\n"

const discovery5554 = "emulator.build=16428233\r\n" +
	"avd.id=Pixel_9_API_36\r\n" +
	"port.serial=5554\r\n" +
	"port.adb=5555\r\n" +
	"avd.name=Pixel_9_API_36\r\n" +
	"grpc.jwks=/x/jwks\r\n" +
	"cmdline=\"/sdk/emulator/qemu/qemu-system-aarch64-headless\" \"-avd\" \"Pixel_9_API_36\"\r\n" +
	"grpc.token=LkeX/c+Q==\r\n" +
	"grpc.port=8554\r\n"

// writeDiscovery advertises an emulator in dir as the emulator does.
func writeDiscovery(t *testing.T, dir, pid, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pid_"+pid+".ini"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// everyAlive is a host on which every discovery file's process runs.
func everyAlive(int) bool { return true }

// aliveOnly is a host on which only pids run.
func aliveOnly(pids ...int) func(int) bool {
	return func(pid int) bool { return slices.Contains(pids, pid) }
}

// discoveryOfAnEndedEmulator is the file an emulator of AVD motion_test
// left behind when it stopped (#679): its process is gone, the file not.
const discoveryOfAnEndedEmulator = "port.serial=5554\navd.name=motion_test\ngrpc.port=8554\n"

func TestParseDiscovery(t *testing.T) {
	e, ok := parseDiscovery([]byte(discovery5554))
	if !ok {
		t.Fatal("not parsed")
	}
	want := emulator{serialPort: 5554, avd: "Pixel_9_API_36", grpcPort: 8554, token: "LkeX/c+Q=="}
	if e != want || e.serial() != "emulator-5554" {
		t.Fatalf("got %+v, want %+v", e, want)
	}
	// Started with -port only: no gRPC keys.
	e, ok = parseDiscovery([]byte("port.serial=5560\navd.name=Small\n"))
	if !ok || e.grpcPort != 0 || e.avd != "Small" {
		t.Fatalf("without gRPC: %+v %v", e, ok)
	}
	if _, ok := parseDiscovery([]byte("avd.name=X\ngrpc.port=8554\n")); ok {
		t.Fatal("a file without port.serial matches no serial")
	}
}

// TestRunningDirPerOS reads the discovery files where each OS's emulator
// writes them, and only those whose emulator process still runs.
func TestRunningDirPerOS(t *testing.T) {
	home := t.TempDir()
	xdg := t.TempDir()
	local := t.TempDir()
	emuHome := t.TempDir()
	prefs := t.TempDir()
	os.Mkdir(filepath.Join(prefs, ".android"), 0o755)
	sdkHome := t.TempDir()
	cases := []struct {
		name string
		h    host
		want []string // the emulator writes in the last
	}{
		{"linux", host{goos: "linux", env: []string{"XDG_RUNTIME_DIR=" + xdg}, home: home, alive: aliveOnly(4242)}, []string{filepath.Join(xdg, "avd", "running")}},
		// The emulator's fallback (#705): /run/user/<uid> when it exists,
		// else its user directory. No user has uid 99999 here.
		{"linux without XDG_RUNTIME_DIR", host{goos: "linux", home: home, uid: 99999, alive: aliveOnly(4242)},
			[]string{filepath.Join("/run/user/99999", "avd", "running"), filepath.Join(home, ".android", "avd", "running")}},
		{"linux without XDG_RUNTIME_DIR, ANDROID_EMULATOR_HOME", host{goos: "linux", env: []string{"ANDROID_PREFS_ROOT=" + prefs, "ANDROID_EMULATOR_HOME=" + emuHome}, home: home, uid: 99999, alive: aliveOnly(4242)},
			[]string{filepath.Join("/run/user/99999", "avd", "running"), filepath.Join(emuHome, "avd", "running")}},
		{"linux without XDG_RUNTIME_DIR, ANDROID_PREFS_ROOT with .android", host{goos: "linux", env: []string{"ANDROID_PREFS_ROOT=" + prefs, "ANDROID_SDK_HOME=" + sdkHome}, home: home, uid: 99999, alive: aliveOnly(4242)},
			[]string{filepath.Join("/run/user/99999", "avd", "running"), filepath.Join(prefs, ".android", "avd", "running")}},
		{"linux without XDG_RUNTIME_DIR, ANDROID_SDK_HOME without .android", host{goos: "linux", env: []string{"ANDROID_SDK_HOME=" + sdkHome}, home: home, uid: 99999, alive: aliveOnly(4242)},
			[]string{filepath.Join("/run/user/99999", "avd", "running"), filepath.Join(sdkHome, "avd", "running")}},
		{"darwin", host{goos: "darwin", env: []string{"XDG_RUNTIME_DIR=" + xdg}, home: home, alive: aliveOnly(4242)}, []string{filepath.Join(home, "Library", "Caches", "TemporaryItems", "avd", "running")}},
		{"windows", host{goos: "windows", env: []string{"LocalAppData=" + local}, home: home, alive: aliveOnly(4242)}, []string{filepath.Join(local, "Temp", "avd", "running")}},
		{"windows without LOCALAPPDATA", host{goos: "windows", home: home, alive: aliveOnly(4242)}, []string{filepath.Join(home, "AppData", "Local", "Temp", "avd", "running")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.h.runningDirs()
			if !slices.Equal(got, c.want) {
				t.Fatalf("runningDirs = %q, want %q", got, c.want)
			}
			dir := got[len(got)-1]
			writeDiscovery(t, dir, "4242", discovery5554)
			// Sorted after pid_4242.ini: read, it would win the serial.
			writeDiscovery(t, dir, "9128004", "port.serial=5554\navd.name=motion_test\ngrpc.port=8556\n")
			writeDiscovery(t, dir, "3128004", "port.serial=5556\navd.name=motion_test\ngrpc.port=8558\n")
			defer os.RemoveAll(dir)
			running := c.h.readRunning()
			if e, ok := running["emulator-5554"]; !ok || e.grpcPort != 8554 || len(running) != 1 {
				t.Fatalf("readRunning in %q = %v, want emulator-5554 of pid 4242 only", dir, running)
			}
		})
	}
}

func TestReadRunningSkipsOtherFiles(t *testing.T) {
	h := host{goos: "linux", env: []string{"XDG_RUNTIME_DIR=" + t.TempDir()}, alive: everyAlive}
	dir := h.runningDirs()[0]
	if len(h.readRunning()) != 0 {
		t.Fatal("a missing directory has no emulators")
	}
	writeDiscovery(t, dir, "1", discovery5554)
	writeDiscovery(t, dir, "x", "port.serial=5558\ngrpc.port=1\n") // no pid to tell it runs
	os.WriteFile(filepath.Join(dir, "notes.ini"), []byte("port.serial=5556\ngrpc.port=1\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "1", "jwks"), 0o755)
	got := h.readRunning()
	if len(got) != 1 {
		t.Fatalf("readRunning = %v, want emulator-5554 only", got)
	}
}

func TestEmulatorSerialsSkipsPhysicalDevices(t *testing.T) {
	running := map[string]emulator{"emulator-5556": {serialPort: 5556}}
	got := emulatorSerials([]byte(adbOutput), running)
	if want := []string{"emulator-5556", "emulator-5554"}; !slices.Equal(got, want) {
		t.Fatalf("emulatorSerials = %q, want %q", got, want)
	}
}

// adb probes the odd ports 5555-5585 and takes any process listening there
// for emulator-<port-1>; offline and without a discovery file, it is not an
// emulator. One in the device state without a file (-port only) still is.
func TestEmulatorSerialsSkipsOfflineWithoutDiscovery(t *testing.T) {
	out := "List of devices attached\n" +
		"emulator-5562          offline transport_id:2\n" +
		"emulator-5556          offline transport_id:7\n" +
		"emulator-5560          device product:p model:m device:d transport_id:3\n"
	got := emulatorSerials([]byte(out), map[string]emulator{"emulator-5556": {serialPort: 5556}})
	if want := []string{"emulator-5556", "emulator-5560"}; !slices.Equal(got, want) {
		t.Fatalf("emulatorSerials = %q, want %q", got, want)
	}
}

func TestIsEmulatorSerial(t *testing.T) {
	for _, s := range []string{"emulator-", "emulator-x", "emulator-05554", "emulator-5554:5555"} {
		if isEmulatorSerial(s) {
			t.Errorf("%q is not an emulator serial", s)
		}
	}
}

func TestItemsMatchesBySerial(t *testing.T) {
	running := map[string]emulator{
		"emulator-5554": {serialPort: 5554, avd: "Pixel_9_API_36", grpcPort: 8554, token: "t"},
		"emulator-5556": {serialPort: 5556, avd: "Small"}, // a file without gRPC
		"emulator-5558": {serialPort: 5558, avd: "Gone", grpcPort: 8556},
	}
	got := items([]string{"emulator-5560", "emulator-5556", "emulator-5554", "emulator-5554"}, running)
	want := []wire.HostScreenEmulatorItem{
		{Serial: "emulator-5554", AVD: "Pixel_9_API_36", Connectable: true},
		{Serial: "emulator-5556", AVD: "Small", Reason: wire.HostScreenNoGRPC},
		{Serial: "emulator-5560", Reason: wire.HostScreenNoGRPC},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("items = %+v\nwant %+v", got, want)
	}
}

func TestAVDName(t *testing.T) {
	for out, want := range map[string]string{
		"Pixel_9_API_36\r\nOK\r\n": "Pixel_9_API_36",
		"OK\r\n":                   "",
		"KO: unknown command\r\n":  "",
		"":                         "",
	} {
		if got := avdName([]byte(out)); got != want {
			t.Errorf("avdName(%q) = %q, want %q", out, got, want)
		}
	}
}

// executable makes an empty executable file named name in dir.
func executable(t *testing.T, dir, name string) string {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestADBPath(t *testing.T) {
	adb := "adb"
	if runtime.GOOS == "windows" {
		adb = "adb.exe"
	}
	t.Run("PATH first", func(t *testing.T) {
		home, bin, sdk := t.TempDir(), t.TempDir(), t.TempDir()
		want := executable(t, bin, adb)
		executable(t, filepath.Join(sdk, "platform-tools"), adb)
		h := host{goos: runtime.GOOS, home: home, env: []string{
			"PATH=relative" + string(os.PathListSeparator) + bin, "ANDROID_HOME=" + sdk,
		}}
		if got := h.adbPath(); got != want {
			t.Fatalf("adbPath = %q, want %q", got, want)
		}
	})
	for _, key := range []string{"ANDROID_HOME", "ANDROID_SDK_ROOT"} {
		t.Run(key, func(t *testing.T) {
			home, sdk := t.TempDir(), t.TempDir()
			want := executable(t, filepath.Join(sdk, "platform-tools"), adb)
			h := host{goos: runtime.GOOS, home: home, env: []string{"PATH=" + t.TempDir(), key + "=" + sdk}}
			if got := h.adbPath(); got != want {
				t.Fatalf("adbPath = %q, want %q", got, want)
			}
		})
	}
	defaults := map[string]func(home, local string) string{
		"darwin":  func(home, _ string) string { return filepath.Join(home, "Library", "Android", "sdk") },
		"linux":   func(home, _ string) string { return filepath.Join(home, "Android", "Sdk") },
		"windows": func(_, local string) string { return filepath.Join(local, "Android", "Sdk") },
	}
	for goos, sdk := range defaults {
		t.Run("default SDK on "+goos, func(t *testing.T) {
			home, local := t.TempDir(), t.TempDir()
			name := "adb"
			if goos == "windows" {
				name = "adb.exe"
			}
			h := host{goos: goos, home: home, env: []string{"LOCALAPPDATA=" + local}}
			if got := h.adbPath(); got != "" {
				t.Fatalf("adbPath without adb = %q", got)
			}
			want := executable(t, filepath.Join(sdk(home, local), "platform-tools"), name)
			if got := h.adbPath(); got != want {
				t.Fatalf("adbPath = %q, want %q", got, want)
			}
		})
	}
}

// fakeADB answers `adb devices -l` with devices and `emu avd name` with
// names.
func fakeADB(devices string, names map[string]string) func(context.Context, string, []string, ...string) ([]byte, error) {
	return func(_ context.Context, _ string, _ []string, args ...string) ([]byte, error) {
		switch {
		case slices.Equal(args, []string{"devices", "-l"}):
			return []byte(devices), nil
		case len(args) == 5 && args[0] == "-s" && args[2] == "emu":
			if n, ok := names[args[1]]; ok {
				return []byte(n + "\r\nOK\r\n"), nil
			}
			return []byte("KO: no such device\r\n"), nil
		}
		return nil, errors.New("unexpected adb " + strings.Join(args, " "))
	}
}

func TestListSection(t *testing.T) {
	home := t.TempDir()
	h := host{goos: "darwin", home: home, alive: everyAlive}
	executable(t, filepath.Join(home, "Library", "Android", "sdk", "platform-tools"), "adb")
	writeDiscovery(t, h.runningDirs()[0], "4242", discovery5554)
	k := &Kind{
		host: func() host { return h },
		adb:  fakeADB(adbOutput, nil),
	}
	got, err := k.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := wire.HostScreenSection[wire.HostScreenEmulatorItem]{Items: []wire.HostScreenEmulatorItem{
		{Serial: "emulator-5554", AVD: "Pixel_9_API_36", Connectable: true},
		// emulator-5556 is offline without a discovery file: not listed.
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List = %+v\nwant %+v", got, want)
	}
}

// On WSL the section says the Windows side is not searched, without adb
// or with it.
func TestListNotesWSL(t *testing.T) {
	home := t.TempDir()
	h := host{goos: "linux", home: home, wsl: true, alive: everyAlive}
	k := &Kind{host: func() host { return h }, adb: fakeADB(adbOutput, nil)}
	for _, withADB := range []bool{false, true} {
		if withADB {
			executable(t, filepath.Join(home, "Android", "Sdk", "platform-tools"), "adb")
		}
		got, err := k.List(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		n := got.(wire.HostScreenSection[wire.HostScreenEmulatorItem]).Note
		if n == nil || n.Code != wire.NoteWSL {
			t.Errorf("adb %v: note = %+v, want %s", withADB, n, wire.NoteWSL)
		}
	}
}

// An offline emulator-<port> without a discovery file is not listed, and
// its console is not asked for a name (that would hold list up).
func TestListSkipsOfflineWithoutDiscovery(t *testing.T) {
	home := t.TempDir()
	h := host{goos: "darwin", home: home, alive: everyAlive}
	executable(t, filepath.Join(home, "Library", "Android", "sdk", "platform-tools"), "adb")
	writeDiscovery(t, h.runningDirs()[0], "4242", discovery5554)
	writeDiscovery(t, h.runningDirs()[0], "4243", "port.serial=5558\navd.name=Booting\n")
	devices := "List of devices attached\n" +
		"emulator-5562          offline transport_id:2\n" +
		"emulator-5558          offline transport_id:6\n" +
		"emulator-5560          device product:p model:m device:d transport_id:3\n" +
		"emulator-5554          device product:p model:m device:d transport_id:4\n"
	fake := fakeADB(devices, map[string]string{"emulator-5560": "PortOnly"})
	var asked []string
	k := &Kind{
		host: func() host { return h },
		adb: func(ctx context.Context, adb string, env []string, args ...string) ([]byte, error) {
			if len(args) > 2 && args[0] == "-s" {
				asked = append(asked, args[1])
			}
			return fake(ctx, adb, env, args...)
		},
	}
	got, err := k.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := wire.HostScreenSection[wire.HostScreenEmulatorItem]{Items: []wire.HostScreenEmulatorItem{
		{Serial: "emulator-5554", AVD: "Pixel_9_API_36", Connectable: true},
		{Serial: "emulator-5558", AVD: "Booting", Reason: wire.HostScreenNoGRPC},
		{Serial: "emulator-5560", AVD: "PortOnly", Reason: wire.HostScreenNoGRPC},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List = %+v\nwant %+v", got, want)
	}
	if slices.Contains(asked, "emulator-5562") {
		t.Fatalf("asked the console of emulator-5562: %q", asked)
	}
}

func TestListWithoutADB(t *testing.T) {
	k := &Kind{
		host: func() host { return host{goos: "linux", home: t.TempDir(), env: []string{"PATH=" + t.TempDir()}} },
		adb:  fakeADB(adbOutput, nil),
	}
	got, err := k.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sec := got.(wire.HostScreenSection[wire.HostScreenEmulatorItem])
	if sec.Missing == nil || sec.Missing.Code != wire.HostScreenADBNotFound || sec.Items == nil || len(sec.Items) != 0 {
		t.Fatalf("List without adb = %+v", sec)
	}
}

// #679: an emulator started with -port only, on the serial of an ended
// emulator's discovery file, has no gRPC. The file's gRPC port is not
// tried.
func TestListShowsAnEmulatorWithAnEndedEmulatorsFileAsNoGRPC(t *testing.T) {
	home := t.TempDir()
	h := host{goos: "linux", env: []string{"XDG_RUNTIME_DIR=" + t.TempDir()}, home: home, alive: aliveOnly()}
	executable(t, filepath.Join(home, "Android", "Sdk", "platform-tools"), "adb")
	writeDiscovery(t, h.runningDirs()[0], "3128004", discoveryOfAnEndedEmulator)
	devices := "List of devices attached\n" +
		"emulator-5554          device product:p model:m device:d transport_id:1\n"
	k := &Kind{host: func() host { return h }, adb: fakeADB(devices, map[string]string{"emulator-5554": "motion_test"})}
	got, err := k.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := wire.HostScreenSection[wire.HostScreenEmulatorItem]{Items: []wire.HostScreenEmulatorItem{
		{Serial: "emulator-5554", AVD: "motion_test", Reason: wire.HostScreenNoGRPC},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List = %+v\nwant %+v", got, want)
	}
}

func TestOpenErrors(t *testing.T) {
	home := t.TempDir()
	h := host{goos: "darwin", home: home, alive: aliveOnly(1, 2, 3)}
	executable(t, filepath.Join(home, "Library", "Android", "sdk", "platform-tools"), "adb")
	writeDiscovery(t, h.runningDirs()[0], "1", "port.serial=5558\navd.name=NoGRPC\n")
	advertise(t, h, "2", 5560, "Silent", closedPort(t))
	writeDiscovery(t, h.runningDirs()[0], "3", fmt.Sprintf("port.serial=5564\ngrpc.port=%d\n", closedPort(t))) // no avd.name
	writeDiscovery(t, h.runningDirs()[0], "3128004", discoveryOfAnEndedEmulator)
	devices := adbOutput +
		"emulator-5560          device product:p model:m device:d transport_id:8\n" +
		"emulator-5564          device product:p model:m device:d transport_id:10\n"
	k := &Kind{host: func() host { return h }, adb: fakeADB(devices, nil)}
	for _, c := range []struct {
		target wire.HostScreenTarget
		code   string
		noGRPC bool
	}{
		{wire.HostScreenTarget{}, wire.ErrBadRequest, false},
		{wire.HostScreenTarget{Serial: "emulator-5556"}, wire.ErrNotFound, false},   // offline, no discovery file: not an emulator
		{wire.HostScreenTarget{Serial: "emulator-5554"}, wire.ErrUnavailable, true}, // adb lists it (device), no discovery file of a running emulator
		{wire.HostScreenTarget{Serial: "emulator-5554", AVD: "motion_test"}, wire.ErrUnavailable, true},
		{wire.HostScreenTarget{Serial: "emulator-5558"}, wire.ErrUnavailable, true},                // a discovery file without gRPC
		{wire.HostScreenTarget{Serial: "emulator-5560", AVD: "Silent"}, wire.ErrUnavailable, true}, // adb lists it, its gRPC does not answer
		{wire.HostScreenTarget{Serial: "emulator-5564"}, wire.ErrUnavailable, true},                // the same, without an AVD to follow
		{wire.HostScreenTarget{Serial: "emulator-5562"}, wire.ErrNotFound, false},
		{wire.HostScreenTarget{Serial: "192.168.100.19:4228"}, wire.ErrNotFound, false},
	} {
		c.target.Kind = wire.HostScreenAndroid
		_, err := k.Open(context.Background(), c.target)
		var we *wire.Error
		if !errors.As(err, &we) || we.Code != c.code || c.noGRPC != strings.Contains(we.Message, wire.HostScreenNoGRPC) {
			t.Errorf("Open(%+v) = %v, want %s (no_grpc %v)", c.target, err, c.code, c.noGRPC)
		}
	}
}
