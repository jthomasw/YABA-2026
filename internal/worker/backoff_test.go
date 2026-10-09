// Tests for when a failed receipt is tried again, and for what happens to a
// receipt that is in flight when the worker is stopped.
//
// Both used to be wrong in ways that only showed up in production: a failure
// went straight back to the queue and all three attempts landed inside one
// second, and a deploy cancelled the context the outcome was then written
// with, so the job sat in 'processing' and lost an attempt on every restart.
package worker

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// fakeClock stands in for the worker's clock, so a retry's due time is decided
// by the test rather than by how long the test takes to run.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// newFakeClock installs a fake clock and a fixed, mid-range jitter on w. With
// the jitter at 0.5 every delay is exactly its nominal value.
func newFakeClock(w *Worker) *fakeClock {
	c := &fakeClock{now: time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)}
	w.now = c.Now
	w.random = func() float64 { return 0.5 }
	return c
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// attempts reads how many tries a job has used.
func (q *queueRig) attempts(id int64) int {
	q.t.Helper()
	var n int
	if err := q.db.QueryRow(`SELECT attempts FROM receipt_jobs WHERE id = ?`, id).Scan(&n); err != nil {
		q.t.Fatalf("read attempts of job %d: %v", id, err)
	}
	return n
}

func TestRetryDelay(t *testing.T) {
	cases := []struct {
		name       string
		attempts   int
		u          float64
		retryAfter time.Duration
		want       time.Duration
	}{
		{"first failure", 1, 0.5, 0, 30 * time.Second},
		{"second failure", 2, 0.5, 0, 2 * time.Minute},
		{"third failure", 3, 0.5, 0, 10 * time.Minute},
		{"capped beyond the table", 9, 0.5, 0, 10 * time.Minute},
		{"attempt zero is treated as the first", 0, 0.5, 0, 30 * time.Second},

		// Jitter spreads a delay by 20% either way and never further.
		{"jitter low end", 1, 0, 0, 24 * time.Second},
		{"jitter high end", 1, 0.999999, 0, 36 * time.Second},

		// A server's Retry-After wins when it asks for longer, is ignored when
		// it asks for less, and is capped so one silly header cannot park a
		// receipt for a day.
		{"Retry-After longer than the backoff", 1, 0.5, 5 * time.Minute, 5 * time.Minute},
		{"Retry-After shorter than the backoff", 2, 0.5, 10 * time.Second, 2 * time.Minute},
		{"Retry-After capped", 1, 0.5, 48 * time.Hour, maxRetryAfter},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := retryDelay(c.attempts, c.u, c.retryAfter)
			if diff := got - c.want; diff < -time.Millisecond || diff > time.Millisecond {
				t.Errorf("retryDelay(%d, %v, %v) = %v, want %v", c.attempts, c.u, c.retryAfter, got, c.want)
			}
		})
	}
}

// TestAFailedJobWaitsOutItsBackoff is the retry storm: a failed job must not
// be claimable again until its delay has passed, however often the worker
// looks.
func TestAFailedJobWaitsOutItsBackoff(t *testing.T) {
	q := newQueueRig(t)
	id := q.enqueue("flaky.jpg")

	w := New(q.store, stubProcessor{err: errStub("gemini: status 503")}, time.Minute)
	clock := newFakeClock(w)
	ctx := context.Background()

	steps := []struct {
		advance time.Duration
		want    bool // whether processNext finds the job
	}{
		{0, true},                 // attempt 1 fails: due again in 30s
		{0, false},                // the storm: immediately reclaimed before
		{29 * time.Second, false}, // still waiting
		{2 * time.Second, true},   // attempt 2 fails: due again in 2m
		{time.Minute, false},
		{58 * time.Second, false},
		{2 * time.Second, true}, // attempt 3 fails and is given up
		{time.Hour, false},      // and is never claimed again
	}
	for i, s := range steps {
		clock.advance(s.advance)
		if got := w.processNext(ctx); got != s.want {
			t.Fatalf("step %d (+%v): processNext = %v, want %v", i, s.advance, got, s.want)
		}
	}

	if state, _ := q.status(id); state != "failed" {
		t.Errorf("status = %q, want failed", state)
	}
	if ns := q.notifications(); len(ns) != 1 || ns[0].Kind != "error" {
		t.Errorf("notifications = %+v, want exactly one error once it was given up", ns)
	}
}

// retryAfterErr is a processor failure carrying a server's Retry-After.
type retryAfterErr struct{ after time.Duration }

func (e retryAfterErr) Error() string             { return "slow down" }
func (e retryAfterErr) RetryAfter() time.Duration { return e.after }

func TestRetryAfterIsHonoured(t *testing.T) {
	q := newQueueRig(t)
	q.enqueue("busy.jpg")

	w := New(q.store, stubProcessor{err: retryAfterErr{5 * time.Minute}}, time.Minute)
	clock := newFakeClock(w)
	ctx := context.Background()

	if !w.processNext(ctx) {
		t.Fatal("the job was not claimed at all")
	}
	// The 30-second backoff alone would have made it due by now.
	clock.advance(4 * time.Minute)
	if w.processNext(ctx) {
		t.Fatal("the job came back before the server's Retry-After")
	}
	clock.advance(2 * time.Minute)
	if !w.processNext(ctx) {
		t.Fatal("the job did not come back once Retry-After had passed")
	}
}

// blockingProcessor waits for its context to be cancelled, the way a Gemini
// call in flight does when the worker is stopped.
type blockingProcessor struct{ started chan struct{} }

func (b blockingProcessor) Process(ctx context.Context, _ store.ReceiptJob) (Draft, error) {
	close(b.started)
	<-ctx.Done()
	return Draft{}, ctx.Err()
}

// TestShutdownHandsTheJobBackWithoutSpendingAnAttempt: a deploy is not the
// receipt's fault, so the job goes back in the queue, due immediately, with
// the attempt it was on given back -- and nobody is told anything failed.
func TestShutdownHandsTheJobBackWithoutSpendingAnAttempt(t *testing.T) {
	q := newQueueRig(t)
	id := q.enqueue("inflight.jpg")

	p := blockingProcessor{started: make(chan struct{})}
	w := New(q.store, p, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.processNext(ctx)
	}()

	<-p.started
	if n := q.attempts(id); n != 1 {
		t.Fatalf("attempts while in flight = %d, want 1", n)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("processNext did not return after its context was cancelled")
	}

	if state, cause := q.status(id); state != "queued" {
		t.Errorf("status = %q (%q), want queued: the job was stranded by shutdown", state, cause)
	}
	if n := q.attempts(id); n != 0 {
		t.Errorf("attempts = %d, want 0: a shutdown must not count against the receipt", n)
	}
	if ns := q.notifications(); len(ns) != 0 {
		t.Errorf("notifications = %+v, want none", ns)
	}

	// And it is claimable straight away by the next process.
	w2 := New(q.store, stubProcessor{draft: Draft{Amount: 500, Payee: "Shop"}}, time.Minute)
	if !w2.processNext(context.Background()) {
		t.Fatal("the handed-back job could not be claimed")
	}
	if state, _ := q.status(id); state != "done" {
		t.Errorf("status after the next start = %q, want done", state)
	}
}

// cancellingProcessor succeeds, but only after shutdown has begun.
type cancellingProcessor struct{ cancel context.CancelFunc }

func (c cancellingProcessor) Process(context.Context, store.ReceiptJob) (Draft, error) {
	c.cancel()
	return Draft{Amount: 1250, Payee: "Bakery"}, nil
}

// TestAReadingFinishedDuringShutdownIsStillRecorded: the outcome is written on
// a context of its own, so a reading that completed as the worker was stopped
// is saved rather than lost to an already-cancelled context.
func TestAReadingFinishedDuringShutdownIsStillRecorded(t *testing.T) {
	q := newQueueRig(t)
	id := q.enqueue("justintime.jpg")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := New(q.store, cancellingProcessor{cancel: cancel}, time.Minute)
	w.processNext(ctx)

	if state, _ := q.status(id); state != "done" {
		t.Errorf("status = %q, want done", state)
	}
	ns := q.notifications()
	if len(ns) != 1 || !strings.Contains(ns[0].Text, "12.50") {
		t.Errorf("notifications = %+v, want the reading announced", ns)
	}
}

// TestAJobInterruptedOnEveryAttemptIsReported: a job still in 'processing'
// with no attempts left is not requeued forever on each restart -- it is
// failed, and its owner is told.
func TestAJobInterruptedOnEveryAttemptIsReported(t *testing.T) {
	q := newQueueRig(t)
	id := q.enqueue("poison.jpg")
	if _, err := q.db.Exec(
		`UPDATE receipt_jobs SET status = 'processing', attempts = ? WHERE id = ?`,
		store.MaxJobAttempts, id); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	w := New(q.store, stubProcessor{draft: Draft{Amount: 100, Payee: "Shop"}}, time.Hour)
	go w.Run(ctx)
	defer w.Stop(2 * time.Second)
	defer cancel()

	// The notification is written after the job is failed, so wait for it
	// rather than for the status, or the check races the worker.
	var ns []store.Notification
	deadline := time.Now().Add(3 * time.Second)
	for len(ns) == 0 {
		if time.Now().After(deadline) {
			state, _ := q.status(id)
			t.Fatalf("nobody was told; status = %q", state)
		}
		time.Sleep(10 * time.Millisecond)
		ns = q.notifications()
	}

	if state, _ := q.status(id); state != "failed" {
		t.Errorf("status = %q, want failed", state)
	}
	if len(ns) != 1 || ns[0].Kind != "error" || !strings.Contains(ns[0].Text, "poison.jpg") {
		t.Errorf("notifications = %+v, want one error naming the receipt", ns)
	}
}
