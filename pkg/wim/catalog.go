package wim

import (
	"strconv"
	"strings"
)

// catalog.go is the single WIM XML catalog serializer, shared by the streaming
// Writer (buildXML) and the in-place Updater. Keeping one code path ensures a
// rebuilt or edited WIM emits the same element shape — notably the preserved
// <WINDOWS> fragment that Windows Setup reads for edition selection.

// imageXMLElement serializes one <IMAGE> element for the given 1-based index.
// Element order follows Microsoft/DISM media: the counts, then the verbatim
// <WINDOWS> block, then the human-readable name/description/flags. Optional
// fields are emitted only when non-empty so a directory-captured image (no
// Windows metadata) stays minimal.
func imageXMLElement(index int, im imageRec) string {
	var sb strings.Builder
	sb.WriteString(`<IMAGE INDEX="`)
	sb.WriteString(strconv.Itoa(index))
	sb.WriteString(`">`)
	sb.WriteString("<DIRCOUNT>" + strconv.FormatInt(im.dirCount, 10) + "</DIRCOUNT>")
	sb.WriteString("<FILECOUNT>" + strconv.FormatInt(im.fileCount, 10) + "</FILECOUNT>")
	sb.WriteString("<TOTALBYTES>" + strconv.FormatInt(im.totalBytes, 10) + "</TOTALBYTES>")
	// The <WINDOWS> fragment is already valid, escaped XML captured verbatim from
	// the source catalog; write it as-is.
	if im.windowsXML != "" {
		sb.WriteString(im.windowsXML)
	}
	sb.WriteString("<NAME>" + xmlEscape(im.name) + "</NAME>")
	if im.description != "" {
		sb.WriteString("<DESCRIPTION>" + xmlEscape(im.description) + "</DESCRIPTION>")
	}
	if im.flags != "" {
		sb.WriteString("<FLAGS>" + xmlEscape(im.flags) + "</FLAGS>")
	}
	if im.displayName != "" {
		sb.WriteString("<DISPLAYNAME>" + xmlEscape(im.displayName) + "</DISPLAYNAME>")
	}
	sb.WriteString("</IMAGE>")
	return sb.String()
}

// xmlEscape escapes the five XML text characters. It is intentionally
// conservative (escaping quotes too) so the same helper is safe for attribute
// values as well as element text.
func xmlEscape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, c := range s {
		switch c {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&apos;")
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}
