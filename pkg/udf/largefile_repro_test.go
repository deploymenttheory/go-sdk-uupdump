package udf_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/udf"
)

// TestWriteReadLargeDirWithBigFile is the end-to-end guard for Windows install media:
// a sources/ directory with many entries (so its FID list spans several logical
// blocks) plus a >4 GiB install.wim. It builds the image and reads it back, asserting
// every entry — including install.wim at its full, untruncated size — is recovered.
// (The contiguous-FID-packing invariant is asserted by the internal
// TestDirFIDsContiguousNoGap; this exercises the full build + a real big file.)
func TestWriteReadLargeDirWithBigFile(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a >4 GiB image")
	}
	src := t.TempDir()
	must := func(err error) { t.Helper(); if err != nil { t.Fatal(err) } }
	mk := func(rel string, b []byte) {
		p := filepath.Join(src, rel)
		must(os.MkdirAll(filepath.Dir(p), 0o755))
		must(os.WriteFile(p, b, 0o644))
	}
	want := map[string]int64{}
	mk("sources/boot.wim", []byte("small boot wim"))
	want["boot.wim"] = 14
	for i := 0; i < 80; i++ {
		name := filepath.Join("sources", "component"+string(rune('a'+i%26))+string(rune('0'+i%10))+"_x.dll")
		mk(name, []byte("y"))
		want[filepath.Base(name)] = 1
	}

	const bigSize = int64(4)*1024*1024*1024 + 300*1024*1024 // ~4.3 GiB (> uint32)
	bp := filepath.Join(src, "sources", "install.wim")
	bf, err := os.Create(bp)
	must(err)
	_, err = bf.Write([]byte("MSWIM\x00\x00\x00"))
	must(err)
	must(bf.Truncate(bigSize))
	_, err = bf.WriteAt([]byte("ENDMARKER!"), bigSize-10)
	must(err)
	must(bf.Close())
	want["install.wim"] = bigSize

	imgPath := filepath.Join(t.TempDir(), "out.udf")
	out, err := os.Create(imgPath)
	must(err)
	if err := udf.Write(out, src, "TESTVOL"); err != nil {
		out.Close()
		t.Fatalf("udf.Write: %v", err)
	}
	out.Close()

	f, err := os.Open(imgPath)
	must(err)
	defer f.Close()
	vol, err := udf.Read(f)
	if err != nil {
		t.Fatalf("udf.Read: %v", err)
	}
	entries, err := vol.ReadDir([]string{"sources"})
	if err != nil {
		t.Fatalf("ReadDir sources: %v", err)
	}
	got := map[string]int64{}
	for _, e := range entries {
		got[e.Name] = e.Size
	}
	for name, sz := range want {
		if got[name] != sz {
			t.Errorf("%s: size %d, want %d (missing or truncated)", name, got[name], sz)
		}
	}
	if len(got) != len(want) {
		t.Errorf("recovered %d entries, want %d", len(got), len(want))
	}
}
