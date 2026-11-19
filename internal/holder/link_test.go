package holder

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/detect"
	"github.com/obutora/stagent/internal/wire"
)

// A daemon stopped deliberately right after it took the holder's
// registration sends daemon.stopping right behind the reply. The holder
// heeds it — it reconnects without starting a daemon — even though the
// read goroutine handles the notification while the link still returns
// from the register call.
func TestStopRightAfterRegisterIsHeeded(t *testing.T) {
	const id = "00000000000000e1"
	h := &Holder{id: id, logf: func(string, ...any) {}, det: detect.New(detect.Config{}), sess: wire.Session{ID: id}}
	h.prompt = newPromptWatch(nil, nil, func(int64) {})
	h.question = newPromptWatch(nil, nil, func(int64) {})
	t.Cleanup(h.det.Stop)

	daemonSide, holderSide := net.Pipe()
	redial := make(chan string, 1)
	first := true
	dial := func() (net.Conn, error) {
		if first {
			first = false
			return holderSide, nil
		}
		redial <- "dial (starts a daemon)"
		return nil, errors.New("no daemon")
	}
	dialExisting := func() (net.Conn, error) {
		redial <- "dialExisting"
		return nil, errors.New("no daemon")
	}
	l := newDaemonLink(h, dial, dialExisting)
	h.link = l
	go l.run()
	t.Cleanup(func() {
		l.end(0, false)
		<-l.done
	})

	// The fake daemon reads holder.register and writes its reply and
	// daemon.stopping in one go, then closes the connection.
	r := bufio.NewReader(daemonSide)
	line, err := r.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var req wire.Msg
	if err := json.Unmarshal(line, &req); err != nil || req.Method != wire.MethodHolderRegister {
		t.Fatalf("first message %s (%v), want holder.register", line, err)
	}
	reply, _ := json.Marshal(wire.Msg{ID: req.ID, Result: json.RawMessage(`{}`)})
	stopping, _ := json.Marshal(wire.Msg{Method: wire.MethodDaemonStopping})
	if _, err := daemonSide.Write(append(append(append(reply, '\n'), stopping...), '\n')); err != nil {
		t.Fatal(err)
	}
	daemonSide.Close()

	select {
	case how := <-redial:
		if how != "dialExisting" {
			t.Fatalf("the holder reconnected with %s after a deliberate stop", how)
		}
	case <-time.After(testTimeout):
		t.Fatal("the holder never reconnected")
	}
}
