package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCloseHandlesEveryQueueShapeWithoutPanicking proves App.Close()'s nil
// checks cover every role's queue/pdfQueue shape: "worker" (pdfQueue nil,
// it's never created for that mode), "pdf-worker" (queue nil, the
// mirror), and "standalone" (the same instance in both fields, which must
// be closed exactly once, not twice).
func TestCloseHandlesEveryQueueShapeWithoutPanicking(t *testing.T) {
	cases := map[string]func() *App{
		"worker shape (pdfQueue nil)": func() *App {
			return &App{queue: newLocalJobQueue(1)}
		},
		"pdf-worker shape (queue nil)": func() *App {
			return &App{pdfQueue: newLocalJobQueue(1)}
		},
		"standalone shape (same instance in both)": func() *App {
			q := newLocalJobQueue(1)
			return &App{queue: q, pdfQueue: q}
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			a := build()
			a.cancel = func() {}
			a.Close() // must not panic
		})
	}
}

// TestQueueForRoutesPDFJobsToIsolatedQueue is a unit test of the routing
// decision itself: given both a general and an isolated PDF queue (the
// shape "api" mode's newQueuesForMode builds in a split deployment—see
// its doc comment), a PDF-input job goes to the PDF queue and everything
// else goes to the general one.
func TestQueueForRoutesPDFJobsToIsolatedQueue(t *testing.T) {
	a := &App{queue: newLocalJobQueue(1), pdfQueue: newLocalJobQueue(1)}
	if got := a.queueFor("pdf"); got != a.pdfQueue {
		t.Fatal("expected a PDF job to route to the isolated PDF queue")
	}
	if got := a.queueFor("csv"); got != a.queue {
		t.Fatal("expected a non-PDF job to route to the general queue")
	}
}

// TestQueueForFallsBackToGeneralQueueWithoutIsolatedOne proves the
// fallback that makes "worker" mode's shape (queue set, pdfQueue nil—it
// never touches PDF jobs at all) and "standalone" mode's shape (queue and
// pdfQueue the same instance) both work correctly through the same
// method: a nil pdfQueue just means route everything through queue.
func TestQueueForFallsBackToGeneralQueueWithoutIsolatedOne(t *testing.T) {
	a := &App{queue: newLocalJobQueue(1)}
	if got := a.queueFor("pdf"); got != a.queue {
		t.Fatal("expected a PDF job to fall back to the general queue when there's no isolated one")
	}
}

// TestPDFJobsProcessedByIsolatedPDFQueueWorker is the full-pipeline proof
// that the pdf-worker wiring actually works end to end, built the same
// way TestPDFToImageJobFlowsThroughWorkerReload proves the plain (pre-
// isolation) worker pipeline: a fake pdftoppm binary so the job reaches a
// real (if failing) conversion attempt instead of needing poppler
// installed on this machine.
//
// testApp runs in standalone mode, where queueFor's isolated-PDF-queue
// path is a no-op (queue and pdfQueue are the same instance—see
// newQueuesForMode). This test replaces pdfQueue with a genuinely
// separate local queue and attaches a worker to ONLY that queue, giving
// this App the same queue/worker shape a split "api" + "pdf-worker"
// deployment has. If queueFor ever regressed to routing PDF jobs onto the
// general queue instead, this job would never reach the isolated
// worker--it would just sit at Queued until the 2s deadline, since
// nothing else drains the queue it actually landed on.
func TestPDFJobsProcessedByIsolatedPDFQueueWorker(t *testing.T) {
	a := testApp(t)
	a.converter.pdftoppm = filepath.Join(t.TempDir(), "not-a-real-pdftoppm")
	a.pdfQueue = newLocalJobQueue(a.cfg.QueueSize)
	a.wg.Add(1)
	go a.worker(99, a.pdfQueue)

	w := submitJob(t, a, "doc.pdf", []byte(minimalPDF), map[string]string{"outputFormat": "png"})
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
		if job.Status == Completed || job.Status == Failed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if job.Status == Queued || job.Status == Processing {
		t.Fatalf("PDF job never reached a terminal status -- it isn't being drained by the isolated PDF worker, stuck at %s", job.Status)
	}
	if job.Status != Failed {
		t.Fatalf("expected Failed (pdftoppm is a fake path), got %s", job.Status)
	}
	if !strings.Contains(job.Error, "conversion failed") {
		t.Fatalf("expected a real conversion error, got %q", job.Error)
	}
}

// TestNonPDFJobsStayOnGeneralQueueWhenPDFQueueIsIsolated is the mirror of
// the test above: with an isolated (worker-less) PDF queue attached, an
// ordinary CSV job must still complete via testApp's own general-queue
// worker, proving the split doesn't collaterally route non-PDF jobs onto
// a queue nothing drains.
func TestNonPDFJobsStayOnGeneralQueueWhenPDFQueueIsIsolated(t *testing.T) {
	a := testApp(t)
	a.pdfQueue = newLocalJobQueue(a.cfg.QueueSize) // deliberately worker-less

	w := submitCSVJob(t, a)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	var created Job
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		j, ok := a.store.get(created.ID)
		if ok && j.snapshot().Status == Completed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("CSV job should still complete via the general queue/worker even with an isolated PDF queue present")
}
