package install

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
)

// pnode is one value of an XML property list (`defaults export`), kept as
// written so that a round trip through `defaults import` changes only what
// stagent edits: data, dates and numbers stay text.
type pnode struct {
	kind string   // element name: dict, array, string, integer, real, date, data, true, false
	text string   // character data of the scalar kinds
	keys []string // dict keys, parallel to vals
	vals []*pnode // dict values, array items
}

func plistString(s string) *pnode { return &pnode{kind: "string", text: s} }

// parsePlist reads an XML property list. An empty <plist/> is an empty
// dict.
func parsePlist(b []byte) (*pnode, error) {
	d := xml.NewDecoder(bytes.NewReader(b))
	inPlist := false
	for {
		tok, err := d.Token()
		if err != nil {
			return nil, fmt.Errorf("property list: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if !inPlist {
				if t.Name.Local != "plist" {
					return nil, errors.New("property list: root element is <" + t.Name.Local + ">")
				}
				inPlist = true
				continue
			}
			return parsePlistValue(d, t)
		case xml.EndElement:
			return &pnode{kind: "dict"}, nil
		}
	}
}

func parsePlistValue(d *xml.Decoder, se xml.StartElement) (*pnode, error) {
	n := &pnode{kind: se.Name.Local}
	switch n.kind {
	case "dict", "array":
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, fmt.Errorf("property list: %w", err)
			}
			switch t := tok.(type) {
			case xml.StartElement:
				if n.kind == "dict" && t.Name.Local == "key" {
					var k string
					if err := d.DecodeElement(&k, &t); err != nil {
						return nil, fmt.Errorf("property list: %w", err)
					}
					n.keys = append(n.keys, k)
					continue
				}
				v, err := parsePlistValue(d, t)
				if err != nil {
					return nil, err
				}
				n.vals = append(n.vals, v)
			case xml.EndElement:
				if n.kind == "dict" && len(n.keys) != len(n.vals) {
					return nil, errors.New("property list: dict key without a value")
				}
				return n, nil
			}
		}
	case "string", "integer", "real", "date", "data", "true", "false":
		if err := d.DecodeElement(&n.text, &se); err != nil {
			return nil, fmt.Errorf("property list: %w", err)
		}
		return n, nil
	}
	return nil, errors.New("property list: unexpected element <" + n.kind + ">")
}

// get returns the value of key in a dict; nil when absent or n is not a
// dict.
func (n *pnode) get(key string) *pnode {
	if n == nil || n.kind != "dict" {
		return nil
	}
	for i, k := range n.keys {
		if k == key {
			return n.vals[i]
		}
	}
	return nil
}

// set replaces or appends key in a dict.
func (n *pnode) set(key string, v *pnode) {
	for i, k := range n.keys {
		if k == key {
			n.vals[i] = v
			return
		}
	}
	n.keys = append(n.keys, key)
	n.vals = append(n.vals, v)
}

// del removes key from a dict.
func (n *pnode) del(key string) {
	for i, k := range n.keys {
		if k == key {
			n.keys = append(n.keys[:i], n.keys[i+1:]...)
			n.vals = append(n.vals[:i], n.vals[i+1:]...)
			return
		}
	}
}

// encode writes n as an XML property list document.
func (n *pnode) encode() []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	n.write(&b, 0)
	b.WriteString("</plist>\n")
	return []byte(b.String())
}

var plistEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func (n *pnode) write(b *strings.Builder, depth int) {
	ind := strings.Repeat("\t", depth)
	switch n.kind {
	case "dict", "array":
		if len(n.vals) == 0 {
			b.WriteString(ind + "<" + n.kind + "/>\n")
			return
		}
		b.WriteString(ind + "<" + n.kind + ">\n")
		for i, v := range n.vals {
			if n.kind == "dict" {
				b.WriteString(ind + "\t<key>" + plistEscaper.Replace(n.keys[i]) + "</key>\n")
			}
			v.write(b, depth+1)
		}
		b.WriteString(ind + "</" + n.kind + ">\n")
	case "true", "false":
		b.WriteString(ind + "<" + n.kind + "/>\n")
	default:
		b.WriteString(ind + "<" + n.kind + ">" + plistEscaper.Replace(n.text) + "</" + n.kind + ">\n")
	}
}
