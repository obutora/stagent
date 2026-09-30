package screen

import (
	"bytes"
	"strings"
)

// stringGuard sits between the program output and the emulator and takes
// control/string sequences (OSC, DCS, APC, PM, SOS) out of its hands when
// their payload contains bytes 0x80–0x9F.
//
// The charmbracelet/x/ansi parser runs the 8-bit C1 controls inside string
// states, so the UTF-8 continuation byte 0x9C (ST) ends an OSC early: Claude
// Code's title "✳ Claude Code" (e2 9c b3 …) became the title "\xe2" and the
// rest of the payload was printed onto the screen. Such strings never reach
// the emulator; window titles (OSC 0/1/2) are parsed here for every payload.
//
// Other strings with only ASCII payloads (colour queries, OSC 8 hyperlinks,
// DECRQSS, …) are forwarded unchanged, so the emulator still answers and
// renders them.
type stringGuard struct {
	state   guardState
	intro   byte // ']' 'P' '_' '^' 'X'
	payload []byte
	over    bool // payload exceeded maxStringPayload; drop it
	onTitle func(string)
}

type guardState uint8

const (
	guardGround guardState = iota
	guardEsc               // saw ESC in ground
	guardString            // inside a string sequence
	guardStrEsc            // saw ESC inside a string (maybe ST)
)

// maxStringPayload matches the parser data size (parserDataSize); longer
// payloads (large OSC 52 copies, inline images) are dropped as before.
const maxStringPayload = parserDataSize

// filter appends to out what the emulator should see for p and returns it.
func (g *stringGuard) filter(out, p []byte) []byte {
	for _, b := range p {
		switch g.state {
		case guardGround:
			if b == 0x1b {
				g.state = guardEsc
				continue
			}
			out = append(out, b)
		case guardEsc:
			switch b {
			case ']', 'P', '_', '^', 'X':
				g.state, g.intro, g.over = guardString, b, false
				g.payload = g.payload[:0]
			case 0x1b:
				out = append(out, 0x1b) // ESC ESC: the first one is complete
			default:
				out = append(out, 0x1b, b)
				g.state = guardGround
			}
		case guardString:
			switch b {
			case 0x07: // BEL terminates OSC (xterm); treat it so for all
				out = g.finish(out, []byte{0x07})
			case 0x1b:
				g.state = guardStrEsc
			case 0x18, 0x1a: // CAN, SUB abort the sequence
				out = append(out, b)
				g.state = guardGround
			default:
				if len(g.payload) < maxStringPayload {
					g.payload = append(g.payload, b)
				} else {
					g.over = true
				}
			}
		case guardStrEsc:
			if b == '\\' {
				out = g.finish(out, []byte{0x1b, '\\'})
				continue
			}
			// ESC without '\' ends the string and starts a new sequence.
			out = g.finish(out, []byte{0x1b, '\\'})
			if b == 0x1b {
				g.state = guardEsc
				continue
			}
			g.state = guardEsc
			out = g.filter(out, []byte{b})
		}
	}
	return out
}

func (g *stringGuard) finish(out, term []byte) []byte {
	g.state = guardGround
	if g.over {
		return out
	}
	p := g.payload
	if g.intro == ']' {
		if code, text, ok := bytes.Cut(p, []byte{';'}); ok {
			switch string(code) {
			case "0", "2":
				if g.onTitle != nil {
					g.onTitle(strings.ToValidUTF8(string(text), "\uFFFD"))
				}
				return out
			case "1": // icon name only
				return out
			}
		}
	}
	for _, c := range p {
		if c >= 0x80 && c <= 0x9f {
			return out
		}
	}
	out = append(out, 0x1b, g.intro)
	out = append(out, p...)
	return append(out, term...)
}
