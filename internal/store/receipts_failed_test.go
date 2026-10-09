package store_test

// Tests for receipts the worker gave up on, and for which receipt states a user
// may act on.
//
// A failed receipt used to be filtered out of /receipts, so it could be neither
// entered by hand nor discarded and its file stayed on disk for good. And a job
// still queued or processing could be opened and attached while the worker was
// about to write its reading onto the same row.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// failedReceipt enqueues a receipt and drives it through every retry until the
// queue gives up on it, the way the worker would.
func failedReceipt(t *testing.T, st *store.Store, sc store.Scope, path string) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := st.EnqueueReceipt(ctx, sc, path, "blurred.jpg")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	for i := 0; i < store.MaxJobAttempts; i++ {
		job, err := st.ClaimReceiptJob(ctx)
		if err != nil {
			t.Fatalf("claim %d: %v", i+1, err)
		}
		if job.ID != id {
			t.Fatalf("claimed job %d, want %d", job.ID, id)
		}
		givenUp, err := st.RetryOrFailReceiptJob(ctx, id, "gemini: 503", time.Now())
		if err != nil {
			t.Fatalf("retry or fail: %v", err)
		}
		if givenUp != (i == store.MaxJobAttempts-1) {
			t.Fatalf("attempt %d: givenUp = %v", i+1, givenUp)
		}
	}
	return id
}

func TestFailedReceiptIsListedWithItsStatus(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	failed := failedReceipt(t, st, sc, "uploads/1/failed.jpg")

	read, err := st.EnqueueReceipt(ctx, sc, "uploads/1/read.png", "read.png")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteReceiptJob(ctx, read, nil); err != nil {
		t.Fatal(err)
	}

	// Still with the worker: not listed.
	if _, err := st.EnqueueReceipt(ctx, sc, "uploads/1/queued.png", "queued.png"); err != nil {
		t.Fatal(err)
	}

	list, err := st.UnattachedReceipts(ctx, sc, 10)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]store.ReceiptJob{}
	for _, j := range list {
		byID[j.ID] = j
	}
	if len(list) != 2 || byID[failed].ID == 0 || byID[read].ID == 0 {
		t.Fatalf("listed %+v, want exactly the read and the failed receipt", list)
	}
	if j := byID[failed]; !j.Failed() || j.Status != store.JobFailed || j.Error == "" {
		t.Errorf("failed receipt listed as %q (error %q), want failed with its cause", j.Status, j.Error)
	}
	if j := byID[read]; j.Failed() || j.Status != store.JobDone {
		t.Errorf("read receipt listed as %q", j.Status)
	}
}

func TestFailedReceiptCanBeEnteredByHand(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()
	id := failedReceipt(t, st, sc, "uploads/1/failed.jpg")

	job, err := st.UnattachedReceipt(ctx, sc, id)
	if err != nil {
		t.Fatalf("a failed receipt should open in the form: %v", err)
	}
	if !job.Failed() {
		t.Errorf("status = %q, want failed", job.Status)
	}

	txID := addExpense(t, st, sc, 1999, "Typed in", "2026-08-01", true)
	if err := st.AttachReceipt(ctx, sc, id, txID); err != nil {
		t.Fatalf("attach: %v", err)
	}
	tx, err := st.ByID(ctx, sc, txID)
	if err != nil {
		t.Fatal(err)
	}
	if tx.ReceiptPath != "uploads/1/failed.jpg" {
		t.Errorf("receipt path = %q", tx.ReceiptPath)
	}
}

func TestFailedReceiptCanBeDiscarded(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()
	id := failedReceipt(t, st, sc, "uploads/1/failed.jpg")

	path, err := st.DiscardReceipt(ctx, sc, id)
	if err != nil {
		t.Fatalf("discard: %v", err)
	}
	if path != "uploads/1/failed.jpg" {
		t.Errorf("path = %q, so the caller cannot delete the file", path)
	}
	if list, _ := st.UnattachedReceipts(ctx, sc, 10); len(list) != 0 {
		t.Errorf("still listed after discard: %+v", list)
	}
}

func TestFailedReceiptIsScopedToTheHousehold(t *testing.T) {
	st, alice := newTestStore(t)
	bob := newSecondUser(t, st, "bob@example.com")
	ctx := context.Background()
	id := failedReceipt(t, st, alice, "uploads/1/hers.jpg")

	if list, _ := st.UnattachedReceipts(ctx, bob, 10); len(list) != 0 {
		t.Errorf("bob sees alice's failed receipt: %+v", list)
	}
	if _, err := st.UnattachedReceipt(ctx, bob, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bob opened alice's failed receipt: %v", err)
	}
	if _, err := st.DiscardReceipt(ctx, bob, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bob discarded alice's failed receipt: %v", err)
	}
	if _, err := st.ReceiptInFlight(ctx, bob, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bob can watch alice's receipt: %v", err)
	}
}

// TestAReceiptWithTheWorkerCannotBeTaken: queued and processing jobs can be
// watched, but not opened, attached or discarded.
func TestAReceiptWithTheWorkerCannotBeTaken(t *testing.T) {
	for _, status := range []store.JobStatus{store.JobQueued, store.JobProcessing} {
		t.Run(string(status), func(t *testing.T) {
			st, sc := newTestStore(t)
			ctx := context.Background()

			id, err := st.EnqueueReceipt(ctx, sc, "uploads/1/busy.png", "busy.png")
			if err != nil {
				t.Fatal(err)
			}
			if status == store.JobProcessing {
				if _, err := st.ClaimReceiptJob(ctx); err != nil {
					t.Fatal(err)
				}
			}

			if _, err := st.UnattachedReceipt(ctx, sc, id); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("opened a %s receipt: %v", status, err)
			}
			txID := addExpense(t, st, sc, 500, "Too soon", "2026-08-01", true)
			if err := st.AttachReceipt(ctx, sc, id, txID); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("attached a %s receipt: %v", status, err)
			}
			if _, err := st.DiscardReceipt(ctx, sc, id); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("discarded a %s receipt: %v", status, err)
			}

			job, err := st.ReceiptInFlight(ctx, sc, id)
			if err != nil {
				t.Fatalf("the progress display cannot see a %s receipt: %v", status, err)
			}
			if job.Status != status {
				t.Errorf("status = %q, want %q", job.Status, status)
			}
		})
	}
}
