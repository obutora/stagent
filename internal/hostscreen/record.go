package hostscreen

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"

	"github.com/obutora/stagent/internal/wire"
)

// MaxRecord bounds N, the length of one record of the binary frame channel
// after its u32 (wire.MaxLine, as for a JSON line).
const MaxRecord = wire.MaxLine

// ErrRecord is a record that breaks the format.
var ErrRecord = errors.New("hostscreen: malformed record")

// WriteRecord writes one record of the binary frame channel to w and
// returns how many bytes that took:
//
//	u32 BE N    length of what follows
//	u16 BE H    length of the JSON header
//	H bytes     the header (h)
//	N-2-H bytes payload
//
// The payload is not copied: w gets the prefix and it as one write when it
// takes several buffers (net.Buffers), as a socket does.
func WriteRecord(w io.Writer, h wire.HostScreenRecordHeader, payload []byte) (int, error) {
	hdr, err := json.Marshal(h)
	if err != nil {
		return 0, err
	}
	if len(hdr) > math.MaxUint16 || 2+len(hdr)+len(payload) > MaxRecord {
		return 0, fmt.Errorf("%w: %d header and %d payload bytes", ErrRecord, len(hdr), len(payload))
	}
	prefix := make([]byte, 6, 6+len(hdr))
	binary.BigEndian.PutUint32(prefix, uint32(2+len(hdr)+len(payload)))
	binary.BigEndian.PutUint16(prefix[4:], uint16(len(hdr)))
	prefix = append(prefix, hdr...)
	bufs := net.Buffers{prefix, payload}
	n, err := bufs.WriteTo(w)
	return int(n), err
}

// ReadRecord reads one record written by WriteRecord. A clean end before
// the record is io.EOF; a record cut short is io.ErrUnexpectedEOF.
func ReadRecord(r io.Reader) (wire.HostScreenRecordHeader, []byte, error) {
	var h wire.HostScreenRecordHeader
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return h, nil, err
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	if n < 2 || n > MaxRecord {
		return h, nil, fmt.Errorf("%w: length %d", ErrRecord, n)
	}
	rec := make([]byte, n)
	if _, err := io.ReadFull(r, rec); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return h, nil, err
	}
	hl := uint32(binary.BigEndian.Uint16(rec))
	if 2+hl > n {
		return h, nil, fmt.Errorf("%w: header of %d bytes in a record of %d", ErrRecord, hl, n)
	}
	if err := json.Unmarshal(rec[2:2+hl], &h); err != nil {
		return h, nil, fmt.Errorf("%w: %v", ErrRecord, err)
	}
	return h, rec[2+hl:], nil
}
