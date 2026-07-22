package wim

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"unicode/utf16"
)

// ImageInfo describes a single image within a WIM, parsed from the XML catalog.
type ImageInfo struct {
	Index            int
	Name             string
	Description      string
	DisplayName      string
	Flags            string
	Edition          string
	Architecture     string
	InstallationType string
	ProductName      string
	DirCount         int64
	FileCount        int64
	TotalBytes       int64
	Languages        []string
	// WindowsXML is the verbatim "<WINDOWS>…</WINDOWS>" fragment from the source
	// catalog (empty when the image has no <WINDOWS> element). It is carried
	// loss-lessly through the writer so a rebuilt WIM keeps every Windows-specific
	// property — INSTALLATIONTYPE, EDITIONID, ARCH, LANGUAGES, VERSION,
	// SERVICINGDATA, WIMBOOT, etc. — that Windows Setup reads to select editions
	// and detect WinPE. Without it, remastered media loses this metadata.
	WindowsXML string
}

// Images returns the images described by the WIM's XML catalog.
func (w *WIM) Images() []ImageInfo { return w.images }

// XML returns the raw (UTF-8) XML catalog.
func (w *WIM) XML() string { return w.xmlUTF8 }

// xmlWIM and friends mirror the WIM XML catalog structure.
type xmlWIM struct {
	Images []xmlImage `xml:"IMAGE"`
}

type xmlImage struct {
	Index       int        `xml:"INDEX,attr"`
	Name        string     `xml:"NAME"`
	Description string     `xml:"DESCRIPTION"`
	DisplayName string     `xml:"DISPLAYNAME"`
	Flags       string     `xml:"FLAGS"`
	DirCount    int64      `xml:"DIRCOUNT"`
	FileCount   int64      `xml:"FILECOUNT"`
	TotalBytes  int64      `xml:"TOTALBYTES"`
	Windows     xmlWindows `xml:"WINDOWS"`
}

type xmlWindows struct {
	Arch             int          `xml:"ARCH"`
	EditionID        string       `xml:"EDITIONID"`
	InstallationType string       `xml:"INSTALLATIONTYPE"`
	ProductName      string       `xml:"PRODUCTNAME"`
	Languages        xmlLanguages `xml:"LANGUAGES"`
	// InnerXML captures every child of <WINDOWS> verbatim (including optional
	// elements this struct does not name — VERSION, SERVICINGDATA, SYSTEMROOT,
	// HAL, WIMBOOT). It is the loss-free escape hatch used to reconstruct the
	// full fragment. The WIM XML <WINDOWS> element carries no attributes, so
	// wrapping InnerXML in <WINDOWS>…</WINDOWS> reproduces it faithfully.
	InnerXML []byte `xml:",innerxml"`
}

type xmlLanguages struct {
	Language []string `xml:"LANGUAGE"`
}

// loadXML reads and parses the (uncompressed) XML catalog resource.
func (w *WIM) loadXML(_ int64) error {
	rd := w.hdr.XMLData
	if rd.CompressedSize == 0 {
		return nil
	}
	if rd.compressed() {
		return fmt.Errorf("wim: compressed XML catalog not yet supported")
	}
	raw, err := w.readResourceRaw(rd)
	if err != nil {
		return err
	}

	w.xmlUTF8 = decodeUTF16(raw)
	imgs, err := parseCatalog(w.xmlUTF8)
	if err != nil {
		return err
	}
	w.images = imgs
	return nil
}

// parseCatalog unmarshals a UTF-8 WIM XML catalog into ImageInfo values,
// capturing the verbatim <WINDOWS> fragment for loss-free re-emission. Shared by
// the reader's loadXML and the in-place Updater.
func parseCatalog(xmlUTF8 string) ([]ImageInfo, error) {
	var doc xmlWIM
	if err := xml.Unmarshal([]byte(xmlUTF8), &doc); err != nil {
		return nil, fmt.Errorf("wim: parse XML catalog: %w", err)
	}
	out := make([]ImageInfo, 0, len(doc.Images))
	for _, im := range doc.Images {
		out = append(out, ImageInfo{
			Index:            im.Index,
			Name:             im.Name,
			Description:      im.Description,
			DisplayName:      im.DisplayName,
			Flags:            im.Flags,
			Edition:          im.Windows.EditionID,
			Architecture:     archName(im.Windows.Arch),
			InstallationType: im.Windows.InstallationType,
			ProductName:      im.Windows.ProductName,
			DirCount:         im.DirCount,
			FileCount:        im.FileCount,
			TotalBytes:       im.TotalBytes,
			Languages:        im.Windows.Languages.Language,
			WindowsXML:       windowsFragment(im.Windows.InnerXML),
		})
	}
	return out, nil
}

// windowsFragment reconstructs the verbatim "<WINDOWS>…</WINDOWS>" element from
// the captured inner XML, or returns "" when the image carried no <WINDOWS>
// element (inner content empty/whitespace only).
func windowsFragment(inner []byte) string {
	if len(bytes.TrimSpace(inner)) == 0 {
		return ""
	}
	return "<WINDOWS>" + string(inner) + "</WINDOWS>"
}

// archName maps a WIM PROCESSOR_ARCHITECTURE code to a name.
func archName(code int) string {
	switch code {
	case 0:
		return "x86"
	case 5:
		return "arm"
	case 6:
		return "ia64"
	case 9:
		return "x64"
	case 12:
		return "arm64"
	default:
		return fmt.Sprintf("arch(%d)", code)
	}
}

// decodeUTF16 converts WIM XML bytes (UTF-16LE, optionally BOM-prefixed) to a
// UTF-8 string. Non-UTF-16 input is returned unchanged.
func decodeUTF16(b []byte) string {
	if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE {
		b = b[2:] // strip little-endian BOM
	}
	if len(b)%2 != 0 {
		return string(b)
	}
	u16 := make([]uint16, len(b)/2)
	for i := range u16 {
		u16[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return string(utf16.Decode(u16))
}
