package app

import (
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// validSVG is a small, entirely benign fixture: a red square on a 100x100
// viewBox, using only elements oksvg actually implements.
const validSVG = `<?xml version="1.0" encoding="UTF-8"?>
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100">
  <rect x="10" y="10" width="80" height="80" fill="#ff0000"/>
</svg>`

// TestValidateSVGAcceptsBenignFixture proves the sanitizer's happy path
// isn't accidentally over-strict.
func TestValidateSVGAcceptsBenignFixture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "in.svg")
	if err := os.WriteFile(path, []byte(validSVG), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateSVG(path); err != nil {
		t.Fatalf("expected a benign SVG to validate, got: %v", err)
	}
}

// TestValidateSVGRejectsHostileConstructs is a table of everything
// validateSVG's sanitizer pass must reject outright: script execution,
// inline event handlers, javascript: URIs (including HTML-entity-encoded
// ones a raw-bytes check would miss but a real XML parser decodes), and
// resource references to anything other than a local #fragment or a safe
// inline data: image.
func TestValidateSVGRejectsHostileConstructs(t *testing.T) {
	cases := []struct {
		name string
		svg  string
	}{
		{"script element", `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`},
		{"foreignObject", `<svg xmlns="http://www.w3.org/2000/svg"><foreignObject><body xmlns="http://www.w3.org/1999/xhtml"><script>alert(1)</script></body></foreignObject></svg>`},
		{"embedded image element", `<svg xmlns="http://www.w3.org/2000/svg"><image href="http://169.254.169.254/latest/meta-data/"/></svg>`},
		{"onload handler", `<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"><rect width="1" height="1"/></svg>`},
		{"onclick handler on a shape", `<svg xmlns="http://www.w3.org/2000/svg"><rect width="1" height="1" onclick="alert(1)"/></svg>`},
		{"plain javascript: URI", `<svg xmlns="http://www.w3.org/2000/svg"><a href="javascript:alert(1)"><rect width="1" height="1"/></a></svg>`},
		{"entity-encoded javascript: URI", `<svg xmlns="http://www.w3.org/2000/svg"><a href="&#106;avascript:alert(1)"><rect width="1" height="1"/></a></svg>`},
		{"use referencing external document", `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"><use xlink:href="http://evil.example/x.svg#y"/></svg>`},
		{"external stylesheet import", `<svg xmlns="http://www.w3.org/2000/svg"><style>@import url(http://evil.example/x.css);</style><rect width="1" height="1"/></svg>`},
		{"external CSS url() in style attribute", `<svg xmlns="http://www.w3.org/2000/svg"><rect width="1" height="1" style="fill:url(http://evil.example/x.png)"/></svg>`},
		{"animate element", `<svg xmlns="http://www.w3.org/2000/svg"><rect width="1" height="1"><animate attributeName="x" to="100" dur="1s"/></rect></svg>`},
		{"non-svg root element", `<html xmlns="http://www.w3.org/1999/xhtml"><body>not an svg</body></html>`},
		{"DOCTYPE declaration", "<!DOCTYPE svg SYSTEM \"http://evil.example/x.dtd\">\n" + `<svg xmlns="http://www.w3.org/2000/svg"><rect width="1" height="1"/></svg>`},
		{"billion-laughs-shaped entity bomb", "<!DOCTYPE svg [<!ENTITY x \"y\"><!ENTITY y \"&x;&x;&x;&x;&x;&x;&x;&x;&x;&x;\">]>\n" + `<svg xmlns="http://www.w3.org/2000/svg"><title>&y;</title></svg>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "in.svg")
			if err := os.WriteFile(path, []byte(tc.svg), 0600); err != nil {
				t.Fatal(err)
			}
			if err := validateSVG(path); err == nil {
				t.Fatalf("expected %q to be rejected, but it validated", tc.name)
			}
		})
	}
}

// TestValidateSVGAcceptsLocalFragmentAndSafeDataURI proves the resource
// reference check isn't just a blanket href rejection: a same-document
// #fragment reference (the only thing oksvg's own <use> implementation
// supports anyway) and a safe inline data: image both pass.
func TestValidateSVGAcceptsLocalFragmentAndSafeDataURI(t *testing.T) {
	cases := []struct {
		name string
		svg  string
	}{
		{"local fragment use", `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"><defs><rect id="r" width="1" height="1"/></defs><use xlink:href="#r"/></svg>`},
		{"safe data URI on an anchor", `<svg xmlns="http://www.w3.org/2000/svg"><a href="data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="><rect width="1" height="1"/></a></svg>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "in.svg")
			if err := os.WriteFile(path, []byte(tc.svg), 0600); err != nil {
				t.Fatal(err)
			}
			if err := validateSVG(path); err != nil {
				t.Fatalf("expected %q to validate, got: %v", tc.name, err)
			}
		})
	}
}

func TestValidateUploadRejectsWrongSVGSignature(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.bin")
	if err := os.WriteFile(path, []byte("this is not xml or svg at all"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateUpload(path, "clip.svg"); err == nil {
		t.Fatal("expected a file with no <svg tag to be rejected")
	}
}

// TestConvertSVGRealRaster proves the rasterization boundary actually
// produces a usable image: converts the benign fixture to both PNG and
// JPEG and checks the output decodes and contains the expected red fill
// at a point inside the rect.
func TestConvertSVGRealRaster(t *testing.T) {
	dir := t.TempDir()
	inPath := filepath.Join(dir, "in.svg")
	if err := os.WriteFile(inPath, []byte(validSVG), 0600); err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{"png", "jpeg"} {
		t.Run(out, func(t *testing.T) {
			outPath := filepath.Join(dir, "out."+out)
			if err := convertSVG(out, inPath, outPath); err != nil {
				t.Fatalf("convertSVG failed: %v", err)
			}
			f, err := os.Open(outPath)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			img, format, err := image.Decode(f)
			if err != nil {
				t.Fatalf("output did not decode as an image: %v", err)
			}
			if (out == "png" && format != "png") || (out == "jpeg" && format != "jpeg") {
				t.Fatalf("expected decoded format %s, got %s", out, format)
			}
			b := img.Bounds()
			if b.Dx() != 100 || b.Dy() != 100 {
				t.Fatalf("expected a 100x100 canvas from the viewBox, got %dx%d", b.Dx(), b.Dy())
			}
			r, g, bl, _ := img.At(50, 50).RGBA()
			// Red rect should dominate at the center; allow JPEG lossy drift.
			if !(r > 0x8000 && g < 0x4000 && bl < 0x4000) {
				t.Fatalf("expected red at center pixel, got r=%d g=%d b=%d", r>>8, g>>8, bl>>8)
			}
		})
	}
}

// TestConvertSVGFallsBackWithoutViewBox exercises the defaultSVGCanvasSize
// fallback path: a root <svg> with no width, height, or viewBox at all,
// confirming it renders onto a fixed canvas instead of dividing by a
// zero ViewBox dimension.
func TestConvertSVGFallsBackWithoutViewBox(t *testing.T) {
	svg := `<svg xmlns="http://www.w3.org/2000/svg"><rect x="0" y="0" width="10" height="10" fill="#00ff00"/></svg>`
	dir := t.TempDir()
	inPath := filepath.Join(dir, "in.svg")
	if err := os.WriteFile(inPath, []byte(svg), 0600); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "out.png")
	if err := convertSVG("png", inPath, outPath); err != nil {
		t.Fatalf("convertSVG failed: %v", err)
	}
	f, err := os.Open(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("output did not decode as PNG: %v", err)
	}
	b := img.Bounds()
	if b.Dx() != defaultSVGCanvasSize || b.Dy() != defaultSVGCanvasSize {
		t.Fatalf("expected the %dx%d fallback canvas, got %dx%d", defaultSVGCanvasSize, defaultSVGCanvasSize, b.Dx(), b.Dy())
	}
}

// TestConvertSVGParsesPixelUnitSuffix locks in a real oksvg behavior
// this feature depends on: its attribute parser strips a "px" unit
// suffix before calling strconv.ParseFloat (verified directly against
// oksvg's source, in utils.go's parseFloat/trimSuffixes), so a common
// real-world width="200px"/height="200px" pair with no viewBox still
// produces a usable 200x200 canvas rather than falling back to
// defaultSVGCanvasSize.
func TestConvertSVGParsesPixelUnitSuffix(t *testing.T) {
	svg := `<svg xmlns="http://www.w3.org/2000/svg" width="200px" height="200px"><rect x="0" y="0" width="10" height="10" fill="#00ff00"/></svg>`
	dir := t.TempDir()
	inPath := filepath.Join(dir, "in.svg")
	if err := os.WriteFile(inPath, []byte(svg), 0600); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "out.png")
	if err := convertSVG("png", inPath, outPath); err != nil {
		t.Fatalf("convertSVG failed: %v", err)
	}
	f, err := os.Open(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("output did not decode as PNG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 200 || b.Dy() != 200 {
		t.Fatalf("expected a 200x200 canvas parsed from \"200px\", got %dx%d", b.Dx(), b.Dy())
	}
}

// TestConvertSVGRejectsOversizedCanvas proves the maxImageDecodedPixels
// ceiling is actually enforced against a declared viewBox before the
// raster canvas is allocated, the SVG analog of the PNG/JPEG
// decompression-bomb guard.
func TestConvertSVGRejectsOversizedCanvas(t *testing.T) {
	svg := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20000 20000"><rect width="1" height="1"/></svg>`
	dir := t.TempDir()
	inPath := filepath.Join(dir, "in.svg")
	if err := os.WriteFile(inPath, []byte(svg), 0600); err != nil {
		t.Fatal(err)
	}
	err := convertSVG("png", inPath, filepath.Join(dir, "out.png"))
	if err == nil {
		t.Fatal("expected an oversized SVG canvas to be rejected")
	}
}

func TestSVGCapabilities(t *testing.T) {
	c := &converter{}
	if !c.supports("svg", "png") {
		t.Fatal("expected svg -> png to be supported (pure Go, no external tool required)")
	}
	if !c.supports("svg", "jpeg") {
		t.Fatal("expected svg -> jpeg to be supported")
	}
	if c.supports("svg", "webp") {
		t.Fatal("svg -> webp must not be advertised (no pure-Go WebP encoder)")
	}
	if c.supports("svg", "svg") {
		t.Fatal("svg -> svg must not be advertised")
	}
	if c.supports("png", "svg") {
		t.Fatal("nothing converts TO svg in this slice")
	}
}

// TestSVGJobFlowsThroughWorker submits a real SVG job over HTTP and lets
// the actual worker goroutine dequeue and rasterize it exactly as
// production does, mirroring TestAudioJobFlowsThroughWorker/
// TestVideoJobFlowsThroughWorker.
func TestSVGJobFlowsThroughWorker(t *testing.T) {
	a := testApp(t)

	w := submitJob(t, a, "clip.svg", []byte(validSVG), map[string]string{"outputFormat": "png"})
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
		t.Fatalf("expected the SVG -> PNG job to complete, got %s (error: %q)", job.Status, job.Error)
	}
}

// TestSVGJobRejectsHostileUpload proves the sanitizer is actually wired
// into the upload path, not just covered as an internal function.
func TestSVGJobRejectsHostileUpload(t *testing.T) {
	a := testApp(t)
	hostile := `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(document.cookie)</script></svg>`
	w := submitJob(t, a, "hostile.svg", []byte(hostile), map[string]string{"outputFormat": "png"})
	if w.Code == http.StatusAccepted {
		t.Fatalf("expected the upload with a <script> element to be rejected, got 202: %s", w.Body.String())
	}
}
