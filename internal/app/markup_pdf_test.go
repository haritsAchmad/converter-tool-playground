package app

import (
	"context"
	"encoding/base64"
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

// TestValidateHTMLForPDF is a table of what validateHTMLForPDF accepts and
// rejects (see its doc comment for the reasoning): active-content tags,
// inline event handlers, javascript: URIs, and any resource reference
// (src/srcset/poster/background/formaction/action/an <object>'s
// data/a <link> href/CSS url()) other than an inline data: URI are
// rejected—including a same-directory relative path or an absolute
// filesystem path, not just an external http(s) URL—while an ordinary
// <a href> hyperlink and a data: URI are left alone.
func TestValidateHTMLForPDF(t *testing.T) {
	validPNGDataURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(tinyPNG(t, 2, 2))
	// A PNG header alone (no pixel data needed—see pngHeader/convert_test.go)
	// declaring dimensions far past maxImageDecodedPixels.
	oversizedPNGDataURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngHeader(t, 40000, 40000))
	mismatchedMimeDataURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("not actually a png"))
	// temuan review P1, round 3: a data: URI whose declared media type
	// isn't a raster image at all. text/css is the concrete exploit—its
	// decoded content is parsed by LibreOffice as an ordinary stylesheet,
	// exactly like a <style> block, so a nested external url() inside it
	// reaches LibreOffice's fetch path the same as if it had been written
	// directly in the HTML, unless the data: URI itself is rejected before
	// ever being decoded.
	cssDataURI := "data:text/css;base64," + base64.StdEncoding.EncodeToString([]byte("body{background:url(http://127.0.0.1:8080/probe.png)}"))
	svgDataURI := "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))

	cases := []struct {
		name    string
		html    string
		rejects bool
	}{
		{"plain paragraph", `<p>Hello <em>world</em></p>`, false},
		{"hyperlink to external site", `<p><a href="https://example.com">link</a></p>`, false},
		{"hyperlink to local path", `<p><a href="../other-job/output.png">link</a></p>`, false},
		{"valid PNG data URI image", `<img src="` + validPNGDataURI + `">`, false},
		{"oversized PNG data URI image (decompression bomb)", `<img src="` + oversizedPNGDataURI + `">`, true},
		{"data URI declaring image/png with non-PNG bytes", `<img src="` + mismatchedMimeDataURI + `">`, true},
		{"data URI stylesheet with a nested external url()", `<link rel="stylesheet" href="` + cssDataURI + `">`, true},
		{"data URI SVG with an embedded script", `<img src="` + svgDataURI + `">`, true},
		{"non-base64 data URI", `<img src="data:image/png,not-base64-encoded">`, true},
		{"script tag", `<script>alert(1)</script>`, true},
		{"iframe tag", `<iframe src="https://example.com"></iframe>`, true},
		{"object tag", `<object data="https://example.com/x.swf"></object>`, true},
		{"embed tag", `<embed src="https://example.com/x.swf">`, true},
		{"base tag", `<base href="http://evil.example/">`, true},
		{"inline event handler", `<div onclick="doStuff()">hi</div>`, true},
		{"javascript URI on img src", `<img src="javascript:alert(1)">`, true},
		{"javascript URI on a href", `<a href="javascript:alert(1)">click</a>`, true},
		{"external image src", `<img src="http://evil.example/x.png">`, true},
		{"external image srcset", `<img srcset="http://evil.example/x.png 1x">`, true},
		{"protocol-relative src", `<img src="//evil.example/x.png">`, true},
		{"external stylesheet link", `<link rel="stylesheet" href="https://evil.example/x.css">`, true},
		{"external CSS url()", `<style>body{background:url(http://evil.example/bg.png)}</style>`, true},
		{"inline style external url()", `<p style="background:url('https://evil.example/bg.png')">hi</p>`, true},
		// temuan review P1, finding 1: an HTML entity-encoded scheme
		// ("&#104;ttp:" decodes to "http:") must still be caught. A raw
		// byte-level regex over the undecoded source misses this; parsing
		// the document and checking the decoded attribute value doesn't.
		{"HTML entity-encoded external URL", `<img src="&#104;ttp://127.0.0.1:8080/probe.png">`, true},
		// temuan review P1, finding 2: a relative or absolute local path is
		// not "just a missing image"—LibreOffice resolves it against the
		// staged document's real location on disk, so it can walk out of
		// the per-job directory into a sibling job's files or anywhere else
		// readable, or read an absolute path directly.
		{"relative path traversal", `<img src="../../other-job/output.png">`, true},
		{"absolute filesystem path", `<img src="/tmp/private.png">`, true},
		{"plain relative image", `<img src="local.png">`, true},
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
	cases := []struct {
		name, in, content string
	}{
		{"html script tag", "html", `<html><body><script>alert(1)</script></body></html>`},
		{"markdown external image", "markdown", "hello ![img](http://evil.example/x.png)"},
		// temuan review P1: an HTML entity-encoded scheme, and a
		// relative/absolute local path, both have to be rejected here too,
		// not just in the validateHTMLForPDF unit table above—proving the
		// full convertMarkupToPDF call path (parse -> validate -> exec)
		// actually stops before LibreOffice for both.
		{"html entity-encoded external URL", "html", `<html><body><img src="&#104;ttp://127.0.0.1:8080/probe.png"></body></html>`},
		{"html path traversal", "html", `<html><body><img src="../../other-job/output.png"></body></html>`},
		{"html absolute filesystem path", "html", `<html><body><img src="/tmp/private.png"></body></html>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inPath := filepath.Join(t.TempDir(), "input.bin")
			if err := os.WriteFile(inPath, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			outPath := filepath.Join(t.TempDir(), "output.pdf")
			err := c.run(context.Background(), tc.in, "pdf", "", inPath, outPath)
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
		{"markdown", "markdown", "# Hello\n\nSome *text* and a [link](https://example.com).\n"},
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
