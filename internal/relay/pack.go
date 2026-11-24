package relay

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/obutora/stagent/internal/task"
)

// imageMember is images/<nnnn>.<ext>.
var imageMember = regexp.MustCompile(`^images/[0-9]{4,}\.(png|jpg|gif|webp)$`)

// Tree members, under TreeDir.
var treeFiles = []string{task.RelayBundle, task.RelayStaged, task.RelayUnstaged}

// allowed tells whether name is a package member and whether it may be a
// symbolic link.
func allowed(name string) (ok, link bool) {
	switch name {
	case ManifestFile, DocumentFile, MemoFile:
		return true, false
	}
	if imageMember.MatchString(name) {
		return true, false
	}
	for _, f := range treeFiles {
		if name == TreeDir+"/"+f {
			return true, false
		}
	}
	if rest, found := strings.CutPrefix(name, TreeDir+"/"+task.RelayUntracked+"/"); found && rest != "" {
		return true, true
	}
	return false, false
}

// cleanMember is name when it is relative, "/" separated and has no "."
// or ".." component or empty one.
func cleanMember(name string) (string, bool) {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, `\`) || filepath.VolumeName(name) != "" {
		return "", false
	}
	for _, c := range strings.Split(name, "/") {
		if c == "" || c == "." || c == ".." {
			return "", false
		}
	}
	return name, path.Clean(name) == name
}

// Pack writes the parts in dir — the manifest, the document, its images,
// the memo and the tree parts — as a gzip tar to out (mode 0600) and
// answers its size and SHA-256 (hex). progress is told the bytes written
// of total.
func Pack(dir, out string, progress func(done, total int64)) (int64, string, error) {
	type member struct {
		name string
		path string
		info fs.FileInfo
	}
	var members []member
	var total int64
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if ok, link := allowed(name); !ok || d.Type()&fs.ModeSymlink != 0 && !link {
			return nil // not a part (the package itself)
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && info.Mode()&fs.ModeSymlink == 0 {
			return nil
		}
		members = append(members, member{name, p, info})
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, "", err
	}
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, "", err
	}
	fail := func(err error) (int64, string, error) {
		f.Close()
		os.Remove(out)
		return 0, "", err
	}
	h := sha256.New()
	cw := &countWriter{w: io.MultiWriter(f, h)}
	gz := gzip.NewWriter(cw)
	tw := tar.NewWriter(gz)
	var done int64
	for _, m := range members {
		hdr := &tar.Header{Name: m.name, ModTime: m.info.ModTime(), Format: tar.FormatPAX}
		if m.info.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(m.path)
			if err != nil {
				return fail(err)
			}
			hdr.Typeflag, hdr.Linkname, hdr.Mode = tar.TypeSymlink, target, 0o777
		} else {
			hdr.Typeflag, hdr.Size, hdr.Mode = tar.TypeReg, m.info.Size(), 0o644
			if m.info.Mode()&0o111 != 0 {
				hdr.Mode = 0o755
			}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fail(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			src, err := os.Open(m.path)
			if err != nil {
				return fail(err)
			}
			_, err = io.Copy(tw, src)
			src.Close()
			if err != nil {
				return fail(err)
			}
		}
		done += m.info.Size()
		if progress != nil {
			progress(done, total)
		}
	}
	if err := errors.Join(tw.Close(), gz.Close()); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(out)
		return 0, "", err
	}
	if chmodOK {
		os.Chmod(out, 0o600)
	}
	return cw.n, hex.EncodeToString(h.Sum(nil)), nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// FileSHA256 is the size and SHA-256 (hex) of the file at p.
func FileSHA256(p string) (int64, string, error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// Unpack checks the package pkg and unpacks it into dest: every member a
// regular file of the table of PROTOCOL.md "Relay" (a symbolic link only
// under tree/untracked/), with a relative path free of ".."; manifest.json
// of version 1. Regular files are written owner-only (0700 with an
// executable bit). progress is told the package's bytes read.
func Unpack(pkg, dest string, progress func(done, total int64)) (*Manifest, error) {
	f, err := os.Open(pkg)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	cr := &countReader{r: f}
	gz, err := gzip.NewReader(cr)
	if err != nil {
		return nil, fmt.Errorf("not a gzip file: %w", err)
	}
	tr := tar.NewReader(gz)
	if err := MkdirPrivate(dest); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("bad package: %w", err)
		}
		name, ok := cleanMember(hdr.Name)
		if !ok {
			return nil, fmt.Errorf("bad member %q", hdr.Name)
		}
		okName, link := allowed(name)
		switch {
		case !okName:
			return nil, fmt.Errorf("unexpected member %q", name)
		case seen[name]:
			return nil, fmt.Errorf("member %q twice", name)
		case hdr.Typeflag == tar.TypeSymlink && !link,
			hdr.Typeflag != tar.TypeSymlink && hdr.Typeflag != tar.TypeReg:
			return nil, fmt.Errorf("member %q is not a regular file", name)
		}
		seen[name] = true
		to, err := safeJoin(dest, name)
		if err != nil {
			return nil, err
		}
		if err := MkdirPrivate(filepath.Dir(to)); err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeSymlink {
			if err := os.Symlink(hdr.Linkname, to); err != nil {
				// Windows without the privilege to make links: a file
				// holding the target, which the relay stage writes as is.
				if err := writeMember(to, strings.NewReader(hdr.Linkname), false); err != nil {
					return nil, err
				}
			}
		} else if err := writeMember(to, tr, hdr.Mode&0o111 != 0); err != nil {
			return nil, err
		}
		if progress != nil {
			progress(cr.n, st.Size())
		}
	}
	if !seen[ManifestFile] || !seen[DocumentFile] {
		return nil, errors.New("the package has no manifest or document")
	}
	return ReadManifest(dest)
}

// safeJoin is dest/name when no directory on the way inside dest is a
// symbolic link (one an earlier member made).
func safeJoin(dest, name string) (string, error) {
	cur := dest
	parts := strings.Split(name, "/")
	for _, c := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, c)
		if st, err := os.Lstat(cur); err == nil && !st.IsDir() {
			return "", fmt.Errorf("member %q is under a file or link", name)
		}
	}
	to := filepath.Join(cur, parts[len(parts)-1])
	if _, err := os.Lstat(to); err == nil {
		return "", fmt.Errorf("member %q twice", name)
	}
	return to, nil
}

func writeMember(to string, r io.Reader, exe bool) error {
	mode := fs.FileMode(0o600)
	if exe {
		mode = 0o700
	}
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if chmodOK {
		return os.Chmod(to, mode)
	}
	return nil
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// CopyParts copies the document's files (manifest, document, images, memo)
// from the source directory src to dest: a relay within one host.
func CopyParts(src, dest string) error {
	if err := MkdirPrivate(dest); err != nil {
		return err
	}
	for _, name := range []string{ManifestFile, DocumentFile, MemoFile} {
		if err := copyIfThere(filepath.Join(src, name), filepath.Join(dest, name)); err != nil {
			return err
		}
	}
	ents, err := os.ReadDir(filepath.Join(src, ImagesDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := MkdirPrivate(filepath.Join(dest, ImagesDir)); err != nil {
		return err
	}
	for _, e := range ents {
		if e.Type().IsRegular() {
			if err := copyIfThere(filepath.Join(src, ImagesDir, e.Name()), filepath.Join(dest, ImagesDir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyIfThere(from, to string) error {
	if _, err := os.Stat(from); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return copyPrivate(from, to, false)
}
