// Tests for the worker driving a real queue.
//
// worker_test.go covers the processor contract in isolation. What is here is the
// half that only shows up against a database: which of the three outcomes a
// processor returns leads to which end state, how many times a failure is
// retried before the user is told, and -- the invariant this whole package was
// designed around -- that no reading of a receipt, however confident, ever
// becomes a transaction on its own.
package worker

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jthomasw/YABA-2026/internal/db"
	"github.com/jthomasw/YABA-2026/internal/store"
)

// queueRig is a real database with one user, one household and a worker.
type queueRig struct {
	t     *testing.T
	store *store.Store
	db    *sql.DB
	scope store.Scope
}

func newQueueRig(t *testing.T) *queueRig {
	t.Helper()

	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "worker.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.Migrate(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	st := store.New(sqlDB)
	uid, err := st.CreateUser(context.Background(), "worker@example.com", "not-a-real-hash")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	hh, err := st.ActiveHousehold(context.Background(), store.User{ID: uid})
	if err != nil {
		t.Fatalf("active household: %v", err)
	}
	return &queueRig{t: t, store: st, db: sqlDB, scope: store.Scope{HouseholdID: hh.ID, UserID: uid}}
}

// enqueue puts one receipt in the queue and returns its job id.
func (q *queueRig) enqueue(name string) int64 {
	q.t.Helper()
	id, err := q.store.EnqueueReceipt(context.Background(), q.scope, "uploads/1/"+name, name)
	if err != nil {
		q.t.Fatalf("enqueue: %v", err)
	}
	return id
}

// status reads a job's end state straight from the table, because queued,
// processing, done and failed are exactly what these tests are about and no
// application query exposes all four.
func (q *queueRig) status(id int64) (state, cause string) {
	q.t.Helper()
	if err := q.db.QueryRow(
		`SELECT status, error FROM receipt_jobs WHERE id = ?`, id).Scan(&state, &cause); err != nil {
		q.t.Fatalf("read job %d: %v", id, err)
	}
	return state, cause
}

// notifications drains what the worker told the user.
func (q *queueRig) notifications() []store.Notification {
	q.t.Helper()
	ns, err := q.store.TakeNotifications(context.Background(), q.scope.UserID)
	if err != nil {
		q.t.Fatalf("take notifications: %v", err)
	}
	return ns
}

// transactionCount is the number that must never move because of OCR.
func (q *queueRig) transactionCount() int {
	q.t.Helper()
	var n int
	if err := q.db.QueryRow(`SELECT COUNT(*) FROM transactions`).Scan(&n); err != nil {
		q.t.Fatalf("count transactions: %v", err)
	}
	return n
}

// drain runs the worker over everything currently queued, without Run's timer.
func (q *queueRig) drain(p Processor) {
	q.t.Helper()
	w := New(q.store, p, time.Minute)
	for w.processNext(context.Background()) {
	}
}

// TestASuccessfulReadingBecomesADraftAndNotATransaction is the invariant the
// package exists to hold. A processor can return a perfectly formed amount with
// full confidence and it still only earns a row against the receipt job and a
// message asking somebody to look: the ledger is untouched until a person
// presses Save. A misread amount that entered the ledger silently would be
// wrong in a way nobody would ever notice again.
func TestASuccessfulReadingBecomesADraftAndNotATransaction(t *testing.T) {
	q := newQueueRig(t)
	id := q.enqueue("lidl.jpg")

	q.drain(stubProcessor{draft: Draft{
		Amount: 4578, Payee: "LIDL", Label: "Groceries",
		Date: "2026-03-14", Confidence: 0.97,
		Items: []store.DraftItem{{Description: "Milk", Amount: 129}},
	}})

	if n := q.transactionCount(); n != 0 {
		t.Fatalf("%d transaction(s) created from a receipt; OCR must never write to the ledger", n)
	}

	state, _ := q.status(id)
	if state != "done" {
		t.Errorf("job status = %q, want done", state)
	}

	job, err := q.store.UnattachedReceipt(context.Background(), q.scope, id)
	if err != nil {
		t.Fatalf("the receipt should still be waiting to be confirmed: %v", err)
	}
	if job.Draft == nil {
		t.Fatal("nothing was saved for the confirmation form to prefill")
	}
	if job.Draft.Total != 4578 || job.Draft.Merchant != "LIDL" || job.Draft.Date != "2026-03-14" {
		t.Errorf("draft = %+v, want the amount, merchant and date that were read", job.Draft)
	}
	if len(job.Draft.Items) != 1 {
		t.Errorf("line items were dropped: %+v", job.Draft.Items)
	}

	ns := q.notifications()
	if len(ns) != 1 {
		t.Fatalf("%d notifications, want exactly one", len(ns))
	}
	if ns[0].Kind != "success" {
		t.Errorf("notification kind = %q, want success", ns[0].Kind)
	}
	// The link has to reach the confirmation form carrying this receipt, or the
	// draft is saved somewhere the user can never see it.
	if !strings.Contains(ns[0].Link, "receipt=") {
		t.Errorf("notification link %q does not open the receipt", ns[0].Link)
	}
	if !strings.Contains(ns[0].Text, "45.78") {
		t.Errorf("notification %q does not say what was read", ns[0].Text)
	}
}

// TestSuccessWithNoAmountFallsBackToReview: a processor is allowed to return no
// error and no amount, and that is not a success -- there is nothing to propose,
// so the user must be asked rather than shown an empty confirmation.
func TestSuccessWithNoAmountFallsBackToReview(t *testing.T) {
	q := newQueueRig(t)
	id := q.enqueue("blurred.jpg")

	// Something was read -- a merchant -- but no amount.
	q.drain(stubProcessor{draft: Draft{Payee: "COSTCO", Text: "COSTCO ..."}})

	if state, _ := q.status(id); state != "done" {
		t.Errorf("job status = %q, want done: an unreadable amount is not a failure", state)
	}
	ns := q.notifications()
	if len(ns) != 1 || ns[0].Kind != "info" {
		t.Fatalf("notifications = %+v, want one info message", ns)
	}
	if !strings.Contains(ns[0].Text, "could not be read") {
		t.Errorf("notification %q does not explain that the amount is missing", ns[0].Text)
	}

	// Whatever WAS read is still kept, so the form is not blank.
	job, err := q.store.UnattachedReceipt(context.Background(), q.scope, id)
	if err != nil {
		t.Fatal(err)
	}
	if job.Draft == nil || job.Draft.Merchant != "COSTCO" {
		t.Errorf("the partial reading was thrown away: %+v", job.Draft)
	}
}

// TestAnEmptyReadingSavesNoDraft: a receipt that yielded literally nothing must
// not leave a row of blanks for the form to prefill from.
func TestAnEmptyReadingSavesNoDraft(t *testing.T) {
	q := newQueueRig(t)
	id := q.enqueue("black.jpg")

	q.drain(stubProcessor{err: ErrNeedsReview})

	job, err := q.store.UnattachedReceipt(context.Background(), q.scope, id)
	if err != nil {
		t.Fatal(err)
	}
	if job.Draft != nil {
		t.Errorf("an empty reading was stored anyway: %+v", job.Draft)
	}
	if ns := q.notifications(); len(ns) != 1 {
		t.Errorf("the user should still be told the receipt is ready to fill in, got %+v", ns)
	}
}

// TestAFailureIsRetriedThenReported: a genuine error puts the job back in the
// queue rather than reporting it, and the user hears about it exactly once, on
// the attempt that gives up. Telling them on the first attempt would cry wolf
// over a transient error; never telling them would lose the receipt in silence.
func TestAFailureIsRetriedThenReported(t *testing.T) {
	q := newQueueRig(t)
	id := q.enqueue("corrupt.jpg")

	broken := stubProcessor{err: errStub("disk on fire")}
	w := New(q.store, broken, time.Minute)

	for attempt := 1; attempt <= store.MaxJobAttempts; attempt++ {
		if !w.processNext(context.Background()) {
			t.Fatalf("attempt %d: nothing left in the queue, but the job has not been given up on", attempt)
		}
		state, _ := q.status(id)
		want := "queued"
		if attempt == store.MaxJobAttempts {
			want = "failed"
		}
		if state != want {
			t.Errorf("after attempt %d: status = %q, want %q", attempt, state, want)
		}
		if got, wantN := len(q.notifications()), 0; attempt < store.MaxJobAttempts && got != wantN {
			t.Errorf("after attempt %d: %d notifications, want %d -- a retry is not news",
				attempt, got, wantN)
		}
	}

	// Nothing is left to claim once it has failed.
	if w.processNext(context.Background()) {
		t.Error("a failed job was claimed again")
	}
	if _, cause := q.status(id); !strings.Contains(cause, "disk on fire") {
		t.Errorf("the recorded cause %q does not say what went wrong", cause)
	}
	if n := q.transactionCount(); n != 0 {
		t.Errorf("%d transactions after a failure", n)
	}
}

// TestStuckJobsAreRequeuedOnStart: a job claimed by a process that was then
// killed sits in 'processing' forever, so it would never run again and never be
// reported. Run recovers those before anything else.
func TestStuckJobsAreRequeuedOnStart(t *testing.T) {
	q := newQueueRig(t)
	id := q.enqueue("interrupted.jpg")

	// Claim it and abandon it, exactly as a kill -9 would.
	if _, err := q.store.ClaimReceiptJob(context.Background()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if state, _ := q.status(id); state != "processing" {
		t.Fatalf("status = %q, want processing", state)
	}

	ctx, cancel := context.WithCancel(context.Background())
	w := New(q.store, stubProcessor{draft: Draft{Amount: 999, Payee: "Shop"}}, time.Minute)
	go w.Run(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for {
		if state, _ := q.status(id); state == "done" {
			break
		}
		if time.Now().After(deadline) {
			state, _ := q.status(id)
			cancel()
			t.Fatalf("the stranded job is still %q; a restart never picks it up", state)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	w.Stop(2 * time.Second)
}

// TestWakeIsNeverBlockingAndAlwaysDrains: the upload handler calls Wake on the
// request path, so it must return whether or not the worker is listening, and a
// burst of uploads must not be spread across one tick each.
func TestWakeIsNeverBlockingAndAlwaysDrains(t *testing.T) {
	q := newQueueRig(t)

	w := New(q.store, stubProcessor{draft: Draft{Amount: 100, Payee: "Shop"}}, time.Hour)

	// Nobody is running yet: these must still return immediately.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			w.Wake()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wake blocked with no worker running; an upload would hang on it")
	}

	ids := []int64{q.enqueue("a.jpg"), q.enqueue("b.jpg"), q.enqueue("c.jpg")}

	ctx, cancel := context.WithCancel(context.Background())
	go w.Run(ctx)
	w.Wake()

	// The tick is an hour away, so anything that finishes did so because the
	// single Wake drained the whole queue rather than one job.
	deadline := time.Now().Add(3 * time.Second)
	for {
		all := true
		for _, id := range ids {
			if state, _ := q.status(id); state != "done" {
				all = false
			}
		}
		if all {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("one Wake did not drain the queue, so a burst of uploads trickles out one per tick")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	w.Stop(2 * time.Second)
}

// TestStopReturnsEvenWhenRunNeverStarted: Stop is called from the shutdown path,
// which must not hang because the worker was never started.
func TestStopReturnsEvenWhenRunNeverStarted(t *testing.T) {
	w := New(newQueueRig(t).store, stubProcessor{}, time.Hour)
	start := time.Now()
	w.Stop(200 * time.Millisecond)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Stop took %v; shutdown would hang", elapsed)
	}
	// And a second call is harmless.
	w.Stop(200 * time.Millisecond)
}

// TestNilProcessorGetsTheSafeDefault: New(store, nil, ...) must not produce a
// worker that panics on the first receipt.
func TestNilProcessorGetsTheSafeDefault(t *testing.T) {
	q := newQueueRig(t)
	id := q.enqueue("anything.jpg")

	w := New(q.store, nil, 0)
	if w.interval <= 0 {
		t.Errorf("interval = %v, want a positive default", w.interval)
	}
	w.processNext(context.Background())

	// ReviewProcessor cannot find the file, which is a real failure and a retry.
	if state, _ := q.status(id); state != "queued" {
		t.Errorf("status = %q, want queued after the first failed attempt", state)
	}
	if n := q.transactionCount(); n != 0 {
		t.Errorf("%d transactions", n)
	}
}
