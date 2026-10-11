package ios

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/obutora/stagent/internal/hostscreen/ios/idbpb"
)

const (
	// idleShutdown is the companion's --idle-shutdown-time in seconds: it
	// ends by itself that long after its last request closed, should
	// stagent die without stopping it (#630: a companion outlives its
	// parent).
	idleShutdown = 5
	// stopWait is how long a stopped companion may take to exit after
	// SIGTERM before it is killed.
	stopWait = 2 * time.Second
	// stderrTail bounds the companion output kept for error messages.
	stderrTail = 512
)

// companionArgs are idb_companion's arguments to serve simulator udid on
// the Unix socket sock only (no TCP port): --only simulator skips the
// device discovery (1.7 s, #630) and --terminate-offline ends it when the
// simulator shuts down, which ends the stream.
func companionArgs(udid, sock string) []string {
	return []string{
		"--udid", udid,
		"--only", "simulator",
		"--grpc-domain-sock", sock,
		"--idle-shutdown-time", strconv.Itoa(idleShutdown),
		"--terminate-offline", "true",
		"--log-level", "info",
	}
}

// companion is one idb_companion process serving one simulator to one
// stream, and the gRPC connection to it.
type companion struct {
	cmd    *exec.Cmd
	dir    string // private directory of the socket
	conn   *grpc.ClientConn
	client idbpb.CompanionServiceClient
	exited chan struct{} // closed once the process was waited for
	stderr *tail
	once   sync.Once
}

// startCompanion runs the idb_companion at path for simulator udid, with
// its socket in a new directory only this user can enter, and connects to
// it. The companion is not known to answer yet (see describe).
func startCompanion(path string, env []string, udid string) (*companion, error) {
	dir, err := os.MkdirTemp("", "stagent-ios-")
	if err != nil {
		return nil, err
	}
	sock := filepath.Join(dir, "c.sock")
	cmd := exec.Command(path, companionArgs(udid, sock)...)
	cmd.Env = env
	c := &companion{cmd: cmd, dir: dir, exited: make(chan struct{}), stderr: &tail{max: stderrTail}}
	cmd.Stdout = c.stderr
	cmd.Stderr = c.stderr
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	go func() {
		cmd.Wait()
		close(c.exited)
	}()
	c.conn, err = grpc.NewClient("unix:"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		c.stop()
		return nil, err
	}
	c.client = idbpb.NewCompanionServiceClient(c.conn)
	return c, nil
}

// describe waits until the companion answers (it takes about a second to
// load the simulator frameworks) and describes its simulator. It fails at
// once when the companion exits first.
func (c *companion) describe(ctx context.Context) (*idbpb.TargetDescription, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go func() {
		select {
		case <-c.exited:
			cancel(fmt.Errorf("idb_companion exited: %s", c.stderr.String()))
		case <-ctx.Done():
		}
	}()
	r, err := c.client.Describe(ctx, &idbpb.TargetDescriptionRequest{}, grpc.WaitForReady(true))
	if err != nil {
		if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
			return nil, cause
		}
		return nil, err
	}
	return r.GetTargetDescription(), nil
}

// stop closes the connection, ends the process (SIGTERM, then a kill
// after stopWait), waits for it and removes the socket's directory. Safe
// to call more than once.
func (c *companion) stop() {
	c.once.Do(func() {
		if c.conn != nil {
			c.conn.Close()
		}
		// SIGTERM lets it shut its server down; Windows (where it never
		// runs) only knows a kill.
		if c.cmd.Process.Signal(syscall.SIGTERM) != nil {
			c.cmd.Process.Kill()
		}
		select {
		case <-c.exited:
		case <-time.After(stopWait):
			c.cmd.Process.Kill()
			<-c.exited
		}
		os.RemoveAll(c.dir)
	})
}

// tail keeps the last max bytes written to it.
type tail struct {
	max int

	mu  sync.Mutex
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}
