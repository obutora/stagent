package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/obutora/stagent/internal/relay"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

const relayID = "0123456789abcdef0123456789abcdef"

// Every relay method is refused to a coding agent's process tree before
// anything happens (ADR 0004); relay.record is the daemon's to refuse.
func TestRelayMethodsRefusedToAgents(t *testing.T) {
	l, a := startBridge(t, func(b *Bridge) {
		b.agentRefusal = func() *wire.Error { return wire.Errorf(wire.ErrAgentRefused, "from claude") }
	})
	for method, params := range map[string]any{
		wire.MethodRelayPrepare: wire.RelayPrepareParams{Harness: wire.HarnessClaude, ConversationID: "c", Target: wire.RelayTargetOtherHost, SourceAgent: wire.RelaySourceKeep, SourceHost: wire.RelayHost{Label: "mac"}},
		wire.MethodRelayRead:    wire.RelayReadParams{RelayID: relayID},
		wire.MethodRelayReceive: wire.RelayReceiveParams{RelayID: relayID, Size: 1, SHA256: hex.EncodeToString(make([]byte, 32))},
		wire.MethodRelayWrite:   wire.RelayWriteParams{RelayID: relayID, Data: []byte("x")},
		wire.MethodRelayStart:   wire.RelayStartParams{RelayID: relayID, Harness: wire.HarnessClaude, TargetHost: wire.RelayHost{Label: "pc"}},
		wire.MethodRelayAbort:   wire.RelayRef{RelayID: relayID},
		wire.MethodRelayDiscard: wire.RelayRef{RelayID: relayID},
	} {
		if werr := a.call(t, method, params, nil); werr == nil || werr.Code != wire.ErrAgentRefused {
			t.Errorf("%s from an agent = %v, want agent_refused", method, werr)
		}
	}
	if _, err := os.Stat(relay.InDir(l, relayID)); err == nil {
		t.Error("a refused relay.receive made the relay's directory")
	}
}

// A package is taken in chunks, each at the bytes received so far; a
// dropped connection's relay.receive goes on from there, another package
// starts again, and nothing may pass the size or come out of order.
func TestRelayReceiveAndWrite(t *testing.T) {
	l, a := startBridge(t)
	data := bytes.Repeat([]byte("package!"), 100)
	sum := sha256.Sum256(data)
	recv := wire.RelayReceiveParams{RelayID: relayID, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	var rr wire.RelayReceiveResult
	if werr := a.call(t, wire.MethodRelayReceive, recv, &rr); werr != nil || rr.Received != 0 {
		t.Fatalf("relay.receive = %+v, %v", rr, werr)
	}
	var wr wire.RelayWriteResult
	if werr := a.call(t, wire.MethodRelayWrite, wire.RelayWriteParams{RelayID: relayID, Data: data[:300]}, &wr); werr != nil || wr.Received != 300 {
		t.Fatalf("relay.write = %+v, %v", wr, werr)
	}
	if werr := a.call(t, wire.MethodRelayWrite, wire.RelayWriteParams{RelayID: relayID, Offset: 10, Data: data[10:20]}, nil); werr == nil || werr.Code != wire.ErrBadRequest {
		t.Errorf("relay.write at a wrong offset = %v, want bad_request", werr)
	}
	// The connection dropped: the same package goes on from 300.
	if werr := a.call(t, wire.MethodRelayReceive, recv, &rr); werr != nil || rr.Received != 300 {
		t.Fatalf("relay.receive again = %+v, %v", rr, werr)
	}
	if werr := a.call(t, wire.MethodRelayWrite, wire.RelayWriteParams{RelayID: relayID, Offset: 300, Data: append(data[300:], 'x')}, nil); werr == nil || werr.Code != wire.ErrBadRequest {
		t.Errorf("relay.write past the size = %v, want bad_request", werr)
	}
	if werr := a.call(t, wire.MethodRelayWrite, wire.RelayWriteParams{RelayID: relayID, Offset: 300, Data: data[300:]}, &wr); werr != nil || wr.Received != int64(len(data)) {
		t.Fatalf("relay.write of the rest = %+v, %v", wr, werr)
	}
	got, err := os.ReadFile(filepath.Join(relay.InDir(l, relayID), relay.PackageFile))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("received package differs (%v)", err)
	}
	// Another package of the same relay starts again.
	other := recv
	other.Size--
	if werr := a.call(t, wire.MethodRelayReceive, other, &rr); werr != nil || rr.Received != 0 {
		t.Fatalf("relay.receive of another package = %+v, %v", rr, werr)
	}
	if werr := a.call(t, wire.MethodRelayReceive, wire.RelayReceiveParams{RelayID: relayID, Size: wire.RelayPackageMax + 1, SHA256: recv.SHA256}, nil); werr == nil || werr.Code != wire.ErrTooLarge {
		t.Errorf("relay.receive over the limit = %v, want too_large", werr)
	}
	if werr := a.call(t, wire.MethodRelayWrite, wire.RelayWriteParams{RelayID: "ffffffffffffffffffffffffffffffff", Data: data}, nil); werr == nil || werr.Code != wire.ErrNotFound {
		t.Errorf("relay.write without relay.receive = %v, want not_found", werr)
	}
	if werr := a.call(t, wire.MethodRelayRead, wire.RelayReadParams{RelayID: relayID}, nil); werr == nil || werr.Code != wire.ErrNotFound {
		t.Errorf("relay.read of no prepared package = %v, want not_found", werr)
	}
	if werr := a.call(t, wire.MethodRelayRead, wire.RelayReadParams{RelayID: "../x"}, nil); werr == nil || werr.Code != wire.ErrBadRequest {
		t.Errorf("relay.read of a bad id = %v, want bad_request", werr)
	}
	// relay.discard of a relay never started removes all of it.
	if werr := a.call(t, wire.MethodRelayDiscard, wire.RelayRef{RelayID: relayID}, nil); werr != nil {
		t.Fatal(werr)
	}
	if _, err := os.Stat(relay.InDir(l, relayID)); !os.IsNotExist(err) {
		t.Errorf("relay.discard left %s (%v)", relay.InDir(l, relayID), err)
	}
}

// relay.read answers the package's bytes from offset in chunks, eof with
// the last.
func TestRelayRead(t *testing.T) {
	l, a := startBridge(t)
	out := relay.OutDir(l, relayID)
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("0123456789"), 10)
	if err := os.WriteFile(filepath.Join(out, relay.PackageFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for {
		var r wire.RelayReadResult
		if werr := a.call(t, wire.MethodRelayRead, wire.RelayReadParams{RelayID: relayID, Offset: int64(len(got)), MaxBytes: 33}, &r); werr != nil {
			t.Fatal(werr)
		}
		got = append(got, r.Data...)
		if r.EOF {
			break
		}
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("read %q", got)
	}
	if werr := a.call(t, wire.MethodRelayRead, wire.RelayReadParams{RelayID: relayID, Offset: 101}, nil); werr == nil || werr.Code != wire.ErrBadRequest {
		t.Errorf("relay.read past the end = %v, want bad_request", werr)
	}
	if werr := a.call(t, wire.MethodRelayRead, wire.RelayReadParams{RelayID: relayID, MaxBytes: wire.RelayChunkMax + 1}, nil); werr == nil || werr.Code != wire.ErrBadRequest {
		t.Errorf("relay.read over the chunk limit = %v, want bad_request", werr)
	}
}

// relay.start refuses what it cannot run before anything is made.
func TestRelayStartRefusesBadParams(t *testing.T) {
	_, a := startBridge(t)
	for name, p := range map[string]wire.RelayStartParams{
		"other harness": {RelayID: relayID, Harness: wire.HarnessOther, TargetHost: wire.RelayHost{Label: "pc"}},
		"bad model":     {RelayID: relayID, Harness: wire.HarnessClaude, Model: "-rf", TargetHost: wire.RelayHost{Label: "pc"}},
		"model quote":   {RelayID: relayID, Harness: wire.HarnessClaude, Model: `a"b`, TargetHost: wire.RelayHost{Label: "pc"}},
		"no label":      {RelayID: relayID, Harness: wire.HarnessClaude},
		"relative dir":  {RelayID: relayID, Harness: wire.HarnessClaude, TargetHost: wire.RelayHost{Label: "pc"}, Clone: &wire.RelayClone{Dir: "x"}},
		"bad id":        {RelayID: "XYZ", Harness: wire.HarnessClaude, TargetHost: wire.RelayHost{Label: "pc"}},
	} {
		if werr := a.call(t, wire.MethodRelayStart, p, nil); werr == nil || werr.Code != wire.ErrBadRequest {
			t.Errorf("%s: relay.start = %v, want bad_request", name, werr)
		}
	}
}

// A relay.prepare that fails once its relay id is made names that id in
// the error's data, for the client's relay.discard.
func TestRelayPrepareErrorCarriesRelayID(t *testing.T) {
	l, a := startBridge(t)
	serveFake(t, l.DaemonAddr, func(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
		if m.Method == wire.MethodSessionsList {
			return wire.SessionsListResult{Sessions: []wire.Session{}}, nil
		}
		return struct{}{}, nil
	})
	werr := a.call(t, wire.MethodRelayPrepare, wire.RelayPrepareParams{
		SessionID: idA, Target: wire.RelayTargetOtherHost, SourceAgent: wire.RelaySourceKeep, SourceHost: wire.RelayHost{Label: "mac"},
	}, nil)
	if werr == nil || werr.Code != wire.ErrNotFound {
		t.Fatalf("relay.prepare of an unknown session = %v, want not_found", werr)
	}
	var data wire.RelayErrorData
	if err := json.Unmarshal(werr.Data, &data); err != nil || !wire.ValidRelayID(data.RelayID) {
		t.Fatalf("error data %s (%v), want {relay_id}", werr.Data, err)
	}
	if werr := a.call(t, wire.MethodRelayDiscard, wire.RelayRef{RelayID: data.RelayID}, nil); werr != nil {
		t.Fatalf("relay.discard of it: %v", werr)
	}
	// Refused before any id is made: no data.
	werr = a.call(t, wire.MethodRelayPrepare, wire.RelayPrepareParams{SessionID: idA, Target: "elsewhere"}, nil)
	if werr == nil || werr.Code != wire.ErrBadRequest || len(werr.Data) != 0 {
		t.Fatalf("relay.prepare with a bad target = %+v", werr)
	}
}
