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
