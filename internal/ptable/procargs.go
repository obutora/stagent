package ptable

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// procArgs2 is the decoded macOS kern.procargs2 buffer. The parser has no
// build tag so it is tested on every platform.
type procArgs2 struct {
	exe  string
	argv []string
	env  []string
}

// parseProcArgs2 decodes a kern.procargs2 buffer: a native-endian int32
// argc, the executable path, NUL padding, argc NUL-terminated arguments,
// then NUL-terminated "KEY=value" entries up to an empty string (the
// "apple" strings that follow are not environment).
func parseProcArgs2(b []byte) (procArgs2, error) {
	if len(b) < 4 {
		return procArgs2{}, errors.New("ptable: short procargs2")
	}
	argc := int(binary.LittleEndian.Uint32(b)) // every macOS target is little-endian
	b = b[4:]
	next := func() string {
		i := bytes.IndexByte(b, 0)
		if i < 0 {
			i = len(b)
		}
		s := string(b[:i])
		b = b[min(i+1, len(b)):]
		return s
	}
	var a procArgs2
	a.exe = next()
	for len(b) > 0 && b[0] == 0 {
		b = b[1:]
	}
	for len(a.argv) < argc && len(b) > 0 {
		a.argv = append(a.argv, next())
	}
	for len(b) > 0 {
		kv := next()
		if kv == "" {
			break
		}
		a.env = append(a.env, kv)
	}
	return a, nil
}

// environ returns the environment. The kernel cuts the buffer after argv
// when it withholds the environment (see ErrEnvWithheld); a process started
// with an empty environment reads the same.
func (a procArgs2) environ() ([]string, error) {
	if a.env == nil {
		return nil, ErrEnvWithheld
	}
	return a.env, nil
}
