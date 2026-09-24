package app

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func submitCSVJob(t *testing.T, a *App) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("outputFormat", "json")
	p, _ := mw.CreateFormFile("file", "people.csv")
	_, _ = p.Write([]byte("name,age\nAda,36\n"))
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}

func TestJobStateSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	cfg := Config{Address: ":0", StorageRoot: root, MaxUploadBytes: 1 << 20, Workers: 1, QueueSize: 2, JobTimeout: time.Second, JobTTL: time.Hour, CleanupInterval: time.Hour, UploadTimeout: time.Second, RateRPS: 100, RateBurst: 100, MaxJobsPerIP: 100}
	a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	w := submitCSVJob(t, a)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var created Job
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		j, _ := a.store.get(created.ID)
		if j.snapshot().Status == Completed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	a.Close()

	statePath := filepath.Join(root, created.ID, jobStateFile)
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("expected persisted job state at %s: %v", statePath, err)
	}

	a2, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Close()
	j, ok := a2.store.get(created.ID)
	if !ok {
		t.Fatal("job not recovered after restart")
	}
	snap := j.snapshot()
	if snap.Status != Completed {
		t.Fatalf("expected recovered job to stay completed, got %s", snap.Status)
	}
	if _, err := os.Stat(j.OutputPath); err != nil {
		t.Fatalf("recovered job output path unusable: %v", err)
	}
}

func TestInterruptedJobRecoveredAsFailed(t *testing.T) {
	root := t.TempDir()
	cfg := Config{Address: ":0", StorageRoot: root, MaxUploadBytes: 1 << 20, Workers: 1, QueueSize: 2, JobTimeout: time.Second, JobTTL: time.Hour, CleanupInterval: time.Hour, UploadTimeout: time.Second, RateRPS: 100, RateBurst: 100, MaxJobsPerIP: 100}
	id := "11111111-1111-1111-1111-111111111111"
	jobDir := filepath.Join(root, id)
	if err := os.MkdirAll(jobDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "input.bin"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	j := &Job{ID: id, Status: Processing, InputFormat: "csv", OutputFormat: "json", OriginalName: "a.csv", OutputName: "a.json", CreatedAt: now, ExpiresAt: now.Add(time.Hour), InputPath: filepath.Join(jobDir, "input.bin")}
	data, _ := json.Marshal(j)
	if err := os.WriteFile(filepath.Join(jobDir, jobStateFile), data, 0600); err != nil {
		t.Fatal(err)
	}

	a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	recovered, ok := a.store.get(id)
	if !ok {
		t.Fatal("interrupted job not recovered")
	}
	snap := recovered.snapshot()
	if snap.Status != Failed {
		t.Fatalf("expected interrupted job marked failed, got %s", snap.Status)
	}
}

func TestSharedStoreReloadSeesWorkerUpdate(t *testing.T) {
	root := t.TempDir()
	apiStore, err := newStore(root)
	if err != nil {
		t.Fatal(err)
	}
	workerStore, err := newStore(root)
	if err != nil {
		t.Fatal(err)
	}
	id := "22222222-2222-2222-2222-222222222222"
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "input.bin"), []byte("a,b\n1,2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	j := &Job{ID: id, Status: Queued, InputFormat: "csv", OutputFormat: "json", OutputName: "result.json", CreatedAt: now, ExpiresAt: now.Add(time.Hour), InputPath: filepath.Join(dir, "input.bin"), OutputPath: filepath.Join(dir, "output.json"), mu: &sync.RWMutex{}}
	apiStore.add(j)
	if err := apiStore.persist(j); err != nil {
		t.Fatal(err)
	}
	workerJob, ok := workerStore.reload(id)
	if !ok {
		t.Fatal("worker could not load API job state")
	}
	workerJob.update(func(job *Job) { job.Status = Completed })
	if err := workerStore.persist(workerJob); err != nil {
		t.Fatal(err)
	}
	refreshed, ok := apiStore.reload(id)
	if !ok || refreshed.snapshot().Status != Completed {
		t.Fatal("API did not observe worker state update")
	}
}

// TestReloadAcceptsZipOutputForPDFToImage is a direct regression lock for
// review finding P1: reload() used to re-derive the expected output
// extension from formats[loaded.OutputFormat].Extensions[0] directly
// (".png"/".jpg"), independently of outputExtension(in, out)—the function
// job creation actually uses, which returns ".zip" for PDF->image. A PDF->
// PNG/JPEG job's OutputName (e.g. "report.zip") therefore failed this
// function's own extension check and reload() rejected it outright, which
// made the worker treat the job as unavailable, ack it off the queue, and
// leave it stuck at Queued forever without ever attempting the conversion.
// Structured the same way TestSharedStoreReloadSeesWorkerUpdate exercises
// reload() (a split API/worker store pair over the same job.json sidecar),
// just with a PDF->PNG pair instead of CSV->JSON.
func TestReloadAcceptsZipOutputForPDFToImage(t *testing.T) {
	root := t.TempDir()
	apiStore, err := newStore(root)
	if err != nil {
		t.Fatal(err)
	}
	id := "33333333-3333-3333-3333-333333333333"
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "input.bin"), []byte(minimalPDF), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	outPath := filepath.Join(dir, "output.zip")
	j := &Job{
		ID: id, Status: Queued, InputFormat: "pdf", OutputFormat: "png", OutputName: "report.zip",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		InputPath: filepath.Join(dir, "input.bin"), OutputPath: outPath, mu: &sync.RWMutex{},
	}
	apiStore.add(j)
	if err := apiStore.persist(j); err != nil {
		t.Fatal(err)
	}
	workerStore, err := newStore(root)
	if err != nil {
		t.Fatal(err)
	}
	loaded, ok := workerStore.reload(id)
	if !ok {
		t.Fatal("worker could not reload a queued PDF->PNG job with a .zip OutputName")
	}
	if loaded.OutputPath != outPath {
		t.Fatalf("expected OutputPath %q, got %q", outPath, loaded.OutputPath)
	}
}

// TestPDFToImageJobFlowsThroughWorkerReload is the full-pipeline regression
// review finding P1 asked for: submit a real PDF->PNG job over HTTP, let
// the actual worker goroutine dequeue and reload() it exactly as
// production does, and confirm status/download stay consistent throughout.
// Before the fix, reload() rejected the job (OutputName "*.zip" didn't
// match the ".png" it independently expected), the worker acked it off
// the queue without converting, and its status stayed "queued" forever—
// never observably failing, just silently stuck. pdftoppm itself is faked
// (unavailable on this dev machine, same as the skip-guarded
// TestConvertPDFRendersAllPagesAsZip) so the job is expected to reach
// Failed via a real exec error, not silently vanish or hang at Queued.
func TestPDFToImageJobFlowsThroughWorkerReload(t *testing.T) {
	a := testApp(t)
	// supports("pdf","png") only needs a non-empty path; the worker will
	// genuinely try to exec it and fail, which is the point—this proves
	// the job REACHES conversion instead of being dropped beforehand.
	a.converter.pdftoppm = filepath.Join(t.TempDir(), "not-a-real-pdftoppm")

	w := submitJob(t, a, "doc.pdf", []byte(minimalPDF), map[string]string{"outputFormat": "png"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	var created Job
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(created.OutputName, ".zip") {
		t.Fatalf("expected a .zip OutputName for PDF->PNG, got %q", created.OutputName)
	}

	status := func() (int, Job) {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+created.ID, nil)
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, r)
		var j Job
		_ = json.Unmarshal(rec.Body.Bytes(), &j)
		return rec.Code, j
	}

	deadline := time.Now().Add(2 * time.Second)
	var code int
	var job Job
	for time.Now().Before(deadline) {
		code, job = status()
		if code != http.StatusOK {
			t.Fatalf("status endpoint returned %d for a live job", code)
		}
		if job.Status != Queued {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if job.Status == Queued {
		t.Fatal("job never left Queued—worker reload() silently dropped it (the P1 regression)")
	}
	if job.Status != Failed {
		t.Fatalf("expected Failed (pdftoppm is a fake path), got %s", job.Status)
	}
	if job.Error == "" || strings.Contains(job.Error, "queued job state is unavailable") {
		t.Fatalf("expected a real conversion error, got %q", job.Error)
	}

	// Download must respond sanely (409 "not complete", not a panic or
	// hang) for a failed job—it was never going to have output to serve.
	r := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+created.ID+"/download", nil)
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 downloading a failed job's output, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestReloadAndDownloadAcceptLegacyPDFToImageOutput is the regression
// review finding P2 asked for: a PDF->PNG/JPEG job that already COMPLETED
// before multi-page rendering made this pair always write a ZIP has a real
// "output.png"/"output.jpg" on disk under the OLD convention. reload()
// must still find it via legacyOutputExtensions—rejecting it outright the
// moment the extension rule changed would silently break status/download
// for every such job for the rest of its retention window, even though
// nothing about that already-finished job's own files changed. This seeds
// a job.json + output file directly (the job is never actually converted
// in this test, matching TestInterruptedJobRecoveredAsFailed's pattern for
// exercising store/reload state without a real worker run) and checks
// both endpoints reviewer explicitly asked for: status AND download.
func TestReloadAndDownloadAcceptLegacyPDFToImageOutput(t *testing.T) {
	cases := []struct {
		id, outputFormat, legacyExt string
	}{
		{"44444444-4444-4444-4444-444444444444", "png", ".png"},
		{"55555555-5555-5555-5555-555555555555", "jpeg", ".jpg"},
	}
	for _, tc := range cases {
		t.Run(tc.outputFormat, func(t *testing.T) {
			a := testApp(t)
			dir := filepath.Join(a.cfg.StorageRoot, tc.id)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "input.bin"), []byte(minimalPDF), 0600); err != nil {
				t.Fatal(err)
			}
			legacyBytes := []byte("legacy " + tc.outputFormat + " output bytes")
			if err := os.WriteFile(filepath.Join(dir, "output"+tc.legacyExt), legacyBytes, 0600); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			finished := now
			j := Job{
				ID: tc.id, Status: Completed, InputFormat: "pdf", OutputFormat: tc.outputFormat,
				OriginalName: "doc.pdf", OutputName: "report" + tc.legacyExt,
				CreatedAt: now, FinishedAt: &finished, ExpiresAt: now.Add(time.Hour),
			}
			data, err := json.Marshal(j)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, jobStateFile), data, 0600); err != nil {
				t.Fatal(err)
			}

			statusReq := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+tc.id, nil)
			statusRec := httptest.NewRecorder()
			a.Handler().ServeHTTP(statusRec, statusReq)
			if statusRec.Code != http.StatusOK {
				t.Fatalf("expected 200 for a legacy completed job's status, got %d: %s", statusRec.Code, statusRec.Body.String())
			}
			var got Job
			if err := json.Unmarshal(statusRec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Status != Completed {
				t.Fatalf("expected status completed, got %s", got.Status)
			}

			dlReq := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+tc.id+"/download", nil)
			dlRec := httptest.NewRecorder()
			a.Handler().ServeHTTP(dlRec, dlReq)
			if dlRec.Code != http.StatusOK {
				t.Fatalf("expected 200 downloading a legacy completed job's output, got %d: %s", dlRec.Code, dlRec.Body.String())
			}
			if !bytes.Equal(dlRec.Body.Bytes(), legacyBytes) {
				t.Fatalf("downloaded bytes %q do not match the legacy output file %q", dlRec.Body.Bytes(), legacyBytes)
			}
		})
	}
}

func TestRateLimitRejectsBurstOverflow(t *testing.T) {
	cfg := Config{Address: ":0", StorageRoot: t.TempDir(), MaxUploadBytes: 1 << 20, Workers: 1, QueueSize: 10, JobTimeout: time.Second, JobTTL: time.Minute, CleanupInterval: time.Hour, UploadTimeout: time.Second, RateRPS: 1, RateBurst: 1, MaxJobsPerIP: 100}
	a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	w1 := submitCSVJob(t, a)
	if w1.Code != http.StatusAccepted {
		t.Fatalf("first request should pass burst, got %d: %s", w1.Code, w1.Body.String())
	}
	w2 := submitCSVJob(t, a)
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("second immediate request should be rate limited, got %d: %s", w2.Code, w2.Body.String())
	}
}

func TestUploadQuotaRejectsTooManyConcurrentJobs(t *testing.T) {
	cfg := Config{Address: ":0", StorageRoot: t.TempDir(), MaxUploadBytes: 1 << 20, Workers: 0, QueueSize: 10, JobTimeout: time.Second, JobTTL: time.Minute, CleanupInterval: time.Hour, UploadTimeout: time.Second, RateRPS: 100, RateBurst: 100, MaxJobsPerIP: 1}
	a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	w1 := submitCSVJob(t, a)
	if w1.Code != http.StatusAccepted {
		t.Fatalf("first job should be accepted, got %d: %s", w1.Code, w1.Body.String())
	}
	w2 := submitCSVJob(t, a)
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("second concurrent job from same IP should be rejected, got %d: %s", w2.Code, w2.Body.String())
	}
}
