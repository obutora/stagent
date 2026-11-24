package relay

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/obutora/stagent/internal/task"
	"github.com/obutora/stagent/internal/wire"
)

func TestNormalizeRemote(t *testing.T) {
	same := []string{
		"git@github.com:o/r.git", "ssh://git@github.com/o/r", "https://github.com/o/r",
		"https://user@GitHub.com/o/r/", "git://github.com/o/r.git/",
	}
	for _, u := range same {
		if got := NormalizeRemote(u); got != "github.com/o/r" {
			t.Errorf("NormalizeRemote(%q) = %q", u, got)
		}
	}
	if NormalizeRemote("/srv/git/r.git") != NormalizeRemote("file:///srv/git/r") {
		t.Errorf("a local path and its file URL differ: %q, %q", NormalizeRemote("/srv/git/r.git"), NormalizeRemote("file:///srv/git/r"))
	}
	if NormalizeRemote("https://github.com/o/r") == NormalizeRemote("https://github.com/o/R") {
		t.Error("paths are compared case-sensitively")
	}
}

func TestRelayBranchAndName(t *testing.T) {
	taken := func(names ...string) func(string) bool {
		return func(n string) bool { return slices.Contains(names, n) }
	}
	const id = "0123456789abcdef0123456789abcdef"
	for _, c := range []struct {
		source string
		taken  []string
		want   string
	}{
		{"feat/x", nil, "feat/x"},
		{"feat/x", []string{"feat/x"}, "feat/x-relay"},
		{"feat/x", []string{"feat/x", "feat/x-relay"}, "feat/x-relay-2"},
		{"", nil, "relay-01234567"},
	} {
		if got := RelayBranch(id, c.source, taken(c.taken...)); got != c.want {
			t.Errorf("RelayBranch(%q, %v) = %q, want %q", c.source, c.taken, got, c.want)
		}
	}
	if got := NameOf("feat/a b:c"); got != "feat-a-b-c" {
		t.Errorf("NameOf = %q", got)
	}
	if got := NameOf(strings.Repeat("é", 70)); len([]rune(got)) != 64 {
		t.Errorf("NameOf keeps %d characters", len([]rune(got)))
	}
}

func TestAgentArgv(t *testing.T) {
	const doc = "/home/u/.ssh-term/agent/state/relay/x/relay.md"
	for _, c := range []struct {
		harness, model string
		want           []string
	}{
		{wire.HarnessClaude, "sonnet", []string{"claude", "--model", "sonnet", Instruction(doc, "linux")}},
		{wire.HarnessOmp, "x/y", []string{"omp", "--model=x/y", Instruction(doc, "linux")}},
		{wire.HarnessCopilot, "", []string{"copilot", "-i", Instruction(doc, "linux")}},
		{wire.HarnessOpenCode, "p/m", []string{"opencode", "--model", "p/m", "--prompt", Instruction(doc, "linux")}},
		{wire.HarnessAntigravity, "", []string{"agy", "-i", Instruction(doc, "linux")}},
	} {
		got, err := AgentArgv(c.harness, c.model, doc, "linux")
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("AgentArgv(%s, %q) = %q, %v", c.harness, c.model, got, err)
		}
	}
	if _, err := AgentArgv(wire.HarnessOther, "", doc, "linux"); err == nil {
		t.Error("AgentArgv of other answers")
	}
	// cmd.exe would expand a % of the path: the variable names it then.
	win := `C:\Users\100%\.ssh-term\agent\state\relay\x\relay.md`
	if got := Instruction(win, "windows"); strings.Contains(got, "%") || !strings.Contains(got, wire.EnvRelayDocument) {
		t.Errorf("Instruction on Windows = %q", got)
	}
	if !ValidModel("claude-sonnet-4-5[1m]") || !ValidModel("openai/gpt-5.1") || ValidModel("-x") || ValidModel("a b") || ValidModel(`a"b`) || ValidModel("a%b") {
		t.Error("ValidModel")
	}
}

func writeTar(t *testing.T, path string, hdrs ...*tar.Header) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, h := range hdrs {
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(h.Name))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte(h.Name))
		}
	}
	tw.Close()
	gz.Close()
	f.Close()
}

// Unpack takes only the members of the table: no path out of the
// directory, no link but under tree/untracked/ and nothing through one.
func TestUnpackRefuses(t *testing.T) {
	reg := func(name string) *tar.Header { return &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644} }
	link := func(name, to string) *tar.Header {
		return &tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: to}
	}
	for name, hdrs := range map[string][]*tar.Header{
		"dot dot":       {reg("tree/untracked/../../../evil")},
		"absolute":      {reg("/etc/evil")},
		"unknown":       {reg("evil.sh")},
		"link document": {link("relay.md", "/etc/passwd")},
		"through link":  {link("tree/untracked/d", "/tmp"), reg("tree/untracked/d/evil")},
		"directory":     {{Name: "tree/untracked/d/", Typeflag: tar.TypeDir}},
		"no manifest":   {reg("relay.md")},
	} {
		dir := t.TempDir()
		pkg := filepath.Join(dir, "p.tar.gz")
		writeTar(t, pkg, hdrs...)
		if _, err := Unpack(pkg, filepath.Join(dir, "out"), nil); err == nil {
			t.Errorf("%s: unpacked", name)
		}
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// The source's working tree comes out as its parts — the unpushed commits
// in a bundle, the patches, the untracked files with their bit and links —
// and survives packing and unpacking whole.
func TestPartsPackUnpack(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	gitIn(t, root, "init", "-q", "-b", "main", "seed")
	seed := filepath.Join(root, "seed")
	os.WriteFile(filepath.Join(seed, "a.txt"), []byte("one\n"), 0o644)
	gitIn(t, seed, "add", ".")
	gitIn(t, seed, "commit", "-q", "-m", "init")
	gitIn(t, root, "clone", "-q", "--bare", seed, "origin.git")
	gitIn(t, root, "clone", "-q", "origin.git", "app")
	app := filepath.Join(root, "app")
	gitIn(t, app, "switch", "-q", "-c", "feat")
	os.WriteFile(filepath.Join(app, "a.txt"), []byte("two\n"), 0o644)
	gitIn(t, app, "commit", "-q", "-am", "local")
	os.WriteFile(filepath.Join(app, "a.txt"), []byte("three\n"), 0o644)
	gitIn(t, app, "add", "a.txt")
	os.WriteFile(filepath.Join(app, "a.txt"), []byte("four\n"), 0o644)
	os.MkdirAll(filepath.Join(app, "sub", "deep"), 0o755)
	os.WriteFile(filepath.Join(app, "sub", "deep", "run.sh"), []byte("#!/bin/sh\n"), 0o755)
	if runtime.GOOS != "windows" {
		os.Symlink("deep/run.sh", filepath.Join(app, "sub", "link"))
	}

	ctx := context.Background()
	c, err := Inspect(ctx, nil, filepath.Join(app, "sub"))
	if err != nil || c == nil {
		t.Fatalf("Inspect = %+v, %v", c, err)
	}
	if c.Branch != "feat" || c.CwdRel != "sub" || c.Remote != "origin" || c.Unpushed != 1 || c.Base == "" || c.Base == c.Head {
		t.Fatalf("checkout %+v", c)
	}
	if !task.SamePath(c.Repo, task.CanonicalPath(app)) {
		t.Fatalf("repo %s", c.Repo)
	}
	out := filepath.Join(root, "out")
	tree := filepath.Join(out, TreeDir)
	if err := MkdirPrivate(tree); err != nil {
		t.Fatal(err)
	}
	if made, err := c.Bundle(ctx, nil, filepath.Join(tree, task.RelayBundle)); err != nil || !made {
		t.Fatalf("Bundle = %v, %v", made, err)
	}
	if n, err := c.Diff(ctx, nil, filepath.Join(tree, task.RelayStaged), filepath.Join(tree, task.RelayUnstaged)); err != nil || n == 0 {
		t.Fatalf("Diff = %d, %v", n, err)
	}
	u, err := c.Untracked(ctx, nil, filepath.Join(tree, task.RelayUntracked), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Files) == 0 || u.Files[0].Path != "sub/deep/run.sh" && (len(u.Files) < 2 || u.Files[1].Path != "sub/deep/run.sh") {
		t.Fatalf("untracked %+v", u.Files)
	}
	os.WriteFile(filepath.Join(out, DocumentFile), []byte("# doc\n"), 0o600)
	if err := WriteManifest(out, &Manifest{Version: ManifestVersion, RelayID: "x"}); err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(root, PackageFile)
	size, sum, err := Pack(out, pkg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s2, sum2, _ := FileSHA256(pkg); s2 != size || sum2 != sum {
		t.Fatalf("Pack answered %d %s, the file is %d %s", size, sum, s2, sum2)
	}
	in := filepath.Join(root, "in")
	if _, err := Unpack(pkg, in, nil); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(in, TreeDir, task.RelayUntracked, "sub", "deep", "run.sh"))
	if err != nil || st.Mode()&0o100 == 0 {
		t.Fatalf("unpacked script %v, %v", st, err)
	}
	if runtime.GOOS != "windows" {
		if target, err := os.Readlink(filepath.Join(in, TreeDir, task.RelayUntracked, "sub", "link")); err != nil || target != "deep/run.sh" {
			t.Fatalf("unpacked link %q, %v", target, err)
		}
		if st, _ := os.Stat(filepath.Join(in, TreeDir, task.RelayStaged)); st.Mode().Perm() != 0o600 {
			t.Fatalf("unpacked patch mode %v", st.Mode())
		}
	}
	for _, f := range []string{task.RelayBundle, task.RelayStaged, task.RelayUnstaged} {
		a, _ := os.ReadFile(filepath.Join(tree, f))
		b, _ := os.ReadFile(filepath.Join(in, TreeDir, f))
		if len(a) == 0 || string(a) != string(b) {
			t.Errorf("%s differs after unpacking", f)
		}
	}
}
