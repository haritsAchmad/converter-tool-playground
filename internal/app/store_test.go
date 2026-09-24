package app

import (
	"errors"
	"os"
	"testing"
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
