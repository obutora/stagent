//go:build !windows

package task

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

// writeFiles writes path → content under root, making directories.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// copyRepo is a checkout that tracks .gitignore (ignoring the patterns
// given) and the tracked files, with the untracked files written beside.
func copyRepo(t *testing.T, ignore string, tracked, untracked map[string]string) string {
	t.Helper()
	_, repo := newRepo(t)
	writeFiles(t, repo, map[string]string{".gitignore": ignore})
	writeFiles(t, repo, tracked)
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "tracked")
	writeFiles(t, repo, untracked)
	return repo
}

func plan(t *testing.T, spec CopySpec) *CopyPlan {
	t.Helper()
	p, err := PlanCopies(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func sortedSkips(s []wire.TaskSkip) []wire.TaskSkip {
	s = slices.Clone(s)
	slices.SortFunc(s, func(a, b wire.TaskSkip) int {
		return cmp.Or(cmp.Compare(a.Source, b.Source), cmp.Compare(a.Entry, b.Entry), cmp.Compare(a.Reason, b.Reason))
	})
	return s
}

func checkPlan(t *testing.T, got *CopyPlan, copies []wire.TaskCopy, skipped []wire.TaskSkip) {
	t.Helper()
	if !slices.Equal(got.Copies, copies) {
		t.Errorf("copies\n%+v\nwant\n%+v", got.Copies, copies)
	}
	if g, w := sortedSkips(got.Skipped), sortedSkips(skipped); !slices.Equal(g, w) {
		t.Errorf("skipped\n%+v\nwant\n%+v", g, w)
	}
}

// .worktreeinclude, as Orca reads it: one literal path per line from the
// root (./ and a trailing / dropped, repeats once), # and blank lines
// ignored; glob, !, absolute, .. and .git lines skipped as invalid. Only
// existing gitignored paths are copied, a directory whole; tracked,
// untracked-but-not-ignored and missing paths are skipped with their
// reason.
func TestPlanCopiesWorktreeInclude(t *testing.T) {
	repo := copyRepo(t, ".env\n*.local.json\nsecrets/\n",
		map[string]string{"tracked.txt": "t", ".worktreeinclude": `# files the worktree needs

.env
./config/app.local.json
secrets/
tracked.txt
notes.txt
absent.env
*.env
[ab].env
!keep
/etc/passwd
../outside
.git/config
.env
`},
		map[string]string{".env": "KEY=1\n", "config/app.local.json": "{}", "secrets/a": "aaa", "secrets/deep/b": "bb", "notes.txt": "n"})

	got := plan(t, CopySpec{Repo: repo})
	checkPlan(t, got, []wire.TaskCopy{
		{Path: ".env", Source: "include", Bytes: 6},
		{Path: "config/app.local.json", Source: "include", Bytes: 2},
		{Path: "secrets", Source: "include", Bytes: 5},
	}, []wire.TaskSkip{
		{Entry: "tracked.txt", Source: "include", Reason: "tracked"},
		{Entry: "notes.txt", Source: "include", Reason: "not_ignored"},
		{Entry: "absent.env", Source: "include", Reason: "missing"},
		{Entry: "*.env", Source: "include", Reason: "invalid"},
		{Entry: "[ab].env", Source: "include", Reason: "invalid"},
		{Entry: "!keep", Source: "include", Reason: "invalid"},
		{Entry: "/etc/passwd", Source: "include", Reason: "invalid"},
		{Entry: "../outside", Source: "include", Reason: "invalid"},
		{Entry: ".git/config", Source: "include", Reason: "invalid"},
	})
	if got.TotalBytes != 13 || len(got.Links) != 0 {
		t.Fatalf("total %d, links %q", got.TotalBytes, got.Links)
	}
}

// A .worktreeinclude over 256 KiB is skipped whole; past 1,000 valid lines
// the rest are skipped (limit).
func TestPlanCopiesWorktreeIncludeBounds(t *testing.T) {
	repo := copyRepo(t, "*.env\n", nil, map[string]string{
		".worktreeinclude": ".env\n" + strings.Repeat("#"+strings.Repeat("x", 1023)+"\n", 256),
		".env":             "x",
	})
	checkPlan(t, plan(t, CopySpec{Repo: repo}), nil, []wire.TaskSkip{{Entry: ".worktreeinclude", Source: "include", Reason: "invalid"}})

	var lines strings.Builder
	for i := range 1000 {
		fmt.Fprintf(&lines, "gone-%d\n", i)
	}
	lines.WriteString("a.env\n")
	repo = copyRepo(t, "*.env\n", nil, map[string]string{".worktreeinclude": lines.String(), "a.env": "x"})
	got := plan(t, CopySpec{Repo: repo})
	if len(got.Copies) != 0 || len(got.Skipped) != 1001 || !slices.Contains(got.Skipped, wire.TaskSkip{Entry: "a.env", Source: "include", Reason: "limit"}) {
		t.Fatalf("copies %+v, %d skipped, last %+v", got.Copies, len(got.Skipped), got.Skipped[len(got.Skipped)-1])
	}
}

// The app's patterns are git pathspec globs from the root: * ? [] and **
// match, case-sensitively; a directory selects everything in it. Only
// gitignored untracked files are copied; tracked and not-ignored matches
// are skipped file by file, a pattern matching nothing as no_match, and
// an invalid pattern as invalid. A path both sources select is copied
// once, as .worktreeinclude's.
func TestPlanCopiesAppPatterns(t *testing.T) {
	repo := copyRepo(t, ".env*\n*.local\n*.LOCAL\nbuild/\n",
		map[string]string{"README.md": "r", "src/main.go": "package main", ".worktreeinclude": ".env\n"},
		map[string]string{
			".env": "e", ".env.test": "tt", "a/b/c.local": "ccc", "a/B.LOCAL": "B", "top.local": "pp",
			"build/out/app.bin": "0123456789", "build/log.txt": "l", "todo.txt": "todo", "cfg1.local": "1", "cfg2.local": "2",
		})
	got := plan(t, CopySpec{Repo: repo, Patterns: []string{
		".env*", "**/*.local", "build", "cfg[1].local", "nothing*", "README.md", "todo.txt", "src", "!x", "../x", "",
	}})
	checkPlan(t, got, []wire.TaskCopy{
		{Path: ".env", Source: "include", Bytes: 1},
		{Path: ".env.test", Source: "app", Bytes: 2},
		{Path: "a/b/c.local", Source: "app", Bytes: 3},
		{Path: "cfg1.local", Source: "app", Bytes: 1},
		{Path: "cfg2.local", Source: "app", Bytes: 1},
		{Path: "top.local", Source: "app", Bytes: 2},
		{Path: "build/log.txt", Source: "app", Bytes: 1},
		{Path: "build/out/app.bin", Source: "app", Bytes: 10},
	}, []wire.TaskSkip{
		{Entry: "nothing*", Source: "app", Reason: "no_match"},
		{Entry: "README.md", Source: "app", Reason: "tracked"},
		{Entry: "todo.txt", Source: "app", Reason: "not_ignored"},
		{Entry: "src/main.go", Source: "app", Reason: "tracked"},
		{Entry: "!x", Source: "app", Reason: "invalid"},
		{Entry: "../x", Source: "app", Reason: "invalid"},
		{Entry: "", Source: "app", Reason: "invalid"},
	})
	if got.TotalBytes != 21 {
		t.Fatalf("total %d", got.TotalBytes)
	}
}

// Shared directories: orca.yaml's that exist, are directories and are
// gitignored are linked (others passed over silently); whatever either
// source lists inside one is skipped as under_link.
func TestPlanCopiesSharedDirectories(t *testing.T) {
	repo := copyRepo(t, "node_modules/\n.cache/\nfile.dat\n",
		map[string]string{"src/a.go": "package a", ".worktreeinclude": "node_modules/pkg\n.cache/x\n"},
		map[string]string{"node_modules/pkg/index.js": "js", "node_modules/other/o.js": "o", ".cache/x": "xx", "file.dat": "d"})
	got := plan(t, CopySpec{Repo: repo, Patterns: []string{"node_modules/other"}, Shared: []string{"node_modules/", "src", "missing", "file.dat", "../up", "node_modules"}})
	if !slices.Equal(got.Links, []string{"node_modules"}) {
		t.Fatalf("links %q", got.Links)
	}
	checkPlan(t, got, []wire.TaskCopy{{Path: ".cache/x", Source: "include", Bytes: 2}}, []wire.TaskSkip{
		{Entry: "node_modules/pkg", Source: "include", Reason: "under_link"},
		{Entry: "node_modules/other/o.js", Source: "app", Reason: "under_link"},
	})
}

// Every shared directory is taken, however many; one inside another
// listed one goes with it, whichever comes first.
func TestPlanCopiesSharedDirectoriesNestedAndMany(t *testing.T) {
	untracked := map[string]string{"node_modules/cache/c": "c", "node_modules/m.js": "m"}
	var shared, want []string
	for i := range 120 {
		d := fmt.Sprintf("dep%03d", i)
		untracked[d+"/x"] = "x"
		shared = append(shared, d)
		want = append(want, d)
	}
	repo := copyRepo(t, "node_modules/\ndep*/\n", nil, untracked)
	got := plan(t, CopySpec{Repo: repo, Shared: append([]string{"node_modules/cache", "node_modules"}, shared...)})
	if want = append([]string{"node_modules"}, want...); !slices.Equal(got.Links, want) {
		t.Fatalf("links %q\nwant %q", got.Links, want)
	}
}

// Many long shared directories are resolved whole: git is not handed one
// command line too long for the system.
func TestPlanCopiesLongSharedDirectories(t *testing.T) {
	deep := ""
	for i := range 14 {
		deep = path.Join(deep, fmt.Sprintf("%02d", i)+strings.Repeat("d", 198))
	}
	untracked := map[string]string{}
	var shared []string
	for i := range 1000 {
		d := fmt.Sprintf("%s/s%03d.cache", deep, i)
		untracked[d+"/x"] = "x"
		shared = append(shared, d)
	}
	repo := copyRepo(t, "*.cache/\n", nil, untracked)
	if got := plan(t, CopySpec{Repo: repo, Shared: shared}); !slices.Equal(got.Links, shared) {
		t.Fatalf("%d links", len(got.Links))
	}
}

// Copies stop at 2 GiB: an entry that would pass it is skipped (limit),
// later ones that fit are still copied.
func TestPlanCopiesLimit(t *testing.T) {
	repo := copyRepo(t, "*.bin\n", nil, map[string]string{"small.bin": "s", ".worktreeinclude": "huge.bin\nsmall.bin\n"})
	f, err := os.Create(filepath.Join(repo, "huge.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxCopyBytes + 1); err != nil { // sparse
		t.Fatal(err)
	}
	f.Close()
	checkPlan(t, plan(t, CopySpec{Repo: repo}),
		[]wire.TaskCopy{{Path: "small.bin", Source: "include", Bytes: 1}},
		[]wire.TaskSkip{{Entry: "huge.bin", Source: "include", Reason: "limit"}})
}

// A shared directory that cannot be linked (on Windows: neither junction
// nor directory symlink) gets one warning line, and what is listed under
// it is copied as ordinary files.
func TestCopyFilesLinkFallback(t *testing.T) {
	repo := copyRepo(t, "node_modules/\n.cache/\n", map[string]string{".worktreeinclude": "node_modules/pkg\n.cache/a\n"},
		map[string]string{"node_modules/pkg/index.js": "js", "node_modules/big/huge.js": "h", ".cache/a/x": "x", ".cache/b/y": "y"})
	dest := filepath.Join(filepath.Dir(repo), "repo-1")
	var out bytes.Buffer
	CopyFiles(context.Background(), CopyJob{
		CopySpec: CopySpec{Repo: repo, Shared: []string{"node_modules", ".cache"}},
		Dest:     dest, Out: &out,
		Link: func(target, link string) error {
			return errors.New("junction: access denied; symlink: privilege not held")
		},
	})
	if got := readFile(t, filepath.Join(dest, "node_modules/pkg/index.js")); got != "js" {
		t.Fatalf("index.js %q", got)
	}
	if fi, err := os.Lstat(filepath.Join(dest, "node_modules")); err != nil || !fi.IsDir() {
		t.Fatalf("node_modules: %v %v", fi, err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "node_modules/big")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an unlisted directory was copied: %v", err)
	}
	if n := strings.Count(out.String(), "warning: could not link"); n != 1 || !strings.Contains(out.String(), "node_modules, .cache") {
		t.Fatalf("%d warning lines:\n%s", n, out.String())
	}
}

// A listed symbolic link is copied as what it points to; links inside a
// copied directory stay links.
func TestCopyFilesSymlinks(t *testing.T) {
	repo := copyRepo(t, "data\nlinked\n", map[string]string{".worktreeinclude": "linked\n"}, map[string]string{"data/a": "A"})
	if err := os.Symlink("a", filepath.Join(repo, "data", "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(repo, "data"), filepath.Join(repo, "linked")); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(filepath.Dir(repo), "repo-2")
	CopyFiles(context.Background(), CopyJob{CopySpec: CopySpec{Repo: repo}, Dest: dest})
	if fi, err := os.Lstat(filepath.Join(dest, "linked")); err != nil || !fi.IsDir() {
		t.Fatalf("linked: %v %v", fi, err)
	}
	if got := readFile(t, filepath.Join(dest, "linked", "a")); got != "A" {
		t.Fatalf("a %q", got)
	}
	if to, err := os.Readlink(filepath.Join(dest, "linked", "alias")); err != nil || to != "a" {
		t.Fatalf("alias -> %q (%v)", to, err)
	}
}

// The copy stage of a worktree task: shared directories linked, the
// included and the app's files copied. Prepared again, it copies only
// what is missing and leaves what is there.
func TestPrepareCopies(t *testing.T) {
	f := newFixture(t, map[string]string{
		".gitignore":       ".env\nnode_modules/\n*.local\n",
		".worktreeinclude": ".env\nnode_modules/pkg\n",
		"orca.yaml":        "worktree:\n  sharedDirectories:\n    - node_modules\n",
	})
	writeFiles(t, f.repo, map[string]string{".env": "KEY=1\n", "node_modules/pkg/index.js": "js", "config/dev.local": "dev"})
	tk := f.task(t, wire.TaskKindIssue, 5, "5-copy")
	p := Prep{Task: tk, WorktreeInclude: []string{"config/*.local"}}
	stages, out, err := f.prepare(t, p)
	if err != nil {
		t.Fatalf("Prepare: %v\n%s", err, out)
	}
	if !slices.Contains(stages, "copy") {
		t.Fatalf("stages %q", stages)
	}
	wt := tk.Worktree
	if got := readFile(t, filepath.Join(wt, ".env")); got != "KEY=1\n" {
		t.Fatalf(".env %q", got)
	}
	if got := readFile(t, filepath.Join(wt, "config/dev.local")); got != "dev" {
		t.Fatalf("dev.local %q", got)
	}
	if to, err := os.Readlink(filepath.Join(wt, "node_modules")); err != nil || to != filepath.Join(f.repo, "node_modules") {
		t.Fatalf("node_modules links to %q (%v)\n%s", to, err, out)
	}

	writeFiles(t, wt, map[string]string{".env": "CHANGED\n"})
	if err := os.Remove(filepath.Join(wt, "config/dev.local")); err != nil {
		t.Fatal(err)
	}
	if _, out, err = f.prepare(t, p); err != nil {
		t.Fatalf("Prepare again: %v\n%s", err, out)
	}
	if got := readFile(t, filepath.Join(wt, ".env")); got != "CHANGED\n" {
		t.Fatalf(".env overwritten: %q", got)
	}
	if got := readFile(t, filepath.Join(wt, "config/dev.local")); got != "dev" {
		t.Fatalf("dev.local %q", got)
	}
	if !strings.Contains(out, "[stagent] skipped .env (.worktreeinclude): already there") {
		t.Fatalf("output:\n%s", out)
	}
}

// A broken orca.yaml in the source checkout loses only its shared
// directories: files are still copied (setup then fails on it).
func TestPrepareCopiesWithBrokenOrca(t *testing.T) {
	f := newFixture(t, map[string]string{".gitignore": ".env\n", ".worktreeinclude": ".env\n", "orca.yaml": "a: 1\na: 2\n"})
	writeFiles(t, f.repo, map[string]string{".env": "E"})
	tk := f.task(t, wire.TaskKindIssue, 6, "6-broken")
	_, out, err := f.prepare(t, Prep{Task: tk})
	if stageOf(err) != "setup" {
		t.Fatalf("Prepare: %v\n%s", err, out)
	}
	if got := readFile(t, filepath.Join(tk.Worktree, ".env")); got != "E" {
		t.Fatalf(".env %q", got)
	}
	if !strings.Contains(out, "worktree.sharedDirectories ignored") {
		t.Fatalf("output:\n%s", out)
	}
}

// config.json's patterns are those of the canonical checkout's key.
func TestSavedPatterns(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.json")
	writeFiles(t, filepath.Dir(cfg), map[string]string{"config.json": `{"repos": {"/src/app": {"worktree_include": ["*.env"]}, "/src/b": {}}}`})
	if got := SavedPatterns(cfg, "/src/app"); !slices.Equal(got, []string{"*.env"}) {
		t.Fatalf("SavedPatterns = %q", got)
	}
	if got := SavedPatterns(cfg, "/src/other"); got != nil {
		t.Fatalf("SavedPatterns(other) = %q", got)
	}
}
