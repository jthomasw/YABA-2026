// Tests for the receipt queue's bookkeeping: when a job may be claimed again,
// what a shutdown or a crash does to its attempts, and that finishing a job
// never undoes what the user did with the receipt while it was being read.
package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// TestCompletingAJobNeverDetachesTheReceipt is the attachment race. It is
// closed twice over. First, a receipt the worker is still reading cannot be
// attached at all. Second, finishing a job never clears a link that is
// already there, so even a late completion (a worker that lost its claim to a
// restart and finished anyway) cannot put an attached receipt back on the
// "waiting" list, where a Discard would delete the file the expense still
// points at.
func TestCompletingAJobNeverDetachesTheReceipt(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	jobID, err := st.EnqueueReceipt(ctx, sc, "uploads/1/race.png", "race.png")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := st.ClaimReceiptJob(ctx); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// While the worker holds it, attaching is refused...
	txID := addExpense(t, st, sc, 1250, "Groceries", "2026-08-01", true)
	if err := st.AttachReceipt(ctx, sc, jobID, txID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("attach while processing: err = %v, want ErrNotFound", err)
	}

	// ...once it is done, it can be attached...
	if err := st.CompleteReceiptJob(ctx, jobID, nil); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := st.AttachReceipt(ctx, sc, jobID, txID); err != nil {
		t.Fatalf("attach after completion: %v", err)
	}

	// ...and a late, duplicate completion knows nothing about that.
	if err := st.CompleteReceiptJob(ctx, jobID, nil); err != nil {
		t.Fatalf("late complete: %v", err)
	}

	if _, err := st.UnattachedReceipt(ctx, sc, jobID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the attached receipt is on offer again (err = %v); a Discard would delete the expense's file", err)
	}
	tx, err := st.ByID(ctx, sc, txID)
	if err != nil {
		t.Fatalf("by id: %v", err)
	}
	if tx.ReceiptPath != "uploads/1/race.png" {
		t.Errorf("expense receipt = %q, want it kept", tx.ReceiptPath)
	}
}

// TestCompletingAJobCanStillLinkATransaction: the COALESCE must not stop an
// explicit link from being written.
func TestCompletingAJobCanStillLinkATransaction(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	jobID, err := st.EnqueueReceipt(ctx, sc, "uploads/1/link.png", "link.png")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	txID := addExpense(t, st, sc, 900, "Dining", "2026-08-02", false)
	if err := st.CompleteReceiptJob(ctx, jobID, &txID); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, err := st.UnattachedReceipt(ctx, sc, jobID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a job completed with a transaction is still unattached: %v", err)
	}
}

// TestAJobIsNotClaimedBeforeItsRetryIsDue: the retry storm, at the store.
func TestAJobIsNotClaimedBeforeItsRetryIsDue(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)

	jobID, err := st.EnqueueReceipt(ctx, sc, "uploads/1/retry.png", "retry.png")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := st.ClaimReceiptJobAt(ctx, t0); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	givenUp, err := st.RetryOrFailReceiptJob(ctx, jobID, "503", t0.Add(30*time.Second))
	if err != nil || givenUp {
		t.Fatalf("retry: givenUp=%v err=%v", givenUp, err)
	}

	if _, err := st.ClaimReceiptJobAt(ctx, t0.Add(29*time.Second)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("claimed before its retry was due (err = %v)", err)
	}
	// A retry time in another zone is the same instant, and must compare so.
	due := t0.Add(30 * time.Second).In(time.FixedZone("UTC+5", 5*3600))
	job, err := st.ClaimReceiptJobAt(ctx, due)
	if err != nil {
		t.Fatalf("not claimed once due: %v", err)
	}
	if job.ID != jobID || job.Attempts != 2 {
		t.Errorf("claimed job %d on attempt %d, want job %d on attempt 2", job.ID, job.Attempts, jobID)
	}
}

// TestAWaitingJobDoesNotHoldUpTheQueue: a job waiting out a retry must not
// block a newer upload behind it.
func TestAWaitingJobDoesNotHoldUpTheQueue(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	waiting, _ := st.EnqueueReceipt(ctx, sc, "uploads/1/a.png", "a.png")
	if _, err := st.ClaimReceiptJobAt(ctx, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RetryOrFailReceiptJob(ctx, waiting, "503", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	fresh, _ := st.EnqueueReceipt(ctx, sc, "uploads/1/b.png", "b.png")

	job, err := st.ClaimReceiptJobAt(ctx, now)
	if err != nil {
		t.Fatalf("the fresh upload was not claimed: %v", err)
	}
	if job.ID != fresh {
		t.Errorf("claimed job %d, want the fresh upload %d", job.ID, fresh)
	}
}

// TestReleasingAJobGivesTheAttemptBack: shutdown hands a job back as if it had
// never been claimed.
func TestReleasingAJobGivesTheAttemptBack(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	jobID, _ := st.EnqueueReceipt(ctx, sc, "uploads/1/r.png", "r.png")
	if _, err := st.ClaimReceiptJob(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.ReleaseReceiptJob(ctx, jobID); err != nil {
		t.Fatalf("release: %v", err)
	}
	job, err := st.ClaimReceiptJob(ctx)
	if err != nil {
		t.Fatalf("a released job could not be claimed: %v", err)
	}
	if job.Attempts != 1 {
		t.Errorf("attempts = %d, want 1: the interrupted attempt should not count", job.Attempts)
	}

	// Only a job actually in flight can be released.
	if err := st.CompleteReceiptJob(ctx, jobID, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.ReleaseReceiptJob(ctx, jobID); err == nil {
		t.Error("a finished job was released back into the queue")
	}
}

// claimToAttempt claims a job, failing and retrying it until it is claimed on
// the given attempt, and leaves it in 'processing' there -- the state a
// process killed mid-read leaves behind.
func claimToAttempt(t *testing.T, st *store.Store, jobID int64, attempt int) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	for i := 1; i <= attempt; i++ {
		job, err := st.ClaimReceiptJobAt(ctx, now)
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if job.ID != jobID {
			t.Fatalf("claimed job %d, want %d", job.ID, jobID)
		}
		if i < attempt {
			if _, err := st.RetryOrFailReceiptJob(ctx, jobID, "failed", now); err != nil {
				t.Fatalf("retry %d: %v", i, err)
			}
		}
	}
}

// TestRecoverStuckJobsFollowsTheRetryPolicy: a job a crashed process left in
// 'processing' is requeued if it has attempts left, and failed -- and handed
// back so its owner can be told -- if it does not. Requeueing everything let a
// receipt that crashed the process do so again on every restart.
func TestRecoverStuckJobsFollowsTheRetryPolicy(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	exhaustedID, _ := st.EnqueueReceipt(ctx, sc, "uploads/1/poison.png", "poison.png")
	claimToAttempt(t, st, exhaustedID, store.MaxJobAttempts)

	spareID, _ := st.EnqueueReceipt(ctx, sc, "uploads/1/ok.png", "ok.png")
	claimToAttempt(t, st, spareID, 1)

	requeued, exhausted, err := st.RecoverStuckJobs(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if requeued != 1 {
		t.Errorf("requeued = %d, want 1", requeued)
	}
	if len(exhausted) != 1 || exhausted[0].ID != exhaustedID || exhausted[0].OriginalName != "poison.png" {
		t.Fatalf("exhausted = %+v, want just the poison receipt", exhausted)
	}

	// The spare one is due straight away; the exhausted one is never offered again.
	job, err := st.ClaimReceiptJob(ctx)
	if err != nil || job.ID != spareID {
		t.Fatalf("claim after recovery = %d, %v; want job %d", job.ID, err, spareID)
	}
	if _, err := st.ClaimReceiptJob(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the exhausted job was requeued (err = %v)", err)
	}
	if n, err := st.PendingReceiptCount(ctx, sc); err != nil || n != 1 {
		t.Errorf("pending = %d, %v; want only the job just claimed", n, err)
	}
}
