package android

import (
	"context"
	"errors"
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
// writes them.
func TestRunningDirPerOS(t *testing.T) {
	home := t.TempDir()
	xdg := t.TempDir()
	local := t.TempDir()
	cases := []struct {
		name string
		h    host
		want string
	}{
		{"linux", host{goos: "linux", env: []string{"XDG_RUNTIME_DIR=" + xdg}, home: home}, filepath.Join(xdg, "avd", "running")},
		{"linux without XDG_RUNTIME_DIR", host{goos: "linux", home: home, uid: 1000}, filepath.Join("/run/user/1000", "avd", "running")},
		{"darwin", host{goos: "darwin", env: []string{"XDG_RUNTIME_DIR=" + xdg}, home: home}, filepath.Join(home, "Library", "Caches", "TemporaryItems", "avd", "running")},
		{"windows", host{goos: "windows", env: []string{"LocalAppData=" + local}, home: home}, filepath.Join(local, "Temp", "avd", "running")},
		{"windows without LOCALAPPDATA", host{goos: "windows", home: home}, filepath.Join(home, "AppData", "Local", "Temp", "avd", "running")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.h.runningDir()
			if got != c.want {
				t.Fatalf("runningDir = %q, want %q", got, c.want)
			}
			if !strings.HasPrefix(got, string(filepath.Separator)+"run") {
				writeDiscovery(t, got, "4242", discovery5554)
				defer os.RemoveAll(got)
				if e, ok := readRunning(got)["emulator-5554"]; !ok || e.grpcPort != 8554 {
					t.Fatalf("readRunning(%q) = %v", got, readRunning(got))
				}
			}
		})
	}
}

func TestReadRunningSkipsOtherFiles(t *testing.T) {
	dir := t.TempDir()
	writeDiscovery(t, dir, "1", discovery5554)
	os.WriteFile(filepath.Join(dir, "notes.ini"), []byte("port.serial=5556\ngrpc.port=1\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "1", "jwks"), 0o755)
	got := readRunning(dir)
	if len(got) != 1 {
		t.Fatalf("readRunning = %v, want emulator-5554 only", got)
	}
	if len(readRunning(filepath.Join(dir, "absent"))) != 0 {
		t.Fatal("a missing directory has no emulators")
	}
}

func TestEmulatorSerialsSkipsPhysicalDevices(t *testing.T) {
	got := emulatorSerials([]byte(adbOutput))
	if want := []string{"emulator-5556", "emulator-5554"}; !slices.Equal(got, want) {
		t.Fatalf("emulatorSerials = %q, want %q", got, want)
	}
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
	h := host{goos: "darwin", home: home}
	executable(t, filepath.Join(home, "Library", "Android", "sdk", "platform-tools"), "adb")
	writeDiscovery(t, h.runningDir(), "4242", discovery5554)
	k := &Kind{
		host: func() host { return h },
		adb:  fakeADB(adbOutput, map[string]string{"emulator-5556": "Small"}),
	}
	got, err := k.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := wire.HostScreenSection[wire.HostScreenEmulatorItem]{Items: []wire.HostScreenEmulatorItem{
		{Serial: "emulator-5554", AVD: "Pixel_9_API_36", Connectable: true},
		{Serial: "emulator-5556", AVD: "Small", Reason: wire.HostScreenNoGRPC},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List = %+v\nwant %+v", got, want)
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

func TestOpenErrors(t *testing.T) {
	home := t.TempDir()
	h := host{goos: "darwin", home: home}
	executable(t, filepath.Join(home, "Library", "Android", "sdk", "platform-tools"), "adb")
	writeDiscovery(t, h.runningDir(), "1", "port.serial=5558\navd.name=NoGRPC\n")
	k := &Kind{host: func() host { return h }, adb: fakeADB(adbOutput, nil)}
	for serial, code := range map[string]string{
		"":                    wire.ErrBadRequest,
		"emulator-5556":       wire.ErrUnavailable, // adb lists it, no discovery file
		"emulator-5558":       wire.ErrUnavailable, // a discovery file without gRPC
		"emulator-5562":       wire.ErrNotFound,
		"192.168.100.19:4228": wire.ErrNotFound,
	} {
		_, err := k.Open(context.Background(), wire.HostScreenTarget{Kind: wire.HostScreenAndroid, Serial: serial})
		var we *wire.Error
		if !errors.As(err, &we) || we.Code != code {
			t.Errorf("Open(%q) = %v, want %s", serial, err, code)
		}
	}
}
