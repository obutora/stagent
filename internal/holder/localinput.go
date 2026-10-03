package holder

import "bytes"

// isTyping reports whether a read from the local terminal holds anything
// the user typed (or pasted), as opposed to bytes the terminal sends on its
// own: replies to the program's queries (CPR, DA, DECRPM, DSR, window and
// keyboard-flag reports, OSC/DCS/APC strings) and focus and mouse reports.
//
// A terminal writes each reply whole, so a sequence cut off by the end of
// the read is taken as keys (Esc, Alt+[, Alt+] ...). The legacy Shift/Ctrl+F3
// key (CSI 1;m R) cannot be told from a cursor position report and is not
// counted.
func isTyping(b []byte) bool {
	for i := 0; i < len(b); {
		if b[i] != 0x1b || i+1 >= len(b) {
			return true
		}
		var n int
		switch b[i+1] {
		case '[':
			n = terminalCSI(b[i:])
		case ']', 'P', '_', '^', 'X': // OSC, DCS, APC, PM, SOS
			n = terminalString(b[i:])
		}
		if n == 0 {
			return true
		}
		i += n
	}
	return false
}

// terminalCSI returns the length of the terminal-generated CSI sequence b
// starts with, or 0 when it is a key (or incomplete).
func terminalCSI(b []byte) int {
	i := 2
	start := i
	for i < len(b) && b[i] >= 0x30 && b[i] <= 0x3f {
		i++
	}
	params := b[start:i]
	start = i
	for i < len(b) && b[i] >= 0x20 && b[i] <= 0x2f {
		i++
	}
	inter := b[start:i]
	if i >= len(b) || b[i] < 0x40 || b[i] > 0x7e {
		return 0
	}
	final := b[i]
	i++
	var prefix byte
	if len(params) > 0 && params[0] >= '<' && params[0] <= '?' {
		prefix = params[0]
	}
	ok := false
	switch final {
	case 'R': // cursor position report (CPR, DECXCPR)
		ok = len(inter) == 0
	case 'n': // device status report
		ok = len(inter) == 0
	case 't': // window reports (XTWINOPS)
		ok = len(inter) == 0 && prefix == 0
	case 'c': // primary / secondary / tertiary device attributes
		ok = len(inter) == 0 && (prefix == '?' || prefix == '>' || prefix == '=')
	case 'y': // mode report (DECRPM)
		ok = bytes.Equal(inter, []byte("$"))
	case 'u': // keyboard flags report (kitty protocol); keys have no '?'
		ok = len(inter) == 0 && prefix == '?'
	case 'I', 'O': // focus in / out
		ok = len(params) == 0 && len(inter) == 0
	case 'm': // SGR mouse release
		ok = len(inter) == 0 && prefix == '<'
	case 'M':
		switch {
		case len(inter) != 0:
		case prefix == '<': // SGR mouse press / motion
			ok = true
		case len(params) == 0: // X10 mouse: three raw bytes follow
			ok = true
			i = min(i+3, len(b))
		case prefix == 0 && bytes.Count(params, []byte(";")) == 2: // urxvt mouse
			ok = true
		}
	}
	if !ok {
		return 0
	}
	return i
}

// terminalString returns the length of the OSC/DCS/APC/PM/SOS string b
// starts with, up to and including its BEL or ST terminator, or 0 when it
// is not terminated.
func terminalString(b []byte) int {
	for i := 2; i < len(b); i++ {
		switch {
		case b[i] == 0x07 && b[1] == ']':
			return i + 1
		case b[i] == 0x1b && i+1 < len(b) && b[i+1] == '\\':
			return i + 2
		}
	}
	return 0
}
