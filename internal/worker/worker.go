// Package worker drains the receipt queue in the background, so an upload handler
// writes one file and one row and returns without waiting for the slow work.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
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
	TaxLines   []store.DraftItem
	Tip        store.Cents
	Items      []store.DraftItem
	Confidence float64
	Text       string

	// ItemsBalanced reports that lines were added to make the items reconcile
	// with the total, not listed on the receipt, so the form can say so.
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
		TaxLines:      d.TaxLines,
		Tip:           d.Tip,
		Items:         d.Items,
		ItemsBalanced: d.ItemsBalanced,
		Confidence:    d.Confidence,
		Text:          d.Text,
	}.Balanced()
}

// ErrNeedsReview means the receipt was readable as a file but its contents
// could not be interpreted.
var ErrNeedsReview = errors.New("receipt needs manual review")

// ErrReaderUnavailable means nothing tried to read the receipt: no API key is
// configured, or the reading service refused the request itself (a bad key, a
// model that does not exist). It is a kind of ErrNeedsReview -- the receipt
// still waits for the user -- but the user is told the real reason. Telling
// them "the amount could not be read" about a perfectly clear photograph sent
// them looking for a problem with the picture, when the server was never set
// up to read it.
var ErrReaderUnavailable = fmt.Errorf("%w: receipt reading is not available", ErrNeedsReview)

// retryDelays is how long a failed job waits before its next attempt, indexed
// by how many attempts it has already had: the first failure waits 30 seconds,
// the second 2 minutes, and anything later the last entry. Short enough that a
// blip at Google costs the user a minute, long enough that an outage of a few
// minutes does not burn every attempt -- which is exactly what happened when a
// failure went straight back to the queue and all three tries landed inside one
// second.
var retryDelays = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute}

// retryJitter spreads each delay by up to this fraction either way, so a batch
// of receipts that failed together during one outage does not come back
// together and hit the API in the same instant again.
const retryJitter = 0.2

// maxRetryAfter caps how long a server's Retry-After can push a job back. A
// receipt that waits an hour is still a receipt; one parked for a day because
// a proxy sent a silly header is a lost one.
const maxRetryAfter = time.Hour

// The writes that record a job's outcome run on a context detached from the
// worker's own (see processNext), so they need limits of their own.
//
// While the server is running the limit is generous. The whole server shares a
// single SQLite connection, so a backup's VACUUM INTO or a slow disk can hold
// it for several seconds; a short limit there failed every write, stranded the
// job in 'processing' and silently dropped the user's notification.
//
// During shutdown it is short, kept inside the five seconds main gives Stop so
// the job is written down before the database is closed under it.
const (
	bookkeepingTimeout         = time.Minute
	shutdownBookkeepingTimeout = 3 * time.Second
)

// sweepEvery is how often an idle worker looks for jobs left in 'processing'
// that nothing is working on. See sweep.
const sweepEvery = 5 * time.Minute

// retryAfterer is implemented by a processor error that knows how long the far
// end asked to be left alone (an HTTP 429 or 503 with Retry-After, say).
type retryAfterer interface {
	RetryAfter() time.Duration
}

// retryDelay is how long to wait before retrying a job that has had attempts
// tries. u is a uniform random number in [0, 1) that picks the jitter, and
// retryAfter is what the server asked for, if anything; the longer of the two
// wins, because retrying sooner than asked only earns another refusal.
func retryDelay(attempts int, u float64, retryAfter time.Duration) time.Duration {
	i := min(max(attempts-1, 0), len(retryDelays)-1)
	base := retryDelays[i]
	d := time.Duration(float64(base) * (1 - retryJitter + 2*retryJitter*u))
	if retryAfter > d {
		d = min(retryAfter, maxRetryAfter)
	}
	return d
}

// Worker polls the queue.
type Worker struct {
	store     *store.Store
	processor Processor
	interval  time.Duration

	// now and random are the clock and the jitter source, fields rather than
	// direct calls so a test can say exactly when a retry falls due.
	now    func() time.Time
	random func() float64

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
		now:       time.Now,
		random:    rand.Float64,
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
//
// The loop never spins: a pass drains every job that is due, and then it waits
// for an upload's Wake or the next tick. A job waiting out a retry delay is not
// due, so it is simply skipped until a later tick finds that its time has come.
func (w *Worker) Run(ctx context.Context) {
	defer close(w.done)

	w.sweep(ctx)
	lastSweep := w.now()

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		// Drain fully on each pass, so a burst of uploads is not spread across one tick each.
		for w.processNext(ctx) {
			if ctx.Err() != nil {
				return
			}
		}

		if w.now().Sub(lastSweep) >= sweepEvery {
			w.sweep(ctx)
			lastSweep = w.now()
		}

		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-ticker.C:
		}
	}
}

// sweep returns jobs stranded in 'processing' to the queue, or gives up on
// them once they have used every attempt.
//
// It is only ever called from Run, between jobs. This worker is the only one
// (main starts a single Run, and the database lock keeps a second server off
// the same file), and it processes one job at a time, so at that point any row
// still marked 'processing' is one nothing is working on: left by a crash, or
// by an outcome write that failed. Before this ran periodically such a job sat
// there until the next restart, unopenable and undiscardable, because only
// settled receipts can be entered or thrown away.
func (w *Worker) sweep(ctx context.Context) {
	n, exhausted, err := w.store.RecoverStuckJobs(ctx)
	switch {
	case err != nil:
		log.Printf("worker: could not recover stuck jobs: %v", err)
	case n > 0:
		log.Printf("worker: requeued %d receipt job(s) that were left in processing", n)
	}
	for _, job := range exhausted {
		log.Printf("worker: receipt job %d was interrupted on every attempt; giving up", job.ID)
		w.notifyGivenUp(ctx, job)
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
	job, err := w.store.ClaimReceiptJobAt(ctx, w.now())
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

	// From here on the outcome has to be written down whether or not the worker
	// is being stopped. Shutdown cancels ctx, usually in the middle of a Gemini
	// call; writing the result with that same ctx failed instantly, left the job
	// in 'processing', and the next start requeued it with an attempt burned --
	// so every deploy cost an in-flight receipt one of its three tries.
	timeout := bookkeepingTimeout
	if ctx.Err() != nil {
		timeout = shutdownBookkeepingTimeout
	}
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	switch {
	case err != nil && !errors.Is(err, ErrNeedsReview) && ctx.Err() != nil:
		// The error is almost certainly the cancellation itself, not anything
		// wrong with the receipt. Hand it back untouched for the next start.
		w.release(bctx, job, err)

	case errors.Is(err, ErrReaderUnavailable):
		// Nothing tried to read it. Said plainly, so a clear photo is not
		// blamed for a server that has no reader configured.
		w.finishUnread(bctx, job)

	case errors.Is(err, ErrNeedsReview):
		// Not a failure. The file was fine; its contents were not legible enough
		// to propose an amount, and whatever else was read is still worth keeping.
		w.finishNeedsReview(bctx, job, draft)

	case err != nil:
		w.finishFailed(bctx, job, err)

	default:
		w.finishDraft(bctx, job, draft)
	}
	return true
}

// release returns a job interrupted by shutdown to the queue without counting
// the attempt.
func (w *Worker) release(ctx context.Context, job store.ReceiptJob, cause error) {
	if err := w.store.ReleaseReceiptJob(ctx, job.ID); err != nil {
		log.Printf("worker: could not hand back receipt job %d after shutdown interrupted it: %v",
			job.ID, err)
		return
	}
	log.Printf("worker: shutdown interrupted receipt job %d (%v); it will run again on the next start",
		job.ID, cause)
}

// safeProcess runs the processor and turns a panic into an ordinary job failure.
//
// This goroutine is started with a bare `go receipts.Run(ctx)`, and an
// unrecovered panic in a goroutine takes the whole PROCESS down -- not just the
// worker. The HTTP recoverPanics middleware does not reach here. What runs
// inside is the receipt-reading path: it sniffs an uploaded file, sends it to
// the Gemini API and decodes whatever JSON comes back -- the file is
// attacker-supplied by definition once anybody can upload a receipt, and the
// reply is whatever a remote model made of it. One index-out-of-range in that
// handling would have signed every user out and stopped the site.
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

// finishUnread records a receipt nothing was able to read, and says so.
func (w *Worker) finishUnread(ctx context.Context, job store.ReceiptJob) {
	if err := w.store.CompleteReceiptJobUnread(ctx, job.ID, store.UnreadReaderUnavailable); err != nil {
		log.Printf("worker: could not mark job %d done: %v", job.ID, err)
	}
	w.notify(ctx, job.UserID, "info",
		fmt.Sprintf("Receipt %s is saved. Automatic reading isn't available on this server, so tap to enter the details.",
			job.OriginalName),
		w.confirmLink(job.ID))
}

// confirmLink is where every processed receipt sends the user: the expense form,
// prefilled with whatever was read and showing the image beside it.
func (w *Worker) confirmLink(jobID int64) string {
	return "/transactions/new?type=expense&receipt=" + fmt.Sprint(jobID)
}

// finishFailed schedules a retry after a backoff, then gives up and tells the user.
func (w *Worker) finishFailed(ctx context.Context, job store.ReceiptJob, cause error) {
	var asked time.Duration
	var ra retryAfterer
	if errors.As(cause, &ra) {
		asked = ra.RetryAfter()
	}
	delay := retryDelay(job.Attempts, w.random(), asked)

	givenUp, err := w.store.RetryOrFailReceiptJob(ctx, job.ID, cause.Error(), w.now().Add(delay))
	if err != nil {
		log.Printf("worker: job %d failed: %v", job.ID, cause)
		log.Printf("worker: could not update failed job %d: %v", job.ID, err)
		return
	}
	if !givenUp {
		log.Printf("worker: job %d failed on attempt %d of %d, retrying in %s: %v",
			job.ID, job.Attempts, store.MaxJobAttempts, delay.Round(time.Second), cause)
		return // it will come round again
	}

	log.Printf("worker: job %d failed on its last attempt, giving up: %v", job.ID, cause)
	w.notifyGivenUp(ctx, job)
}

// notifyGivenUp tells the uploader a receipt could not be imported at all.
func (w *Worker) notifyGivenUp(ctx context.Context, job store.ReceiptJob) {
	// Written to the database rather than pushed to a live page, so a failure still
	// reaches the user whenever they next sign in.
	// The link opens the form for this receipt, not a blank one: a failed job
	// can still be entered by hand with its image attached, and a blank form
	// left the job and its file sitting on /receipts for good.
	w.notify(ctx, job.UserID, "error",
		fmt.Sprintf("Could not import the receipt %s. Please add it manually.", job.OriginalName),
		w.confirmLink(job.ID))
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
	return Draft{}, ErrReaderUnavailable
}

// localPath converts a stored path to the local separator.
func localPath(stored string) string {
	return filepath.FromSlash(stored)
}
