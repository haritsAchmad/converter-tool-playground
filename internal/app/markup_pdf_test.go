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

// TestSupportsMarkdownAndHTMLToPDF proves Markdown/HTML->PDF is advertised
// exactly when LibreOffice is available, mirroring how Office->PDF is
// gated (see TestOfficeCapabilitiesDependOnLibreOffice), and that it isn't
// advertised at all without it.
func TestSupportsMarkdownAndHTMLToPDF(t *testing.T) {
	withLO := &converter{libreoffice: "/usr/bin/libreoffice"}
	withoutLO := &converter{}
	for _, in := range []string{"markdown", "html"} {
		if !withLO.supports(in, "pdf") {
			t.Errorf("expected %s->pdf to be supported when libreoffice is available", in)
		}
		if withoutLO.supports(in, "pdf") {
			t.Errorf("expected %s->pdf to be unsupported without libreoffice", in)
		}
	}
}

// TestValidateHTMLForPDF is a table of what convertMarkupToPDF's
// dangerousHTMLPattern reject-list accepts and rejects (see its doc
// comment for the reasoning): active-content tags, inline event handlers,
// javascript: URIs, and any src/srcset/<link href>/CSS url() pointing at
// an external origin are rejected, while an ordinary <a href> hyperlink,
// a relative reference, and a data: URI are left alone.
func TestValidateHTMLForPDF(t *testing.T) {
	cases := []struct {
		name    string
		html    string
		rejects bool
	}{
		{"plain paragraph", `<p>Hello <em>world</em></p>`, false},
		{"hyperlink to external site", `<p><a href="https://example.com">link</a></p>`, false},
		{"relative image", `<img src="local.png">`, false},
		{"data URI image", `<img src="data:image/png;base64,aGVsbG8=">`, false},
		{"script tag", `<script>alert(1)</script>`, true},
		{"iframe tag", `<iframe src="https://example.com"></iframe>`, true},
		{"object tag", `<object data="https://example.com/x.swf"></object>`, true},
		{"embed tag", `<embed src="https://example.com/x.swf">`, true},
		{"base tag", `<base href="http://evil.example/">`, true},
		{"inline event handler", `<div onclick="doStuff()">hi</div>`, true},
		{"javascript URI", `<a href="javascript:alert(1)">click</a>`, true},
		{"external image src", `<img src="http://evil.example/x.png">`, true},
		{"external image srcset", `<img srcset="http://evil.example/x.png 1x">`, true},
		{"protocol-relative src", `<img src="//evil.example/x.png">`, true},
		{"external stylesheet link", `<link rel="stylesheet" href="https://evil.example/x.css">`, true},
		{"external CSS url()", `<style>body{background:url(http://evil.example/bg.png)}</style>`, true},
		{"inline style external url()", `<p style="background:url('https://evil.example/bg.png')">hi</p>`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHTMLForPDF([]byte(tc.html))
			if tc.rejects && err == nil {
				t.Fatalf("expected %q to be rejected, got no error", tc.html)
			}
			if !tc.rejects && err != nil {
				t.Fatalf("expected %q to be accepted, got %v", tc.html, err)
			}
		})
	}
}

// fakeLibreOfficeConverter returns a converter whose libreoffice field
// points at a path that doesn't exist, so any code path that actually
// tries to exec it fails with a distinct, recognizable error rather than
// succeeding—used the same way TestPDFToImageJobFlowsThroughWorkerReload
// fakes pdftoppm, to prove a call site was REACHED without needing a real
// LibreOffice install on this machine.
func fakeLibreOfficeConverter(t *testing.T) *converter {
	t.Helper()
	return &converter{libreoffice: filepath.Join(t.TempDir(), "not-a-real-libreoffice")}
}

// TestConvertMarkupToPDFRejectsDangerousContentBeforeInvokingLibreOffice
// proves validateHTMLForPDF actually gates convertMarkupToPDF: dangerous
// HTML must fail with its own error, not with a "LibreOffice conversion
// failed" exec error, even though the fake libreoffice binary here would
// fail either way—the distinct error message is what proves the reject
// happened before the subprocess was ever started.
func TestConvertMarkupToPDFRejectsDangerousContentBeforeInvokingLibreOffice(t *testing.T) {
	c := fakeLibreOfficeConverter(t)
	cases := map[string]string{
		"html":     `<html><body><script>alert(1)</script></body></html>`,
		"markdown": "hello ![img](http://evil.example/x.png)",
	}
	for in, content := range cases {
		t.Run(in, func(t *testing.T) {
			inPath := filepath.Join(t.TempDir(), "input.bin")
			if err := os.WriteFile(inPath, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			outPath := filepath.Join(t.TempDir(), "output.pdf")
			err := c.run(context.Background(), in, "pdf", "", inPath, outPath)
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), "LibreOffice conversion failed") {
				t.Fatalf("dangerous content reached LibreOffice instead of being rejected first: %v", err)
			}
			if !strings.Contains(err.Error(), "not accepted for PDF rendering") {
				t.Fatalf("expected the HTML-safety rejection, got %v", err)
			}
		})
	}
}

// TestConvertMarkupToPDFReachesLibreOfficeForSafeContent is the mirror of
// the test above: safe HTML (and safe Markdown, including Markdown whose
// source has raw <script> HTML that goldmark's default-safe rendering
// already drops—see convertMarkupToPDF's doc comment) must actually reach
// the LibreOffice exec call rather than being rejected, so this expects
// the fake binary's own exec failure, not the HTML-safety error.
func TestConvertMarkupToPDFReachesLibreOfficeForSafeContent(t *testing.T) {
	c := fakeLibreOfficeConverter(t)
	cases := map[string]string{
		"html":                       `<html><body><p>Hello <em>world</em></p></body></html>`,
		"markdown":                   "# Hello\n\nSome *text*.\n",
		"markdown with raw <script>": "# Hello\n\n<script>alert(1)</script>\n\nSome text.\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			in := "html"
			if strings.HasPrefix(name, "markdown") {
				in = "markdown"
			}
			inPath := filepath.Join(t.TempDir(), "input.bin")
			if err := os.WriteFile(inPath, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			outPath := filepath.Join(t.TempDir(), "output.pdf")
			err := c.run(context.Background(), in, "pdf", "", inPath, outPath)
			if err == nil {
				t.Fatal("expected an error from the fake libreoffice binary")
			}
			if !strings.Contains(err.Error(), "LibreOffice conversion failed") {
				t.Fatalf("expected safe content to reach LibreOffice and fail there, got %v", err)
			}
		})
	}
}

// TestMarkdownAndHTMLToPDFJobsFlowThroughWorker is the full-pipeline
// regression: submit a real Markdown and a real HTML job over HTTP, let
// the actual worker goroutine dequeue and convert it exactly as
// production does (libreoffice faked, same as
// TestPDFToImageJobFlowsThroughWorkerReload does for pdftoppm), and
// confirm the job reaches Failed via a real exec error instead of getting
// dropped or stuck at Queued the way an unadvertised/unsupported pair
// would.
func TestMarkdownAndHTMLToPDFJobsFlowThroughWorker(t *testing.T) {
	cases := []struct {
		name     string
		filename string
		body     string
	}{
		{"markdown", "doc.md", "# Hello\n\nSome *text*.\n"},
		{"html", "doc.html", "<html><body><p>Hello</p></body></html>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(t)
			a.converter.libreoffice = filepath.Join(t.TempDir(), "not-a-real-libreoffice")

			w := submitJob(t, a, tc.filename, []byte(tc.body), map[string]string{"outputFormat": "pdf"})
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

// TestConvertMarkdownAndHTMLToPDF is the real end-to-end case, skipped
// where libreoffice/soffice isn't installed (this project's own Windows
// dev machine included, same as the Office->PDF regression corpus).
func TestConvertMarkdownAndHTMLToPDF(t *testing.T) {
	officeLookPath(t)
	c := newConverter()
	cases := []struct {
		name string
		in   string
		body string
	}{
		{"markdown", "markdown", "# Hello\n\nSome *text* and a local image ![alt](local.png).\n"},
		{"html", "html", `<html><body><h1>Hello</h1><p>Some text and a <a href="https://example.com">link</a>.</p></body></html>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inPath := filepath.Join(t.TempDir(), "input.bin")
			if err := os.WriteFile(inPath, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			outPath := filepath.Join(t.TempDir(), "output.pdf")
			if err := c.run(context.Background(), tc.in, "pdf", "", inPath, outPath); err != nil {
				t.Fatalf("%s to PDF conversion failed: %v", tc.in, err)
			}
			assertValidPDF(t, outPath)
		})
	}
}
