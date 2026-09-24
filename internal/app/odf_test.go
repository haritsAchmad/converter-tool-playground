package app

import (
	"archive/zip"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeODFFixture builds a minimal but structurally real ODF package: the
// mandatory first "mimetype" entry (stored/uncompressed, no extra field,
// exactly as isODFMimetypeEntry and the ODF package spec require—see
// office_pdf_test.go's writeMinimalDOCX for the OOXML equivalent of this
// "build a real fixture, not a placeholder" approach), a manifest, and an
// empty content.xml. extra lets a test add or override specific parts to
// probe validateODF's rejection paths, mirroring writeOOXMLFixture.
//
// The mimetype entry is written via zw.CreateRaw with a precomputed
// CRC32/size rather than the more obvious zw.CreateHeader+Write—see
// writeMinimalODF (odf_pdf_test.go) for why: CreateHeader's streaming
// Write() defers the entry's CRC32/size into a trailing data descriptor
// since it doesn't know the final size up front, which produces a
// technically-invalid "first entry" by the OASIS package spec's own
// requirement (immediately readable, no descriptor, no extra field)—Go's
// own archive/zip.Reader tolerates it fine (it always trusts the central
// directory), which is exactly why this went unnoticed here until a real
// LibreOffice rejected an ODF built the older way outright.
func writeODFFixture(t *testing.T, format string, extra map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input.bin")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	mimeBytes := []byte(odfMimeType[format])
	mw, err := zw.CreateRaw(&zip.FileHeader{
		Name:               "mimetype",
		Method:             zip.Store,
		CRC32:              crc32.ChecksumIEEE(mimeBytes),
		CompressedSize64:   uint64(len(mimeBytes)),
		UncompressedSize64: uint64(len(mimeBytes)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mw.Write(mimeBytes); err != nil {
		t.Fatal(err)
	}
	parts := map[string]string{
		"META-INF/manifest.xml": `<?xml version="1.0" encoding="UTF-8"?>` +
			`<manifest:manifest xmlns:manifest="urn:oasis:names:tc:opendocument:xmlns:manifest:1.0" manifest:version="1.3">` +
			`<manifest:file-entry manifest:full-path="/" manifest:media-type="` + odfMimeType[format] + `"/>` +
			`<manifest:file-entry manifest:full-path="content.xml" manifest:media-type="text/xml"/>` +
			`</manifest:manifest>`,
		"content.xml": `<?xml version="1.0" encoding="UTF-8"?><office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0"><office:body><office:text><text:p>hello</text:p></office:text></office:body></office:document-content>`,
	}
	for name, body := range extra {
		parts[name] = body
	}
	for name, body := range parts {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidateODFFormats(t *testing.T) {
	for _, format := range []string{"odt", "ods", "odp"} {
		t.Run(format, func(t *testing.T) {
			path := writeODFFixture(t, format, nil)
			got, err := validateUpload(path, "document."+format)
			if err != nil {
				t.Fatalf("expected valid %s package: %v", format, err)
			}
			if got != format {
				t.Fatalf("got format %q, want %q", got, format)
			}
		})
	}
}

func TestValidateODFRejectsWrongPackageFamily(t *testing.T) {
	path := writeODFFixture(t, "odt", nil)
	if _, err := validateUpload(path, "renamed.ods"); err == nil {
		t.Fatal("expected ODT renamed as ODS to be rejected")
	}
}

func TestValidateODFRejectsMacroEmbeddedObjectAndTraversal(t *testing.T) {
	for name, extra := range map[string]map[string]string{
		"basic macro":     {"Basic/Standard/Module1.xba": "macro"},
		"script macro":    {"Scripts/python/Module1.py": "macro"},
		"embedded object": {"Object1/content.xml": "<embedded/>"},
		"traversal":       {"../outside.xml": "bad"},
		"absolute path":   {"/etc/passwd": "bad"},
	} {
		t.Run(name, func(t *testing.T) {
			path := writeODFFixture(t, "odt", extra)
			if _, err := validateUpload(path, "document.odt"); err == nil {
				t.Fatal("expected unsafe ODF package to be rejected")
			}
		})
	}
}

func TestValidateODFRejectsExternalResourceButAllowsHyperlink(t *testing.T) {
	externalImage := `<?xml version="1.0" encoding="UTF-8"?><office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0" xmlns:xlink="http://www.w3.org/1999/xlink"><office:body><office:text><draw:image xlink:href="https://example.test/pixel.png"/></office:text></office:body></office:document-content>`
	path := writeODFFixture(t, "odt", map[string]string{"content.xml": externalImage})
	if _, err := validateUpload(path, "document.odt"); err == nil {
		t.Fatal("expected external image reference to be rejected")
	}

	hyperlink := `<?xml version="1.0" encoding="UTF-8"?><office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" xmlns:xlink="http://www.w3.org/1999/xlink"><office:body><office:text><text:p><text:a xlink:href="https://example.test/">link</text:a></text:p></office:text></office:body></office:document-content>`
	path = writeODFFixture(t, "odt", map[string]string{"content.xml": hyperlink})
	if _, err := validateUpload(path, "document.odt"); err != nil {
		t.Fatalf("ordinary hyperlink should remain allowed: %v", err)
	}
}

// TestValidateODFRejectsResourceReferenceNotInPackage proves the
// default-deny policy validateODFResourceHref applies: a package-relative
// (no scheme, no traversal) href is still rejected unless it names a real
// entry already present in the ZIP—not just "any relative-looking path".
func TestValidateODFRejectsResourceReferenceNotInPackage(t *testing.T) {
	danglingRef := `<?xml version="1.0" encoding="UTF-8"?><office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0" xmlns:xlink="http://www.w3.org/1999/xlink"><office:body><office:text><draw:image xlink:href="Pictures/does-not-exist.png"/></office:text></office:body></office:document-content>`
	path := writeODFFixture(t, "odt", map[string]string{"content.xml": danglingRef})
	if _, err := validateUpload(path, "document.odt"); err == nil {
		t.Fatal("expected a reference to a non-existent package entry to be rejected")
	}
}

// TestValidateODFAcceptsResourceReferenceInPackage is the positive
// counterpart: a package-relative image reference that DOES name a real
// ZIP entry is accepted, proving the check isn't simply rejecting every
// draw:image outright.
func TestValidateODFAcceptsResourceReferenceInPackage(t *testing.T) {
	realRef := `<?xml version="1.0" encoding="UTF-8"?><office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0" xmlns:xlink="http://www.w3.org/1999/xlink"><office:body><office:text><draw:image xlink:href="Pictures/logo.png"/></office:text></office:body></office:document-content>`
	path := writeODFFixture(t, "odt", map[string]string{
		"content.xml":       realRef,
		"Pictures/logo.png": "not a real png but that's fine, validateODF doesn't decode it",
	})
	if _, err := validateUpload(path, "document.odt"); err != nil {
		t.Fatalf("expected a reference to a real package entry to be accepted: %v", err)
	}
}

// TestValidateODFRejectsResourceInsideHyperlink proves the hyperlink
// exemption in validateODFResourceReferences applies only to a text:a/
// draw:a element's OWN href, not to every href found anywhere inside one:
// a draw:a can wrap a draw:frame/draw:image (an image that's also a
// clickable link), and that inner image's href is a real resource
// LibreOffice fetches during rendering regardless of the hyperlink
// wrapper around it (temuan review P1: an earlier version tracked
// hyperlink-ness as a subtree flag that stayed set for every descendant
// until the closing tag, which wrongly let this exact case through).
func TestValidateODFRejectsResourceInsideHyperlink(t *testing.T) {
	nestedInHyperlink := `<?xml version="1.0" encoding="UTF-8"?><office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0" xmlns:xlink="http://www.w3.org/1999/xlink"><office:body><office:text><draw:a xlink:href="https://example.test/"><draw:frame><draw:image xlink:href="http://127.0.0.1:8080/probe.png"/></draw:frame></draw:a></office:text></office:body></office:document-content>`
	path := writeODFFixture(t, "odt", map[string]string{"content.xml": nestedInHyperlink})
	if _, err := validateUpload(path, "document.odt"); err == nil {
		t.Fatal("expected the external image nested inside a hyperlink to still be rejected")
	}
}

// TestValidateODFAcceptsPercentEncodedPackageReference proves a
// percent-encoded href (ordinary, spec-legal URI encoding for a package
// entry name containing a space or other reserved character—not an
// attack) is matched against the actual, decoded ZIP entry name rather
// than rejected outright for not matching the still-encoded text (temuan
// review P2).
func TestValidateODFAcceptsPercentEncodedPackageReference(t *testing.T) {
	encodedRef := `<?xml version="1.0" encoding="UTF-8"?><office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0" xmlns:xlink="http://www.w3.org/1999/xlink"><office:body><office:text><draw:image xlink:href="Pictures/my%20photo.png"/></office:text></office:body></office:document-content>`
	path := writeODFFixture(t, "odt", map[string]string{
		"content.xml":           encodedRef,
		"Pictures/my photo.png": "not a real png but that's fine, validateODF doesn't decode it",
	})
	if _, err := validateUpload(path, "document.odt"); err != nil {
		t.Fatalf("expected a percent-encoded reference to a real package entry to be accepted: %v", err)
	}
}

func TestODFCapabilitiesDependOnLibreOffice(t *testing.T) {
	c := &converter{}
	if c.supports("odt", "pdf") {
		t.Fatal("ODF conversion advertised without LibreOffice")
	}
	c.libreoffice = filepath.Join("tools", "libreoffice")
	for _, format := range []string{"odt", "ods", "odp"} {
		if !c.supports(format, "pdf") {
			t.Fatalf("expected %s -> PDF support with LibreOffice", strings.ToUpper(format))
		}
	}
	if c.supports("pdf", "odt") {
		t.Fatal("PDF -> ODT must not be advertised")
	}
}
