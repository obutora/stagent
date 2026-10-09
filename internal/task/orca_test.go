package task

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func writeOrca(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, OrcaFile), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadOrca(t *testing.T) {
	if o, err := LoadOrca(t.TempDir()); o != nil || err != nil {
		t.Fatalf("no file: %+v, %v", o, err)
	}
	o, err := LoadOrca(writeOrca(t, `
scripts:
  setup: |
    pnpm install
    pnpm build
  archive: docker compose down
setupAgentStartupPolicy: wait-for-setup
worktree:
  sharedDirectories: [node_modules, .cache, 3, {a: b}]
`))
	want := &Orca{Setup: "pnpm install\npnpm build\n", Archive: "docker compose down", SharedDirectories: []string{"node_modules", ".cache"}}
	if err != nil || !reflect.DeepEqual(o, want) {
		t.Fatalf("LoadOrca = %+v, %v; want %+v", o, err, want)
	}
	// A value of the wrong type is dropped alone.
	o, err = LoadOrca(writeOrca(t, "scripts:\n  setup: [a, b]\n  archive: ok\nworktree:\n  sharedDirectories: cache\n"))
	if err != nil || o.Setup != "" || o.Archive != "ok" || o.SharedDirectories != nil {
		t.Fatalf("wrong types: %+v, %v", o, err)
	}
	for _, empty := range []string{"", "# only a comment\n"} {
		if o, err := LoadOrca(writeOrca(t, empty)); err != nil || !reflect.DeepEqual(o, &Orca{}) {
			t.Fatalf("%q: %+v, %v", empty, o, err)
		}
	}
}

// Files Orca refuses whole are *OrcaError: invalid YAML, a repeated key,
// no mapping, over 256 KiB.
func TestLoadOrcaRefuses(t *testing.T) {
	for name, content := range map[string]string{
		"invalid":     "scripts: [\n",
		"repeated":    "scripts:\n  setup: a\n  setup: b\n",
		"not mapping": "- setup\n",
		"too large":   "scripts:\n  setup: echo\n#" + strings.Repeat("x", 256<<10) + "\n",
	} {
		_, err := LoadOrca(writeOrca(t, content))
		var oe *OrcaError
		if !errors.As(err, &oe) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestPlanScript(t *testing.T) {
	s := Script{Text: "npm ci\n\n:: comment\nnpm run build\n", Dir: `C:\src\app-3`, Root: `C:\src\app`, Env: []string{"ComSpec=C:\\Windows\\system32\\cmd.exe"}}

	p, err := PlanScript("linux", Script{Text: "make", Dir: "/s/app-3", Root: "/s/app", Env: []string{"SHELL=/usr/bin/zsh"}}, "")
	if err != nil || p.Program != "/usr/bin/zsh" || !slices.Equal(p.Args, []string{"-l", "-c", "set -e\nmake"}) || p.Runner != "" ||
		!slices.Contains(p.Env, "ORCA_ROOT_PATH=/s/app") || !slices.Contains(p.Env, "ORCA_WORKTREE_PATH=/s/app-3") || !slices.Contains(p.Env, "ORCA_WORKSPACE_NAME=app-3") {
		t.Fatalf("linux plan %+v, %v", p, err)
	}
	if p, _ := PlanScript("darwin", Script{Text: "make", Env: []string{"SHELL=/opt/homebrew/bin/fish"}}, ""); p.Program != "/bin/sh" {
		t.Fatalf("fish user: %+v", p)
	}

	// Windows: a .cmd runner calling each line, stopping at the first
	// failure.
	p, err = PlanScript("windows", s, "")
	wantRunner := "@echo off\r\nsetlocal EnableExtensions DisableDelayedExpansion\r\n" +
		"call npm ci\r\nif errorlevel 1 exit /b %errorlevel%\r\n" +
		"call npm run build\r\nif errorlevel 1 exit /b %errorlevel%\r\n" +
		"exit /b 0\r\n"
	if err != nil || p.Program != `C:\Windows\system32\cmd.exe` || p.RunnerExt != ".cmd" || p.Runner != wantRunner ||
		!slices.Contains(p.Env, `ORCA_ROOT_PATH=C:\src\app`) || !slices.Contains(p.Env, "ORCA_WORKSPACE_NAME=app-3") {
		t.Fatalf("windows plan %+v, %v", p, err)
	}

	// Windows #!: Git for Windows' bash.exe next to git, else an error.
	s.Text = "#!/usr/bin/env bash\n./scripts/setup.sh\n"
	if _, err := PlanScript("windows", s, ""); err == nil {
		t.Fatal("#! without git ran")
	}
	gitRoot := t.TempDir()
	git := filepath.Join(gitRoot, "cmd", "git.exe")
	if _, err := PlanScript("windows", s, git); err == nil || !strings.Contains(err.Error(), "bash.exe") {
		t.Fatalf("#! without Git Bash: %v", err)
	}
	bash := filepath.Join(gitRoot, "bin", "bash.exe")
	if err := os.MkdirAll(filepath.Dir(bash), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bash, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err = PlanScript("windows", s, git)
	if err != nil || p.Program != bash || p.RunnerExt != ".sh" || p.Runner != "set -e\n"+s.Text ||
		!slices.Contains(p.Env, "ORCA_ROOT_PATH=/c/src/app") || !slices.Contains(p.Env, "ORCA_WORKTREE_PATH=/c/src/app-3") {
		t.Fatalf("Git Bash plan %+v, %v", p, err)
	}
	// mingw64\bin\git.exe finds it too.
	if p, _ := PlanScript("windows", s, filepath.Join(gitRoot, "mingw64", "bin", "git.exe")); p.Program != bash {
		t.Fatalf("from mingw64: %+v", p)
	}
}
