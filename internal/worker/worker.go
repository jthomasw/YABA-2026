// Package worker drains the receipt queue in the background, so an upload handler
// writes one file and one row and returns without waiting for the slow work.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// Processor turns a receipt image into a draft transaction.
type Processor interface {
	// Process examines the image at path and returns a draft.
	Process(ctx context.Context, job store.ReceiptJob) (Draft, error)
}

// Draft is what a processor managed to extract.
type Draft struct {
	Label  string
	Payee  string
	Place  string
	Amount store.Cents
	Date   string
}

// ErrNeedsReview means the receipt was readable as a file but its contents
// could not be interpreted.
var ErrNeedsReview = errors.New("receipt needs manual review")

// Worker polls the queue.
type Worker struct {
	store     *store.Store
	processor Processor
	interval  time.Duration

	// wake lets an upload nudge the worker instead of waiting for the next tick.
	wake chan struct{}

	stopOnce sync.Once
	done     chan struct{}
}

// New builds a Worker. A nil processor gets ReviewProcessor.
func New(st *store.Store, p Processor, interval time.Duration) *Worker {
	if p == nil {
		p = ReviewProcessor{}
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &Worker{
		store:     st,
		processor: p,
		interval:  interval,
		wake:      make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
}

// Wake asks the worker to check the queue now. Safe to call from any goroutine
// and never blocks.
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run processes jobs until ctx is cancelled. A job left in 'processing' by a killed
// process is requeued first, or it would never run and never be reported as failed.
func (w *Worker) Run(ctx context.Context) {
	defer close(w.done)

	if n, err := w.store.RecoverStuckJobs(ctx); err != nil {
		log.Printf("worker: could not recover stuck jobs: %v", err)
	} else if n > 0 {
		log.Printf("worker: requeued %d receipt job(s) left over from a previous run", n)
	}

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		// Drain fully on each pass, so a burst of uploads is not spread across one tick each.
		for w.processNext(ctx) {
			if ctx.Err() != nil {
				return
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-ticker.C:
		}
	}
}

// Stop waits for Run to return, up to a timeout.
func (w *Worker) Stop(timeout time.Duration) {
	w.stopOnce.Do(func() {
		select {
		case <-w.done:
		case <-time.After(timeout):
			log.Printf("worker: did not stop within %s", timeout)
		}
	})
}

// processNext handles one job and reports whether there was one.
func (w *Worker) processNext(ctx context.Context) bool {
	job, err := w.store.ClaimReceiptJob(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	if err != nil {
		log.Printf("worker: claim failed: %v", err)
		return false
	}

	log.Printf("worker: processing receipt job %d for user %d (attempt %d)",
		job.ID, job.UserID, job.Attempts)

	draft, err := w.processor.Process(ctx, job)

	switch {
	case errors.Is(err, ErrNeedsReview):
		w.finishNeedsReview(ctx, job)

	case err != nil:
		w.finishFailed(ctx, job, err)

	default:
		w.finishExtracted(ctx, job, draft)
	}
	return true
}

// finishExtracted records a transaction built from a successful extraction.
func (w *Worker) finishExtracted(ctx context.Context, job store.ReceiptJob, d Draft) {
	if d.Amount <= 0 {
		// A processor that reports success with no amount is buggy; treat it as
		// needing review rather than writing a zero expense.
		w.finishNeedsReview(ctx, job)
		return
	}
	if d.Date == "" {
		d.Date = store.Today()
	}
	if d.Label == "" {
		d.Label = "Scanned receipt"
	}

	// The scope comes off the job, not the uploader's current household: they may have
	// switched budgets since, and the expense belongs to the one they were looking at.
	sc := store.Scope{HouseholdID: job.HouseholdID, UserID: job.UserID}

	essential := true
	txID, err := w.store.Add(ctx, sc, store.NewTransaction{
		Kind:        store.KindExpense,
		Label:       d.Label,
		Amount:      d.Amount,
		OccurredOn:  d.Date,
		Essential:   &essential,
		Payee:       d.Payee,
		Place:       d.Place,
		Note:        "Imported from receipt " + job.OriginalName,
		ReceiptPath: job.Path,
		ReceiptName: job.OriginalName,
	})
	if err != nil {
		w.finishFailed(ctx, job, fmt.Errorf("could not save the expense: %w", err))
		return
	}

	if err := w.store.CompleteReceiptJob(ctx, job.ID, &txID); err != nil {
		log.Printf("worker: could not mark job %d done: %v", job.ID, err)
	}
	// Recurring-expense funding depends on the month's expenses, so a new one
	// has to trigger a recalculation.
	if err := w.store.ReallocateMonthOf(ctx, sc, d.Date); err != nil {
		log.Printf("worker: reallocate after receipt %d: %v", job.ID, err)
	}

	w.notify(ctx, job.UserID, "success",
		fmt.Sprintf("Receipt %s imported as %s.", job.OriginalName, d.Amount.Display()),
		fmt.Sprintf("/transactions/%d/edit", txID))
}

// finishNeedsReview stores the image against a draft the user completes.
func (w *Worker) finishNeedsReview(ctx context.Context, job store.ReceiptJob) {
	if err := w.store.CompleteReceiptJob(ctx, job.ID, nil); err != nil {
		log.Printf("worker: could not mark job %d done: %v", job.ID, err)
	}
	w.notify(ctx, job.UserID, "info",
		fmt.Sprintf("Receipt %s is ready, but the amount could not be read. Tap to enter it.",
			job.OriginalName),
		"/transactions/new?type=expense&receipt="+fmt.Sprint(job.ID))
}

// finishFailed retries, then gives up and tells the user.
func (w *Worker) finishFailed(ctx context.Context, job store.ReceiptJob, cause error) {
	log.Printf("worker: job %d failed: %v", job.ID, cause)

	givenUp, err := w.store.RetryOrFailReceiptJob(ctx, job.ID, cause.Error())
	if err != nil {
		log.Printf("worker: could not update failed job %d: %v", job.ID, err)
		return
	}
	if !givenUp {
		return // it will come round again
	}

	// Written to the database rather than pushed to a live page, so a failure still
	// reaches the user whenever they next sign in.
	w.notify(ctx, job.UserID, "error",
		fmt.Sprintf("Could not import the receipt %s. Please add it manually.", job.OriginalName),
		"/transactions/new?type=expense")
}

func (w *Worker) notify(ctx context.Context, userID int64, kind, text, link string) {
	if err := w.store.Notify(ctx, userID, kind, text, link); err != nil {
		log.Printf("worker: could not notify user %d: %v", userID, err)
	}
}

// ── the default processor ─────────────────────────────────────────────────────

// ReviewProcessor checks the stored file is readable and then hands the receipt to the
// user.
type ReviewProcessor struct{}

// Process verifies the stored file and asks for review.
func (ReviewProcessor) Process(_ context.Context, job store.ReceiptJob) (Draft, error) {
	info, err := os.Stat(localPath(job.Path))
	if err != nil {
		// A genuine failure: the file is missing or unreadable, so retrying may
		// help and the user must eventually be told if it does not.
		return Draft{}, fmt.Errorf("stored receipt is unreadable: %w", err)
	}
	if info.Size() == 0 {
		return Draft{}, errors.New("stored receipt is empty")
	}
	return Draft{}, ErrNeedsReview
}

// localPath converts a stored path to the local separator.
func localPath(stored string) string {
	return filepath.FromSlash(stored)
}
