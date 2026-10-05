package worker

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// TestAStrandedJobIsSweptWithoutARestart: a job left in 'processing' while the
// server keeps running -- an outcome write that failed, say -- used to stay
// there until the next restart, and in that state it can be neither entered
// nor discarded. The worker now sweeps for such jobs while it is idle.
func TestAStrandedJobIsSweptWithoutARestart(t *testing.T) {
	q := newQueueRig(t)
	w := New(q.store, stubProcessor{draft: Draft{Amount: 1250, Payee: "Shop"}}, time.Minute)
	clock := newFakeClock(w)

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		w.Stop(2 * time.Second)
	}()
	go w.Run(ctx)
	// Let Run finish its start-up sweep and settle into waiting, so the job
	// below is stranded under a running worker rather than recovered at start.
	time.Sleep(200 * time.Millisecond)

	id := q.enqueue("stranded.jpg")
	if _, err := q.store.ClaimReceiptJob(context.Background()); err != nil {
		t.Fatalf("claim (the worker must not have taken it first): %v", err)
	}

	// Waking the worker before a sweep is due must not touch it: the sweep is
	// periodic, not on every pass.
	w.Wake()
	time.Sleep(100 * time.Millisecond)
	if state, _ := q.status(id); state != "processing" {
		t.Fatalf("status = %q before a sweep was due, want processing", state)
	}

	clock.advance(sweepEvery)
	deadline := time.Now().Add(3 * time.Second)
	for {
		w.Wake()
		if state, _ := q.status(id); state == "done" {
			break
		}
		if time.Now().After(deadline) {
			state, _ := q.status(id)
			t.Fatalf("the stranded job is still %q; the idle sweep never recovered it", state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestGivingUpLinksToTheReceipt: the "could not import" notification opens
// the form for that receipt, so the saved image is attached when the user
// enters it by hand instead of being left on /receipts.
func TestGivingUpLinksToTheReceipt(t *testing.T) {
	q := newQueueRig(t)
	id := q.enqueue("unreadable.jpg")

	w := New(q.store, stubProcessor{err: errStub("still broken")}, time.Minute)
	clock := newFakeClock(w)
	for attempt := 1; attempt <= store.MaxJobAttempts; attempt++ {
		clock.advance(maxRetryAfter)
		if !w.processNext(context.Background()) {
			t.Fatalf("attempt %d: nothing to claim", attempt)
		}
	}

	ns := q.notifications()
	if len(ns) != 1 {
		t.Fatalf("%d notifications, want 1", len(ns))
	}
	if want := fmt.Sprintf("receipt=%d", id); !strings.Contains(ns[0].Link, want) {
		t.Errorf("notification link = %q, want it to carry %s", ns[0].Link, want)
	}
}
