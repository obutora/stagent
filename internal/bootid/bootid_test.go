package bootid

import (
	"runtime"
	"testing"
)

// The hosts that keep stopped agents read a boot id, the same each time.
func TestGet(t *testing.T) {
	id := Get()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		if id != "" {
			t.Fatalf("boot id %q on %s", id, runtime.GOOS)
		}
		return
	}
	if id == "" || Get() != id {
		t.Fatalf("boot id %q, then %q", id, Get())
	}
}
