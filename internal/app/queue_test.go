package app

import (
	"context"
	"errors"
	"testing"
)

func TestLocalQueueIsBoundedAndFIFO(t *testing.T) {
	q := newLocalJobQueue(2)
	ctx := context.Background()
	if err := q.Enqueue(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(ctx, "third"); !errors.Is(err, errQueueFull) {
		t.Fatalf("expected full queue, got %v", err)
	}
	for _, want := range []string{"first", "second"} {
		got, err := q.Dequeue(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

// TestNewQueuesForModeStandaloneReusesOneQueue proves a standalone
// deployment gets a single local queue shared by both the "general" and
// "PDF" roles rather than two separate instances: a single process has
// nothing to isolate a second queue from, so App.queueFor's split (see
// its doc comment) is a no-op there by construction.
func TestNewQueuesForModeStandaloneReusesOneQueue(t *testing.T) {
	cfg := Config{Mode: "standalone", QueueSize: 4}
	queue, pdfQueue, err := newQueuesForMode(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if queue == nil || pdfQueue == nil {
		t.Fatal("expected both queue and pdfQueue to be set")
	}
	if queue != pdfQueue {
		t.Fatal("expected standalone mode to reuse the same queue instance for both roles")
	}
}
