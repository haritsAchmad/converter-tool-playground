package app

import (
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// waitForJob polls the status endpoint until the job leaves the queue.
func waitForJob(t *testing.T, a *App, id string) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var job Job
	for time.Now().Before(deadline) {
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+id, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status endpoint returned %d for a live job", rec.Code)
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &job)
		if job.Status == Completed || job.Status == Failed {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish, last status %s", id, job.Status)
	return job
}

func submitSVG(t *testing.T, a *App, svg string) string {
	t.Helper()
	w := submitJob(t, a, "in.svg", []byte(svg), map[string]string{"outputFormat": "png"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	var created Job
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	return created.ID
}

// panicSVG is the exact reproduction from review finding P1: it passes
// validateSVG, then oksvg.ReadIcon panics ("slice bounds out of range
// [:-1]") parsing the malformed hsl() color.
const panicSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect width="10" height="10" fill="hsl(0,,)"/></svg>`

// TestSVGParserPanicFailsJobWithoutKillingWorker goes through the real
// upload + worker path: the hostile job must end Failed, and the same
// worker must still complete an ordinary job afterwards. Before the fix
// the panic escaped the worker goroutine and crashed the test binary.
func TestSVGParserPanicFailsJobWithoutKillingWorker(t *testing.T) {
	a := testApp(t)
	if job := waitForJob(t, a, submitSVG(t, a, panicSVG)); job.Status != Failed {
		t.Fatalf("expected the panicking SVG job to fail, got %s", job.Status)
	}
	if job := waitForJob(t, a, submitSVG(t, a, validSVG)); job.Status != Completed {
		t.Fatalf("worker did not survive: follow-up job ended %s (error: %q)", job.Status, job.Error)
	}
}

func TestConvertSVGRecoversParserPanic(t *testing.T) {
	inPath := writeSVG(t, panicSVG)
	if err := convertSVG(context.Background(), "png", inPath, filepath.Join(t.TempDir(), "out.png")); err == nil {
		t.Fatal("expected an error from a panicking SVG parse")
	}
}

// Review finding P2: ctx never reached convertSVG, so an already-expired
// job still produced output and returned success.
func TestConvertSVGHonorsCancellation(t *testing.T) {
	inPath := writeSVG(t, validSVG)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	outPath := filepath.Join(t.TempDir(), "out.png")
	if err := convertSVG(ctx, "png", inPath, outPath); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Fatalf("a cancelled conversion must not write output (stat err: %v)", err)
	}
}

// Review finding P2: the canvas must follow the declared viewport
// (width/height), with the viewBox only mapping coordinates onto it.
func TestConvertSVGCanvasFollowsViewport(t *testing.T) {
	const red = `fill="#ff0000"`
	cases := []struct {
		name           string
		svg            string
		w, h           int
		redAt, clearAt []image.Point
	}{
		{
			name: "review reproduction",
			svg:  `<svg xmlns="http://www.w3.org/2000/svg" width="200" height="100" viewBox="0 0 20 10"><rect width="20" height="10" ` + red + `/></svg>`,
			w:    200, h: 100,
			redAt: []image.Point{{5, 5}, {100, 50}, {194, 94}},
		},
		{
			// Non-zero viewBox origin: oksvg's own SetTarget would shift
			// this by the unscaled origin once the scale is not 1.
			name: "viewBox with origin",
			svg:  `<svg xmlns="http://www.w3.org/2000/svg" width="200" height="100" viewBox="10 10 20 10"><rect x="10" y="10" width="10" height="10" ` + red + `/></svg>`,
			w:    200, h: 100,
			redAt:   []image.Point{{5, 5}, {95, 95}},
			clearAt: []image.Point{{105, 50}},
		},
		{
			// Default xMidYMid meet: a square viewBox centered in a 2:1
			// viewport, transparent bars left and right.
			name: "meet letterboxes",
			svg:  `<svg xmlns="http://www.w3.org/2000/svg" width="200" height="100" viewBox="0 0 10 10"><rect width="10" height="10" ` + red + `/></svg>`,
			w:    200, h: 100,
			redAt:   []image.Point{{100, 50}, {55, 50}},
			clearAt: []image.Point{{25, 50}, {175, 50}},
		},
		{
			name: "preserveAspectRatio none stretches",
			svg:  `<svg xmlns="http://www.w3.org/2000/svg" width="200" height="100" viewBox="0 0 10 10" preserveAspectRatio="none"><rect width="10" height="10" ` + red + `/></svg>`,
			w:    200, h: 100,
			redAt: []image.Point{{25, 50}, {175, 50}},
		},
		{
			name: "only width uses viewBox aspect ratio",
			svg:  `<svg xmlns="http://www.w3.org/2000/svg" width="300" viewBox="0 0 20 10"><rect width="20" height="10" ` + red + `/></svg>`,
			w:    300, h: 150,
		},
		{
			name: "absolute units",
			svg:  `<svg xmlns="http://www.w3.org/2000/svg" width="2in" height="1in" viewBox="0 0 20 10"><rect width="20" height="10" ` + red + `/></svg>`,
			w:    192, h: 96,
		},
		{
			name: "percentages fall back to viewBox",
			svg:  `<svg xmlns="http://www.w3.org/2000/svg" width="100%" height="100%" viewBox="0 0 20 10"><rect width="20" height="10" ` + red + `/></svg>`,
			w:    20, h: 10,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outPath := filepath.Join(t.TempDir(), "out.png")
			if err := convertSVG(context.Background(), "png", writeSVG(t, tc.svg), outPath); err != nil {
				t.Fatalf("convertSVG failed: %v", err)
			}
			img := decodePNG(t, outPath)
			if b := img.Bounds(); b.Dx() != tc.w || b.Dy() != tc.h {
				t.Fatalf("expected %dx%d canvas, got %dx%d", tc.w, tc.h, b.Dx(), b.Dy())
			}
			for _, p := range tc.redAt {
				if r, g, b, a := img.At(p.X, p.Y).RGBA(); !(a > 0xf000 && r > 0xf000 && g < 0x1000 && b < 0x1000) {
					t.Errorf("expected red at %v, got r=%d g=%d b=%d a=%d", p, r>>8, g>>8, b>>8, a>>8)
				}
			}
			for _, p := range tc.clearAt {
				if _, _, _, a := img.At(p.X, p.Y).RGBA(); a != 0 {
					t.Errorf("expected transparent at %v, got alpha %d", p, a>>8)
				}
			}
		})
	}
}

func writeSVG(t *testing.T, svg string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "in.svg")
	if err := os.WriteFile(path, []byte(svg), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func decodePNG(t *testing.T, path string) image.Image {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("output did not decode as PNG: %v", err)
	}
	return img
}
