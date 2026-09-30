package detect

import (
	"bytes"
	"encoding/base64"
	"strings"
	"unicode"
)

// Parser states. Only 7-bit C1 introducers (ESC ] etc.) are recognized:
// 0x9b/0x9d are UTF-8 continuation bytes in practice.
const (
	stGround = iota
	stEsc
	stCSI
	stOSC
	stOSCEsc // ESC seen inside an OSC (ST is ESC \)
	stStr    // DCS / SOS / PM / APC payload, ignored
	stStrEsc
)

const (
	maxOSC   = 8 << 10 // longer OSC payloads are not notifications we parse
	maxField = 256     // runes kept of a title or body
)

// parser is an incremental scanner for the few sequences the detector cares
// about. State survives across feed calls so sequences may be split
// anywhere.
type parser struct {
	st       int
	osc      []byte
	overflow bool
	kitty    kittyPending
}

// kittyPending accumulates a chunked OSC 99 notification (d=0 parts).
type kittyPending struct {
	id          string
	title, body string
	open        bool
}

func (p *parser) feed(b []byte, emit func(Notification)) {
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch p.st {
		case stGround:
			switch c {
			case 0x1b:
				p.st = stEsc
			case 0x07:
				emit(Notification{Bell: true})
			}
		case stEsc:
			p.esc(c)
		case stCSI:
			switch {
			case c >= 0x40 && c <= 0x7e:
				p.st = stGround
			case c == 0x1b:
				p.st = stEsc
			case c == 0x18 || c == 0x1a:
				p.st = stGround
			case c == 0x07:
				// C0 controls inside a CSI are executed.
				emit(Notification{Bell: true})
			}
		case stOSC:
			switch c {
			case 0x07:
				p.dispatchOSC(emit)
				p.st = stGround
			case 0x1b:
				p.st = stOSCEsc
			case 0x18, 0x1a:
				p.st = stGround
			default:
				if len(p.osc) < maxOSC {
					p.osc = append(p.osc, c)
				} else {
					p.overflow = true
				}
			}
		case stOSCEsc:
			if c == '\\' {
				p.dispatchOSC(emit)
				p.st = stGround
				break
			}
			// Any other ESC sequence aborts the OSC.
			p.esc(c)
		case stStr:
			switch c {
			case 0x1b:
				p.st = stStrEsc
			case 0x18, 0x1a:
				p.st = stGround
			}
		case stStrEsc:
			if c == '\\' {
				p.st = stGround
				break
			}
			p.esc(c)
		}
	}
}

// esc handles the byte after an ESC.
func (p *parser) esc(c byte) {
	switch c {
	case ']':
		p.st = stOSC
		p.osc = p.osc[:0]
		p.overflow = false
	case '[':
		p.st = stCSI
	case 'P', 'X', '^', '_':
		p.st = stStr
	case 0x1b:
		p.st = stEsc
	default:
		p.st = stGround
	}
}

func (p *parser) dispatchOSC(emit func(Notification)) {
	if p.overflow {
		return
	}
	cmd, rest, _ := bytes.Cut(p.osc, []byte{';'})
	switch string(cmd) {
	case "9":
		// iTerm2 growl-style notification: OSC 9 ; body. ConEmu reuses
		// OSC 9 ; <number> ; … for progress bars and other commands.
		if isConEmu(rest) {
			return
		}
		if body := clean(string(rest)); body != "" {
			emit(Notification{Body: body})
		}
	case "777":
		// rxvt-unicode: OSC 777 ; notify ; title ; body
		parts := strings.SplitN(string(rest), ";", 3)
		if len(parts) < 2 || parts[0] != "notify" {
			return
		}
		n := Notification{Title: clean(parts[1])}
		if len(parts) == 3 {
			n.Body = clean(parts[2])
		}
		if n.Title != "" || n.Body != "" {
			emit(n)
		}
	case "99":
		p.kittyOSC(rest, emit)
	}
}

func isConEmu(rest []byte) bool {
	num, _, _ := bytes.Cut(rest, []byte{';'})
	if len(num) == 0 {
		return false
	}
	for _, c := range num {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// kittyOSC handles kitty's desktop notifications: OSC 99 ; metadata ;
// payload, metadata being colon-separated key=value pairs. d=0 means more
// parts follow; p selects title (default) or body; e=1 base64-encodes the
// payload. Other payload kinds (icons, buttons, queries) are ignored.
func (p *parser) kittyOSC(rest []byte, emit func(Notification)) {
	meta, payload, _ := bytes.Cut(rest, []byte{';'})
	id, done, kind, b64 := "", true, "title", false
	for _, kv := range strings.Split(string(meta), ":") {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "i":
			id = v
		case "d":
			done = v != "0"
		case "p":
			kind = v
		case "e":
			b64 = v == "1"
		}
	}
	k := &p.kitty
	if !k.open || k.id != id {
		*k = kittyPending{id: id, open: true}
	}
	text := string(payload)
	if b64 {
		dec, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			text = ""
		} else {
			text = string(dec)
		}
	}
	switch kind {
	case "title":
		k.title = truncate(k.title + text)
	case "body":
		k.body = truncate(k.body + text)
	}
	if !done {
		return
	}
	n := Notification{Title: clean(k.title), Body: clean(k.body)}
	*k = kittyPending{}
	if n.Title != "" || n.Body != "" {
		emit(n)
	}
}

func truncate(s string) string {
	if len(s) > maxField*4 {
		s = s[:maxField*4]
	}
	return s
}

// clean makes notification text safe to forward: valid UTF-8, no control
// characters, single line, bounded length.
func clean(s string) string {
	s = strings.ToValidUTF8(s, "")
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= maxField {
			break
		}
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			r = ' '
		case unicode.IsControl(r):
			continue
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}
