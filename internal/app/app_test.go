package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testApp(t *testing.T) *App {
	t.Helper()
	cfg := Config{Address: ":0", StorageRoot: t.TempDir(), MaxUploadBytes: 1 << 20, Workers: 1, QueueSize: 2, JobTimeout: time.Second, JobTTL: time.Minute, CleanupInterval: time.Hour, UploadTimeout: time.Second, RateRPS: 100, RateBurst: 100, MaxJobsPerIP: 100}
	a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

func TestCSVToJSONJob(t *testing.T) {
	a := testApp(t)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("outputFormat", "json")
	_ = mw.WriteField("outputName", "../safe-name")
	p, _ := mw.CreateFormFile("file", "people.csv")
	_, _ = p.Write([]byte("name,age\nAda,36\n"))
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var created Job
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(created.OutputName, "..") || strings.ContainsAny(created.OutputName, "/\\") {
		t.Fatalf("unsafe name %q", created.OutputName)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		j, _ := a.store.get(created.ID)
		if j.snapshot().Status == Completed {
			b, err := os.ReadFile(j.OutputPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(b, []byte(`"name": "Ada"`)) {
				t.Fatalf("unexpected output: %s", b)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job did not complete")
}

func TestRejectsDisguisedExecutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.bin")
	if err := os.WriteFile(path, []byte("MZnot really json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateUpload(path, "data.json"); err == nil {
		t.Fatal("expected executable signature rejection")
	}
}

func TestRejectsDisguisedActiveContent(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		content  string
	}{
		{name: "unsupported PDF extension", filename: "invoice.pdf", content: "percent-PDF-not-supported"},
		{name: "PHP content renamed to CSV", filename: "people.csv", content: "name,age\n<?php echo 1; ?>,1\n"},
		{name: "blocked extension hidden before CSV", filename: "people.php.csv", content: "name,age\nAda,36\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "input.bin")
			if err := os.WriteFile(path, []byte(tt.content), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := validateUpload(path, tt.filename); err == nil {
				t.Fatalf("expected %q to be rejected", tt.filename)
			}
		})
	}
}

type rejectingScanner struct{}

func (rejectingScanner) Scan(context.Context, string) error { return errMalwareDetected }

func TestMalwareScanRejectsBeforeQueue(t *testing.T) {
	a := testApp(t)
	a.scanner = rejectingScanner{}
	w := submitCSVJob(t, a)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected malware rejection, got %d: %s", w.Code, w.Body.String())
	}
	if len(a.queue) != 0 {
		t.Fatal("rejected upload reached conversion queue")
	}
}

func TestWithin(t *testing.T) {
	root := t.TempDir()
	if !within(root, filepath.Join(root, "job", "out")) {
		t.Fatal("valid child rejected")
	}
	if within(root, filepath.Join(root, "..", "outside")) {
		t.Fatal("traversal accepted")
	}
}
