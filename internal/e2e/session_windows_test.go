package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/version"
	"github.com/obutora/stagent/internal/wire"
)

// A test binary started with this variable plays Win32-OpenSSH's session
// process (sshd -z) for one exec channel: it creates the pipes `stagent
// bridge` runs on, as sshd -z does.
const envFakeSessionStagent = "STAGENT_E2E_FAKE_SESSION_STAGENT"

func TestMain(m *testing.M) {
	if stagent := os.Getenv(envFakeSessionStagent); stagent != "" {
		os.Exit(fakeSession(stagent))
	}
	os.Exit(m.Run())
}

// fakeSession runs the bridge on a stdin whose write end the bridge itself
// also inherits, so that stdin never reaches EOF however the session ends,
// as when the write end outlives sshd -z in another process. It answers
// one hello through the bridge, prints the bridge's pid and runs until its
// own stdin closes or it is killed.
func fakeSession(stagent string) int {
	fail := func(err error) int {
		fmt.Fprintln(os.Stderr, "fake session:", err)
		return 1
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	leaked := windows.Handle(inW.Fd())
	if err := windows.SetHandleInformation(leaked, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
		return fail(err)
	}
	bridge := exec.Command(stagent, "bridge")
	bridge.Stdin, bridge.Stdout = inR, outW
	bridge.SysProcAttr = &syscall.SysProcAttr{AdditionalInheritedHandles: []syscall.Handle{syscall.Handle(leaked)}}
	if err := bridge.Start(); err != nil {
		return fail(err)
	}
	inR.Close()
	outW.Close()

	params, _ := json.Marshal(wire.HelloParams{Protocol: version.Protocol, Client: "e2e"})
	fmt.Fprintf(inW, `{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`+"\n", wire.MethodHello, params)
	line, err := bufio.NewReader(outR).ReadString('\n')
	if err != nil {
		return fail(fmt.Errorf("hello: %w", err))
	}
	if !strings.Contains(line, `"result"`) {
		return fail(fmt.Errorf("hello answered %s", line))
	}
	fmt.Println(bridge.Process.Pid)
	io.Copy(io.Discard, os.Stdin)
	return 0
}

// The bridge ends when the SSH session process that runs it dies, even
// though its stdin never reaches EOF (#270): killing sshd -z left bridges
// running for hours.
func TestWindowsBridgeEndsWithItsSessionProcess(t *testing.T) {
	bin := compileStagent(t)
	t.Setenv(paths.EnvHome, t.TempDir())
	session := exec.Command(os.Args[0])
	session.Env = append(os.Environ(), envFakeSessionStagent+"="+bin)
	if _, err := session.StdinPipe(); err != nil { // keeps it running until killed
		t.Fatal(err)
	}
	out, err := session.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	session.Stderr = &stderr
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		session.Process.Kill()
		session.Wait()
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	pid, perr := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || perr != nil {
		session.Wait()
		t.Fatalf("fake session did not start the bridge (%q, %v):\n%s", line, err, stderr.String())
	}
	bridge, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bridge.Kill() })
	exited := make(chan struct{})
	go func() {
		bridge.Wait()
		close(exited)
	}()

	select {
	case <-exited:
		t.Fatalf("the bridge ended while its session process runs:\n%s", stderr.String())
	case <-time.After(500 * time.Millisecond):
	}
	session.Process.Kill()
	session.Wait()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the bridge still runs 5 s after its session process died")
	}
}
