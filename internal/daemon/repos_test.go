package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

// config.set stores repos.<checkout>.worktree_include as a Merge Patch —
// one checkout at a time, null deleting one — and refuses (bad_request,
// nothing written) a key that is not an absolute clean path and a
// pattern that is empty, a negation, absolute or has .. or .git in it.
// Whether the checkout exists does not matter.
func TestConfigSetRepos(t *testing.T) {
	e := startDaemon(t, wire.Config{}, Options{})
	c, _ := e.client()
	a := filepath.Join(t.TempDir(), "nowhere", "app")
	b := filepath.Join(filepath.Dir(a), "b")
	set := func(repos map[string]any) error {
		patch, _ := json.Marshal(map[string]any{"repos": repos})
		return c.Call(t.Context(), wire.MethodConfigSet, wire.ConfigSetParams{Config: patch}, nil)
	}
	get := func() map[string]wire.RepoConfig {
		var res wire.ConfigResult
		call(t, c, wire.MethodConfigGet, nil, &res)
		return res.Config.Repos
	}

	if err := set(map[string]any{a: map[string]any{"worktree_include": []string{".env*", "config/**/*.local"}}}); err != nil {
		t.Fatal(err)
	}
	if err := set(map[string]any{b: map[string]any{"worktree_include": []string{"secrets"}}}); err != nil {
		t.Fatal(err)
	}
	if got := get(); len(got) != 2 || !slices.Equal(got[a].WorktreeInclude, []string{".env*", "config/**/*.local"}) ||
		!slices.Equal(got[b].WorktreeInclude, []string{"secrets"}) {
		t.Fatalf("repos %+v", got)
	}
	if err := set(map[string]any{a: nil}); err != nil {
		t.Fatal(err)
	}
	if got := get(); len(got) != 1 || got[b].WorktreeInclude == nil {
		t.Fatalf("repos after null %+v", got)
	}

	before, _ := os.ReadFile(e.layout.Config)
	for _, bad := range []map[string]any{
		{"relative/app": map[string]any{"worktree_include": []string{"x"}}},
		{a + string(filepath.Separator): map[string]any{"worktree_include": []string{"x"}}},
		{filepath.Join(a, "sub") + string(filepath.Separator) + ".." + string(filepath.Separator) + "app": map[string]any{}},
		{a: map[string]any{"worktree_include": []string{""}}},
		{a: map[string]any{"worktree_include": []string{"!x"}}},
		{a: map[string]any{"worktree_include": []string{"/etc/x"}}},
		{a: map[string]any{"worktree_include": []string{`C:\x`}}},
		{a: map[string]any{"worktree_include": []string{"../x"}}},
		{a: map[string]any{"worktree_include": []string{"sub/.git/config"}}},
		{a: map[string]any{"worktree_include": "x"}},
	} {
		var we *wire.Error
		if err := set(bad); !errors.As(err, &we) || we.Code != wire.ErrBadRequest {
			t.Errorf("config.set repos %v: %v, want bad_request", bad, err)
		}
	}
	if after, _ := os.ReadFile(e.layout.Config); !bytes.Equal(after, before) {
		t.Fatalf("a refused config.set wrote %s", after)
	}
}
