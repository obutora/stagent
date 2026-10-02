package attachcli

const (
	esc = 0x1b
	bel = 0x07
)

// maxReply bounds a held sequence: real replies are far shorter (a long DA1
// is ~60 bytes), so anything longer is input that merely looks like one.
const maxReply = 256

// replyFilter removes the local terminal's answers to terminal queries from
// keyboard input. The program's queries reach the local terminal with its
// output, but the holder already answers them (PROTOCOL.md), so a second
// answer would arrive as typed garbage. Filtered: CPR / DECXCPR
// (ESC[r;cR, ESC[?r;c[;p]R), DA1/DA2 (ESC[?…c, ESC[>…c), DECRPM
// (ESC[?…$y, ESC[…$y), OSC 10/11/12/4 colour replies and XTVERSION
// (ESC P>|…ESC\). Keys, mouse reports and focus in/out (ESC[I, ESC[O) pass.
//
// xterm encodes F3 with modifiers as ESC[1;<mod>R, which is
// indistinguishable from a CPR and therefore dropped too.
//
// A reply can be split across reads, so an ESC sequence that could still
// become one is held back; the caller releases it with flush when no more
// input follows shortly (a bare ESC keypress must reach the program).
type replyFilter struct {
	held []byte
}

type match int

const (
	noMatch match = iota // not a reply: pass the bytes on
	partial              // a proper prefix of a reply
	full                 // a complete reply: drop it
)

// filter appends src minus query replies to dst and returns it.
func (f *replyFilter) filter(dst, src []byte) []byte {
	for _, c := range src {
		if len(f.held) == 0 && c != esc {
			dst = append(dst, c)
			continue
		}
		f.held = append(f.held, c)
		dst = f.settle(dst)
	}
	return dst
}

// settle classifies held after a byte was added. A sequence that turned out
// not to be a reply is released up to the next ESC, which may start one.
func (f *replyFilter) settle(dst []byte) []byte {
	for len(f.held) > 0 {
		switch classify(f.held) {
		case full:
			f.held = f.held[:0]
			return dst
		case partial:
			return dst
		}
		dst = append(dst, f.held[0])
		rest := f.held[1:]
		i := 0
		for i < len(rest) && rest[i] != esc {
			i++
		}
		dst = append(dst, rest[:i]...)
		f.held = append(f.held[:0], rest[i:]...)
	}
	return dst
}

// holding reports whether bytes are held back.
func (f *replyFilter) holding() bool { return len(f.held) > 0 }

// flush appends the held bytes to dst unfiltered.
func (f *replyFilter) flush(dst []byte) []byte {
	dst = append(dst, f.held...)
	f.held = f.held[:0]
	return dst
}

// classify matches s, which starts with ESC, against the filtered replies.
func classify(s []byte) match {
	switch {
	case len(s) > maxReply:
		return noMatch
	case len(s) == 1:
		return partial
	}
	switch s[1] {
	case '[':
		return classifyCSI(s[2:])
	case ']':
		return classifyOSC(s[2:])
	case 'P':
		return classifyDCS(s[2:])
	}
	return noMatch
}

// classifyCSI matches what follows ESC [: an optional ? or > marker,
// numeric parameters, then R (CPR), c (DA) or $y (DECRPM).
func classifyCSI(b []byte) match {
	i := 0
	var marker byte
	if i < len(b) && (b[i] == '?' || b[i] == '>') {
		marker = b[i]
		i++
	}
	start := i
	for i < len(b) && (b[i] >= '0' && b[i] <= '9' || b[i] == ';') {
		i++
	}
	params := b[start:i]
	if i == len(b) {
		return partial
	}
	dollar := b[i] == '$'
	if dollar {
		if i++; i == len(b) {
			return partial
		}
	}
	n := numbers(params)
	switch final := b[i]; {
	case dollar:
		if final == 'y' && marker != '>' && n == 2 {
			return full
		}
	case final == 'R':
		if marker == 0 && n == 2 || marker == '?' && (n == 2 || n == 3) {
			return full
		}
	case final == 'c':
		if marker != 0 && n > 0 {
			return full
		}
	}
	return noMatch
}

// numbers counts the ;-separated parameters, or returns -1 when one of
// them is empty.
func numbers(p []byte) int {
	if len(p) == 0 {
		return 0
	}
	n, empty := 1, true
	for _, c := range p {
		if c == ';' {
			if empty {
				return -1
			}
			n++
			empty = true
			continue
		}
		empty = false
	}
	if empty {
		return -1
	}
	return n
}

// oscReplies are the OSC numbers whose replies are filtered (with their
// separator): foreground, background and cursor colour, palette entries.
var oscReplies = [...]string{"10;", "11;", "12;", "4;"}

// classifyOSC matches what follows ESC ]: a colour reply terminated by BEL
// or ST.
func classifyOSC(b []byte) match {
	for _, id := range oscReplies {
		if len(b) < len(id) {
			if string(b) == id[:len(b)] {
				return partial
			}
			continue
		}
		if string(b[:len(id)]) == id {
			return terminated(b[len(id):], true)
		}
	}
	return noMatch
}

// classifyDCS matches what follows ESC P: the XTVERSION reply >|text ST.
func classifyDCS(b []byte) match {
	const id = ">|"
	if len(b) < len(id) {
		if string(b) == id[:len(b)] {
			return partial
		}
		return noMatch
	}
	if string(b[:len(id)]) != id {
		return noMatch
	}
	return terminated(b[len(id):], false)
}

// terminated matches a string body ending in ST (ESC \), or BEL when
// allowed. Other control characters cannot occur in a reply.
func terminated(body []byte, belEnds bool) match {
	for i, c := range body {
		switch {
		case c == bel && belEnds:
			return full
		case c == esc:
			if i+1 == len(body) {
				return partial
			}
			if body[i+1] == '\\' {
				return full
			}
			return noMatch
		case c < 0x20 || c == 0x7f:
			return noMatch
		}
	}
	return partial
}
