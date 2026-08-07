package udf_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/udf"
)

func writeSrc(t *testing.T, root, rel string, content []byte) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestWriteReadUDF writes a directory tree as a UDF image and reads it back with
// the strict reader (udf.Read) as the trusted source.
func TestWriteReadUDF(t *testing.T) {
	src := t.TempDir()
	files := map[string][]byte{
		"readme.txt":    []byte("hello udf world"),
		"dir/a.txt":     []byte("nested file a"),
		"dir/sub/b.bin": bytes.Repeat([]byte{0xCD}, 3000), // spans two sectors
		"日本語.txt":       []byte("unicode name"),           // exercises UTF-16 d-chars
	}
	for rel, content := range files {
		writeSrc(t, src, rel, content)
	}

	imgPath := filepath.Join(t.TempDir(), "out.udf")
	out, err := os.Create(imgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := udf.Write(out, src, "TESTVOL"); err != nil {
		out.Close()
		t.Fatalf("udf.Write: %v", err)
	}
	out.Close()

	f, err := os.Open(imgPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	vol, err := udf.Read(f)
	if err != nil {
		t.Fatalf("udf.Read: %v", err)
	}
	for rel, want := range files {
		got, err := vol.ReadFile(strings.Split(rel, "/"))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: content mismatch (%d vs %d bytes)", rel, len(got), len(want))
		}
	}
}

type failWriterAt struct{}

func (failWriterAt) WriteAt([]byte, int64) (int, error) { return 0, io.ErrShortWrite }

func TestWriteErrors(t *testing.T) {
	src := t.TempDir()
	writeSrc(t, src, "a.txt", []byte("x"))
	if err := udf.Write(failWriterAt{}, src, "X"); err == nil {
		t.Error("expected write error")
	}
	out, err := os.Create(filepath.Join(t.TempDir(), "x.udf"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if err := udf.Write(out, filepath.Join(t.TempDir(), "nope"), "X"); err == nil {
		t.Error("expected error for missing source dir")
	}
}

