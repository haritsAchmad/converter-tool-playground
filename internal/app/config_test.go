package app

import "testing"

// TestLoadConfigAcceptsPDFWorkerMode proves "pdf-worker" is now a
// recognized Config.Mode alongside standalone/api/worker, and, like
// api/worker, requires CONVERTBOX_REDIS_URL rather than silently running
// standalone—a pdf-worker process only makes sense as part of a split
// deployment with a real queue to drain.
func TestLoadConfigAcceptsPDFWorkerMode(t *testing.T) {
	t.Setenv("CONVERTBOX_MODE", "pdf-worker")
	t.Setenv("CONVERTBOX_REDIS_URL", "redis://localhost:6379/0")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("expected pdf-worker mode with a Redis URL to be valid, got: %v", err)
	}
	if cfg.Mode != "pdf-worker" {
		t.Fatalf("expected Mode %q, got %q", "pdf-worker", cfg.Mode)
	}
}

func TestLoadConfigRejectsPDFWorkerModeWithoutRedis(t *testing.T) {
	t.Setenv("CONVERTBOX_MODE", "pdf-worker")
	t.Setenv("CONVERTBOX_REDIS_URL", "")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected pdf-worker mode without a Redis URL to be rejected")
	}
}

func TestLoadConfigRejectsUnknownMode(t *testing.T) {
	t.Setenv("CONVERTBOX_MODE", "not-a-real-mode")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected an unrecognized mode to be rejected")
	}
}
