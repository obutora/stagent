package hostscreen

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

// A full frame record is u32 N, u16 H, H bytes of JSON header, then the
// JPEG (PROTOCOL.md "Host screen", binary frame channel).
func TestFullFrameRecordLayout(t *testing.T) {
	var b bytes.Buffer
	n, err := WriteRecord(&b, wire.HostScreenRecordHeader{Kind: wire.HostScreenRecordFull, Seq: 7, Width: 2, Height: 1}, []byte{0xff, 0xd8, 0xff})
	if err != nil {
		t.Fatal(err)
	}
	header := `{"kind":"full","seq":7,"width":2,"height":1}`
	// N = 2 + len(header) + 3 = 2 + 44 + 3 = 49 = 0x31; H = 44 = 0x2c.
	want := "00000031" + "002c" + hex.EncodeToString([]byte(header)) + "ffd8ff"
	if got := hex.EncodeToString(b.Bytes()); got != want {
		t.Fatalf("record\n got %s\nwant %s", got, want)
	}
	if n != b.Len() {
		t.Fatalf("WriteRecord returned %d, wrote %d", n, b.Len())
	}
}

func TestRecordsReadBackInOrder(t *testing.T) {
	var b bytes.Buffer
	WriteRecord(&b, wire.HostScreenRecordHeader{Kind: wire.HostScreenRecordFull, Seq: 1, Width: 800, Height: 600}, []byte("jpeg-one"))
	WriteRecord(&b, wire.HostScreenRecordHeader{Kind: wire.HostScreenRecordFull, Seq: 2, Width: 600, Height: 800}, []byte("jpeg-two"))
	for i, want := range []struct {
		seq     int64
		w       int
		payload string
	}{{1, 800, "jpeg-one"}, {2, 600, "jpeg-two"}} {
		h, p, err := ReadRecord(&b)
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if h.Kind != wire.HostScreenRecordFull || h.Seq != want.seq || h.Width != want.w || string(p) != want.payload {
			t.Fatalf("record %d: %+v %q", i, h, p)
		}
	}
	if _, _, err := ReadRecord(&b); !errors.Is(err, io.EOF) {
		t.Fatalf("after the last record: %v, want EOF", err)
	}
}

func TestMalformedRecordsAreRefused(t *testing.T) {
	for name, raw := range map[string]string{
		"header longer than the record": "00000004" + "0010" + "7b7d",
		"too short for its header size": "00000001" + "00",
		"over MaxRecord":                "01000001",
		"truncated":                     "00000010" + "0002" + "7b7d",
	} {
		t.Run(name, func(t *testing.T) {
			b, _ := hex.DecodeString(raw)
			if _, _, err := ReadRecord(bytes.NewReader(b)); err == nil {
				t.Fatal("no error")
			}
		})
	}
}
