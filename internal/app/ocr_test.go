package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseOCRLanguages(t *testing.T) {
	valid := map[string][]string{
		"eng":             {"eng"},
		"ind+eng":         {"ind", "eng"},
		" ind + eng ":     {"ind", "eng"},
		"eng+ind+eng":     {"eng", "ind"},
		"chi_sim+deu":     {"chi_sim", "deu"},
		"ara+fra":         {"ara", "fra"},
		"eng+ind+fra+deu": {"eng", "ind", "fra", "deu"},
	}
	for spec, want := range valid {
		got, err := parseOCRLanguages(spec)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("parseOCRLanguages(%q) = %v, %v; want %v", spec, got, err, want)
		}
	}
	// Everything here would otherwise reach Tesseract's -l argument or a
	// path under tessdata.
	for _, spec := range []string{"", "eng+", "+eng", "english", "en", "ENG", "eng;rm", "../eng", "eng ind", "eng/ind", "-l", "eng+ind+fra+deu+spa"} {
		if got, err := parseOCRLanguages(spec); err == nil {
			t.Errorf("parseOCRLanguages(%q) = %v, expected an error", spec, got)
		}
	}
}

func TestCleanOCRText(t *testing.T) {
	got := cleanOCRText("SURAT KETERANGAN\r\n\r\nNomor 4471\n\n\f")
	if want := "SURAT KETERANGAN\n\nNomor 4471"; got != want {
		t.Fatalf("cleanOCRText = %q, want %q", got, want)
	}
	if got := cleanOCRText(" \n\f"); got != "" {
		t.Fatalf("an empty page should clean to an empty string, got %q", got)
	}
}

func TestCappedBufferNeverReportsShortWrite(t *testing.T) {
	b := cappedBuffer{limit: 5}
	for _, chunk := range []string{"abc", "defg", "hij"} {
		n, err := b.Write([]byte(chunk))
		if n != len(chunk) || err != nil {
			t.Fatalf("Write(%q) = %d, %v; a short write would make os/exec kill the process", chunk, n, err)
		}
	}
	if b.buf.String() != "abcde" || !b.overflowed {
		t.Fatalf("buffer = %q overflowed=%v, want the first 5 bytes and overflowed", b.buf.String(), b.overflowed)
	}
	exact := cappedBuffer{limit: 3}
	_, _ = exact.Write([]byte("abc"))
	if exact.overflowed {
		t.Fatal("writing exactly limit bytes is not an overflow")
	}
}

// recordingOCR is a pageOCR that returns canned text per page and
// records which pages it was asked for.
type recordingOCR struct {
	pages []int
	reply func(page int) (string, error)
}

func (r *recordingOCR) ocr(_ context.Context, _ string, page int) (string, error) {
	r.pages = append(r.pages, page)
	return r.reply(page)
}

func writeFixture(t *testing.T, data []byte) (inPath, outPath string) {
	t.Helper()
	dir := t.TempDir()
	inPath = filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(inPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	return inPath, filepath.Join(dir, "out.docx")
}

func TestPDFToDocxOCRsOnlyPagesWithoutText(t *testing.T) {
	// Page 2 has an empty text-showing operator: a real page with nothing
	// extractable, between two pages that do have a text layer.
	inPath, outPath := writeFixture(t, buildTextPDF([]string{"First page text", "", "Third page text"}))
	rec := &recordingOCR{reply: func(page int) (string, error) { return fmt.Sprintf("scanned page %d", page), nil }}
	c := &converter{ocrOverride: rec.ocr}
	if err := c.convertPDFToDocx(context.Background(), inPath, outPath); err != nil {
		t.Fatalf("convertPDFToDocx failed: %v", err)
	}
	if !reflect.DeepEqual(rec.pages, []int{2}) {
		t.Fatalf("OCR ran for pages %v, expected only the page without a text layer (2)", rec.pages)
	}
	var texts []string
	for _, tok := range parseDocx(t, outPath) {
		if tok != "" && tok != pageBreakToken {
			texts = append(texts, tok)
		}
	}
	if want := []string{"First page text", "scanned page 2", "Third page text"}; !reflect.DeepEqual(texts, want) {
		t.Fatalf("DOCX text = %v, want %v (OCR text in its own page's position)", texts, want)
	}
}

func TestPDFToDocxNeverOCRsATextPDF(t *testing.T) {
	inPath, outPath := writeFixture(t, buildTextPDF([]string{"One", "Two"}))
	rec := &recordingOCR{reply: func(int) (string, error) { return "should not be used", nil }}
	if err := (&converter{ocrOverride: rec.ocr}).convertPDFToDocx(context.Background(), inPath, outPath); err != nil {
		t.Fatal(err)
	}
	if len(rec.pages) != 0 {
		t.Fatalf("OCR ran for pages %v of a PDF whose every page has a text layer", rec.pages)
	}
}

func TestPDFToDocxOCRsAFullyScannedPDF(t *testing.T) {
	inPath, outPath := writeFixture(t, buildBlankPDF(3))
	rec := &recordingOCR{reply: func(page int) (string, error) {
		if page == 2 {
			return "", errors.New("render failed") // one bad page must not sink the job
		}
		return fmt.Sprintf("halaman %d\nbaris kedua", page), nil
	}}
	if err := (&converter{ocrOverride: rec.ocr}).convertPDFToDocx(context.Background(), inPath, outPath); err != nil {
		t.Fatalf("convertPDFToDocx failed: %v", err)
	}
	if !reflect.DeepEqual(rec.pages, []int{1, 2, 3}) {
		t.Fatalf("OCR ran for pages %v, want every page in order", rec.pages)
	}
	got := strings.Join(parseDocx(t, outPath), "|")
	want := strings.Join([]string{"halaman 1", "baris kedua", pageBreakToken, "", pageBreakToken, "halaman 3", "baris kedua"}, "|")
	if got != want {
		t.Fatalf("DOCX tokens = %q, want %q", got, want)
	}
}

func TestPDFToDocxOCRFailureModes(t *testing.T) {
	cases := []struct {
		name  string
		c     *converter
		wants string
	}{
		{"OCR unavailable", &converter{}, "OCR is not available"},
		{"OCR finds nothing", &converter{ocrOverride: func(context.Context, string, int) (string, error) { return " \n", nil }}, "even with OCR"},
		{"OCR errors on every page", &converter{ocrOverride: func(context.Context, string, int) (string, error) {
			return "", errors.New("tesseract exploded")
		}}, "OCR failed: tesseract exploded"},
	}
	for _, tc := range cases {
		inPath, outPath := writeFixture(t, buildBlankPDF(2))
		err := tc.c.convertPDFToDocx(context.Background(), inPath, outPath)
		if err == nil || !strings.Contains(err.Error(), tc.wants) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.wants)
		}
		if _, statErr := os.Stat(outPath); statErr == nil {
			t.Errorf("%s: an output file was left behind for a failed conversion", tc.name)
		}
	}
}

func TestPDFToDocxOCRPageLimit(t *testing.T) {
	// A fully scanned PDF over the cap is refused before any OCR runs:
	// a DOCX that silently stops at page 50 is worse than a clear error.
	inPath, outPath := writeFixture(t, buildBlankPDF(maxOCRPages+1))
	rec := &recordingOCR{reply: func(int) (string, error) { return "text", nil }}
	err := (&converter{ocrOverride: rec.ocr}).convertPDFToDocx(context.Background(), inPath, outPath)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("limited to %d pages", maxOCRPages)) || len(rec.pages) != 0 {
		t.Fatalf("err = %v after %d OCR calls, want a page-limit error and no OCR at all", err, len(rec.pages))
	}

	// Exactly at the cap is fine.
	inPath, outPath = writeFixture(t, buildBlankPDF(maxOCRPages))
	rec = &recordingOCR{reply: func(int) (string, error) { return "text", nil }}
	if err := (&converter{ocrOverride: rec.ocr}).convertPDFToDocx(context.Background(), inPath, outPath); err != nil || len(rec.pages) != maxOCRPages {
		t.Fatalf("err = %v after %d OCR calls, want success with %d", err, len(rec.pages), maxOCRPages)
	}

	// A PDF that does have text plus more textless pages than the cap
	// still converts: its text is all there, and OCR covers the first
	// maxOCRPages textless pages rather than failing the whole job.
	texts := make([]string, maxOCRPages+6)
	texts[0] = "Real text layer"
	inPath, outPath = writeFixture(t, buildTextPDF(texts))
	rec = &recordingOCR{reply: func(int) (string, error) { return "figure caption", nil }}
	if err := (&converter{ocrOverride: rec.ocr}).convertPDFToDocx(context.Background(), inPath, outPath); err != nil {
		t.Fatalf("a mostly-text PDF over the OCR cap should still convert: %v", err)
	}
	if len(rec.pages) != maxOCRPages || rec.pages[0] != 2 || rec.pages[len(rec.pages)-1] != maxOCRPages+1 {
		t.Fatalf("OCR ran for %d pages (%d..%d), want exactly the first %d textless pages", len(rec.pages), rec.pages[0], rec.pages[len(rec.pages)-1], maxOCRPages)
	}
}

func TestPDFToDocxStopsOCRWhenCancelled(t *testing.T) {
	inPath, outPath := writeFixture(t, buildBlankPDF(5))
	ctx, cancel := context.WithCancel(context.Background())
	rec := &recordingOCR{reply: func(page int) (string, error) {
		if page == 2 {
			cancel() // the job is cancelled while page 2 is being recognized
			return "", context.Canceled
		}
		return "text", nil
	}}
	err := (&converter{ocrOverride: rec.ocr}).convertPDFToDocx(ctx, inPath, outPath)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if !reflect.DeepEqual(rec.pages, []int{1, 2}) {
		t.Fatalf("OCR ran for pages %v, want it to stop at the page that was cancelled", rec.pages)
	}
	if _, statErr := os.Stat(outPath); statErr == nil {
		t.Fatal("a cancelled conversion left an output file behind")
	}
}

func TestOCRAvailabilityNeedsBothToolsAndALanguage(t *testing.T) {
	full := converter{tesseract: "/usr/bin/tesseract", pdftoppm: "/usr/bin/pdftoppm", ocrLangs: "eng"}
	if !full.ocrAvailable() || full.pageOCR() == nil {
		t.Fatal("both tools and a language should make OCR available")
	}
	for name, c := range map[string]converter{
		"no tesseract": {pdftoppm: "/usr/bin/pdftoppm", ocrLangs: "eng"},
		"no pdftoppm":  {tesseract: "/usr/bin/tesseract", ocrLangs: "eng"},
		"no language":  {tesseract: "/usr/bin/tesseract", pdftoppm: "/usr/bin/pdftoppm"},
	} {
		if c.ocrAvailable() || c.pageOCR() != nil {
			t.Errorf("%s: OCR must not be available", name)
		}
	}
	// configureOCR without Tesseract leaves OCR off whatever is requested.
	none := converter{pdftoppm: "/usr/bin/pdftoppm"}
	none.configureOCR("eng")
	if none.ocrLangs != "" {
		t.Fatalf("ocrLangs = %q without a Tesseract executable", none.ocrLangs)
	}
}

func TestFormatsEndpointReportsPDFOCR(t *testing.T) {
	a := testApp(t)
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/formats", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	got, ok := body["pdfOCR"].(bool)
	if !ok || got != a.converter.ocrAvailable() {
		t.Fatalf("pdfOCR = %v (present=%v), want %v", body["pdfOCR"], ok, a.converter.ocrAvailable())
	}
}

// requireOCRTools skips unless both real tools are installed and at
// least English is—the same self-skip the other real-tool tests use; CI
// and the Docker image install both so this path isn't untested
// everywhere.
func requireOCRTools(t *testing.T) *converter {
	t.Helper()
	c := newConverter()
	c.configureOCR("ind+eng")
	if !c.ocrAvailable() {
		t.Skip("pdftoppm and tesseract (with eng or ind) not installed, skipping real OCR test")
	}
	return c
}

// scannedPDF builds a genuine image-only PDF out of this codebase's own
// pieces: a text PDF is rendered to a PNG by the real pdftoppm, and that
// PNG is wrapped back into a PDF by convertImageToPDF (pdfcpu). The
// result has the words as pixels and no text layer at all—what a
// scanner produces.
func scannedPDF(t *testing.T, c *converter, text string) []byte {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.pdf")
	if err := os.WriteFile(source, buildTextPDF([]string{text}), 0600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "page")
	if out, err := exec.Command(c.pdftoppm, "-r", "150", "-png", "-singlefile", source, root).CombinedOutput(); err != nil {
		t.Fatalf("rendering the fixture page failed: %v (%s)", err, out)
	}
	scanned := filepath.Join(dir, "scanned.pdf")
	if err := convertImageToPDF(root+".png", scanned); err != nil {
		t.Fatalf("wrapping the rendered page into a PDF failed: %v", err)
	}
	data, err := os.ReadFile(scanned)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestConfigureOCRKeepsOnlyInstalledLanguages(t *testing.T) {
	c := requireOCRTools(t)
	installed := installedOCRLanguages(c.tesseract)
	if len(installed) == 0 {
		t.Fatal("requireOCRTools passed but --list-langs parsed to nothing")
	}
	// "zzz" matches the name pattern but is not a model anyone ships.
	c.configureOCR("zzz+eng+zzz")
	if installed["eng"] && c.ocrLangs != "eng" {
		t.Fatalf("ocrLangs = %q, want the uninstalled language dropped and eng kept", c.ocrLangs)
	}
	c.configureOCR("zzz")
	if c.ocrAvailable() {
		t.Fatalf("OCR must be unavailable when no requested language is installed (ocrLangs = %q)", c.ocrLangs)
	}
	c.configureOCR("not a language")
	if c.ocrAvailable() {
		t.Fatal("an invalid language spec must leave OCR unavailable")
	}
}

func TestRealOCRReadsAScannedPDF(t *testing.T) {
	c := requireOCRTools(t)
	inPath, outPath := writeFixture(t, scannedPDF(t, c, "Invoice number 4471 is due on Friday"))

	// The fixture really has no text layer: without OCR it is rejected.
	if err := (&converter{}).convertPDFToDocx(context.Background(), inPath, outPath); err == nil {
		t.Fatal("the scanned fixture unexpectedly has extractable text; it would not exercise OCR")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := c.run(ctx, "pdf", "docx", "", inPath, outPath); err != nil {
		t.Fatalf("PDF -> DOCX with OCR failed: %v", err)
	}
	text := strings.Join(parseDocx(t, outPath), " ")
	for _, word := range []string{"Invoice", "number", "4471", "Friday"} {
		if !strings.Contains(text, word) {
			t.Fatalf("OCR text %q is missing %q", text, word)
		}
	}
	// The rendered page image must not outlive the conversion.
	entries, _ := os.ReadDir(filepath.Dir(inPath))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "ocr-page-") {
			t.Fatalf("intermediate OCR image %s was left in the job directory", e.Name())
		}
	}
}

func TestRealOCRIsKilledByAnExpiredDeadline(t *testing.T) {
	c := requireOCRTools(t)
	inPath, outPath := writeFixture(t, scannedPDF(t, c, "Deadline test page"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	if _, err := c.ocrPDFPage(ctx, inPath, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ocrPDFPage with an expired deadline returned %v, want context.DeadlineExceeded", err)
	}
	if err := c.convertPDFToDocx(ctx, inPath, outPath); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("convertPDFToDocx with an expired deadline returned %v, want context.DeadlineExceeded", err)
	}
}

// TestScannedPDFJobFlowsThroughWorker submits a scanned PDF over HTTP
// and lets the real worker OCR it, mirroring
// TestPDFToDocxJobFlowsThroughWorker.
func TestScannedPDFJobFlowsThroughWorker(t *testing.T) {
	c := requireOCRTools(t)
	// Not testApp: its 1s JobTimeout is sized for pure-Go conversions. One
	// OCR page is a pdftoppm render plus a Tesseract run, which took longer
	// than that on a CI runner under -race and failed this test with the
	// job's deadline, not with anything OCR did wrong.
	cfg := Config{Address: ":0", StorageRoot: t.TempDir(), MaxUploadBytes: 1 << 20, Workers: 1, QueueSize: 2, JobTimeout: 60 * time.Second, JobTTL: time.Minute, CleanupInterval: time.Hour, UploadTimeout: time.Second, RateRPS: 100, RateBurst: 100, MaxJobsPerIP: 100}
	a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	if !a.converter.ocrAvailable() {
		t.Skip("the app's own converter has no OCR languages installed")
	}
	w := submitJob(t, a, "scan.pdf", scannedPDF(t, c, "Scanned through the worker"), map[string]string{"outputFormat": "docx"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	var created Job
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(90 * time.Second)
	var job Job
	for time.Now().Before(deadline) {
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+created.ID, nil))
		_ = json.Unmarshal(rec.Body.Bytes(), &job)
		if job.Status == Completed || job.Status == Failed {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if job.Status != Completed {
		t.Fatalf("expected the scanned PDF -> DOCX job to complete, got %s (error: %q)", job.Status, job.Error)
	}
	drec := httptest.NewRecorder()
	a.Handler().ServeHTTP(drec, httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+created.ID+"/download", nil))
	outPath := filepath.Join(t.TempDir(), "downloaded.docx")
	if err := os.WriteFile(outPath, drec.Body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if text := strings.Join(parseDocx(t, outPath), " "); !strings.Contains(text, "Scanned through the worker") {
		t.Fatalf("expected the OCR text in the downloaded DOCX, got %q", text)
	}
}

func TestLoadConfigValidatesOCRLanguages(t *testing.T) {
	t.Setenv("CONVERTBOX_OCR_LANGUAGES", "")
	cfg, err := LoadConfig()
	if err != nil || cfg.OCRLanguages != defaultOCRLanguages {
		t.Fatalf("unset: OCRLanguages = %q, err = %v; want the default %q", cfg.OCRLanguages, err, defaultOCRLanguages)
	}
	t.Setenv("CONVERTBOX_OCR_LANGUAGES", "eng+fra")
	if cfg, err = LoadConfig(); err != nil || cfg.OCRLanguages != "eng+fra" {
		t.Fatalf("eng+fra: OCRLanguages = %q, err = %v", cfg.OCRLanguages, err)
	}
	// A typo fails startup instead of silently turning OCR off.
	t.Setenv("CONVERTBOX_OCR_LANGUAGES", "english")
	if _, err = LoadConfig(); err == nil || !strings.Contains(err.Error(), "CONVERTBOX_OCR_LANGUAGES") {
		t.Fatalf("an invalid language spec should fail LoadConfig naming the variable, got %v", err)
	}
}
