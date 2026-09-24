package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestReadWithRetryRetriesTransientErrorsThenSucceeds proves readJobState's
// retry logic: an error that isn't "file doesn't exist" (the class a
// Windows rename-vs-concurrent-read sharing violation falls into) is
// retried instead of failing the request immediately, so a live job isn't
// wrongly reported 404 for a purely transient read glitch.
func TestReadWithRetryRetriesTransientErrorsThenSucceeds(t *testing.T) {
	transient := errors.New("sharing violation")
	calls := 0
	data, err := readWithRetry(func() ([]byte, error) {
		calls++
		if calls < 3 {
			return nil, transient
		}
		return []byte("ok"), nil
	})
	if err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if string(data) != "ok" {
		t.Fatalf("expected %q, got %q", "ok", data)
	}
	if calls != 3 {
		t.Fatalf("expected 3 attempts, got %d", calls)
	}
}

// TestReadWithRetryFailsFastOnNotExist proves a genuinely missing file
// (wrong job ID, or a job already expired and removed) is reported
// immediately rather than paying the retry delay meant for a transient
// error on a file that actually exists.
func TestReadWithRetryFailsFastOnNotExist(t *testing.T) {
	calls := 0
	_, err := readWithRetry(func() ([]byte, error) {
		calls++
		return nil, os.ErrNotExist
	})
	if !os.IsNotExist(err) {
		t.Fatalf("expected a not-exist error, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 attempt for a not-exist error, got %d", calls)
	}
}

// TestReadWithRetryGivesUpAfterMaxAttempts proves a persistent (not just
// transient) error is still surfaced rather than retried forever or
// silently swallowed.
func TestReadWithRetryGivesUpAfterMaxAttempts(t *testing.T) {
	persistent := errors.New("still broken")
	calls := 0
	_, err := readWithRetry(func() ([]byte, error) {
		calls++
		return nil, persistent
	})
	if !errors.Is(err, persistent) {
		t.Fatalf("expected the persistent error to surface, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected exactly 3 attempts, got %d", calls)
	}
}

// TestRenameWithRetrySucceedsPastConcurrentReader reproduces, with a real
// file and a real concurrent reader (not a mocked error function—
// renameWithRetry wraps os.Rename directly, unlike readWithRetry's
// injectable read func), the exact Windows condition persist() hits
// under load: os.Rename onto a destination another goroutine currently
// has open for reading fails with "Access is denied" until that reader
// closes it. Found by instrumenting store.persist's own os.Rename call
// and running the real SVG->PNG worker pipeline test (svg_test.go's
// TestSVGJobFlowsThroughWorker) under `go test -race`, where SVG's
// near-instant conversion time packs several persist() calls close
// together against a 10ms-interval HTTP status poll also reading
// job.json—reproducing "Access is denied" on nearly every run before
// this fix. This test pins the fix down directly against the real OS
// rename behavior, independent of that timing-sensitive full pipeline.
func TestRenameWithRetrySucceedsPastConcurrentReader(t *testing.T) {
	dir := t.TempDir()
	oldpath := filepath.Join(dir, "state.json.tmp")
	newpath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(newpath, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldpath, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(newpath)
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = f.Close()
		close(closed)
	}()
	if err := renameWithRetry(oldpath, newpath); err != nil {
		t.Fatalf("expected the rename to eventually succeed once the reader closed, got: %v", err)
	}
	<-closed
	data, err := os.ReadFile(newpath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("expected the renamed content to win, got %q", data)
	}
}

// TestRenameWithRetryGivesUpOnPersistentFailure proves a genuinely
// unrecoverable rename (destination directory doesn't exist) still
// surfaces an error instead of retrying forever.
func TestRenameWithRetryGivesUpOnPersistentFailure(t *testing.T) {
	dir := t.TempDir()
	oldpath := filepath.Join(dir, "state.json.tmp")
	if err := os.WriteFile(oldpath, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	err := renameWithRetry(oldpath, filepath.Join(dir, "no-such-subdir", "state.json"))
	if err == nil {
		t.Fatal("expected a rename into a nonexistent directory to fail")
	}
}
