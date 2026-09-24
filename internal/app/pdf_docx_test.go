package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// buildTextPDF hand-builds a minimal, multi-page PDF where each page has
// a real content stream showing the given text with the standard
// Helvetica font (WinAnsiEncoding, no embedding needed)—the simplest
// case any PDF text extractor should handle, and the only way to get a
// fixture with genuinely extractable text here: pdfcpu's own
// AddTextWatermarksFile was tried first and empirically confirmed NOT
// to work for this (its stamped text renders through a Form XObject
// the page's own /Contents stream merely /Do-invokes, which
// github.com/ledongthuc/pdf's simple content-stream interpreter doesn't
// descend into—GetPlainText came back empty, not an error, for every
// page). Like this codebase's other hand-built PDF fixtures, the xref
// table's byte offsets are computed from the actual bytes written as
// they're written, not hand-counted (a prior temuan review caught a
// hand-counted xref table that silently drifted).
func buildTextPDF(texts []string) []byte {
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")

	n := len(texts)
	// Object numbering: 1=Catalog, 2=Pages, 3=Font, then for each page i
	// (0-indexed): page obj = 4+2i, content obj = 5+2i.
	offsets := make([]int, 4+2*n) // index 0 unused; index = object number

	writeObj := func(num int, body string) {
		offsets[num] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", num, body)
	}

	kids := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			kids += " "
		}
		kids += fmt.Sprintf("%d 0 R", 4+2*i)
	}

	writeObj(1, "<< /Type /Catalog /Pages 2 0 R >>")
	writeObj(2, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids, n))
	writeObj(3, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")

	for i, text := range texts {
		pageNum := 4 + 2*i
		contentNum := 5 + 2*i
		writeObj(pageNum, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", contentNum))
		stream := fmt.Sprintf("BT /F1 24 Tf 72 700 Td (%s) Tj ET", text)
		writeObj(contentNum, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	}

	xrefStart := buf.Len()
	total := len(offsets)
	fmt.Fprintf(&buf, "xref\n0 %d\n", total)
	buf.WriteString("0000000000 65535 f \n")
	for i := 1; i < total; i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF", total, xrefStart)
	return buf.Bytes()
}

// buildBlankPDF is buildTextPDF's counterpart with no text-showing
// operator at all (an empty content stream)—a real, structurally valid
// PDF that just has nothing for a text extractor to find, the "scanned/
// image-only page" case convertPDFToDocx's no-extractable-text error
// covers.
func buildBlankPDF(pages int) []byte {
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, 4+2*pages)
	writeObj := func(num int, body string) {
		offsets[num] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", num, body)
	}
	kids := ""
	for i := 0; i < pages; i++ {
		if i > 0 {
			kids += " "
		}
		kids += fmt.Sprintf("%d 0 R", 4+2*i)
	}
	writeObj(1, "<< /Type /Catalog /Pages 2 0 R >>")
	writeObj(2, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids, pages))
	writeObj(3, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	for i := 0; i < pages; i++ {
		pageNum := 4 + 2*i
		contentNum := 5 + 2*i
		writeObj(pageNum, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", contentNum))
		writeObj(contentNum, "<< /Length 0 >>\nstream\n\nendstream")
	}
	xrefStart := buf.Len()
	total := len(offsets)
	fmt.Fprintf(&buf, "xref\n0 %d\n", total)
	buf.WriteString("0000000000 65535 f \n")
	for i := 1; i < total; i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF", total, xrefStart)
	return buf.Bytes()
}

// docxDocument is a minimal, independent reader for this test file's own
// writeDocx output: it deliberately doesn't reuse any of writeDocx's own
// string-building code, so a bug there (e.g. a malformed tag) is caught
// by a real second parser instead of round-tripping through the same
// logic that produced it. Namespace prefixes (w:p, w:r, w:t, w:br) are
// matched by local name only, which is how encoding/xml resolves an
// untagged struct field regardless of namespace.
type docxDocument struct {
	Body struct {
		Paragraphs []struct {
			Runs []struct {
				Text  string `xml:"t"`
				Break *struct {
					Type string `xml:"type,attr"`
				} `xml:"br"`
			} `xml:"r"`
		} `xml:"p"`
	} `xml:"body"`
}

// docxToken renders parseDocx's flattened output: each paragraph's text
// (possibly empty, for a blank line) or "\x00PAGEBREAK\x00" for a page
// break, in document order, so a test can assert ordering with a plain
// slice comparison.
const pageBreakToken = "\x00PAGEBREAK\x00"

func parseDocx(t *testing.T, path string) []string {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("output is not a valid zip: %v", err)
	}
	defer zr.Close()
	var found bool
	var tokens []string
	for _, f := range zr.File {
		if f.Name != "word/document.xml" {
			continue
		}
		found = true
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		var doc docxDocument
		if err := xml.Unmarshal(data, &doc); err != nil {
			t.Fatalf("word/document.xml is not well-formed: %v", err)
		}
		for _, p := range doc.Body.Paragraphs {
			isBreak := false
			text := ""
			for _, r := range p.Runs {
				if r.Break != nil && r.Break.Type == "page" {
					isBreak = true
				}
				text += r.Text
			}
			if isBreak {
				tokens = append(tokens, pageBreakToken)
			} else {
				tokens = append(tokens, text)
			}
		}
	}
	if !found {
		t.Fatal("output zip has no word/document.xml entry")
	}
	return tokens
}

func TestPDFToDocxRequiredPartsExist(t *testing.T) {
	dir := t.TempDir()
	inPath := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(inPath, buildTextPDF([]string{"Hello"}), 0600); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "out.docx")
	if err := convertPDFToDocx(context.Background(), inPath, outPath); err != nil {
		t.Fatalf("convertPDFToDocx failed: %v", err)
	}
	names := zipEntryNames(t, outPath)
	for _, want := range []string{"[Content_Types].xml", "_rels/.rels", "word/document.xml"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected zip entry %q, got entries %v", want, names)
		}
	}
}

// TestPDFToDocxExtractsRealTextInOrder is the core correctness case:
// build a real 2-page PDF with distinct, genuinely extracted (not
// hand-copied) text per page, convert it, and confirm the output DOCX's
// own document.xml—read back by an independent parser—has both pages'
// text in the right order with a page break in between.
func TestPDFToDocxExtractsRealTextInOrder(t *testing.T) {
	dir := t.TempDir()
	inPath := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(inPath, buildTextPDF([]string{"Hello Page One", "Hello Page Two"}), 0600); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "out.docx")
	if err := convertPDFToDocx(context.Background(), inPath, outPath); err != nil {
		t.Fatalf("convertPDFToDocx failed: %v", err)
	}
	tokens := parseDocx(t, outPath)

	joined := ""
	sawBreak := false
	firstIdx, secondIdx := -1, -1
	for i, tok := range tokens {
		joined += tok + "|"
		if tok == pageBreakToken {
			sawBreak = true
		}
		if tok == "Hello Page One" {
			firstIdx = i
		}
		if tok == "Hello Page Two" {
			secondIdx = i
		}
	}
	if firstIdx == -1 || secondIdx == -1 {
		t.Fatalf("expected both pages' text present, got tokens: %v", tokens)
	}
	if firstIdx >= secondIdx {
		t.Fatalf("expected page one's text before page two's, got tokens: %v", tokens)
	}
	if !sawBreak {
		t.Fatalf("expected a page break token between pages, got tokens: %v", tokens)
	}
}

func TestPDFToDocxRejectsWhenNoTextFound(t *testing.T) {
	dir := t.TempDir()
	inPath := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(inPath, buildBlankPDF(2), 0600); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "out.docx")
	err := convertPDFToDocx(context.Background(), inPath, outPath)
	if err == nil {
		t.Fatal("expected a PDF with no extractable text to be rejected")
	}
	if _, statErr := os.Stat(outPath); statErr == nil {
		t.Fatal("expected no output file to be left behind for a rejected extraction")
	}
}

// TestWriteDocxEscapesSpecialCharacters proves writeDocx's own output is
// well-formed XML even for text containing XML metacharacters and
// non-ASCII content—not routed through a real PDF at all, isolating
// writeDocx's own escaping logic from GetPlainText's extraction.
func TestWriteDocxEscapesSpecialCharacters(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "out.docx")
	pages := []string{"Tom & Jerry <tag> \"quoted\" café\nsecond line\n\nafter a blank line"}
	if err := writeDocx(outPath, pages); err != nil {
		t.Fatalf("writeDocx failed: %v", err)
	}
	tokens := parseDocx(t, outPath)
	want := []string{"Tom & Jerry <tag> \"quoted\" café", "second line", "", "after a blank line"}
	if len(tokens) != len(want) {
		t.Fatalf("expected %d paragraphs, got %d: %v", len(want), len(tokens), tokens)
	}
	for i := range want {
		if tokens[i] != want[i] {
			t.Fatalf("paragraph %d: expected %q, got %q", i, want[i], tokens[i])
		}
	}
}

func TestPDFToDocxAlwaysAdvertised(t *testing.T) {
	c := &converter{}
	if !c.supports("pdf", "docx") {
		t.Fatal("expected pdf -> docx text extraction to be supported without any external tool")
	}
	if c.supports("docx", "pdf") {
		// docx -> pdf needs libreoffice; a bare converter{} has none configured.
		t.Fatal("docx -> pdf must still require libreoffice")
	}
}

// TestPDFToDocxJobFlowsThroughWorker submits a real generated PDF over
// HTTP and lets the actual worker goroutine dequeue and extract it
// exactly as production does, mirroring the other *JobFlowsThroughWorker
// tests in this codebase.
func TestPDFToDocxJobFlowsThroughWorker(t *testing.T) {
	a := testApp(t)
	fixture := buildTextPDF([]string{"Worker pipeline text"})

	w := submitJob(t, a, "doc.pdf", fixture, map[string]string{"outputFormat": "docx"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	var created Job
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
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
	if job.Status != Completed {
		t.Fatalf("expected the PDF -> DOCX job to complete, got %s (error: %q)", job.Status, job.Error)
	}

	dr := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+created.ID+"/download", nil)
	drec := httptest.NewRecorder()
	a.Handler().ServeHTTP(drec, dr)
	if drec.Code != http.StatusOK {
		t.Fatalf("download returned %d", drec.Code)
	}
	outPath := filepath.Join(t.TempDir(), "downloaded.docx")
	if err := os.WriteFile(outPath, drec.Body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	tokens := parseDocx(t, outPath)
	found := false
	for _, tok := range tokens {
		if tok == "Worker pipeline text" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the extracted text in the downloaded DOCX, got tokens: %v", tokens)
	}
}
