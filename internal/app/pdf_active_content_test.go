package app

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// buildTestPDF assembles a minimal PDF from raw object bodies (each
// already formatted as "N 0 obj\n<<...>>\nendobj\n" or similar), computing
// each xref offset from the actual bytes written rather than hand-counting
// them the way minimalPDF above does—needed here since each active-content
// fixture has a different object count/size. rootObjNum names the Catalog
// object for the trailer's /Root entry.
func buildTestPDF(t *testing.T, rootObjNum int, objects map[int]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.7\n")
	offsets := make(map[int]int, len(objects))
	maxObjNum := 0
	for n := range objects {
		if n > maxObjNum {
			maxObjNum = n
		}
	}
	for n := 1; n <= maxObjNum; n++ {
		body, ok := objects[n]
		if !ok {
			continue
		}
		offsets[n] = buf.Len()
		buf.WriteString(body)
	}
	xrefStart := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", maxObjNum+1)
	buf.WriteString("0000000000 65535 f \n")
	for n := 1; n <= maxObjNum; n++ {
		if off, ok := offsets[n]; ok {
			fmt.Fprintf(&buf, "%010d 00000 n \n", off)
		} else {
			buf.WriteString("0000000000 00000 f \n")
		}
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root %d 0 R >>\nstartxref\n%d\n%%%%EOF", maxObjNum+1, rootObjNum, xrefStart)
	return buf.Bytes()
}

func validatePDFFixture(t *testing.T, pdf []byte) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input.bin")
	if err := os.WriteFile(path, pdf, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := validateUpload(path, "doc.pdf")
	return err
}

// TestValidatePDFAcceptsOrdinaryDocument is the baseline: a Catalog with
// no OpenAction/AA/Names, no attachments, must still validate—proving the
// new active-content checks don't collaterally reject a plain PDF the way
// minimalPDF's own test already does, but built through buildTestPDF so
// every other fixture in this file is directly comparable to it.
func TestValidatePDFAcceptsOrdinaryDocument(t *testing.T) {
	pdf := buildTestPDF(t, 1, map[int]string{
		1: "1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n",
		2: "2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n",
		3: "3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] >>\nendobj\n",
	})
	if err := validatePDFFixture(t, pdf); err != nil {
		t.Fatalf("expected an ordinary PDF to validate, got: %v", err)
	}
}

// TestValidatePDFRejectsOpenAction proves a document-level /OpenAction
// (fires automatically the moment a viewer opens the document, no click
// needed) is rejected outright, regardless of the action's own subtype.
func TestValidatePDFRejectsOpenAction(t *testing.T) {
	pdf := buildTestPDF(t, 1, map[int]string{
		1: "1 0 obj\n<< /Type /Catalog /Pages 2 0 R /OpenAction << /Type /Action /S /JavaScript /JS (app.alert\\(1\\)) >> >>\nendobj\n",
		2: "2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n",
		3: "3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] >>\nendobj\n",
	})
	if err := validatePDFFixture(t, pdf); err == nil {
		t.Fatal("expected a PDF with an /OpenAction to be rejected")
	}
}

// TestValidatePDFRejectsDocumentLevelAdditionalActions proves a
// document-level /AA entry (e.g. WillClose/WillSave firing without any
// user interaction) is rejected.
func TestValidatePDFRejectsDocumentLevelAdditionalActions(t *testing.T) {
	pdf := buildTestPDF(t, 1, map[int]string{
		1: "1 0 obj\n<< /Type /Catalog /Pages 2 0 R /AA << /WC 4 0 R >> >>\nendobj\n",
		2: "2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n",
		3: "3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] >>\nendobj\n",
		4: "4 0 obj\n<< /Type /Action /S /JavaScript /JS (app.alert\\(1\\)) >>\nendobj\n",
	})
	if err := validatePDFFixture(t, pdf); err == nil {
		t.Fatal("expected a PDF with a document-level /AA entry to be rejected")
	}
}

// TestValidatePDFRejectsJavaScriptNameTree proves an embedded JavaScript
// name tree is rejected even without an explicit /OpenAction pointing to
// it directly—the script can still be reachable via other document/field
// events once it's registered there at all.
func TestValidatePDFRejectsJavaScriptNameTree(t *testing.T) {
	pdf := buildTestPDF(t, 1, map[int]string{
		1: "1 0 obj\n<< /Type /Catalog /Pages 2 0 R /Names << /JavaScript 4 0 R >> >>\nendobj\n",
		2: "2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n",
		3: "3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] >>\nendobj\n",
		4: "4 0 obj\n<< /Names [(myScript) 5 0 R] >>\nendobj\n",
		5: "5 0 obj\n<< /S /JavaScript /JS (app.alert\\(1\\)) >>\nendobj\n",
	})
	if err := validatePDFFixture(t, pdf); err == nil {
		t.Fatal("expected a PDF with an embedded JavaScript name tree to be rejected")
	}
}

// TestValidatePDFRejectsEmbeddedFileAttachment proves an embedded file
// attachment (registered via the /EmbeddedFiles name tree, PDF's own
// mechanism for smuggling an arbitrary file inside a document) is
// rejected.
func TestValidatePDFRejectsEmbeddedFileAttachment(t *testing.T) {
	pdf := buildTestPDF(t, 1, map[int]string{
		1: "1 0 obj\n<< /Type /Catalog /Pages 2 0 R /Names << /EmbeddedFiles 4 0 R >> >>\nendobj\n",
		2: "2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n",
		3: "3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] >>\nendobj\n",
		4: "4 0 obj\n<< /Names [(payload.txt) 5 0 R] >>\nendobj\n",
		5: "5 0 obj\n<< /Type /Filespec /F (payload.txt) /EF << /F 6 0 R >> >>\nendobj\n",
		6: "6 0 obj\n<< /Type /EmbeddedFile /Length 4 >>\nstream\ntest\nendstream\nendobj\n",
	})
	if err := validatePDFFixture(t, pdf); err == nil {
		t.Fatal("expected a PDF with an embedded file attachment to be rejected")
	}
}

// TestValidatePDFRejectsFileAttachmentAnnotation proves an attachment
// declared entirely through a page's /Annots (a /Subtype /FileAttachment
// annotation carrying its own /FS/EF, PDF's "paperclip icon" attachment)
// is rejected even with no /EmbeddedFiles name tree at all—the gap
// neither Names["EmbeddedFiles"] nor ctx.ListAttachments() covers on
// their own, since both only look at the catalog's Names dictionary
// (temuan review P2).
func TestValidatePDFRejectsFileAttachmentAnnotation(t *testing.T) {
	pdf := buildTestPDF(t, 1, map[int]string{
		1: "1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n",
		2: "2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n",
		3: "3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Annots [4 0 R] >>\nendobj\n",
		4: "4 0 obj\n<< /Type /Annot /Subtype /FileAttachment /Rect [0 0 10 10] /FS 5 0 R >>\nendobj\n",
		5: "5 0 obj\n<< /Type /Filespec /F (payload.txt) /EF << /F 6 0 R >> >>\nendobj\n",
		6: "6 0 obj\n<< /Type /EmbeddedFile /Length 4 >>\nstream\ntest\nendstream\nendobj\n",
	})
	if err := validatePDFFixture(t, pdf); err == nil {
		t.Fatal("expected a PDF with a FileAttachment annotation to be rejected")
	}
}
