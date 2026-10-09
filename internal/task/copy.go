package task

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/obutora/stagent/internal/wire"
)

// What a new worktree gets from its source checkout (#488): copies of the
// gitignored files the checkout's .worktreeinclude names (Orca's rules)
// and the app's patterns select (config.json repos.<repo>.worktree_include),
// and links to the gitignored directories orca.yaml's
// worktree.sharedDirectories names. The copy stage of `stagent task run`
// and task.preview resolve them with the same code.

// IncludeFile is Orca's list of paths to copy, at a checkout's root.
const IncludeFile = ".worktreeinclude"

const (
	maxIncludeSize  = 256 << 10 // a larger .worktreeinclude is skipped whole
	maxIncludeLines = 1000      // valid lines taken
	// maxArgBytes bounds the pathspecs of one git command line (Windows
	// takes 32 KiB in all; pathspec files need git 2.26).
	maxArgBytes = 16 << 10
	// MaxCopyBytes and MaxCopyEntries bound what one worktree gets; an
	// entry that would pass either is skipped (limit).
	MaxCopyBytes   = 2 << 30
	MaxCopyEntries = 50_000
)

// CopySpec is what a worktree of Repo gets copied and linked.
type CopySpec struct {
	// Repo is the source checkout (canonical).
	Repo string
	// Env is git's environment (nil: this process's).
	Env []string
	// Patterns are the app's (config.json repos.<Repo>.worktree_include).
	Patterns []string
	// Shared is orca.yaml's worktree.sharedDirectories of Repo.
	Shared []string
}

// CopyPlan is a resolved CopySpec, every shared directory taken as linked.
type CopyPlan struct {
	Copies     []wire.TaskCopy
	Links      []string // shared directories, relative with / separators
	Skipped    []wire.TaskSkip
	TotalBytes int64
}

// PlanCopies resolves spec: what a new worktree would get when every
// shared directory links. Only git failing is an error.
func PlanCopies(ctx context.Context, spec CopySpec) (*CopyPlan, error) {
	r, err := resolve(ctx, spec)
	if err != nil {
		return nil, err
	}
	copies, skipped, total := r.choose(r.shared)
	plan := &CopyPlan{Links: r.shared, Skipped: append(r.skipped, skipped...), TotalBytes: total}
	for _, c := range copies {
		plan.Copies = append(plan.Copies, wire.TaskCopy{Path: c.path, Source: c.source, Bytes: c.bytes})
	}
	return plan, nil
}

// Preview is task.preview for the source checkout repo (canonical) with
// the app's patterns: the plan, at most wire.TaskPreviewRows copies and
// skips, and the checkout's orca.yaml scripts.setup or why that file is
// unusable (its sharedDirectories then ignored).
func Preview(ctx context.Context, env []string, repo string, patterns []string) (*wire.TaskPreviewResult, error) {
	res := &wire.TaskPreviewResult{Repo: repo, Copies: []wire.TaskCopy{}, Links: []wire.TaskLink{}, Skipped: []wire.TaskSkip{}}
	spec := CopySpec{Repo: repo, Env: env, Patterns: patterns}
	o, err := LoadOrca(repo)
	switch {
	case err != nil:
		res.OrcaError = oneLine(err.Error())
	case o != nil:
		res.Setup, spec.Shared = o.Setup, o.SharedDirectories
	}
	plan, err := PlanCopies(ctx, spec)
	if err != nil {
		return nil, err
	}
	res.TotalBytes = plan.TotalBytes
	for _, l := range plan.Links {
		res.Links = append(res.Links, wire.TaskLink{Path: l})
	}
	res.Copies = append(res.Copies, plan.Copies[:min(len(plan.Copies), wire.TaskPreviewRows)]...)
	res.Skipped = append(res.Skipped, plan.Skipped[:min(len(plan.Skipped), wire.TaskPreviewRows)]...)
	res.Truncated = len(plan.Copies) > wire.TaskPreviewRows || len(plan.Skipped) > wire.TaskPreviewRows
	return res, nil
}

func oneLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// SavedPatterns reads the app's patterns for the source checkout repo
// (canonical) from config.json at configPath. Only the daemon writes that
// file (config.set); a missing or unreadable one has none.
func SavedPatterns(configPath, repo string) []string {
	b, err := os.ReadFile(configPath)
	if err != nil {
		return nil
	}
	var c wire.Config
	if json.Unmarshal(b, &c) != nil {
		return nil
	}
	for key, rc := range c.Repos {
		if SamePath(key, repo) {
			return rc.WorktreeInclude
		}
	}
	return nil
}

// ValidRepoKey reports why key cannot be a key of config.json's repos: it
// must be an absolute, clean path (no trailing separator). Whether it
// exists does not matter.
func ValidRepoKey(key string) error {
	switch {
	case !filepath.IsAbs(key):
		return errors.New("not an absolute path")
	case filepath.Clean(key) != key || os.IsPathSeparator(key[len(key)-1]):
		return errors.New("not a clean path")
	}
	return nil
}

// ValidPattern reports why p cannot be an app pattern (a git pathspec
// glob from the checkout's root): empty, a negation (!), absolute, or
// with a .. or .git component.
func ValidPattern(p string) error {
	switch {
	case p == "":
		return errors.New("empty")
	case strings.HasPrefix(p, "!"):
		return errors.New("negation is not supported")
	case isAbsLike(p):
		return errors.New("absolute")
	case hasBadComponent(p):
		return errors.New("contains .. or .git")
	}
	return nil
}

// isAbsLike: rooted, or with a drive letter, on any OS.
func isAbsLike(p string) bool {
	return filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) ||
		len(p) >= 2 && p[1] == ':' && ('a' <= p[0]|0x20 && p[0]|0x20 <= 'z')
}

func hasBadComponent(p string) bool {
	for _, c := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if c == ".." || strings.EqualFold(c, ".git") {
			return true
		}
	}
	return false
}

// candidate is an existing, gitignored, untracked entry to copy.
type candidate struct {
	path    string // relative, / separators
	source  string
	bytes   int64
	entries int // filesystem entries, itself included
}

type resolved struct {
	cands   []candidate
	skipped []wire.TaskSkip
	shared  []string // existing gitignored directories
}

func (r *resolved) skip(entry, source, reason string) {
	r.skipped = append(r.skipped, wire.TaskSkip{Entry: entry, Source: source, Reason: reason})
}

// resolve finds the candidates of spec: .worktreeinclude's lines first,
// then the app's patterns, each path once.
func resolve(ctx context.Context, spec CopySpec) (*resolved, error) {
	r := &resolved{}
	g := &gitIn{ctx: ctx, env: spec.Env, dir: spec.Repo}
	seen := map[string]bool{}
	// under reports whether p is (inside) a path already taken.
	under := func(p string) bool {
		for q := p; q != "." && q != "/"; q = path.Dir(q) {
			if seen[q] {
				return true
			}
		}
		return false
	}

	lines := r.includeLines(spec.Repo)
	var exist []string
	for _, p := range lines {
		if _, err := os.Lstat(r.abs(spec.Repo, p)); err != nil {
			r.skip(p, wire.TaskSourceInclude, wire.TaskSkipMissing)
			continue
		}
		exist = append(exist, p)
	}
	tracked, err := g.tracked(exist)
	if err != nil {
		return nil, err
	}
	var untracked []string
	for _, p := range exist {
		if tracked(p) {
			r.skip(p, wire.TaskSourceInclude, wire.TaskSkipTracked)
		} else {
			untracked = append(untracked, p)
		}
	}
	ignored, err := g.ignored(untracked)
	if err != nil {
		return nil, err
	}
	for _, p := range untracked {
		if !ignored[p] {
			r.skip(p, wire.TaskSourceInclude, wire.TaskSkipNotIgnored)
			continue
		}
		if under(p) {
			continue
		}
		if c, ok := measure(spec.Repo, p, wire.TaskSourceInclude); ok {
			seen[p] = true
			r.cands = append(r.cands, c)
		} else {
			r.skip(p, wire.TaskSourceInclude, wire.TaskSkipMissing)
		}
	}

	for _, pat := range spec.Patterns {
		if ValidPattern(pat) != nil {
			r.skip(pat, wire.TaskSourceApp, wire.TaskSkipInvalid)
			continue
		}
		ps := ":(glob)" + pat
		ign, err := g.list("ls-files", "-o", "-i", "--exclude-standard", "-z", "--", ps)
		if err != nil {
			return nil, err
		}
		trk, err := g.list("ls-files", "-c", "-z", "--", ps)
		if err != nil {
			return nil, err
		}
		oth, err := g.list("ls-files", "-o", "--exclude-standard", "-z", "--", ps)
		if err != nil {
			return nil, err
		}
		if len(ign)+len(trk)+len(oth) == 0 {
			r.skip(pat, wire.TaskSourceApp, wire.TaskSkipNoMatch)
			continue
		}
		for _, p := range trk {
			r.skip(p, wire.TaskSourceApp, wire.TaskSkipTracked)
		}
		for _, p := range oth {
			r.skip(p, wire.TaskSourceApp, wire.TaskSkipNotIgnored)
		}
		for _, p := range ign {
			if under(p) {
				continue
			}
			if c, ok := measure(spec.Repo, p, wire.TaskSourceApp); ok {
				seen[p] = true
				r.cands = append(r.cands, c)
			}
		}
	}

	// Shared directories: existing gitignored directories only; anything
	// else is passed over without a word, as Orca does. One inside another
	// listed one goes with it (the outer one links).
	var dirs []string
	for _, d := range spec.Shared {
		p, ok := normalizeInclude(d)
		if !ok || slicesContains(dirs, p) {
			continue
		}
		if fi, err := os.Stat(r.abs(spec.Repo, p)); err != nil || !fi.IsDir() {
			continue
		}
		dirs = append(dirs, p)
	}
	dirs = slices.DeleteFunc(slices.Clone(dirs), func(p string) bool {
		return slices.ContainsFunc(dirs, func(d string) bool { return strings.HasPrefix(p, d+"/") })
	})
	tracked, err = g.tracked(dirs)
	if err != nil {
		return nil, err
	}
	var free []string
	for _, d := range dirs {
		if !tracked(d) {
			free = append(free, d)
		}
	}
	if ignored, err = g.ignored(free); err != nil {
		return nil, err
	}
	for _, d := range free {
		if ignored[d] {
			r.shared = append(r.shared, d)
		}
	}
	return r, nil
}

func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func (r *resolved) abs(repo, p string) string { return filepath.Join(repo, filepath.FromSlash(p)) }

// includeLines reads the checkout's .worktreeinclude as Orca does: one
// literal path per line, blank and # lines ignored; glob, !, absolute, ..
// and .git lines skipped (invalid); a file over 256 KiB skipped whole;
// 1,000 valid lines at most (limit past them).
func (r *resolved) includeLines(repo string) []string {
	f, err := os.Open(filepath.Join(repo, IncludeFile))
	if err != nil {
		return nil
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || fi.Size() > maxIncludeSize || !fi.Mode().IsRegular() {
		r.skip(IncludeFile, wire.TaskSourceInclude, wire.TaskSkipInvalid)
		return nil
	}
	var lines []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxIncludeSize+1)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p, ok := normalizeInclude(line)
		switch {
		case !ok:
			r.skip(line, wire.TaskSourceInclude, wire.TaskSkipInvalid)
		case seen[p]:
		case len(lines) == maxIncludeLines:
			r.skip(p, wire.TaskSourceInclude, wire.TaskSkipLimit)
		default:
			seen[p] = true
			lines = append(lines, p)
		}
	}
	return lines
}

// normalizeInclude makes a .worktreeinclude line (or a shared directory)
// a relative path with / separators, without ./ before or / after. A
// glob (* ? [), !, absolute, .. or .git one is not taken.
func normalizeInclude(line string) (string, bool) {
	p := strings.ReplaceAll(line, `\`, "/")
	if strings.HasPrefix(p, "!") || strings.ContainsAny(p, "*?[") || isAbsLike(p) || hasBadComponent(p) {
		return "", false
	}
	for strings.HasPrefix(p, "./") {
		p = strings.TrimLeft(p[2:], "/")
	}
	p = strings.TrimRight(p, "/")
	if p == "" || p == "." {
		return "", false
	}
	return path.Clean(p), true
}

// measure sizes the entry p of repo: a top-level symbolic link counts as
// what it points to (it is copied as that), links inside a directory as
// links. Counting stops past the limits: the entry cannot fit anyway.
func measure(repo, p, source string) (candidate, bool) {
	c := candidate{path: p, source: source}
	root := filepath.Join(repo, filepath.FromSlash(p))
	fi, err := os.Stat(root)
	if err != nil {
		return c, false
	}
	if !fi.IsDir() {
		c.bytes, c.entries = fi.Size(), 1
		return c, true
	}
	// A top-level link to a directory is walked as that directory.
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return c, false
	}
	stop := errors.New("stop")
	filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		c.entries++
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				c.bytes += info.Size()
			}
		}
		if c.entries > MaxCopyEntries || c.bytes > MaxCopyBytes {
			return stop
		}
		return nil
	})
	return c, true
}

// choose takes the candidates in order, but not those inside a linked
// directory (under_link) nor one that would pass the limits (limit).
func (r *resolved) choose(linked []string) (copies []candidate, skipped []wire.TaskSkip, total int64) {
	entries := 0
	for _, c := range r.cands {
		if within(c.path, linked) {
			skipped = append(skipped, wire.TaskSkip{Entry: c.path, Source: c.source, Reason: wire.TaskSkipUnderLink})
			continue
		}
		if total+c.bytes > MaxCopyBytes || entries+c.entries > MaxCopyEntries {
			skipped = append(skipped, wire.TaskSkip{Entry: c.path, Source: c.source, Reason: wire.TaskSkipLimit})
			continue
		}
		total += c.bytes
		entries += c.entries
		copies = append(copies, c)
	}
	return copies, skipped, total
}

// within reports whether p is one of dirs or inside one.
func within(p string, dirs []string) bool {
	for _, d := range dirs {
		if p == d || strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}

// gitIn runs git in the source checkout.
type gitIn struct {
	ctx context.Context
	env []string
	dir string
}

// list runs a git command printing NUL-separated paths.
func (g *gitIn) list(args ...string) ([]string, error) {
	out, err := Git(g.ctx, g.env, g.dir, args...)
	if err != nil {
		return nil, err
	}
	return splitNUL(out), nil
}

func splitNUL(b []byte) []string {
	var out []string
	for _, f := range bytes.Split(b, []byte{0}) {
		if len(f) > 0 {
			out = append(out, string(f))
		}
	}
	return out
}

// tracked tells which of paths git tracks: a file it tracks, or a
// directory holding one. The paths go to git in command lines of at most
// maxArgBytes.
func (g *gitIn) tracked(paths []string) (func(string) bool, error) {
	var files []string
	for len(paths) > 0 {
		args := []string{"ls-files", "-c", "-z", "--"}
		n, size := 0, 0
		for ; n < len(paths); n++ {
			ps := ":(literal)" + paths[n]
			if n > 0 && size+len(ps)+1 > maxArgBytes {
				break
			}
			size += len(ps) + 1
			args = append(args, ps)
		}
		paths = paths[n:]
		out, err := g.list(args...)
		if err != nil {
			return nil, err
		}
		files = append(files, out...)
	}
	return func(p string) bool {
		for _, f := range files {
			if f == p || strings.HasPrefix(f, p+"/") {
				return true
			}
		}
		return false
	}, nil
}

// ignored tells which of the untracked paths are gitignored
// (`git check-ignore`).
func (g *gitIn) ignored(paths []string) (map[string]bool, error) {
	set := map[string]bool{}
	if len(paths) == 0 {
		return set, nil
	}
	var in bytes.Buffer
	for _, p := range paths {
		in.WriteString(p)
		in.WriteByte(0)
	}
	bin, err := lookProgram(g.env, "git")
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(g.ctx, bin, gitArgs([]string{"check-ignore", "-z", "--stdin"})...)
	cmd.Dir, cmd.Env, cmd.Stdin = g.dir, g.env, &in
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	// Exit 1: none of them is ignored.
	var ee *exec.ExitError
	if err != nil && !(errors.As(err, &ee) && ee.ExitCode() == 1) {
		return nil, &GitError{Args: cmd.Args[1:], Stderr: oneLine(stderr.String()), Err: err}
	}
	for _, p := range splitNUL(out) {
		set[p] = true
	}
	return set, nil
}

// CopyJob is the copy stage: spec's links and copies made in Dest, a new
// worktree of spec.Repo.
type CopyJob struct {
	CopySpec
	Dest string
	// Out (the terminal) gets one line per link, copy and skip.
	Out io.Writer
	// Link makes link point to the directory target; nil: LinkDir.
	Link func(target, link string) error
}

// logLines bounds the lines of one kind the copy stage prints.
const logLines = 100

// CopyFiles links the shared directories and then copies what is to be
// copied into j.Dest, leaving every destination that exists as it is (a
// retry copies only what is missing), and nothing under a link. A shared
// directory that cannot be linked gets one warning line; what is listed
// under it is copied as ordinary files instead. Nothing here fails the
// task: errors are lines in the terminal.
func CopyFiles(ctx context.Context, j CopyJob) {
	out := j.Out
	if out == nil {
		out = io.Discard
	}
	link := j.Link
	if link == nil {
		link = LinkDir
	}
	logf := func(format string, args ...any) { fmt.Fprintf(out, "[stagent] "+format+"\n", args...) }
	r, err := resolve(ctx, j.CopySpec)
	if err != nil {
		logf("warning: no files copied: %v", err)
		return
	}

	var linked, failed []string
	var linkErr error
	for _, d := range r.shared {
		dst := r.abs(j.Dest, d)
		if fi, err := os.Lstat(dst); err == nil {
			if _, err := os.Readlink(dst); err == nil {
				linked = append(linked, d)
			} else if fi.IsDir() {
				logf("%s: already there, not linked", d)
			} else {
				logf("%s: already there", d)
			}
			continue
		}
		err := os.MkdirAll(filepath.Dir(dst), 0o755)
		if err == nil {
			err = link(r.abs(j.Repo, d), dst)
		}
		if err != nil {
			failed, linkErr = append(failed, d), err
			continue
		}
		linked = append(linked, d)
		logf("linked %s", d)
	}
	if len(failed) > 0 {
		logf("warning: could not link %s (%v); copying what is listed under it instead", strings.Join(failed, ", "), linkErr)
	}

	copies, skipped, _ := r.choose(linked)
	shown := 0
	for _, s := range append(r.skipped, skipped...) {
		text, warn := skipText(s.Reason)
		if text == "" {
			continue
		}
		if shown++; shown > logLines {
			continue
		}
		prefix := ""
		if warn {
			prefix = "warning: "
		}
		logf("%sskipped %s (%s): %s", prefix, s.Entry, sourceText(s.Source), text)
	}
	if shown > logLines {
		logf("… %d more skipped", shown-logLines)
	}
	files, done, existed := 0, 0, 0
	for _, c := range copies {
		n, err := copyEntry(r.abs(j.Repo, c.path), r.abs(j.Dest, c.path))
		files += n
		switch {
		case err != nil:
			logf("warning: copying %s: %v", c.path, err)
		case n == 0:
			if existed++; existed <= logLines {
				logf("skipped %s (%s): already there", c.path, sourceText(c.source))
			}
		default:
			if done++; done <= logLines {
				logf("copied %s", c.path)
			}
		}
	}
	if done > logLines {
		logf("… %d more copied", done-logLines)
	}
	if existed > logLines {
		logf("… %d more already there", existed-logLines)
	}
	if len(copies) > 0 {
		logf("files copied: %d", files)
	}
}

func sourceText(source string) string {
	if source == wire.TaskSourceInclude {
		return IncludeFile
	}
	return source
}

// skipText is the terminal's words for a skip ("" for none: tracked
// files are skipped silently) and whether it is a warning.
func skipText(reason string) (string, bool) {
	switch reason {
	case wire.TaskSkipNotIgnored:
		return "not gitignored", true
	case wire.TaskSkipMissing:
		return "not found", false
	case wire.TaskSkipNoMatch:
		return "matches nothing", false
	case wire.TaskSkipUnderLink:
		return "inside a shared directory", false
	case wire.TaskSkipInvalid:
		return "not a path stagent copies", true
	case wire.TaskSkipLimit:
		return "over the limit of 2 GiB / 50,000 entries", true
	}
	return "", false
}

// copyEntry copies src to dst: a file, or a directory merged into what is
// at dst. A top-level symbolic link is copied as what it points to, links
// inside a directory as links. What exists at a destination is left alone.
// It returns how many files and links it wrote.
func copyEntry(src, dst string) (int, error) {
	fi, err := os.Stat(src)
	if err != nil {
		return 0, err
	}
	if !fi.IsDir() {
		if _, err := os.Lstat(dst); err == nil {
			return 0, nil
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return 0, err
		}
		if err := copyFile(src, dst, fi.Mode().Perm()); err != nil {
			return 0, err
		}
		return 1, nil
	}
	if src, err = filepath.EvalSymlinks(src); err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			mode := fs.FileMode(0o755)
			if info, err := d.Info(); err == nil {
				mode = info.Mode().Perm() | 0o700
			}
			if err := os.MkdirAll(target, mode); err != nil {
				errs = append(errs, err)
				return fs.SkipDir
			}
			return nil
		}
		if _, err := os.Lstat(target); err == nil {
			return nil
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			to, err := os.Readlink(p)
			if err == nil {
				err = os.Symlink(to, target)
			}
			if err != nil {
				errs = append(errs, err)
				return nil
			}
		case d.Type().IsRegular():
			info, err := d.Info()
			if err == nil {
				err = copyFile(p, target, info.Mode().Perm())
			}
			if err != nil {
				errs = append(errs, err)
				return nil
			}
		default:
			return nil // sockets, pipes, devices
		}
		n++
		return nil
	})
	return n, joinLine(errs)
}

// joinLine is errs as one line ("" → nil).
func joinLine(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	msgs := make([]string, len(errs))
	for i, err := range errs {
		msgs[i] = oneLine(err.Error())
	}
	return errors.New(strings.Join(msgs, "; "))
}

// copyFile writes src's content to dst through a temporary file renamed
// into place, so an interrupted copy leaves no partial dst behind.
func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".stagent-*")
	if err != nil {
		return err
	}
	_, err = io.Copy(tmp, in)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && runtime.GOOS != "windows" {
		err = os.Chmod(tmp.Name(), perm)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// firstLink makes link point to target with the first way that works, or
// fails with every way's error.
func firstLink(target, link string, ways ...func(target, link string) error) error {
	var errs []error
	for _, way := range ways {
		err := way(target, link)
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	return joinLine(errs)
}
