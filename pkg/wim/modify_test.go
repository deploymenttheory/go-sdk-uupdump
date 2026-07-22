package wim

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestUpdaterSetPropertyBypass is the Phase-2.1/Phase-3 guard: setting
// WINDOWS/INSTALLATIONTYPE=Server on every image (the Win11 requirement bypass)
// must survive a commit + reopen, must apply to all images, and must not corrupt
// the WIM — files still extract afterwards.
func TestUpdaterSetPropertyBypass(t *testing.T) {
	src := t.TempDir()
	writeSrcFile(t, src, "readme.txt", []byte("hello"))
	writeSrcFile(t, src, "sub/data.bin", bytes.Repeat([]byte{0xAB}, 5000))

	wimPath := filepath.Join(t.TempDir(), "install.wim")
	out, err := os.Create(wimPath)
	if err != nil {
		t.Fatal(err)
	}
	// Two images so "all images" is meaningfully exercised.
	ww, err := NewWriter(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := ww.AddImage(src, "Home"); err != nil {
		t.Fatal(err)
	}
	if err := ww.AddImage(src, "Pro"); err != nil {
		t.Fatal(err)
	}
	if err := ww.Close(); err != nil {
		t.Fatal(err)
	}
	out.Close()

	// Edit: create the <WINDOWS> element from scratch (these images have none).
	u, err := OpenForUpdate(wimPath)
	if err != nil {
		t.Fatalf("OpenForUpdate: %v", err)
	}
	if err := u.SetPropertyAll("WINDOWS/INSTALLATIONTYPE", "Server"); err != nil {
		t.Fatalf("SetPropertyAll: %v", err)
	}
	if err := u.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := u.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen and assert the property stuck on every image.
	w, err := Open(wimPath)
	if err != nil {
		t.Fatalf("Open after commit: %v", err)
	}
	defer w.Close()
	imgs := w.Images()
	if len(imgs) != 2 {
		t.Fatalf("got %d images, want 2", len(imgs))
	}
	for _, im := range imgs {
		if im.InstallationType != "Server" {
			t.Errorf("image %q InstallationType=%q, want Server", im.Name, im.InstallationType)
		}
	}
	// The names must be untouched by the edit.
	if imgs[0].Name != "Home" || imgs[1].Name != "Pro" {
		t.Errorf("image names changed: %q, %q", imgs[0].Name, imgs[1].Name)
	}

	// Structural integrity: content still extracts.
	dest := t.TempDir()
	if err := w.ExtractImage(1, dest); err != nil {
		t.Fatalf("ExtractImage after commit: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dest, "readme.txt"))
	if string(got) != "hello" {
		t.Errorf("extracted content = %q", got)
	}
}

// TestUpdaterReplaceExistingProperty exercises the replace path: a second edit
// overwrites the value the first inserted, rather than duplicating the element.
func TestUpdaterReplaceExistingProperty(t *testing.T) {
	src := t.TempDir()
	writeSrcFile(t, src, "a.txt", []byte("x"))
	wimPath := filepath.Join(t.TempDir(), "img.wim")
	out, _ := os.Create(wimPath)
	if err := CreateFromDir(out, src, "One"); err != nil {
		t.Fatal(err)
	}
	out.Close()

	set := func(v string) {
		u, err := OpenForUpdate(wimPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := u.SetProperty(1, "WINDOWS/INSTALLATIONTYPE", v); err != nil {
			t.Fatal(err)
		}
		if err := u.Commit(); err != nil {
			t.Fatal(err)
		}
		u.Close()
	}
	set("Client")
	set("Server")

	w, err := Open(wimPath)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if it := w.Images()[0].InstallationType; it != "Server" {
		t.Fatalf("InstallationType=%q, want Server", it)
	}
	// Exactly one INSTALLATIONTYPE element (no duplication from the replace).
	if n := bytes.Count([]byte(w.XML()), []byte("<INSTALLATIONTYPE>")); n != 1 {
		t.Errorf("found %d INSTALLATIONTYPE elements, want 1", n)
	}
}

func TestSetWindowsChild(t *testing.T) {
	// Insert into empty fragment.
	if got := setWindowsChild("", "INSTALLATIONTYPE", "Server"); got != "<WINDOWS><INSTALLATIONTYPE>Server</INSTALLATIONTYPE></WINDOWS>" {
		t.Errorf("create: %q", got)
	}
	// Replace existing.
	in := "<WINDOWS><ARCH>9</ARCH><INSTALLATIONTYPE>Client</INSTALLATIONTYPE></WINDOWS>"
	want := "<WINDOWS><ARCH>9</ARCH><INSTALLATIONTYPE>Server</INSTALLATIONTYPE></WINDOWS>"
	if got := setWindowsChild(in, "INSTALLATIONTYPE", "Server"); got != want {
		t.Errorf("replace:\n got %q\nwant %q", got, want)
	}
	// Insert when absent (goes right after <WINDOWS>).
	got := setWindowsChild("<WINDOWS><ARCH>9</ARCH></WINDOWS>", "INSTALLATIONTYPE", "Server")
	if got != "<WINDOWS><INSTALLATIONTYPE>Server</INSTALLATIONTYPE><ARCH>9</ARCH></WINDOWS>" {
		t.Errorf("insert: %q", got)
	}
	// A value containing a regex-special '$' is inserted literally.
	if got := setWindowsChild("", "PRODUCTNAME", "A$1B"); got != "<WINDOWS><PRODUCTNAME>A$1B</PRODUCTNAME></WINDOWS>" {
		t.Errorf("literal: %q", got)
	}
}

// TestSerializeHeaderRoundTrip confirms the header rewrite is faithful.
func TestSerializeHeaderRoundTrip(t *testing.T) {
	src := t.TempDir()
	writeSrcFile(t, src, "a.txt", []byte("x"))
	wimPath := filepath.Join(t.TempDir(), "h.wim")
	out, _ := os.Create(wimPath)
	if err := CreateFromDir(out, src, "One"); err != nil {
		t.Fatal(err)
	}
	out.Close()

	raw, _ := os.ReadFile(wimPath)
	h, err := parseHeader(raw[:headerSize])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(serializeHeader(h), raw[:headerSize]) {
		t.Error("serializeHeader(parseHeader(hdr)) != original header bytes")
	}
}
