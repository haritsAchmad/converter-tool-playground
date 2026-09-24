package app

import (
	"archive/zip"
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"
)

// A hand-built, minimal but structurally complete single-page PDF, used
// instead of a binary fixture so the test stays readable and self-contained.
const minimalPDF = "%PDF-1.4\n" +
	"1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n" +
	"2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n" +
	"3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] >>\nendobj\n" +
	"xref\n0 4\n0000000000 65535 f \n0000000009 00000 n \n0000000058 00000 n \n0000000115 00000 n \n" +
	"trailer\n<< /Size 4 /Root 1 0 R >>\nstartxref\n190\n%%EOF"

func TestValidatePDFAcceptsWellFormedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.bin")
	if err := os.WriteFile(path, []byte(minimalPDF), 0600); err != nil {
		t.Fatal(err)
	}
	candidate, err := validateUpload(path, "doc.pdf")
	if err != nil {
		t.Fatalf("expected well-formed PDF to validate, got: %v", err)
	}
	if candidate != "pdf" {
		t.Fatalf("expected candidate %q, got %q", "pdf", candidate)
	}
}

func TestValidatePDFRejectsFakeSignature(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.bin")
	if err := os.WriteFile(path, []byte("not actually a pdf, just renamed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateUpload(path, "doc.pdf"); err == nil {
		t.Fatal("expected file without a %PDF- signature to be rejected")
	}
}

// TestValidatePDFRejectsStructurallyBroken has a real %PDF- signature (so it
// clears the magic-byte check) but garbage everywhere else, proving the
// pdfcpu structural pass in validateSyntax catches what a signature check
// alone would let through.
func TestValidatePDFRejectsStructurallyBroken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.bin")
	if err := os.WriteFile(path, []byte("%PDF-1.4\nthis is not a valid PDF body at all\n%%EOF"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateUpload(path, "doc.pdf"); err == nil {
		t.Fatal("expected structurally broken PDF to be rejected")
	}
}

// tinyPNG returns a solid-color w*h PNG, small enough that building multi-
// page fixtures (or, deliberately, a fixture with hundreds of pages) with
// it stays fast.
func tinyPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// zipEntryNames returns the sorted-by-appearance entry names of a zip file,
// failing the test if it doesn't parse as a zip at all.
func zipEntryNames(t *testing.T, path string) []string {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("expected output to be a valid zip, got: %v", err)
	}
	defer r.Close()
	names := make([]string, 0, len(r.File))
	for _, f := range r.File {
		names = append(names, f.Name)
	}
	return names
}

// TestConvertPDFRendersAllPagesAsZip exercises the actual pdftoppm
// subprocess; it skips itself where poppler-utils isn't installed (this
// project's own Windows dev machine included), and runs for real in the
// Docker image and in CI, which both install poppler-utils specifically so
// this path isn't silently untested everywhere.
func TestConvertPDFRendersAllPagesAsZip(t *testing.T) {
	if _, err := exec.LookPath("pdftoppm"); err != nil {
		t.Skip("pdftoppm not installed, skipping PDF rendering test")
	}
	dir := t.TempDir()
	inPath := filepath.Join(dir, "input.bin")
	if err := os.WriteFile(inPath, []byte(minimalPDF), 0600); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "output.zip")
	c := newConverter()
	if err := c.run(context.Background(), "pdf", "png", "", inPath, outPath); err != nil {
		t.Fatalf("PDF render failed: %v", err)
	}
	names := zipEntryNames(t, outPath)
	if len(names) != 1 || names[0] != "page-1.png" {
		t.Fatalf("expected a single page-1.png entry, got %v", names)
	}
	r, _ := zip.OpenReader(outPath)
	defer r.Close()
	rc, err := r.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 8)
	_, _ = rc.Read(head)
	_ = rc.Close()
	if string(head[1:4]) != "PNG" {
		t.Fatalf("zip entry does not look like a PNG: %x", head)
	}
	// The intermediate root-1.png pdftoppm produced must not survive
	// alongside the zip—convertPDF is documented to clean those up.
	entries, _ := os.ReadDir(dir)
	pngCount := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".png" {
			pngCount++
		}
	}
	if pngCount != 0 {
		t.Fatalf("expected intermediate per-page PNGs to be removed, found %d still on disk", pngCount)
	}
}

// TestConvertPDFRendersMultiplePagesAsZip builds a real multi-page PDF (via
// convertImageToPDF's own pdfcpu import path, feeding it several synthetic
// images) and confirms every page comes back as its own zip entry, in page
// order.
func TestConvertPDFRendersMultiplePagesAsZip(t *testing.T) {
	if _, err := exec.LookPath("pdftoppm"); err != nil {
		t.Skip("pdftoppm not installed, skipping PDF rendering test")
	}
	dir := t.TempDir()
	const pageCount = 3
	imgPaths := make([]string, pageCount)
	for i := range imgPaths {
		p := filepath.Join(dir, "page"+string(rune('a'+i))+".png")
		if err := os.WriteFile(p, tinyPNG(t, 20, 20), 0600); err != nil {
			t.Fatal(err)
		}
		imgPaths[i] = p
	}
	multiPagePDF := filepath.Join(dir, "multi.pdf")
	imp := api.DefaultImportConfig()
	if err := api.ImportImagesFile(imgPaths, multiPagePDF, imp, nil); err != nil {
		t.Fatalf("could not build multi-page PDF fixture: %v", err)
	}

	inPath := filepath.Join(dir, "input.bin")
	pdfBytes, err := os.ReadFile(multiPagePDF)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inPath, pdfBytes, 0600); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "output.zip")
	c := newConverter()
	if err := c.run(context.Background(), "pdf", "jpeg", "", inPath, outPath); err != nil {
		t.Fatalf("PDF render failed: %v", err)
	}
	names := zipEntryNames(t, outPath)
	want := []string{"page-1.jpg", "page-2.jpg", "page-3.jpg"}
	if len(names) != len(want) {
		t.Fatalf("expected %d pages, got %v", pageCount, names)
	}
	for i, w := range want {
		if names[i] != w {
			t.Fatalf("expected entries in page order %v, got %v", want, names)
		}
	}
}

// TestRenderedPDFPagesSortsNumerically locks in that page ordering comes
// from the parsed page number, not filename string order, where poppler's
// zero-padding width (which depends on total page count) would otherwise
// make e.g. "root-10.png" sort before "root-2.png".
func TestRenderedPDFPagesSortsNumerically(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	for _, n := range []string{"2", "10", "1"} {
		if err := os.WriteFile(root+"-"+n+".png", []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	pages, err := renderedPDFPages(root, "png")
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 3 || pages[0].number != 1 || pages[1].number != 2 || pages[2].number != 10 {
		t.Fatalf("expected pages sorted 1, 2, 10 by number, got %+v", pages)
	}
}

// TestZipRenderedPagesRoundTrip exercises zipRenderedPages directly (no
// pdftoppm needed) so the archive-building logic itself is covered even on
// machines without poppler-utils installed.
func TestZipRenderedPagesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	pages := []renderedPDFPage{
		{number: 1, path: filepath.Join(dir, "root-1.png")},
		{number: 2, path: filepath.Join(dir, "root-2.png")},
	}
	for i, p := range pages {
		if err := os.WriteFile(p.path, []byte{byte(i), 'x'}, 0600); err != nil {
			t.Fatal(err)
		}
	}
	outPath := filepath.Join(dir, "out.zip")
	if err := zipRenderedPages(outPath, pages, "png"); err != nil {
		t.Fatal(err)
	}
	names := zipEntryNames(t, outPath)
	if len(names) != 2 || names[0] != "page-1.png" || names[1] != "page-2.png" {
		t.Fatalf("expected page-1.png, page-2.png, got %v", names)
	}
}

// TestConvertImageToPDF is pure Go (pdfcpu, no external tool), so it runs
// for real on every machine including this project's Windows dev box.
func TestConvertImageToPDF(t *testing.T) {
	dir := t.TempDir()
	inPath := filepath.Join(dir, "input.bin")
	if err := os.WriteFile(inPath, tinyPNG(t, 300, 150), 0600); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "output.pdf")
	if err := convertImageToPDF(inPath, outPath); err != nil {
		t.Fatalf("image to PDF conversion failed: %v", err)
	}
	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(b, []byte("%PDF-")) {
		t.Fatalf("output does not look like a PDF: %x", b[:8])
	}
	count, err := api.PageCountFile(outPath)
	if err != nil {
		t.Fatalf("could not read page count of generated PDF: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected a single-page PDF, got %d pages", count)
	}
}

// TestConvertImageToPDFRejectsInvalidImage is the malformed-input case for
// the new image->PDF path: garbage input must fail cleanly, not panic.
func TestConvertImageToPDFRejectsInvalidImage(t *testing.T) {
	dir := t.TempDir()
	inPath := filepath.Join(dir, "input.bin")
	if err := os.WriteFile(inPath, []byte("not an image"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := convertImageToPDF(inPath, filepath.Join(dir, "output.pdf")); err == nil {
		t.Fatal("expected non-image input to be rejected")
	}
}

// TestImageToPDFIsAlwaysAdvertised locks in that PNG/JPEG/WebP -> PDF is
// pure Go (via pdfcpu) and so, unlike every other conversion pair touching
// PDF or WebP in this codebase, doesn't depend on any external tool being
// on PATH.
func TestImageToPDFIsAlwaysAdvertised(t *testing.T) {
	c := &converter{}
	for _, format := range []string{"png", "jpeg", "webp"} {
		if !c.supports(format, "pdf") {
			t.Fatalf("expected %s -> PDF to be supported without any external tool configured", format)
		}
	}
	if c.supports("pdf", "png") {
		t.Fatal("PDF -> PNG must still require pdftoppm")
	}
	if c.supports("csv", "pdf") {
		t.Fatal("structured-data -> PDF must not be advertised (out of scope)")
	}
}

// TestValidatePDFRejectsExcessivePageCount is a resource-boundary test:
// convertPDF renders and zips one file per page, so page count needs an
// upstream cap the same way OOXML entry count and image pixel count do.
func TestValidatePDFRejectsExcessivePageCount(t *testing.T) {
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "page.png")
	if err := os.WriteFile(imgPath, tinyPNG(t, 4, 4), 0600); err != nil {
		t.Fatal(err)
	}
	imgPaths := make([]string, maxPDFPages+1)
	for i := range imgPaths {
		imgPaths[i] = imgPath
	}
	bigPDF := filepath.Join(dir, "big.pdf")
	imp := api.DefaultImportConfig()
	if err := api.ImportImagesFile(imgPaths, bigPDF, imp, nil); err != nil {
		t.Fatalf("could not build oversized PDF fixture: %v", err)
	}
	pdfBytes, err := os.ReadFile(bigPDF)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "input.bin")
	if err := os.WriteFile(path, pdfBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateUpload(path, "big.pdf"); err == nil {
		t.Fatalf("expected a %d-page PDF to be rejected (limit is %d)", maxPDFPages+1, maxPDFPages)
	}
}
