package scrollback

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// stream returns n deterministic bytes where byte i encodes its offset, so a
// read at the wrong offset is detected.
func stream(from, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((from + i) % 251)
	}
	return b
}

func segFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "*"+segExt))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRotationDropsOldestSegmentsAndKeepsOffsets(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sess")
	s, err := open(dir, 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// 47 bytes in uneven writes: segments [0,10) … [40,47); cap 3 keeps
	// [20,30) [30,40) [40,47).
	off := 0
	for _, n := range []int{7, 13, 1, 20, 6} {
		if _, err := s.Write(stream(off, n)); err != nil {
			t.Fatal(err)
		}
		off += n
	}
	first, end := s.Bounds()
	if first != 20 || end != 47 {
		t.Fatalf("bounds = [%d,%d), want [20,47)", first, end)
	}
	if got := len(segFiles(t, dir)); got != 3 {
		t.Fatalf("%d segment files on disk, want 3", got)
	}

	data, start, e, f, err := s.Read(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if start != 20 || e != 47 || f != 20 || !bytes.Equal(data, stream(20, 27)) {
		t.Fatalf("Read(0,100) = %d bytes [%d,%d) first %d", len(data), start, e, f)
	}
}

func TestReadPagesBackwardsAcrossSegments(t *testing.T) {
	s, err := open(filepath.Join(t.TempDir(), "sess"), 8, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Write(stream(0, 100)); err != nil {
		t.Fatal(err)
	}

	var got []byte
	before := int64(0)
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("paging did not reach the beginning")
		}
		data, start, end, first, err := s.Read(before, 30)
		if err != nil {
			t.Fatal(err)
		}
		if int(end-start) != len(data) || len(data) > 30 {
			t.Fatalf("page [%d,%d) carries %d bytes", start, end, len(data))
		}
		if before != 0 && end != before {
			t.Fatalf("page ends at %d, want %d", end, before)
		}
		got = append(append([]byte{}, data...), got...)
		if start == first {
			break
		}
		before = start
	}
	if !bytes.Equal(got, stream(0, 100)) {
		t.Fatal("pages do not reassemble the stream")
	}
}

func TestReadClampsBeforeAndSize(t *testing.T) {
	s, err := open(filepath.Join(t.TempDir(), "sess"), 2, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Write(stream(0, 35)) // retained: [20,35)

	// before older than the retained window → empty page at first.
	data, start, end, first, err := s.Read(5, 10)
	if err != nil || len(data) != 0 || start != 20 || end != 20 || first != 20 {
		t.Fatalf("Read(5) = %q [%d,%d) first %d err %v", data, start, end, first, err)
	}
	// before past the end is clamped to the end.
	data, start, end, _, _ = s.Read(1000, 4)
	if start != 31 || end != 35 || !bytes.Equal(data, stream(31, 4)) {
		t.Fatalf("Read(1000,4) = [%d,%d)", start, end)
	}
	// Oversized requests are capped at MaxReadBytes.
	big, err := open(filepath.Join(t.TempDir(), "big"), 8, SegmentSize)
	if err != nil {
		t.Fatal(err)
	}
	defer big.Close()
	big.Write(make([]byte, MaxReadBytes+100))
	data, _, _, _, _ = big.Read(0, 10*MaxReadBytes)
	if len(data) != MaxReadBytes {
		t.Fatalf("read %d bytes, want cap %d", len(data), MaxReadBytes)
	}
	data, _, _, _, _ = big.Read(0, 0)
	if len(data) != DefaultReadBytes {
		t.Fatalf("default read %d bytes, want %d", len(data), DefaultReadBytes)
	}
}

func TestOpenDiscardsStaleSegmentsAndUsesPrivateModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sess")
	s1, _ := open(dir, 4, 10)
	s1.Write(stream(0, 25))
	s1.Close()

	s2, err := open(dir, 4, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if first, end := s2.Bounds(); first != 0 || end != 0 {
		t.Fatalf("reopened store bounds [%d,%d), want empty", first, end)
	}
	s2.Write([]byte("abc"))
	files := segFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("segments after reopen: %v", files)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(files[0])
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("segment mode %v, want 0600", st.Mode().Perm())
		}
		dst, _ := os.Stat(dir)
		if dst.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode %v, want 0700", dst.Mode().Perm())
		}
	}
}
