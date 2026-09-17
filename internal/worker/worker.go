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
	"runtime/debug"
	"sync"
	"time"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// Processor turns a receipt image into a draft transaction.
type Processor interface {
	// Process examines the image at path and returns a draft.
	Process(ctx context.Context, job store.ReceiptJob) (Draft, error)
}

// Draft is what a processor managed to extract. It is a proposal and never a
// fact: nothing here reaches the ledger until a person has seen it beside the
// photograph and pressed Save.
//
// That is the answer to the objection this package was written with, recorded in
// worker_test.go: a misread amount is dangerous precisely because it is
// silently believed, entering the month's total, the category breakdown, the
// trend line and the emergency-fund projection with nothing on screen admitting
// it was a guess. A draft cannot do any of that. It is shown, labelled as read
// from the receipt, and confirmed or corrected before it becomes a transaction.
type Draft struct {
	Label  string
	Payee  string
	Place  string
	Amount store.Cents
	Date   string

	// Everything below is what OCR adds over the older, amount-only contract.
	Category   string
	Subtotal   store.Cents
	Tax        store.Cents
	Tip        store.Cents
	Items      []store.DraftItem
	Confidence float64
	Text       string

	// ItemsBalanced reports that the last item is a balancing line the parser
	// added, not something the receipt listed, so the form can say so.
	ItemsBalanced bool
}

// Empty reports whether a draft carries nothing worth storing, so a receipt that
// yielded literally nothing does not write a row of blanks.
func (d Draft) Empty() bool {
	return d.Amount == 0 && d.Label == "" && d.Payee == "" &&
		d.Date == "" && d.Text == "" && len(d.Items) == 0
}

// toStore converts a processor's draft into the form the database holds.
func (d Draft) toStore() store.ReceiptDraft {
	return store.ReceiptDraft{
		Merchant:      d.Payee,
		Category:      d.Label,
		Date:          d.Date,
		Total:         d.Amount,
		Subtotal:      d.Subtotal,
		Tax:           d.Tax,
		Tip:           d.Tip,
		Items:         d.Items,
		ItemsBalanced: d.ItemsBalanced,
		Confidence:    d.Confidence,
		Text:          d.Text,
	}
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

	draft, err := w.safeProcess(ctx, job)

	switch {
	case errors.Is(err, ErrNeedsReview):
		// Not a failure. The file was fine; its contents were not legible enough
		// to propose an amount, and whatever else was read is still worth keeping.
		w.finishNeedsReview(ctx, job, draft)

	case err != nil:
		w.finishFailed(ctx, job, err)

	default:
		w.finishDraft(ctx, job, draft)
	}
	return true
}

// safeProcess runs the processor and turns a panic into an ordinary job failure.
//
// This goroutine is started with a bare `go receipts.Run(ctx)`, and an
// unrecovered panic in a goroutine takes the whole PROCESS down -- not just the
// worker. The HTTP recoverPanics middleware does not reach here. What runs
// inside is the OCR path: it shells out to tesseract and pdftoppm and then
// parses whatever text comes back, which is attacker-supplied by definition
// once anybody can upload a receipt. One index-out-of-range in that parser
// would have signed every user out and stopped the site.
//
// One bad receipt now fails one job.
func (w *Worker) safeProcess(ctx context.Context, job store.ReceiptJob) (d Draft, err error) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("worker: PANIC processing receipt job %d: %v\n%s",
				job.ID, p, debug.Stack())
			d, err = Draft{}, fmt.Errorf("the receipt could not be read")
		}
	}()
	return w.processor.Process(ctx, job)
}

// saveDraft records the proposal against the job, if there is one worth
// recording, and reports whether it got there.
//
// The caller needs the answer. finishDraft used to ignore it and then notify the
// user "Read $45.78 from Tesco. Tap to check it and save." -- so when the write
// had failed they clicked through to an empty form, the figure they had been
// promised gone, and the job already marked done so it could never be retried.
func (w *Worker) saveDraft(ctx context.Context, job store.ReceiptJob, d Draft) bool {
	if d.Empty() {
		return true
	}
	if err := w.store.SaveReceiptDraft(ctx, job.ID, d.toStore()); err != nil {
		log.Printf("worker: could not save the draft for receipt job %d: %v", job.ID, err)
		return false
	}
	return true
}

// finishDraft records a successful reading and invites the user to confirm it.
//
// Note what this function does NOT do, and what an earlier version of it did: it
// does not call store.Add. No transaction is created, no allocation is
// recalculated and no total on any page moves. OCR is good enough to save
// somebody typing and nowhere near good enough to be trusted unattended -- on a
// hard photograph it will read $45.78 for a $45.70 receipt, which is wrong in a
// way that looks entirely reasonable and would never be noticed again.
func (w *Worker) finishDraft(ctx context.Context, job store.ReceiptJob, d Draft) {
	if d.Amount <= 0 {
		// A processor reporting success with no amount has nothing to propose.
		w.finishNeedsReview(ctx, job, d)
		return
	}

	// If the proposal could not be stored there is nothing to invite the user to
	// confirm, so this is a failure rather than a success with a broken link.
	// Treating it as one also leaves the job retryable.
	if !w.saveDraft(ctx, job, d) {
		w.finishFailed(ctx, job, fmt.Errorf("the reading could not be stored"))
		return
	}

	if err := w.store.CompleteReceiptJob(ctx, job.ID, nil); err != nil {
		log.Printf("worker: could not mark job %d done: %v", job.ID, err)
	}

	where := ""
	if d.Payee != "" {
		where = " from " + d.Payee
	}
	w.notify(ctx, job.UserID, "success",
		fmt.Sprintf("Read %s%s off %s. Tap to check it and save.",
			d.Amount.Display(), where, job.OriginalName),
		w.confirmLink(job.ID))
}

// finishNeedsReview stores whatever was read against a form the user completes.
func (w *Worker) finishNeedsReview(ctx context.Context, job store.ReceiptJob, d Draft) {
	w.saveDraft(ctx, job, d)
	if err := w.store.CompleteReceiptJob(ctx, job.ID, nil); err != nil {
		log.Printf("worker: could not mark job %d done: %v", job.ID, err)
	}
	w.notify(ctx, job.UserID, "info",
		fmt.Sprintf("Receipt %s is ready, but the amount could not be read. Tap to enter it.",
			job.OriginalName),
		w.confirmLink(job.ID))
}

// confirmLink is where every processed receipt sends the user: the expense form,
// prefilled with whatever was read and showing the image beside it.
func (w *Worker) confirmLink(jobID int64) string {
	return "/transactions/new?type=expense&receipt=" + fmt.Sprint(jobID)
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
