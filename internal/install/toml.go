package install

import "strings"

// Minimal, line-based editing of Codex's config.toml. Only the lines we add,
// change or remove are touched; every other byte of the file is kept.

type tomlLine struct {
	raw    string // including its line terminator
	header string // normalized table name when this line is a table header
	array  bool   // header is an array of tables ([[...]])
	key    string // normalized key of a key/value line starting here
	cont   bool   // continuation of a multi-line value
}

func (l tomlLine) text() string {
	return strings.TrimSuffix(strings.TrimSuffix(l.raw, "\n"), "\r")
}

type tomlDoc struct {
	lines []tomlLine
	eol   string
}

func parseTOMLLines(b []byte) *tomlDoc {
	d := &tomlDoc{eol: "\n"}
	for _, raw := range splitLines(b) {
		d.lines = append(d.lines, tomlLine{raw: raw})
	}
	if len(d.lines) > 0 && strings.HasSuffix(d.lines[0].raw, "\r\n") {
		d.eol = "\r\n"
	}
	d.classify()
	return d
}

func (d *tomlDoc) bytes() []byte {
	var sb strings.Builder
	for _, l := range d.lines {
		sb.WriteString(l.raw)
	}
	return []byte(sb.String())
}

// classify marks headers, key lines and continuation lines. It tracks
// multi-line strings and brackets so that their content is never mistaken
// for structure.
func (d *tomlDoc) classify() {
	var ml string // open multi-line string delimiter
	depth := 0
	for i := range d.lines {
		l := &d.lines[i]
		l.header, l.key, l.array, l.cont = "", "", false, false
		t := l.text()
		if ml != "" || depth > 0 {
			l.cont = true
			ml, depth = scanTOMLValue(t, ml, depth)
			continue
		}
		s := strings.TrimSpace(t)
		if s == "" || s[0] == '#' {
			continue
		}
		if s[0] == '[' {
			name, arr := parseTOMLHeader(s)
			l.header, l.array = name, arr
			if l.header == "" {
				l.header = "\x00invalid"
			}
			continue
		}
		eq := keyEnd(s)
		if eq < 0 {
			continue
		}
		l.key = normalizeTOMLKey(s[:eq])
		ml, depth = scanTOMLValue(s[eq+1:], "", 0)
	}
}

// keyEnd finds the '=' separating key and value, outside quoted key parts.
func keyEnd(s string) int {
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if c == '\\' && q == '"' {
				i++
			} else if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			q = c
		case c == '=':
			return i
		}
	}
	return -1
}

// scanTOMLValue advances the multi-line string / bracket state over s.
func scanTOMLValue(s, ml string, depth int) (string, int) {
	i := 0
	for i < len(s) {
		if ml != "" {
			j := strings.Index(s[i:], ml)
			if ml == `"""` {
				// skip escaped quotes
				for j >= 0 && i+j > 0 && s[i+j-1] == '\\' {
					k := strings.Index(s[i+j+1:], ml)
					if k < 0 {
						j = -1
						break
					}
					j += 1 + k
				}
			}
			if j < 0 {
				return ml, depth
			}
			i += j + 3
			ml = ""
			continue
		}
		c := s[i]
		switch {
		case c == '#':
			return "", depth
		case strings.HasPrefix(s[i:], `"""`):
			ml = `"""`
			i += 3
		case strings.HasPrefix(s[i:], `'''`):
			ml = `'''`
			i += 3
		case c == '"':
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' {
					i++
				}
				i++
			}
			i++
		case c == '\'':
			i++
			for i < len(s) && s[i] != '\'' {
				i++
			}
			i++
		case c == '[' || c == '{':
			depth++
			i++
		case c == ']' || c == '}':
			if depth > 0 {
				depth--
			}
			i++
		default:
			i++
		}
	}
	return ml, depth
}

func parseTOMLHeader(s string) (name string, array bool) {
	if strings.HasPrefix(s, "[[") {
		array = true
		s = s[2:]
	} else {
		s = s[1:]
	}
	// find the closing bracket outside quotes
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if c == '\\' && q == '"' {
				i++
			} else if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			q = c
		case c == ']':
			return normalizeTOMLKey(s[:i]), array
		}
	}
	return "", array
}

// normalizeTOMLKey turns a (dotted, possibly quoted) key into its parts
// joined by '.', unquoted and trimmed.
func normalizeTOMLKey(k string) string {
	var parts []string
	var cur strings.Builder
	var q byte
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case q != 0:
			if c == '\\' && q == '"' && i+1 < len(k) {
				i++
				cur.WriteByte(k[i])
			} else if c == q {
				q = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '"' || c == '\'':
			q = c
		case c == '.':
			parts = append(parts, strings.TrimSpace(cur.String()))
			cur.Reset()
		case c == ' ' || c == '\t':
			// whitespace around dots is insignificant outside quotes
		default:
			cur.WriteByte(c)
		}
	}
	parts = append(parts, strings.TrimSpace(cur.String()))
	return strings.Join(parts, ".")
}

// tomlValue returns the value text of a key line without trailing comment.
func tomlValue(line string) string {
	s := strings.TrimSpace(line)
	eq := keyEnd(s)
	if eq < 0 {
		return ""
	}
	v := strings.TrimSpace(s[eq+1:])
	// strip a trailing comment outside strings
	var q byte
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case q != 0:
			if c == '\\' && q == '"' {
				i++
			} else if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			q = c
		case c == '#':
			return strings.TrimSpace(v[:i])
		}
	}
	return v
}

// firstHeader is the index of the first table header (len(lines) if none).
func (d *tomlDoc) firstHeader() int {
	for i, l := range d.lines {
		if l.header != "" {
			return i
		}
	}
	return len(d.lines)
}

// table returns the header index of table name and the end (exclusive) of
// its body, or -1.
func (d *tomlDoc) table(name string) (int, int) {
	for i, l := range d.lines {
		if l.header == name && !l.array {
			end := i + 1
			for end < len(d.lines) && d.lines[end].header == "" {
				end++
			}
			return i, end
		}
	}
	return -1, -1
}

// findKey finds a key line within [from, to).
func (d *tomlDoc) findKey(key string, from, to int) int {
	for i := from; i < to; i++ {
		if !d.lines[i].cont && d.lines[i].key == key {
			return i
		}
	}
	return -1
}

func (d *tomlDoc) insert(at int, texts ...string) {
	if at > 0 && at == len(d.lines) && !strings.HasSuffix(d.lines[at-1].raw, "\n") {
		d.lines[at-1].raw += d.eol
	}
	add := make([]tomlLine, len(texts))
	for i, t := range texts {
		add[i] = tomlLine{raw: t + d.eol}
	}
	d.lines = append(d.lines[:at], append(add, d.lines[at:]...)...)
	d.classify()
}

func (d *tomlDoc) remove(from, to int) {
	d.lines = append(d.lines[:from], d.lines[to:]...)
	d.classify()
}

// replace swaps the text of line i, keeping its terminator.
func (d *tomlDoc) replace(i int, text string) {
	raw := d.lines[i].raw
	term := raw[len(d.lines[i].text()):]
	d.lines[i].raw = text + term
	d.classify()
}

func leadingSpace(s string) string {
	return s[:len(s)-len(strings.TrimLeft(s, " \t"))]
}

func isBlank(l tomlLine) bool { return strings.TrimSpace(l.raw) == "" }

var errTOMLInlineFeatures = unmanagedFile("config.toml defines features as an inline table")

// ensureFeatureHooks makes `[features] hooks = true` effective. edit is nil
// when the file already enables it.
func ensureFeatureHooks(d *tomlDoc) (*TOMLEdit, error) {
	top := d.firstHeader()
	if i := d.findKey("features", 0, top); i >= 0 {
		return nil, errTOMLInlineFeatures
	}
	if i := d.findKey("features.hooks", 0, top); i >= 0 {
		return setHooksLine(d, i, "features.hooks")
	}
	h, end := d.table("features")
	if h < 0 {
		// Append a new table at the end of the file.
		e := &TOMLEdit{Kind: "features_hooks", Action: "added", AddedHeader: true}
		var add []string
		if n := len(d.lines); n > 0 && !isBlank(d.lines[n-1]) {
			add = append(add, "")
			e.AddedBlank = true
		}
		add = append(add, "[features]", "hooks = true")
		d.insert(len(d.lines), add...)
		return e, nil
	}
	if i := d.findKey("hooks", h+1, end); i >= 0 {
		return setHooksLine(d, i, "hooks")
	}
	d.insert(h+1, "hooks = true")
	return &TOMLEdit{Kind: "features_hooks", Action: "added"}, nil
}

func setHooksLine(d *tomlDoc, i int, key string) (*TOMLEdit, error) {
	t := d.lines[i].text()
	if tomlValue(t) == "true" {
		return nil, nil
	}
	d.replace(i, leadingSpace(t)+key+" = true")
	return &TOMLEdit{Kind: "features_hooks", Action: "changed", PreviousLine: t}, nil
}

// featureHooksEnabled reports the value of features.hooks: set is false when
// the file does not mention it.
func featureHooksEnabled(d *tomlDoc) (enabled, set bool) {
	top := d.firstHeader()
	i := d.findKey("features.hooks", 0, top)
	if i < 0 {
		if h, end := d.table("features"); h >= 0 {
			i = d.findKey("hooks", h+1, end)
		}
	}
	if i < 0 {
		return false, false
	}
	return tomlValue(d.lines[i].text()) == "true", true
}

// revertFeatureHooks undoes ensureFeatureHooks as recorded in edit. It only
// acts while the line still says `true`; ok is false when the user changed
// it since (nothing is touched then).
func revertFeatureHooks(d *tomlDoc, edit TOMLEdit) (changed, ok bool) {
	top := d.firstHeader()
	i := d.findKey("features.hooks", 0, top)
	h, end := d.table("features")
	if i < 0 && h >= 0 {
		i = d.findKey("hooks", h+1, end)
	}
	if i < 0 {
		return false, true // already gone
	}
	if tomlValue(d.lines[i].text()) != "true" {
		return false, false
	}
	if edit.Action == "changed" && edit.PreviousLine != "" {
		d.replace(i, edit.PreviousLine)
		return true, true
	}
	d.remove(i, i+1)
	if edit.AddedHeader {
		h, end = d.table("features")
		if h >= 0 {
			empty := true
			for j := h + 1; j < end; j++ {
				if !isBlank(d.lines[j]) {
					empty = false
				}
			}
			if empty {
				from := h
				if edit.AddedBlank && from > 0 && isBlank(d.lines[from-1]) {
					from--
				}
				d.remove(from, end)
			}
		}
	}
	return true, true
}

// notifyLine is the Codex notify program pointing at stagent.
func notifyLine(bin string) string {
	return "notify = [" + tomlString(bin) + `, "hook", "codex", "notify"]`
}

// tomlString quotes s as a TOML basic string.
func tomlString(s string) string {
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			sb.WriteString(`\"`)
		case r == '\\':
			sb.WriteString(`\\`)
		case r < 0x20 || r == 0x7f:
			sb.WriteString(`\u00`)
			const hex = "0123456789abcdef"
			sb.WriteByte(hex[r>>4])
			sb.WriteByte(hex[r&0xf])
		default:
			sb.WriteRune(r)
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

// topLevelNotify returns the line index of a top-level `notify` key.
func topLevelNotify(d *tomlDoc) int {
	return d.findKey("notify", 0, d.firstHeader())
}

// addNotify inserts our notify line unless a notify program is configured;
// existing is then its (first) line and nothing changes.
func addNotify(d *tomlDoc, bin string) (added bool, existing string) {
	if i := topLevelNotify(d); i >= 0 {
		return false, d.lines[i].text()
	}
	at := d.firstHeader()
	for at > 0 && isBlank(d.lines[at-1]) {
		at--
	}
	d.insert(at, notifyLine(bin))
	return true, ""
}

// removeNotify deletes a single-line top-level notify that runs stagent.
func removeNotify(d *tomlDoc) bool {
	i := topLevelNotify(d)
	if i < 0 || !isOurCommand(d.lines[i].text()) {
		return false
	}
	if i+1 < len(d.lines) && d.lines[i+1].cont {
		return false // multi-line: edited by the user, leave it
	}
	d.remove(i, i+1)
	return true
}

// removeTables deletes the named tables (header plus body, keeping blank
// lines that separate the next table). It returns how many were removed.
func removeTables(d *tomlDoc, names map[string]bool) int {
	n := 0
	for i := 0; i < len(d.lines); i++ {
		l := d.lines[i]
		if l.array || !names[l.header] {
			continue
		}
		end := i + 1
		for end < len(d.lines) && d.lines[end].header == "" {
			end++
		}
		for end > i+1 && isBlank(d.lines[end-1]) && end < len(d.lines) {
			end--
		}
		d.remove(i, end)
		i--
		n++
	}
	return n
}
