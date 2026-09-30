package ptable

import (
	"bytes"
	"encoding/binary"
)

// macOS keeps the current logins in /var/run/utmpx: records of struct
// utmpx32 (Libc gen/utmpx-darwin.h) — ut_user[256], ut_id[4], ut_line[32],
// ut_pid, ut_type, ut_tv (two int32), ut_host[256], ut_pad[16] — the first
// a signature record. The parser has no build tag so it is tested on every
// platform.
const (
	utmpxSize        = 628
	utmpxUserLen     = 256
	utmpxLineOff     = 260
	utmpxLineLen     = 32
	utmpxTypeOff     = 296
	utmpxHostOff     = 308
	utmpxHostLen     = 256
	utmpxUserProcess = 7
	utmpxSignature   = 10
	utmpxMagic       = "utmpx-1.00"
)

// utmpxLogin is a login utmpx records: the terminal (below /dev) and the
// remote host `login -h` names.
type utmpxLogin struct {
	line, host string
}

// parseUtmpx returns the user logins of a utmpx file, nil when its
// signature record is missing (another layout).
func parseUtmpx(b []byte) []utmpxLogin {
	if len(b) < utmpxSize || cString(b[:utmpxUserLen]) != utmpxMagic ||
		binary.LittleEndian.Uint16(b[utmpxTypeOff:]) != utmpxSignature {
		return nil
	}
	var out []utmpxLogin
	for r := b[utmpxSize:]; len(r) >= utmpxSize; r = r[utmpxSize:] {
		if binary.LittleEndian.Uint16(r[utmpxTypeOff:]) != utmpxUserProcess {
			continue
		}
		out = append(out, utmpxLogin{
			line: cString(r[utmpxLineOff : utmpxLineOff+utmpxLineLen]),
			host: cString(r[utmpxHostOff : utmpxHostOff+utmpxHostLen]),
		})
	}
	return out
}

func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}
