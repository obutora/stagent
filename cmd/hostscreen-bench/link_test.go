package main

import (
	"bytes"
	"io"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

func TestLinkSendsAtItsBandwidthThenAddsItsDelay(t *testing.T) {
	l := NewLink(5_000_000, 40*time.Millisecond)
	// 62,500 bytes = 500,000 bits = 100 ms at 5 Mbps.
	if got, want := l.schedule(t0, 62_500), t0.Add(140*time.Millisecond); !got.Equal(want) {
		t.Fatalf("arrival = %v, want %v", got.Sub(t0), want.Sub(t0))
	}
}

func TestLinkQueuesWhatIsWrittenWhileItIsBusy(t *testing.T) {
	l := NewLink(5_000_000, 40*time.Millisecond)
	l.schedule(t0, 62_500) // busy until t0+100ms
	// Written at t0+10ms: waits until t0+100ms, sends 50 ms, arrives 40 ms later.
	got := l.schedule(t0.Add(10*time.Millisecond), 31_250)
	if want := t0.Add(190 * time.Millisecond); !got.Equal(want) {
		t.Fatalf("arrival = %v, want %v", got.Sub(t0), want.Sub(t0))
	}
}

func TestLinkIdleAgainSendsAtOnce(t *testing.T) {
	l := NewLink(5_000_000, 40*time.Millisecond)
	l.schedule(t0, 62_500)
	got := l.schedule(t0.Add(time.Second), 625) // 1 ms of bits
	if want := t0.Add(time.Second + 41*time.Millisecond); !got.Equal(want) {
		t.Fatalf("arrival = %v, want %v", got.Sub(t0), want.Sub(t0))
	}
}

func TestLinkReaderDeliversEveryByteAndCountsIt(t *testing.T) {
	l := NewLink(1e9, 0)
	var mu sync.Mutex
	counted := 0
	l.OnDeliver = func(_ time.Time, n int) { mu.Lock(); counted += n; mu.Unlock() }
	want := bytes.Repeat([]byte("frame "), 10_000) // several chunks
	got, err := io.ReadAll(l.Reader(bytes.NewReader(want)))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("read %d bytes (err %v), want the %d written", len(got), err, len(want))
	}
	if counted != len(want) {
		t.Fatalf("counted %d bytes, want %d", counted, len(want))
	}
}
