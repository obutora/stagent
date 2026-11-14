package task

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// squashScanMax bounds the commits of a base looked through for the one a
// squash merge made.
const squashScanMax = 200

// Merged reports whether removing a branch at commit head loses nothing
// its bases do not have. bases are branch names, each compared as origin's
// remote-tracking branch and as the local branch, whichever exist: head is
// an ancestor of one (a merge), or its changes are in one already (a
// squash or rebase merge). That is, `git merge-tree --write-tree` of the
// base and head comes out as the base's own tree; when the base changed
// the same lines since, every commit of a branch without merges has its
// patch in the base (`git cherry`), or merging head into the base's commit
// that touched exactly the files head changed comes out as that commit's
// tree. Changes nobody made (empty commits) are not merged. What git
// writes meanwhile goes to a temporary object directory: telling leaves
// nothing in the repository.
//
// An error means git could not tell: one before 2.38 has no merge-tree
// --write-tree.
func Merged(ctx context.Context, env []string, repo, head string, bases ...string) (bool, error) {
	var targets []string
	for _, b := range bases {
		for _, ref := range []string{"refs/remotes/origin/" + b, "refs/heads/" + b} {
			if oid := objectOf(ctx, env, repo, ref+"^{commit}"); oid != "" && !slices.Contains(targets, oid) {
				targets = append(targets, oid)
			}
		}
	}
	for _, t := range targets {
		if _, err := Git(ctx, env, repo, "merge-base", "--is-ancestor", head, t); err == nil {
			return true, nil
		}
	}
	m := &mergeCheck{ctx: ctx, env: env, repo: repo, head: head}
	defer m.close()
	for _, t := range targets {
		if ok, err := m.in(t); ok || err != nil {
			return ok, err
		}
	}
	return false, nil
}

// mergeCheck tells whether head's changes are in a base commit.
type mergeCheck struct {
	ctx        context.Context
	env        []string
	repo, head string
	scratch    []string // env with objects written to a temporary directory
	tmp        string
}

// in reports whether head's changes are in commit base (see Merged).
func (m *mergeCheck) in(base string) (bool, error) {
	mb := gitLine(m.ctx, m.env, m.repo, "merge-base", base, m.head)
	if mb == "" {
		return false, nil // unrelated or shallow histories prove nothing
	}
	files, err := Git(m.ctx, m.env, m.repo, "diff-tree", "-r", "--name-only", "--no-renames", mb, m.head)
	if err != nil || len(strings.TrimSpace(string(files))) == 0 {
		return false, nil
	}
	tree, clean, err := m.mergeTree(base)
	if err != nil {
		return false, err
	}
	if clean {
		return tree == objectOf(m.ctx, m.env, m.repo, base+"^{tree}"), nil
	}
	// The base changed some of the same lines since it took them in.
	if m.linear(base) && m.cherryPicked(base) {
		return true, nil
	}
	return m.squashed(mb, base, sortedLines(string(files)))
}

// mergeTree is `git merge-tree --write-tree base head`: the merged tree,
// and whether it merged without conflicts.
func (m *mergeCheck) mergeTree(base string) (tree string, clean bool, err error) {
	if m.scratch == nil {
		if err := m.open(); err != nil {
			return "", false, err
		}
	}
	out, err := Git(m.ctx, m.scratch, m.repo, "merge-tree", "--write-tree", base, m.head)
	tree, _, _ = strings.Cut(strings.TrimSpace(string(out)), "\n")
	var exit *exec.ExitError
	switch {
	case err == nil:
		return tree, true, nil
	case errors.As(err, &exit) && exit.ExitCode() == 1 && tree != "":
		return tree, false, nil // conflicts
	}
	return "", false, err
}

// open points new objects at a temporary directory, the repository's
// still read through GIT_ALTERNATE_OBJECT_DIRECTORIES.
func (m *mergeCheck) open() error {
	objects := gitLine(m.ctx, m.env, m.repo, "rev-parse", "--git-path", "objects")
	if objects == "" {
		return fmt.Errorf("git rev-parse --git-path objects: no answer in %s", m.repo)
	}
	if !filepath.IsAbs(objects) {
		objects = filepath.Join(m.repo, objects)
	}
	tmp, err := os.MkdirTemp("", "stagent-merged-")
	if err != nil {
		return err
	}
	m.tmp = tmp
	alt := objects
	if prev, ok := lookupEnv(m.env, "GIT_ALTERNATE_OBJECT_DIRECTORIES"); ok && prev != "" {
		alt += string(os.PathListSeparator) + prev
	}
	env := m.env
	if env == nil {
		env = os.Environ()
	}
	m.scratch = append(slices.Clip(env), "GIT_OBJECT_DIRECTORY="+tmp, "GIT_ALTERNATE_OBJECT_DIRECTORIES="+alt)
	return nil
}

func (m *mergeCheck) close() {
	if m.tmp != "" {
		os.RemoveAll(m.tmp)
	}
}

// linear reports whether head has no merge commits base lacks: git cherry
// passes over merges.
func (m *mergeCheck) linear(base string) bool {
	return gitLine(m.ctx, m.env, m.repo, "rev-list", "--count", "--merges", base+".."+m.head) == "0"
}

// cherryPicked reports whether each of head's commits base lacks has its
// patch in base (`git cherry` marks them all -).
func (m *mergeCheck) cherryPicked(base string) bool {
	out, err := Git(m.ctx, m.env, m.repo, "cherry", base, m.head)
	if err != nil {
		return false
	}
	lines := strings.Fields(string(out))
	for i := 0; i < len(lines); i += 2 {
		if lines[i] != "-" {
			return false
		}
	}
	return len(lines) > 0
}

// squashed reports whether a commit of base since mb (on its ancestry
// path, merges left out) that touches exactly files — head's changed files
// — takes head's changes in: merged with head it comes out as its own
// tree. Over squashScanMax such commits since mb are not looked through.
func (m *mergeCheck) squashed(mb, base, files string) (bool, error) {
	out, err := Git(m.ctx, m.env, m.repo, "log", "--no-show-signature", "--no-merges", "--ancestry-path",
		"--format=%x00%H", "--name-only", "--no-renames", fmt.Sprintf("--max-count=%d", squashScanMax+1), mb+".."+base)
	if err != nil {
		return false, nil
	}
	entries := strings.Split(string(out), "\x00")[1:]
	if len(entries) > squashScanMax {
		return false, nil
	}
	for _, e := range entries {
		commit, names, _ := strings.Cut(e, "\n")
		if sortedLines(names) != files {
			continue
		}
		tree, clean, err := m.mergeTree(commit)
		if err != nil {
			return false, err
		}
		if clean && tree == objectOf(m.ctx, m.env, m.repo, commit+"^{tree}") {
			return true, nil
		}
	}
	return false, nil
}

// sortedLines is s's non-blank lines, sorted, one per line.
func sortedLines(s string) string {
	var lines []string
	for l := range strings.Lines(s) {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

// objectOf is the object rev names in the checkout repo, "" when none.
func objectOf(ctx context.Context, env []string, repo, rev string) string {
	return gitLine(ctx, env, repo, "rev-parse", "--verify", "--quiet", rev)
}

// gitLine is git's trimmed output, "" when it fails.
func gitLine(ctx context.Context, env []string, repo string, args ...string) string {
	out, err := Git(ctx, env, repo, args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
