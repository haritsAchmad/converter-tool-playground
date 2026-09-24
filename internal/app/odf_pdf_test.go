package app

import (
	"context"
	"encoding/json"
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
				if job.Status != Queued {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if job.Status == Queued {
				t.Fatal("job never left Queued")
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
