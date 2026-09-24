package app

import (
	"archive/zip"
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
func writeODFFixture(t *testing.T, format string, extra map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input.bin")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	mw, err := zw.CreateHeader(&zip.FileHeader{Name: "mimetype", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mw.Write([]byte(odfMimeType[format])); err != nil {
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
