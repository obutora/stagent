package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// Order-preserving JSON editing for harness settings files. Untouched
// scalars keep their exact source text; objects keep key order; output uses
// the file's indentation unit (2 spaces by default, as Claude Code and Codex
// write) and its line ending.

type jkind uint8

const (
	jScalar jkind = iota
	jObject
	jArray
)

type jnode struct {
	kind jkind
	raw  []byte   // scalar source text
	keys []string // object keys (decoded)
	rkey [][]byte // object keys (source text)
	vals []*jnode // object values / array elements
}

// jdoc is a parsed JSON file with its formatting conventions.
type jdoc struct {
	root   *jnode
	indent string
	eol    string
	bom    bool
	final  bool // trailing newline
}

var errNotJSON = errors.New("not valid JSON")

func parseJSONDoc(b []byte) (*jdoc, error) {
	d := &jdoc{indent: "  ", eol: "\n", final: true}
	if len(bytes.TrimSpace(b)) == 0 {
		d.root = &jnode{kind: jObject}
		return d, nil
	}
	if bytes.HasPrefix(b, []byte("\xef\xbb\xbf")) {
		d.bom = true
		b = b[3:]
	}
	if !json.Valid(b) {
		return nil, errNotJSON
	}
	if bytes.Contains(b, []byte("\r\n")) {
		d.eol = "\r\n"
	}
	d.final = bytes.HasSuffix(bytes.TrimRight(b, " \t"), []byte("\n"))
	d.indent = detectIndent(b)
	p := jparser{b: b}
	p.ws()
	d.root = p.value()
	return d, nil
}

// detectIndent returns the leading whitespace of the first indented line.
func detectIndent(b []byte) string {
	for i := 0; i < len(b); i++ {
		if b[i] != '\n' {
			continue
		}
		j := i + 1
		for j < len(b) && (b[j] == ' ' || b[j] == '\t') {
			j++
		}
		if j > i+1 && j < len(b) && b[j] != '\n' && b[j] != '\r' {
			return string(b[i+1 : j])
		}
	}
	return "  "
}

type jparser struct {
	b []byte
	i int
}

func (p *jparser) ws() {
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

// value parses one value; the input is known to be valid JSON.
func (p *jparser) value() *jnode {
	switch p.b[p.i] {
	case '{':
		n := &jnode{kind: jObject}
		p.i++
		p.ws()
		if p.b[p.i] == '}' {
			p.i++
			return n
		}
		for {
			p.ws()
			rk := p.str()
			var k string
			json.Unmarshal(rk, &k)
			p.ws()
			p.i++ // ':'
			p.ws()
			v := p.value()
			n.keys = append(n.keys, k)
			n.rkey = append(n.rkey, rk)
			n.vals = append(n.vals, v)
			p.ws()
			c := p.b[p.i]
			p.i++
			if c == '}' {
				return n
			}
		}
	case '[':
		n := &jnode{kind: jArray}
		p.i++
		p.ws()
		if p.b[p.i] == ']' {
			p.i++
			return n
		}
		for {
			p.ws()
			n.vals = append(n.vals, p.value())
			p.ws()
			c := p.b[p.i]
			p.i++
			if c == ']' {
				return n
			}
		}
	case '"':
		return &jnode{kind: jScalar, raw: p.str()}
	default:
		start := p.i
		for p.i < len(p.b) {
			c := p.b[p.i]
			if c == ',' || c == '}' || c == ']' || c == ' ' || c == '\t' || c == '\n' || c == '\r' {
				break
			}
			p.i++
		}
		return &jnode{kind: jScalar, raw: p.b[start:p.i]}
	}
}

func (p *jparser) str() []byte {
	start := p.i
	p.i++
	for p.b[p.i] != '"' {
		if p.b[p.i] == '\\' {
			p.i++
		}
		p.i++
	}
	p.i++
	return p.b[start:p.i]
}

func (d *jdoc) encode() []byte {
	var buf bytes.Buffer
	if d.bom {
		buf.WriteString("\xef\xbb\xbf")
	}
	d.write(&buf, d.root, 0)
	if d.final {
		buf.WriteString(d.eol)
	}
	return buf.Bytes()
}

func (d *jdoc) write(buf *bytes.Buffer, n *jnode, depth int) {
	switch n.kind {
	case jScalar:
		buf.Write(n.raw)
	case jObject, jArray:
		open, close := byte('{'), byte('}')
		if n.kind == jArray {
			open, close = '[', ']'
		}
		buf.WriteByte(open)
		if len(n.vals) == 0 {
			buf.WriteByte(close)
			return
		}
		for i, v := range n.vals {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.WriteString(d.eol)
			buf.WriteString(strings.Repeat(d.indent, depth+1))
			if n.kind == jObject {
				buf.Write(n.rkey[i])
				buf.WriteString(": ")
			}
			d.write(buf, v, depth+1)
		}
		buf.WriteString(d.eol)
		buf.WriteString(strings.Repeat(d.indent, depth))
		buf.WriteByte(close)
	}
}

// --- node helpers ---------------------------------------------------------

func (n *jnode) get(key string) *jnode {
	if n == nil || n.kind != jObject {
		return nil
	}
	for i, k := range n.keys {
		if k == key {
			return n.vals[i]
		}
	}
	return nil
}

// set replaces key's value or appends the key at the end.
func (n *jnode) set(key string, v *jnode) {
	for i, k := range n.keys {
		if k == key {
			n.vals[i] = v
			return
		}
	}
	n.keys = append(n.keys, key)
	n.rkey = append(n.rkey, jsonString(key))
	n.vals = append(n.vals, v)
}

func (n *jnode) del(key string) {
	for i, k := range n.keys {
		if k == key {
			n.keys = append(n.keys[:i], n.keys[i+1:]...)
			n.rkey = append(n.rkey[:i], n.rkey[i+1:]...)
			n.vals = append(n.vals[:i], n.vals[i+1:]...)
			return
		}
	}
}

// str decodes a string scalar; ok is false for anything else.
func (n *jnode) str() (string, bool) {
	if n == nil || n.kind != jScalar || len(n.raw) == 0 || n.raw[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(n.raw, &s) != nil {
		return "", false
	}
	return s, true
}

// jsonString encodes s the way JSON.stringify does (no HTML escaping).
func jsonString(s string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(s)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func jStr(s string) *jnode { return &jnode{kind: jScalar, raw: jsonString(s)} }

func jInt(i int) *jnode { return &jnode{kind: jScalar, raw: []byte(strconv.Itoa(i))} }

func jObj(kv ...any) *jnode {
	n := &jnode{kind: jObject}
	for i := 0; i+1 < len(kv); i += 2 {
		n.set(kv[i].(string), kv[i+1].(*jnode))
	}
	return n
}

func jArr(vs ...*jnode) *jnode { return &jnode{kind: jArray, vals: vs} }

// --- harness hook entries -------------------------------------------------
//
// Claude Code's settings.json and Codex's hooks.json share the shape
//   {"hooks": {"<Event>": [{"matcher"?: ..., "hooks": [{"type": "command",
//     "command": "...", "timeout": N}]}]}}
// Our entries are recognised by their command (see isOurCommand); nothing
// stagent-specific is added to the schema.

// binMarker identifies stagent's own hook commands, notify programs and
// wrapper blocks: the install path of the binary, with '/' separators.
const binMarker = ".ssh-term/agent/bin/stagent"

// isOurCommand matches the marker with any path separator: '/', a single
// backslash, or the escaped double backslash of a raw TOML line.
func isOurCommand(cmd string) bool {
	cmd = strings.ReplaceAll(cmd, `\\`, "/")
	return strings.Contains(strings.ReplaceAll(cmd, `\`, "/"), binMarker)
}

type hookSpec struct {
	Event   string
	Timeout int
}

var errHookShape = errors.New(`unexpected "hooks" layout`)

func ourHookEntry(command string, timeout int) *jnode {
	return jObj("type", jStr("command"), "command", jStr(command), "timeout", jInt(timeout))
}

// mergeHooks makes root.hooks.<event> contain exactly one entry of ours per
// spec, with the given command and timeout. Existing entries of ours are
// updated in place (duplicates dropped); new ones are appended as their own
// group after everything else, so indices of foreign hooks never move.
// created lists the JSON pointers of containers that had to be added.
func mergeHooks(root *jnode, command string, specs []hookSpec) (created []string, changed bool, err error) {
	if root.kind != jObject {
		return nil, false, errHookShape
	}
	hooks := root.get("hooks")
	if hooks == nil {
		hooks = jObj()
		root.set("hooks", hooks)
		created = append(created, "/hooks")
		changed = true
	} else if hooks.kind != jObject {
		return nil, false, errHookShape
	}
	for _, sp := range specs {
		arr := hooks.get(sp.Event)
		if arr == nil {
			arr = jArr()
			hooks.set(sp.Event, arr)
			created = append(created, "/hooks/"+sp.Event)
		} else if arr.kind != jArray {
			return nil, false, errHookShape
		}
		want := ourHookEntry(command, sp.Timeout)
		kept := false
		for gi := 0; gi < len(arr.vals); gi++ {
			g := arr.vals[gi]
			list := g.get("hooks")
			if list == nil || list.kind != jArray {
				continue
			}
			removedHere := false
			for hi := 0; hi < len(list.vals); hi++ {
				cmd, _ := list.vals[hi].get("command").str()
				if !isOurCommand(cmd) {
					continue
				}
				if !kept {
					kept = true
					if !sameEntry(list.vals[hi], command, sp.Timeout) {
						list.vals[hi] = want
						changed = true
					}
					continue
				}
				list.vals = append(list.vals[:hi], list.vals[hi+1:]...)
				hi--
				removedHere = true
				changed = true
			}
			if removedHere && len(list.vals) == 0 {
				arr.vals = append(arr.vals[:gi], arr.vals[gi+1:]...)
				gi--
			}
		}
		if !kept {
			arr.vals = append(arr.vals, jObj("hooks", jArr(want)))
			changed = true
		}
	}
	return created, changed, nil
}

func sameEntry(e *jnode, command string, timeout int) bool {
	t, _ := e.get("type").str()
	c, _ := e.get("command").str()
	to := e.get("timeout")
	return t == "command" && c == command && to != nil && string(to.raw) == strconv.Itoa(timeout)
}

// unmergeHooks removes every entry of ours. Groups and event arrays emptied
// by that removal are dropped too; with a manifest record (created != nil)
// only containers we created are dropped, otherwise any container that our
// removal emptied. It returns the number of entries removed.
func unmergeHooks(root *jnode, created []string, haveRecord bool) int {
	hooks := root.get("hooks")
	if hooks == nil || hooks.kind != jObject {
		return 0
	}
	drop := func(ptr string) bool {
		if !haveRecord {
			return true
		}
		for _, c := range created {
			if c == ptr {
				return true
			}
		}
		return false
	}
	removed := 0
	for ei := 0; ei < len(hooks.keys); ei++ {
		event, arr := hooks.keys[ei], hooks.vals[ei]
		if arr.kind != jArray {
			continue
		}
		removedEvent := 0
		for gi := 0; gi < len(arr.vals); gi++ {
			list := arr.vals[gi].get("hooks")
			if list == nil || list.kind != jArray {
				continue
			}
			n := 0
			for hi := 0; hi < len(list.vals); hi++ {
				cmd, _ := list.vals[hi].get("command").str()
				if isOurCommand(cmd) {
					list.vals = append(list.vals[:hi], list.vals[hi+1:]...)
					hi--
					n++
				}
			}
			if n > 0 && len(list.vals) == 0 {
				arr.vals = append(arr.vals[:gi], arr.vals[gi+1:]...)
				gi--
			}
			removedEvent += n
		}
		removed += removedEvent
		if removedEvent > 0 && len(arr.vals) == 0 && drop("/hooks/"+event) {
			hooks.del(event)
			ei--
		}
	}
	if removed > 0 && len(hooks.keys) == 0 && drop("/hooks") {
		root.del("hooks")
	}
	return removed
}

// ourHookPositions lists, per event, the (group, hook) indices of our
// entries — Codex keys hook trust by them.
func ourHookPositions(root *jnode) map[string][][2]int {
	out := map[string][][2]int{}
	hooks := root.get("hooks")
	if hooks == nil || hooks.kind != jObject {
		return out
	}
	for ei, event := range hooks.keys {
		arr := hooks.vals[ei]
		if arr.kind != jArray {
			continue
		}
		for gi, g := range arr.vals {
			list := g.get("hooks")
			if list == nil || list.kind != jArray {
				continue
			}
			for hi, h := range list.vals {
				if cmd, _ := h.get("command").str(); isOurCommand(cmd) {
					out[event] = append(out[event], [2]int{gi, hi})
				}
			}
		}
	}
	return out
}

// ourHookEvents lists the events that carry at least one entry of ours, in
// file order.
func ourHookEvents(root *jnode) []string {
	var out []string
	hooks := root.get("hooks")
	if hooks == nil || hooks.kind != jObject {
		return nil
	}
	pos := ourHookPositions(root)
	for _, e := range hooks.keys {
		if len(pos[e]) > 0 {
			out = append(out, e)
		}
	}
	return out
}
