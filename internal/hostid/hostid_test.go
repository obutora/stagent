package hostid

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// The first Ensure creates the id; later calls and Read return it.
// Concurrent first calls agree on one id.
func TestEnsureCreatesOnceAndKeeps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent", "host-id")
	if Read(path) != "" {
		t.Fatal("id before Ensure")
	}
	const n = 8
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := Ensure(path)
			if err != nil {
				t.Error(err)
			}
			ids[i] = id
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] || !Valid(id) {
			t.Fatalf("ids %v", ids)
		}
	}
	if got, _ := Ensure(path); got != ids[0] || Read(path) != ids[0] {
		t.Fatalf("id changed: %q, %q", got, Read(path))
	}
	if left, _ := filepath.Glob(path + ".tmp*"); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}

// A file without a valid id is replaced.
func TestEnsureReplacesInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host-id")
	if err := os.WriteFile(path, []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := Ensure(path)
	if err != nil || !Valid(id) || Read(path) != id {
		t.Fatalf("Ensure = %q, %v; Read %q", id, err, Read(path))
	}
}
