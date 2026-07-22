package wim

import (
	"os"
	"path/filepath"
	"testing"
)

// TestUpdaterAddRemoveFile is the Phase-2.2 guard: injecting and removing files
// inside an image survives commit + reopen, leaves other files and other images
// intact, and keeps the WIM extractable.
func TestUpdaterAddRemoveFile(t *testing.T) {
	// Two-image WIM: image 1 will be edited, image 2 must stay untouched.
	tree1 := t.TempDir()
	writeSrcFile(t, tree1, "keep.txt", []byte("keep me"))
	writeSrcFile(t, tree1, "sub/old.txt", []byte("delete me"))
	tree2 := t.TempDir()
	writeSrcFile(t, tree2, "other.txt", []byte("image two"))

	wimPath := filepath.Join(t.TempDir(), "boot.wim")
	out, _ := os.Create(wimPath)
	ww, err := NewWriter(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := ww.AddImage(tree1, "One"); err != nil {
		t.Fatal(err)
	}
	if err := ww.AddImage(tree2, "Two"); err != nil {
		t.Fatal(err)
	}
	if err := ww.Close(); err != nil {
		t.Fatal(err)
	}
	out.Close()

	// Inject autounattend.xml at the image root and a nested file; remove old.txt.
	u, err := OpenForUpdate(wimPath)
	if err != nil {
		t.Fatalf("OpenForUpdate: %v", err)
	}
	autounattend := []byte("<unattend>injected</unattend>")
	if err := u.AddFile(1, "autounattend.xml", autounattend); err != nil {
		t.Fatal(err)
	}
	if err := u.AddFile(1, "Windows/System32/added.cmd", []byte("echo hi")); err != nil {
		t.Fatal(err)
	}
	if err := u.RemoveFile(1, "sub/old.txt"); err != nil {
		t.Fatal(err)
	}
	if err := u.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	u.Close()

	// Reopen and extract image 1.
	got1 := extractImageMap(t, wimPath, 1)
	if string(got1["autounattend.xml"]) != string(autounattend) {
		t.Errorf("injected autounattend.xml = %q", got1["autounattend.xml"])
	}
	if string(got1["Windows/System32/added.cmd"]) != "echo hi" {
		t.Errorf("nested injected file = %q", got1["Windows/System32/added.cmd"])
	}
	if string(got1["keep.txt"]) != "keep me" {
		t.Errorf("existing file not preserved: %q", got1["keep.txt"])
	}
	if _, present := got1["sub/old.txt"]; present {
		t.Error("removed file still present")
	}

	// Image 2 must be byte-for-byte unchanged.
	got2 := extractImageMap(t, wimPath, 2)
	if len(got2) != 1 || string(got2["other.txt"]) != "image two" {
		t.Errorf("image 2 changed: %v", got2)
	}
}

// TestUpdaterReplaceFile replaces an existing file's content.
func TestUpdaterReplaceFile(t *testing.T) {
	tree := t.TempDir()
	writeSrcFile(t, tree, "config.ini", []byte("old"))
	wimPath := filepath.Join(t.TempDir(), "x.wim")
	out, _ := os.Create(wimPath)
	if err := CreateFromDir(out, tree, "One"); err != nil {
		t.Fatal(err)
	}
	out.Close()

	u, err := OpenForUpdate(wimPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.AddFile(1, "config.ini", []byte("new content")); err != nil {
		t.Fatal(err)
	}
	if err := u.Commit(); err != nil {
		t.Fatal(err)
	}
	u.Close()

	got := extractImageMap(t, wimPath, 1)
	if string(got["config.ini"]) != "new content" {
		t.Errorf("config.ini = %q, want replaced", got["config.ini"])
	}
}

func extractImageMap(t *testing.T, wimPath string, index int) map[string][]byte {
	t.Helper()
	w, err := Open(wimPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer w.Close()
	dest := t.TempDir()
	if err := w.ExtractImage(index, dest); err != nil {
		t.Fatalf("extract image %d: %v", index, err)
	}
	m := map[string][]byte{}
	_ = filepath.WalkDir(dest, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dest, p)
		b, _ := os.ReadFile(p)
		m[filepath.ToSlash(rel)] = b
		return nil
	})
	return m
}

// TestUpdaterAddFileValidation covers argument checks.
func TestUpdaterAddFileValidation(t *testing.T) {
	tree := t.TempDir()
	writeSrcFile(t, tree, "a.txt", []byte("x"))
	wimPath := filepath.Join(t.TempDir(), "v.wim")
	out, _ := os.Create(wimPath)
	if err := CreateFromDir(out, tree, "One"); err != nil {
		t.Fatal(err)
	}
	out.Close()

	u, err := OpenForUpdate(wimPath)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	if err := u.AddFile(5, "x", []byte("y")); err == nil {
		t.Error("expected out-of-range image error")
	}
	if err := u.AddFile(1, "  ", []byte("y")); err == nil {
		t.Error("expected empty-path error")
	}
	if err := u.RemoveFile(1, "does/not/exist"); err != nil {
		t.Fatal(err) // scheduling succeeds
	}
	if err := u.Commit(); err == nil {
		t.Error("expected commit error for removing a nonexistent file")
	}
}
