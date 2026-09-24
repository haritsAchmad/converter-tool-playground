package app

import (
	"archive/zip"
	"context"
	"encoding/json"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestResolvePDFModeRejectsForODF proves pdfMode stays inapplicable for
// ODF, even though odt/ods/odp -> pdf goes through the same LibreOffice
// pipeline as Office->PDF: the fidelity filter options (SinglePageSheets,
// EmbedStandardFonts, ReduceImageResolution) were verified specifically
// against OOXML's filter registry entries, not ODF's, so this pair is
// deliberately kept out of officeFormats (see odfFormats's doc comment)
// rather than silently inheriting a feature nobody checked applies here.
func TestResolvePDFModeRejectsForODF(t *testing.T) {
	for _, in := range []string{"odt", "ods", "odp"} {
		if _, err := resolvePDFMode(in, "pdf", "optimized"); err == nil {
			t.Fatalf("expected pdfMode to be rejected for %s->PDF", in)
		}
	}
}

// TestConvertODFReachesLibreOffice proves converter.run actually routes
// odt/ods/odp -> pdf through convertOffice (the LibreOffice pipeline)
// instead of silently falling through to some other path or a no-op: a
// fake libreoffice binary (same technique as
// TestConvertMarkupToPDFReachesLibreOfficeForSafeContent) must fail with
// its own distinct exec error, proving the call was actually reached.
func TestConvertODFReachesLibreOffice(t *testing.T) {
	c := &converter{libreoffice: filepath.Join(t.TempDir(), "not-a-real-libreoffice")}
	for _, in := range []string{"odt", "ods", "odp"} {
		t.Run(in, func(t *testing.T) {
			path := writeODFFixture(t, in, nil)
			outPath := filepath.Join(t.TempDir(), "output.pdf")
			err := c.run(context.Background(), in, "pdf", "", path, outPath)
			if err == nil {
				t.Fatal("expected an error from the fake libreoffice binary")
			}
			if !strings.Contains(err.Error(), "Office conversion failed") {
				t.Fatalf("expected the conversion to reach LibreOffice and fail there, got %v", err)
			}
		})
	}
}

// TestODFToPDFJobFlowsThroughWorker is the full-pipeline regression,
// mirroring TestMarkdownAndHTMLToPDFJobsFlowThroughWorker and
// TestPDFToImageJobFlowsThroughWorkerReload: submit a real ODF job over
// HTTP, let the actual worker goroutine dequeue and convert it (libreoffice
// faked), and confirm the job reaches Failed via a real exec error instead
// of getting dropped or stuck at Queued.
func TestODFToPDFJobFlowsThroughWorker(t *testing.T) {
	for _, in := range []string{"odt", "ods", "odp"} {
		t.Run(in, func(t *testing.T) {
			a := testApp(t)
			a.converter.libreoffice = filepath.Join(t.TempDir(), "not-a-real-libreoffice")

			fixturePath := writeODFFixture(t, in, nil)
			body, err := os.ReadFile(fixturePath)
			if err != nil {
				t.Fatal(err)
			}
			w := submitJob(t, a, "document."+in, body, map[string]string{"outputFormat": "pdf"})
			if w.Code != http.StatusAccepted {
				t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
			}
			var created Job
			if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}

			deadline := time.Now().Add(2 * time.Second)
			var job Job
			for time.Now().Before(deadline) {
				r := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+created.ID, nil)
				rec := httptest.NewRecorder()
				a.Handler().ServeHTTP(rec, r)
				if rec.Code != http.StatusOK {
					t.Fatalf("status endpoint returned %d for a live job", rec.Code)
				}
				_ = json.Unmarshal(rec.Body.Bytes(), &job)
				if job.Status == Completed || job.Status == Failed {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if job.Status == Queued || job.Status == Processing {
				t.Fatalf("job never reached a terminal status, stuck at %s", job.Status)
			}
			if job.Status != Failed {
				t.Fatalf("expected Failed (libreoffice is a fake path), got %s", job.Status)
			}
			if !strings.Contains(job.Error, "conversion failed") {
				t.Fatalf("expected a real conversion error, got %q", job.Error)
			}
		})
	}
}

// odfDocumentXMLBody is writeMinimalODF's per-format content.xml body,
// each shaped to what that format's own OASIS schema actually needs
// inside office:body—unlike writeODFFixture (odf_test.go), which reuses
// an office:text/text:p body for all three formats since it only needs
// to pass validateODF's own package-structure checks, never open in a
// real consumer. odf_test.go's fixture is fine for that (narrower)
// purpose; a real conversion test needs a genuinely correct body per
// format, so this is a separate, deliberately not-shared builder.
var odfDocumentXMLBody = map[string]string{
	"odt": `<office:body><office:text><text:p>Regression corpus fixture paragraph.</text:p></office:text></office:body>`,
	"ods": `<office:body><office:spreadsheet><table:table table:name="Sheet1"><table:table-column table:number-columns-repeated="1"/><table:table-row><table:table-cell office:value-type="string"><text:p>Fixture cell</text:p></table:table-cell></table:table-row></table:table></office:spreadsheet></office:body>`,
	"odp": `<office:body><office:presentation><draw:page draw:name="page1" draw:master-page-name="Default"><draw:frame draw:layer="layout" svg:width="20cm" svg:height="2cm" svg:x="2cm" svg:y="2cm"><draw:text-box><text:p>Regression corpus fixture slide</text:p></draw:text-box></draw:frame></draw:page></office:presentation></office:body>`,
}

// writeMinimalODF builds a real, LibreOffice-openable ODT/ODS/ODP: the
// roadmap's own previously-blocking note ("no LibreOffice-capable dev
// environment was available here to verify a hand-built ODT/ODS/ODP
// fixture actually opens") is exactly what this closes, the same way
// writeMinimalPPTX closed the equivalent gap for Impress—iterated
// against the real soffice --headless --convert-to pdf this codebase's
// own convertOffice invokes (writer_pdf_Export/calc_pdf_Export/
// impress_pdf_Export), running inside this project's own Docker image,
// not assumed correct from the OASIS ODF schema alone. The content.xml
// body is genuinely format-specific (odfDocumentXMLBody) rather than
// reused across all three, since validateODF's own structural checks
// don't care what's in office:body but a real LibreOffice import very
// much does—see writeODFFixture's doc comment for why THAT fixture
// doesn't attempt this.
//
// The mimetype entry specifically is written via zw.CreateRaw with a
// precomputed CRC32/size, not the zw.CreateHeader+Write pattern
// writeODFFixture uses: that pattern makes Go's zip writer defer the
// entry's CRC32/size into a trailing data descriptor (general-purpose
// flag bit 3 set, zeros in the local header itself) since it doesn't
// know the final size until the write completes—which real LibreOffice
// rejected outright with "source file could not be loaded" (reproduced
// directly; go's own archive/zip.Reader has no problem with it, since
// it always trusts the central directory rather than the local header,
// which is exactly why this went unnoticed by validateODF's own tests
// this whole time). The OASIS Open Document spec requires this first
// entry specifically to be immediately readable without a trailing
// descriptor or an extra field (it exists so a reader can identify an
// ODF package's exact media type from just the first ~40 bytes, without
// parsing the central directory)—CreateRaw with the size/CRC already
// known (Method: Store, so "compressed" size is just the raw length)
// produces exactly that.
func writeMinimalODF(t *testing.T, format string) string {
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
		"content.xml": `<?xml version="1.0" encoding="UTF-8"?>` +
			`<office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:style="urn:oasis:names:tc:opendocument:xmlns:style:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0" xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0" xmlns:fo="urn:oasis:names:tc:opendocument:xmlns:xsl-fo-compatible:1.0" xmlns:svg="urn:oasis:names:tc:opendocument:xmlns:svg-compatible:1.0" office:version="1.3">` +
			odfDocumentXMLBody[format] +
			`</office:document-content>`,
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

// TestConvertODFToStandardPDF is the ODF counterpart to
// TestConvertOfficeToStandardPDF: a real ODT, ODS, and ODP, actually run
// through LibreOffice via convertOffice's pdfMode="" path (ODF never
// takes pdfMode—see TestResolvePDFModeRejectsForODF), producing a valid
// PDF. Skips where libreoffice/soffice isn't installed, same as every
// other real-tool test in this codebase.
func TestConvertODFToStandardPDF(t *testing.T) {
	officeLookPath(t)
	c := newConverter()
	for _, format := range []string{"odt", "ods", "odp"} {
		t.Run(format, func(t *testing.T) {
			inPath := writeMinimalODF(t, format)
			outPath := filepath.Join(t.TempDir(), "output.pdf")
			if err := c.run(context.Background(), format, "pdf", "", inPath, outPath); err != nil {
				t.Fatalf("ODF conversion failed: %v", err)
			}
			assertValidPDF(t, outPath)
		})
	}
}
