package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
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

// TestConvertPDFRendersFirstPage exercises the actual pdftoppm subprocess;
// it skips itself where poppler-utils isn't installed (this project's own
// Windows dev machine included), and runs for real in the Docker image and
// in CI, which both install poppler-utils specifically so this path isn't
// silently untested everywhere.
func TestConvertPDFRendersFirstPage(t *testing.T) {
	if _, err := exec.LookPath("pdftoppm"); err != nil {
		t.Skip("pdftoppm not installed, skipping PDF rendering test")
	}
	dir := t.TempDir()
	inPath := filepath.Join(dir, "input.bin")
	if err := os.WriteFile(inPath, []byte(minimalPDF), 0600); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "output.png")
	c := newConverter()
	if err := c.run(context.Background(), "pdf", "png", inPath, outPath); err != nil {
		t.Fatalf("PDF render failed: %v", err)
	}
	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatalf("expected rendered output, got: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("rendered PNG is empty")
	}
	head := make([]byte, 8)
	f, _ := os.Open(outPath)
	_, _ = f.Read(head)
	_ = f.Close()
	if string(head[1:4]) != "PNG" {
		t.Fatalf("output does not look like a PNG: %x", head)
	}
}
