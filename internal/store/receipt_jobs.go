package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// JobStatus tracks a receipt through the queue.
type JobStatus string

const (
	// JobQueued is waiting to be picked up.
	JobQueued JobStatus = "queued"
	// JobProcessing has been claimed by a worker.
	JobProcessing JobStatus = "processing"
	// JobDone finished successfully.
	JobDone JobStatus = "done"
	// JobFailed gave up. The user must be told.
	JobFailed JobStatus = "failed"
)

// receiptSettledSQL restricts a receipt_jobs query to jobs the worker has
// finished with, successfully or not. Only those may be opened, attached or
// discarded by a user: a queued or processing job still belongs to the worker,
// which is about to write a draft onto the row and may be reading the file.
// A failed job is included deliberately -- reading it failed, but the image is
// saved, and the user can still type the expense in by hand or throw it away.
const receiptSettledSQL = `status IN ('done', 'failed')`

// DraftItem is one product line OCR read off a receipt.
type DraftItem struct {
	Description string `json:"description"`
	Amount      Cents  `json:"amount"`

	// Added marks a line the app put in so the items add up to the total --
	// a tax or tip line, "Other", or an adjustment -- rather than a product the
	// receipt listed. See ReceiptDraft.Balanced.
	Added bool `json:"added,omitempty"`
}

// ReceiptDraft is what OCR made of a receipt: a proposal, never a fact. Nothing
// in it has reached the ledger, and nothing will until a person has seen these
// numbers next to the photograph they came from and pressed Save.
//
// Everything is optional. A receipt photographed in bad light yields a draft
// with nothing but Text, which is still an improvement on nothing: the user gets
// the image on screen beside an empty form instead of having to find it again.
type ReceiptDraft struct {
	Merchant string `json:"merchant,omitempty"`
	Category string `json:"category,omitempty"`
	Date     string `json:"date,omitempty"`

	Total    Cents `json:"total,omitempty"`
	Subtotal Cents `json:"subtotal,omitempty"`
	Tax      Cents `json:"tax,omitempty"`

	// TaxLines are the receipt's tax lines as printed ("TAX 1 5.5%" $2.84,
	// "TAX 4 8%" $0.24), when it shows more than one. Tax is their total.
	TaxLines []DraftItem `json:"tax_lines,omitempty"`
	Tip      Cents       `json:"tip,omitempty"`

	Items []DraftItem `json:"items,omitempty"`

	// ItemsBalanced reports that lines were appended (tax, tip, "Other" or an
	// adjustment) so the items add up to what was charged. See Balanced. The
	// form says so, so an added line is never mistaken for one the receipt
	// listed.
	ItemsBalanced bool `json:"items_balanced,omitempty"`

	// Confidence is 0..1. It decides how emphatically the form asks the user to
	// check the number, and nothing else.
	Confidence float64 `json:"confidence,omitempty"`

	// Text is the raw OCR output, shown on request and kept for diagnosis.
	Text string `json:"-"`
}

// HasTotal reports whether there is an amount worth prefilling.
func (d ReceiptDraft) HasTotal() bool { return d.Total > 0 }

// Certainty buckets the confidence for the interface, which needs three states
// rather than a number: a percentage implies a precision this does not have.
func (d ReceiptDraft) Certainty() string {
	switch {
	case !d.HasTotal():
		return "none"
	case d.Confidence >= 0.85:
		return "high"
	case d.Confidence >= 0.55:
		return "medium"
	default:
		return "low"
	}
}

// ItemsTotal is the sum of the draft's line items, for a template that wants to
// show whether they reconcile.
func (d ReceiptDraft) ItemsTotal() Cents {
	var sum Cents
	for _, it := range d.Items {
		sum += it.Amount
	}
	return sum
}

// Labels for the lines Balanced adds. Exported so the template and tests can
// name them without repeating the spelling.
const (
	DraftLineTax        = "Tax"
	DraftLineTip        = "Tip"
	DraftLineOther      = "Other (not itemised)"
	DraftLineAdjustment = "Discount / adjustment"
)

// Balanced returns the draft with lines appended so its items add up exactly to
// Total, which is what saving a breakdown requires.
//
// A receipt's item prices almost never sum to what was charged: tax and tip sit
// outside them, a price may be unreadable, and a discount may be missed. Without
// this the prefilled form was refused on its first save ("the line items add up
// to $10.00 but the total is $10.80"). Tax and tip are added as their own lines
// when the gap has room for them -- so a model that already listed tax as an
// item does not get it twice -- and whatever gap remains becomes one "Other"
// line (or a negative adjustment when the items exceed the total).
//
// Calling it on an already balanced draft changes nothing, so it is safe to
// apply both when the draft is saved and when an older stored draft is shown.
func (d ReceiptDraft) Balanced() ReceiptDraft {
	if d.Total <= 0 || len(d.Items) == 0 {
		return d
	}
	sum := d.ItemsTotal()
	if sum == d.Total {
		return d
	}

	items := append([]DraftItem(nil), d.Items...)
	add := func(desc string, c Cents) {
		items = append(items, DraftItem{Description: desc, Amount: c, Added: true})
		sum += c
	}

	// Each printed tax line becomes its own row, so "TAX 1 5.5%" and "TAX 4 8%"
	// both appear rather than one lump. They are added only when the gap has
	// room for all of them: a reading that already listed tax among the items
	// must not get it twice.
	taxes := d.taxLines()
	var taxSum Cents
	for _, t := range taxes {
		taxSum += t.Amount
	}
	if taxSum > 0 && d.Total-sum >= taxSum {
		for _, t := range taxes {
			add(t.Description, t.Amount)
		}
	}
	if d.Tip > 0 && d.Total-sum >= d.Tip {
		add(DraftLineTip, d.Tip)
	}
	switch gap := d.Total - sum; {
	case gap > 0:
		add(DraftLineOther, gap)
	case gap < 0:
		add(DraftLineAdjustment, gap)
	}

	d.Items = items
	d.ItemsBalanced = true
	return d
}

// taxLines returns the tax rows Balanced may add: the printed tax lines with
// an amount, each labelled so it reads as tax, or a single "Tax" line from
// the total when no breakdown was read.
func (d ReceiptDraft) taxLines() []DraftItem {
	var out []DraftItem
	for _, t := range d.TaxLines {
		if t.Amount <= 0 {
			continue // "TAX 12 0%" adds nothing to the breakdown
		}
		label := strings.Join(strings.Fields(t.Description), " ")
		switch {
		case label == "":
			label = DraftLineTax
		case !strings.Contains(strings.ToLower(label), "tax"):
			label = DraftLineTax + " (" + label + ")"
		}
		out = append(out, DraftItem{Description: label, Amount: t.Amount})
	}
	if len(out) == 0 && d.Tax > 0 {
		out = append(out, DraftItem{Description: DraftLineTax, Amount: d.Tax})
	}
	return out
}

// ReadItemCount is how many lines came off the receipt, not counting the
// ones Balanced added.
func (d ReceiptDraft) ReadItemCount() int {
	n := 0
	for _, it := range d.Items {
		if !it.Added {
			n++
		}
	}
	return n
}

// ReceiptJob is one queued receipt image.
type ReceiptJob struct {
	ID int64

	// UserID is who uploaded the file; HouseholdID is which budget the expense will belong
	// to.
	UserID      int64
	HouseholdID int64

	Path          string
	OriginalName  string
	Status        JobStatus
	Error         string
	Attempts      int
	TransactionID *int64
	CreatedAt     string
	FinishedAt    string

	// Draft is what OCR read, or nil when the receipt has not been processed or
	// nothing could be read from it.
	Draft *ReceiptDraft
}

// Failed reports whether the worker gave up on this receipt. Error then holds
// the worker's internal cause, which can name server paths and is for the log,
// not for the page.
func (j ReceiptJob) Failed() bool { return j.Status == JobFailed }

// receiptJobColumns is the column list every receipt_jobs read shares, so a
// column added to the table is added to one place rather than four.
const receiptJobColumns = `id, user_id, household_id, path, original_name, status,
	error, attempts, transaction_id, created_at, IFNULL(finished_at, ''),
	parsed_total_cents, parsed_confidence, parsed_json, ocr_text`

// scanReceiptJob reads one row in the order receiptJobColumns lists.
func scanReceiptJob(row rowScanner) (ReceiptJob, error) {
	var j ReceiptJob
	var total int64
	var confidence float64
	var parsed, text string

	if err := row.Scan(&j.ID, &j.UserID, &j.HouseholdID, &j.Path, &j.OriginalName,
		&j.Status, &j.Error, &j.Attempts, &j.TransactionID, &j.CreatedAt,
		&j.FinishedAt, &total, &confidence, &parsed, &text); err != nil {
		return ReceiptJob{}, err
	}

	// A draft exists only once the worker has written one. An empty column is
	// the normal state for a job that is still queued.
	if parsed == "" && total == 0 && text == "" {
		return j, nil
	}

	d := ReceiptDraft{}
	if parsed != "" {
		if err := json.Unmarshal([]byte(parsed), &d); err != nil {
			// A draft that will not decode is a bug in this package, not a
			// reason to fail the user's page: the receipt is still there and can
			// still be typed in by hand.
			log.Printf("store: receipt job %d has an undecodable draft: %v", j.ID, err)
			d = ReceiptDraft{}
		}
	}
	// The two promoted columns are authoritative over the document, because they
	// are what any query filters or sorts on.
	d.Total = Cents(total)
	d.Confidence = confidence
	d.Text = text
	j.Draft = &d
	return j, nil
}

// SaveReceiptDraft records what OCR made of a receipt. It deliberately does not
// touch the transactions table: the worker proposes, and only the user disposes.
func (s *Store) SaveReceiptDraft(ctx context.Context, jobID int64, d ReceiptDraft) error {
	// Text travels in its own column, so it is cleared before the document is
	// encoded rather than being stored twice.
	text := d.Text
	d.Text = ""

	blob, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("encode receipt draft: %w", err)
	}

	// The OCR text of a long receipt is a few kilobytes; a pathological image
	// could produce far more, and none of it past the first few thousand
	// characters is any use to anybody.
	const maxText = 20_000
	if len(text) > maxText {
		text = text[:maxText]
	}

	res, err := s.db.ExecContext(ctx, `
		UPDATE receipt_jobs
		SET parsed_total_cents = ?, parsed_confidence = ?, parsed_json = ?, ocr_text = ?
		WHERE id = ?`,
		int64(d.Total), d.Confidence, string(blob), text, jobID)
	if err != nil {
		return fmt.Errorf("save receipt draft: %w", err)
	}
	return requireOneRow(res)
}

// MaxJobAttempts is how many times a receipt is retried before the user is told it
// failed.
const MaxJobAttempts = 3

// EnqueueReceipt adds a receipt to the processing queue and returns its id, so the
// handler returns immediately rather than holding the user on a spinner.
func (s *Store) EnqueueReceipt(ctx context.Context, sc Scope, path, originalName string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO receipt_jobs(user_id, household_id, path, original_name) VALUES(?, ?, ?, ?)`,
		sc.UserID, sc.HouseholdID, path, originalName)
	if err != nil {
		return 0, fmt.Errorf("enqueue receipt: %w", err)
	}
	return res.LastInsertId()
}

// ClaimReceiptJob atomically takes the oldest queued job that is due now.
func (s *Store) ClaimReceiptJob(ctx context.Context) (ReceiptJob, error) {
	return s.ClaimReceiptJobAt(ctx, time.Now())
}

// CompleteReceiptJob marks a job done, optionally linking the transaction it produced.
//
// A nil txID never clears a link that is already there. The user can attach a
// receipt to an expense while the worker is still reading it, and the worker
// always completes with nil; overwriting the column would detach the receipt
// from that expense, put it back on the "waiting" list, and let a later Discard
// delete a file the expense still points at.
func (s *Store) CompleteReceiptJob(ctx context.Context, jobID int64, txID *int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE receipt_jobs
		SET status = 'done', error = '', transaction_id = COALESCE(?, transaction_id),
		    finished_at = ?, next_attempt_at = NULL
		WHERE id = ?`, txID, time.Now().UTC().Format(time.RFC3339), jobID)
	if err != nil {
		return fmt.Errorf("complete job: %w", err)
	}
	return nil
}

// UnreadReaderUnavailable is recorded in the error column of a job that was
// finished without being read because no reader was available -- no API key
// is configured, or the reading service refused the server's own request. It
// is a fixed code, never a cause from outside, so the status page may branch
// on it and tell the user why, which "the amount could not be read" did not.
const UnreadReaderUnavailable = "reader-unavailable"

// CompleteReceiptJobUnread marks a job done without a reading, recording why.
// The receipt is saved and waits for the user to enter it by hand.
func (s *Store) CompleteReceiptJobUnread(ctx context.Context, jobID int64, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE receipt_jobs
		SET status = 'done', error = ?, finished_at = ?, next_attempt_at = NULL
		WHERE id = ?`, reason, time.Now().UTC().Format(time.RFC3339), jobID)
	if err != nil {
		return fmt.Errorf("complete job: %w", err)
	}
	return nil
}

// ReaderUnavailable reports whether this job was finished without being read
// because no receipt reader was available.
func (j ReceiptJob) ReaderUnavailable() bool {
	return j.Status == JobDone && j.Error == UnreadReaderUnavailable
}

// RetryOrFailReceiptJob puts a job back in the queue to be tried again no
// earlier than retryAt, or gives up on it once MaxJobAttempts is reached.
//
// When to retry is the caller's decision, because the right delay depends on
// why it failed (an API's Retry-After, say) and the worker knows that while
// the store does not. What the store guarantees is that ClaimReceiptJob will
// not hand the job out again before then.
func (s *Store) RetryOrFailReceiptJob(ctx context.Context, jobID int64, cause string, retryAt time.Time) (givenUp bool, err error) {
	var attempts int
	if err := s.db.QueryRowContext(ctx,
		`SELECT attempts FROM receipt_jobs WHERE id = ?`, jobID).Scan(&attempts); err != nil {
		return false, fmt.Errorf("read attempts: %w", err)
	}

	if attempts >= MaxJobAttempts {
		_, err := s.db.ExecContext(ctx, `
			UPDATE receipt_jobs SET status = 'failed', error = ?, finished_at = ?, next_attempt_at = NULL
			WHERE id = ?`, cause, time.Now().UTC().Format(time.RFC3339), jobID)
		if err != nil {
			return false, fmt.Errorf("fail job: %w", err)
		}
		return true, nil
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE receipt_jobs SET status = 'queued', error = ?, next_attempt_at = ? WHERE id = ?`,
		cause, queueTime(retryAt), jobID); err != nil {
		return false, fmt.Errorf("requeue job: %w", err)
	}
	return false, nil
}

// interruptedCause is recorded against a job that was in flight when the
// process died once too often, so the failure is explained rather than blank.
const interruptedCause = "processing was interrupted too many times"

// RecoverStuckJobs deals with jobs a previous process left in 'processing'.
//
// A job is only left there when the process died without finishing it -- a
// graceful shutdown hands its job back with ReleaseReceiptJob -- and the claim
// already counted that attempt. So the same MaxJobAttempts rule applies as for
// any other failure: a job with attempts to spare goes back in the queue, due
// immediately, and one that has used them all is failed and returned so the
// caller can tell its owner. Requeueing those too would let a receipt that
// crashes the process take it down again on every restart, forever.
func (s *Store) RecoverStuckJobs(ctx context.Context) (requeued int64, exhausted []ReceiptJob, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT `+receiptJobColumns+` FROM receipt_jobs
			WHERE status = 'processing' AND attempts >= ?
			ORDER BY id ASC`, MaxJobAttempts)
		if err != nil {
			return fmt.Errorf("find exhausted jobs: %w", err)
		}
		for rows.Next() {
			j, err := scanReceiptJob(rows)
			if err != nil {
				rows.Close()
				return fmt.Errorf("scan exhausted job: %w", err)
			}
			exhausted = append(exhausted, j)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		now := time.Now().UTC().Format(time.RFC3339)
		for i := range exhausted {
			if _, err := tx.ExecContext(ctx, `
				UPDATE receipt_jobs
				SET status = 'failed', error = ?, finished_at = ?, next_attempt_at = NULL
				WHERE id = ?`, interruptedCause, now, exhausted[i].ID); err != nil {
				return fmt.Errorf("fail exhausted job: %w", err)
			}
			exhausted[i].Status = "failed"
			exhausted[i].Error = interruptedCause
		}

		res, err := tx.ExecContext(ctx, `
			UPDATE receipt_jobs SET status = 'queued', next_attempt_at = NULL
			WHERE status = 'processing'`)
		if err != nil {
			return fmt.Errorf("requeue stuck jobs: %w", err)
		}
		requeued, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return 0, nil, fmt.Errorf("recover stuck jobs: %w", err)
	}
	return requeued, exhausted, nil
}

// ClaimReceiptJobAt atomically takes the oldest queued job whose retry time, if
// it has one, is not after now. The time is a parameter so the worker's clock,
// and a test's, decides what "due" means.
func (s *Store) ClaimReceiptJobAt(ctx context.Context, now time.Time) (ReceiptJob, error) {
	var job ReceiptJob

	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx, `
			SELECT id FROM receipt_jobs
			WHERE status = 'queued'
			  AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
			ORDER BY id ASC
			LIMIT 1`, queueTime(now)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("find queued job: %w", err)
		}

		res, err := tx.ExecContext(ctx, `
			UPDATE receipt_jobs
			SET status = 'processing', attempts = attempts + 1, started_at = ?
			WHERE id = ? AND status = 'queued'`,
			time.Now().UTC().Format(time.RFC3339), id)
		if err != nil {
			return fmt.Errorf("claim job: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// Another worker got there first.
			return ErrNotFound
		}

		var scanErr error
		job, scanErr = scanReceiptJob(tx.QueryRowContext(ctx,
			`SELECT `+receiptJobColumns+` FROM receipt_jobs WHERE id = ?`, id))
		return scanErr
	})

	return job, err
}

// ReleaseReceiptJob hands a claimed job back to the queue without counting the
// attempt, for a job the worker had to put down because the process is
// shutting down. Nothing was wrong with the receipt, so a deploy must not use
// up one of its tries -- three restarts during one slow Gemini call used to be
// enough to fail a perfectly good upload. It is due again immediately.
func (s *Store) ReleaseReceiptJob(ctx context.Context, jobID int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE receipt_jobs
		SET status = 'queued', attempts = MAX(attempts - 1, 0),
		    started_at = NULL, next_attempt_at = NULL
		WHERE id = ? AND status = 'processing'`, jobID)
	if err != nil {
		return fmt.Errorf("release job: %w", err)
	}
	return requireOneRow(res)
}

// queueTime formats a time for next_attempt_at. Always UTC at one-second
// precision, so the column compares correctly as text: SQLite has no time type,
// and "…T10:00:00Z" against "…T10:00:00.5Z" or a "+01:00" offset would not.
func queueTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// UnattachedReceipt fetches a receipt the worker has finished with -- read or
// failed -- that has not yet become an expense. This is the gate for opening a
// receipt in the expense form and for attaching it, so a job still queued or
// processing is ErrNotFound here: see receiptSettledSQL. To follow a receipt
// while it is in flight, use ReceiptInFlight.
func (s *Store) UnattachedReceipt(ctx context.Context, sc Scope, jobID int64) (ReceiptJob, error) {
	j, err := scanReceiptJob(s.db.QueryRowContext(ctx, `
		SELECT `+receiptJobColumns+`
		FROM receipt_jobs
		WHERE id = ? AND household_id = ? AND transaction_id IS NULL
		  AND `+receiptSettledSQL,
		jobID, sc.HouseholdID))
	if errors.Is(err, sql.ErrNoRows) {
		return ReceiptJob{}, ErrNotFound
	}
	if err != nil {
		return ReceiptJob{}, fmt.Errorf("unattached receipt: %w", err)
	}
	return j, nil
}

// ReceiptInFlight fetches an unattached receipt in this budget whatever its
// status, for the upload page's progress display, which has to see a job while
// it is still queued or processing. It grants nothing: opening or attaching the
// receipt still goes through UnattachedReceipt.
func (s *Store) ReceiptInFlight(ctx context.Context, sc Scope, jobID int64) (ReceiptJob, error) {
	j, err := scanReceiptJob(s.db.QueryRowContext(ctx, `
		SELECT `+receiptJobColumns+`
		FROM receipt_jobs
		WHERE id = ? AND household_id = ? AND transaction_id IS NULL`,
		jobID, sc.HouseholdID))
	if errors.Is(err, sql.ErrNoRows) {
		return ReceiptJob{}, ErrNotFound
	}
	if err != nil {
		return ReceiptJob{}, fmt.Errorf("receipt in flight: %w", err)
	}
	return j, nil
}

// UnattachedReceipts lists receipts in this budget that the worker has finished
// with and nobody has turned into an expense, newest first. Without it an upload
// is a dead end whenever the notification is missed.
//
// Failed receipts are listed alongside read ones (check Status, or Failed):
// leaving them out meant a receipt that could not be read was unreachable --
// it could be neither entered by hand nor discarded, and its file stayed on
// disk for good.
func (s *Store) UnattachedReceipts(ctx context.Context, sc Scope, limit int) ([]ReceiptJob, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+receiptJobColumns+`
		FROM receipt_jobs
		WHERE household_id = ? AND transaction_id IS NULL AND `+receiptSettledSQL+`
		ORDER BY id DESC
		LIMIT ?`, sc.HouseholdID, limit)
	if err != nil {
		return nil, fmt.Errorf("unattached receipts: %w", err)
	}
	defer rows.Close()

	out := []ReceiptJob{}
	for rows.Next() {
		j, err := scanReceiptJob(rows)
		if err != nil {
			return nil, fmt.Errorf("scan unattached receipt: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// DiscardReceipt throws away an uploaded receipt nobody wants, returning the stored
// path so the caller can delete the file too. Read and failed receipts can be
// discarded; one still queued or processing cannot, because the worker may be
// reading the very file the caller is about to delete.
func (s *Store) DiscardReceipt(ctx context.Context, sc Scope, jobID int64) (string, error) {
	var path string
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			SELECT path FROM receipt_jobs
			WHERE id = ? AND household_id = ? AND transaction_id IS NULL
			  AND `+receiptSettledSQL,
			jobID, sc.HouseholdID).Scan(&path)
		if errors.Is(err, sql.ErrNoRows) {
			// It belongs to another budget, is already an expense, or is
			// still with the worker.
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read receipt job: %w", err)
		}

		res, err := tx.ExecContext(ctx, `
			DELETE FROM receipt_jobs
			WHERE id = ? AND household_id = ? AND transaction_id IS NULL
			  AND `+receiptSettledSQL,
			jobID, sc.HouseholdID)
		if err != nil {
			return fmt.Errorf("discard receipt: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return path, nil
}

// AttachReceipt links a read or failed receipt to the expense it became. The
// same states UnattachedReceipt allows, checked again here because this is the
// write, and a form can be submitted with any job id in it.
func (s *Store) AttachReceipt(ctx context.Context, sc Scope, jobID, txID int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var path, name string
		err := tx.QueryRowContext(ctx, `
			SELECT path, original_name FROM receipt_jobs
			WHERE id = ? AND household_id = ? AND transaction_id IS NULL
			  AND `+receiptSettledSQL,
			jobID, sc.HouseholdID).Scan(&path, &name)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read receipt job: %w", err)
		}

		res, err := tx.ExecContext(ctx, `
			UPDATE transactions SET receipt_path = ?, receipt_name = ?
			WHERE id = ? AND household_id = ?`, path, name, txID, sc.HouseholdID)
		if err != nil {
			return fmt.Errorf("attach receipt to transaction: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE receipt_jobs SET transaction_id = ?
			WHERE id = ? AND household_id = ? AND transaction_id IS NULL`,
			txID, jobID, sc.HouseholdID); err != nil {
			return fmt.Errorf("mark receipt attached: %w", err)
		}
		return nil
	})
}

// PendingReceiptCount is how many receipts destined for this budget are still in
// flight.
func (s *Store) PendingReceiptCount(ctx context.Context, sc Scope) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM receipt_jobs
		 WHERE household_id = ? AND status IN ('queued','processing')`,
		sc.HouseholdID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("pending receipts: %w", err)
	}
	return n, nil
}
