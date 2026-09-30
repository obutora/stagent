package install

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/version"
)

const manifestVersion = 1

// Manifest is the installation ledger (layout.Manifest): everything stagent
// created or changed on the host, so that uninstall can take it back out.
type Manifest struct {
	ManifestVersion int    `json:"manifest_version"`
	Version         string `json:"version"` // stagent version that last wrote it
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`

	Layout     LayoutInfo `json:"layout"`
	DaemonAddr string     `json:"daemon_addr"` // actual socket / pipe location

	Files    []FileEntry    `json:"files"`    // files created by install (the binary)
	Dirs     []string       `json:"dirs"`     // directories created by install
	Configs  []*ConfigEntry `json:"configs"`  // integration targets created or modified
	Services []ServiceEntry `json:"services"` // login services / scheduled tasks
	// Backups are the pre-edit copies of modified config files. They stay
	// after unhook and are deleted by purge.
	Backups []string `json:"backups"`
}

// LayoutInfo is the part of the layout the app shows and the uninstaller
// needs even if the home directory changes.
type LayoutInfo struct {
	Root            string `json:"root"`
	Bin             string `json:"bin"`
	RunDir          string `json:"run_dir"`
	DataDir         string `json:"data_dir"`
	DataOnNetworkFS bool   `json:"data_on_network_fs"`
}

func layoutInfo(l *paths.Layout) LayoutInfo {
	return LayoutInfo{Root: l.Root, Bin: l.Bin, RunDir: l.RunDir, DataDir: l.DataDir, DataOnNetworkFS: l.DataOnNetworkFS}
}

type FileEntry struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	SHA256 string `json:"sha256,omitempty"`
}

// ConfigEntry records one integration target. For files we own entirely
// (omp extension, fish wrapper, service unit) Owned is set and removal
// deletes the file; for shared config files our elements are identified by
// Identify and removed element-wise, unless the file is unchanged since our
// last edit and a valid backup exists, in which case it is restored.
type ConfigEntry struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Owned    bool   `json:"owned,omitempty"`
	Created  bool   `json:"created"` // the file did not exist before
	Identify string `json:"identify"`

	SHA256Before string `json:"sha256_before,omitempty"`
	SHA256After  string `json:"sha256_after"`
	// Backup restores the file to its pre-stagent content. Cleared when the
	// file was changed by someone else between two of our edits.
	Backup string `json:"backup,omitempty"`

	// CreatedContainers are JSON pointers (e.g. "/hooks/Stop") of objects
	// and arrays we added; they are removed again once empty.
	CreatedContainers []string   `json:"created_containers,omitempty"`
	TOMLEdits         []TOMLEdit `json:"toml_edits,omitempty"`
	ModifiedAt        int64      `json:"modified_at"`
}

// TOMLEdit records a line-level change to Codex's config.toml.
type TOMLEdit struct {
	Kind         string `json:"kind"`   // features_hooks | notify
	Action       string `json:"action"` // added | changed
	PreviousLine string `json:"previous_line,omitempty"`
	AddedHeader  bool   `json:"added_header,omitempty"` // we also added [features]
	AddedBlank   bool   `json:"added_blank,omitempty"`  // and a blank line before it
}

type ServiceEntry struct {
	Kind   string `json:"kind"` // systemd | launchd | schtasks
	Name   string `json:"name"` // unit / label / task name
	Path   string `json:"path,omitempty"`
	SHA256 string `json:"sha256,omitempty"` // task definition (schtasks)
}

func newManifest(l *paths.Layout, now int64) *Manifest {
	return &Manifest{
		ManifestVersion: manifestVersion,
		Version:         version.Version,
		CreatedAt:       now,
		Layout:          layoutInfo(l),
		DaemonAddr:      l.DaemonAddr,
	}
}

// loadManifest reads the manifest; ok is false when there is none.
func loadManifest(path string) (m *Manifest, ok bool, err error) {
	b, err := readOptional(path)
	if err != nil {
		return nil, false, err
	}
	if b == nil {
		return nil, false, nil
	}
	m = &Manifest{}
	if err := json.Unmarshal(b, m); err != nil {
		return nil, false, fmt.Errorf("manifest %s: %w", path, err)
	}
	return m, true, nil
}

func (m *Manifest) save(path string, now int64) error {
	m.ManifestVersion = manifestVersion
	m.Version = version.Version
	m.UpdatedAt = now
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return err
	}
	return atomicWrite(path, buf.Bytes(), 0o600)
}

func (m *Manifest) config(id, path string) *ConfigEntry {
	for _, c := range m.Configs {
		if c.ID == id && c.Path == path {
			return c
		}
	}
	return nil
}

func (m *Manifest) dropConfig(c *ConfigEntry) {
	m.Configs = slices.DeleteFunc(m.Configs, func(x *ConfigEntry) bool { return x == c })
}

func (m *Manifest) addDir(d string) {
	if !slices.Contains(m.Dirs, d) {
		m.Dirs = append(m.Dirs, d)
	}
}

func (m *Manifest) setFile(f FileEntry) {
	for i := range m.Files {
		if m.Files[i].Path == f.Path {
			m.Files[i] = f
			return
		}
	}
	m.Files = append(m.Files, f)
}

func (m *Manifest) setService(s ServiceEntry) {
	for i := range m.Services {
		if m.Services[i].Kind == s.Kind && m.Services[i].Name == s.Name {
			m.Services[i] = s
			return
		}
	}
	m.Services = append(m.Services, s)
}

func (m *Manifest) dropService(kind string) {
	m.Services = slices.DeleteFunc(m.Services, func(s ServiceEntry) bool { return s.Kind == kind })
}

func (m *Manifest) hasService(kind string) bool {
	return slices.ContainsFunc(m.Services, func(s ServiceEntry) bool { return s.Kind == kind })
}

func addUnique(list []string, items ...string) []string {
	for _, it := range items {
		if !slices.Contains(list, it) {
			list = append(list, it)
		}
	}
	return list
}
