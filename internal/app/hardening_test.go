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
