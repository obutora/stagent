package task

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// OrcaFile is Orca's per-repository settings file, at a checkout's root.
// stagent only reads it.
const OrcaFile = "orca.yaml"

// Orca refuses a file over 256 KiB and drops strings over 64 KiB.
const (
	orcaMaxFile   = 256 << 10
	orcaMaxString = 64 << 10
)

// Orca is what stagent uses of an orca.yaml. Which checkout's file counts
// depends on the key: scripts.setup comes from the task's worktree;
// scripts.archive and worktree.sharedDirectories from the source checkout
// (元のチェックアウト).
type Orca struct {
	Setup             string   // scripts.setup ("": none)
	Archive           string   // scripts.archive ("": none)
	SharedDirectories []string // worktree.sharedDirectories, as written
}

// OrcaError is an orca.yaml that cannot be used at all.
type OrcaError struct {
	Path string
	Err  error
}

func (e *OrcaError) Error() string { return e.Path + ": " + e.Err.Error() }

func (e *OrcaError) Unwrap() error { return e.Err }

// LoadOrca reads the orca.yaml in dir. No file is (nil, nil). Like Orca, a
// file that is larger than 256 KiB, is not valid YAML, repeats a key or is
// not a mapping is refused whole (*OrcaError); a value of the wrong type,
// or a string over 64 KiB, is dropped alone and the rest kept. Unknown
// keys are ignored.
func LoadOrca(dir string) (*Orca, error) {
	path := filepath.Join(dir, OrcaFile)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, &OrcaError{Path: path, Err: err}
	}
	if len(b) > orcaMaxFile {
		return nil, &OrcaError{Path: path, Err: fmt.Errorf("larger than %d KiB", orcaMaxFile>>10)}
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, &OrcaError{Path: path, Err: err}
	}
	o := &Orca{}
	if len(doc.Content) == 0 {
		return o, nil // empty, or comments only
	}
	root := deref(doc.Content[0])
	if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
		return o, nil
	}
	if root.Kind != yaml.MappingNode {
		return nil, &OrcaError{Path: path, Err: errors.New("not a mapping")}
	}
	if err := noDuplicateKeys(root, 0); err != nil {
		return nil, &OrcaError{Path: path, Err: err}
	}
	if scripts := field(root, "scripts"); scripts != nil {
		o.Setup = str(field(scripts, "setup"))
		o.Archive = str(field(scripts, "archive"))
	}
	if wt := field(root, "worktree"); wt != nil {
		if dirs := field(wt, "sharedDirectories"); dirs != nil && dirs.Kind == yaml.SequenceNode {
			for _, d := range dirs.Content {
				if s := str(d); s != "" {
					o.SharedDirectories = append(o.SharedDirectories, s)
				}
			}
		}
	}
	return o, nil
}

// deref follows an alias.
func deref(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

// field is the value of key in mapping m (nil: absent, or m is no
// mapping).
func field(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return deref(m.Content[i+1])
		}
	}
	return nil
}

// str is the string n holds: "" for anything else, and for one over 64
// KiB.
func str(n *yaml.Node) string {
	n = deref(n)
	if n == nil || n.Kind != yaml.ScalarNode || n.Tag != "!!str" || len(n.Value) > orcaMaxString {
		return ""
	}
	return n.Value
}

// noDuplicateKeys fails on a mapping, at any depth, that repeats a key.
func noDuplicateKeys(n *yaml.Node, depth int) error {
	if n == nil || depth > 64 {
		return nil
	}
	if n.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind == yaml.ScalarNode {
				if seen[k.Value] {
					return fmt.Errorf("line %d: key %q repeated", k.Line, k.Value)
				}
				seen[k.Value] = true
			}
		}
	}
	for _, c := range n.Content {
		if err := noDuplicateKeys(c, depth+1); err != nil {
			return err
		}
	}
	return nil
}
