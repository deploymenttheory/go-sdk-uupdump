package wim

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestSplitJoinRoundTrip is the Phase-2.3 guard: splitting a multi-blob WIM into
// several ".swm" parts and joining them back must reconstruct a WIM whose images
// and file contents match the original — proving the spanned-set layout (shared
// GUID, per-part blob tables with correct part numbers, metadata in part 1) is
// internally consistent, the same merge-on-read model Windows Setup uses.
func TestSplitJoinRoundTrip(t *testing.T) {
	// Build a two-image WIM with several distinct blobs so packing spans parts.
	srcTree := t.TempDir()
	writeSrcFile(t, srcTree, "a.bin", bytes.Repeat([]byte{0x11}, 2000))
	writeSrcFile(t, srcTree, "b.bin", bytes.Repeat([]byte{0x22}, 2000))
	writeSrcFile(t, srcTree, "c.bin", bytes.Repeat([]byte{0x33}, 2000))
	writeSrcFile(t, srcTree, "sub/d.bin", bytes.Repeat([]byte{0x44}, 2000))

	srcPath := filepath.Join(t.TempDir(), "install.wim")
	out, _ := os.Create(srcPath)
	ww, err := NewWriter(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := ww.AddImage(srcTree, "Edition A"); err != nil {
		t.Fatal(err)
	}
	// A second image sharing most blobs plus one unique file.
	srcTree2 := t.TempDir()
	writeSrcFile(t, srcTree2, "a.bin", bytes.Repeat([]byte{0x11}, 2000)) // dup blob
	writeSrcFile(t, srcTree2, "e.bin", bytes.Repeat([]byte{0x55}, 2000))
	if err := ww.AddImage(srcTree2, "Edition B"); err != nil {
		t.Fatal(err)
	}
	if err := ww.Close(); err != nil {
		t.Fatal(err)
	}
	out.Close()

	// Reference extraction from the original.
	wantA := extractToMap(t, srcPath, 1)
	wantB := extractToMap(t, srcPath, 2)

	// Split with a small part size to force multiple parts.
	outDir := t.TempDir()
	parts, err := Split(srcPath, outDir, "install", 2500)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if len(parts) < 2 {
		t.Fatalf("expected multiple parts, got %d", len(parts))
	}
	if filepath.Base(parts[0]) != "install.swm" || filepath.Base(parts[1]) != "install2.swm" {
		t.Errorf("unexpected part names: %v", parts)
	}

	// Each part must be a spanned WIM with the right part number / total.
	for i, p := range parts {
		raw, _ := os.ReadFile(p)
		h, err := parseHeader(raw[:headerSize])
		if err != nil {
			t.Fatalf("part %d header: %v", i+1, err)
		}
		if h.Flags&flagSpanned == 0 {
			t.Errorf("part %d not marked spanned", i+1)
		}
		if int(h.PartNumber) != i+1 || int(h.TotalParts) != len(parts) {
			t.Errorf("part %d has PartNumber=%d TotalParts=%d", i+1, h.PartNumber, h.TotalParts)
		}
	}

	// Join back and compare.
	joined := filepath.Join(t.TempDir(), "joined.wim")
	if err := Join(parts, joined); err != nil {
		t.Fatalf("Join: %v", err)
	}
	jw, err := Open(joined)
	if err != nil {
		t.Fatalf("open joined: %v", err)
	}
	defer jw.Close()
	if len(jw.Images()) != 2 {
		t.Fatalf("joined image count = %d, want 2", len(jw.Images()))
	}
	if got := extractToMap(t, joined, 1); !mapsEqual(got, wantA) {
		t.Error("image 1 content differs after split/join")
	}
	if got := extractToMap(t, joined, 2); !mapsEqual(got, wantB) {
		t.Error("image 2 content differs after split/join")
	}
}

func TestSplitErrors(t *testing.T) {
	srcTree := t.TempDir()
	writeSrcFile(t, srcTree, "a.bin", []byte("x"))
	srcPath := filepath.Join(t.TempDir(), "s.wim")
	out, _ := os.Create(srcPath)
	if err := CreateFromDir(out, srcTree, "One"); err != nil {
		t.Fatal(err)
	}
	out.Close()

	if _, err := Split(srcPath, t.TempDir(), "s", 0); err == nil {
		t.Error("expected error for non-positive part size")
	}
	if _, err := Split(filepath.Join(t.TempDir(), "missing.wim"), t.TempDir(), "s", 100); err == nil {
		t.Error("expected error for missing source")
	}
}

// extractToMap extracts an image to a temp dir and returns rel-path -> content.
func extractToMap(t *testing.T, wimPath string, index int) map[string]string {
	t.Helper()
	w, err := Open(wimPath)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	dest := t.TempDir()
	if err := w.ExtractImage(index, dest); err != nil {
		t.Fatalf("extract image %d: %v", index, err)
	}
	m := map[string]string{}
	_ = filepath.Walk(dest, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dest, p)
		b, _ := os.ReadFile(p)
		m[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	return m
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
