package wim

import (
	"bytes"
	"strings"
	"testing"
)

// TestWindowsMetadataRoundTrip is the Phase-1a guard: the <WINDOWS> fragment
// must be captured on parse and re-emitted verbatim, so a remaster preserves
// INSTALLATIONTYPE / EDITIONID / ARCH / LANGUAGES — the metadata Windows Setup
// reads for edition selection and that the INSTALLATIONTYPE=Server bypass edits.
// Before this change buildXML dropped the whole <WINDOWS> element.
func TestWindowsMetadataRoundTrip(t *testing.T) {
	src := buildWIM(0, 32768, 2, sampleXML)
	w, err := OpenReaderAt(bytes.NewReader(src), int64(len(src)))
	if err != nil {
		t.Fatalf("OpenReaderAt: %v", err)
	}
	img := w.Images()[0]
	if !strings.Contains(img.WindowsXML, "<INSTALLATIONTYPE>Client</INSTALLATIONTYPE>") {
		t.Fatalf("WindowsXML not captured verbatim: %q", img.WindowsXML)
	}
	if img.InstallationType != "Client" || img.Edition != "Professional" || img.Architecture != "x64" {
		t.Fatalf("parsed properties wrong: %+v", img)
	}

	// Re-emit through the shared serializer exactly as AddImageFromWIM + buildXML
	// do, then reparse and assert nothing was lost.
	recs := make([]imageRec, len(w.Images()))
	for i, im := range w.Images() {
		recs[i] = imageRec{
			name:        im.Name,
			description: im.Description,
			displayName: im.DisplayName,
			flags:       im.Flags,
			windowsXML:  im.WindowsXML,
			dirCount:    im.DirCount,
			fileCount:   im.FileCount,
			totalBytes:  im.TotalBytes,
		}
	}
	catalogUTF8 := decodeUTF16(buildXML(recs))
	rebuilt := buildWIM(0, 32768, len(recs), catalogUTF8)
	w2, err := OpenReaderAt(bytes.NewReader(rebuilt), int64(len(rebuilt)))
	if err != nil {
		t.Fatalf("reparse rebuilt catalog: %v", err)
	}

	img2 := w2.Images()[0]
	if img2.InstallationType != "Client" || img2.Edition != "Professional" || img2.Architecture != "x64" {
		t.Fatalf("Windows metadata lost on re-emit: %+v", img2)
	}
	if len(img2.Languages) != 1 || img2.Languages[0] != "en-US" {
		t.Fatalf("languages lost on re-emit: %+v", img2.Languages)
	}
	// The second image's minimal ARCH-only <WINDOWS> must survive too.
	if w2.Images()[1].Architecture != "arm64" {
		t.Fatalf("image 2 arch lost on re-emit: %q", w2.Images()[1].Architecture)
	}
}

func TestWindowsFragment(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"   \n\t ", ""},
		{"<ARCH>9</ARCH>", "<WINDOWS><ARCH>9</ARCH></WINDOWS>"},
	}
	for _, c := range cases {
		if got := windowsFragment([]byte(c.in)); got != c.want {
			t.Errorf("windowsFragment(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestImageXMLElementOptionalFields(t *testing.T) {
	// A bare directory-captured image (no Windows metadata) stays minimal: no
	// <WINDOWS>, <DESCRIPTION>, <FLAGS>, or <DISPLAYNAME>.
	got := imageXMLElement(1, imageRec{name: "Bare", dirCount: 1, fileCount: 2, totalBytes: 3})
	for _, tag := range []string{"<WINDOWS>", "<DESCRIPTION>", "<FLAGS>", "<DISPLAYNAME>"} {
		if strings.Contains(got, tag) {
			t.Errorf("bare image element should omit %s: %s", tag, got)
		}
	}
	if !strings.Contains(got, "<NAME>Bare</NAME>") {
		t.Errorf("missing name: %s", got)
	}

	// A full image emits every field, with <WINDOWS> ahead of <NAME>.
	full := imageXMLElement(2, imageRec{
		name: "Pro", description: "d", displayName: "dn", flags: "Professional",
		windowsXML: "<WINDOWS><ARCH>9</ARCH></WINDOWS>", dirCount: 1, fileCount: 1, totalBytes: 1,
	})
	if i, j := strings.Index(full, "<WINDOWS>"), strings.Index(full, "<NAME>"); i < 0 || j < 0 || i > j {
		t.Errorf("<WINDOWS> must precede <NAME>: %s", full)
	}
	for _, want := range []string{"<FLAGS>Professional</FLAGS>", "<DISPLAYNAME>dn</DISPLAYNAME>", "<DESCRIPTION>d</DESCRIPTION>"} {
		if !strings.Contains(full, want) {
			t.Errorf("missing %s: %s", want, full)
		}
	}
}
