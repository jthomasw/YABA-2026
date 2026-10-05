package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// TestStatusFollowsAReceiptStillBeingRead: the upload page polls a receipt
// from the moment it is queued. The status endpoint must report it as queued,
// not as "Already entered" -- which is what it would say if it used the
// settled-only lookup the expense form uses.
func TestStatusFollowsAReceiptStillBeingRead(t *testing.T) {
	rig := newRig(t)
	rig.login()

	jobID, err := rig.store.EnqueueReceipt(context.Background(), rig.scope,
		"uploads/test/queued.png", "queued.png")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	rec := rig.do("GET", fmt.Sprintf("/receipts/%d/status", jobID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got receiptStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if got.Status != string(store.JobQueued) || got.Done {
		t.Errorf("queued receipt reported as %+v, want status %q and not done", got, store.JobQueued)
	}

	// The upload page itself must agree to watch it.
	page := rig.do("GET", fmt.Sprintf("/expense?receipt=%d", jobID), nil).Body.String()
	if !strings.Contains(page, fmt.Sprintf(`data-status-url="/receipts/%d/status"`, jobID)) {
		t.Error("the expense page does not watch a receipt that is still queued")
	}
}

// TestFailedReceiptIsListedAndCanBeEntered: a receipt the reader gave up on
// stays on /receipts with a plain explanation (never the internal cause), and
// its Enter details link opens the form for it.
func TestFailedReceiptIsListedAndCanBeEntered(t *testing.T) {
	rig := newRig(t)
	rig.login()
	ctx := context.Background()

	jobID, err := rig.store.EnqueueReceipt(ctx, rig.scope, "uploads/test/blurred.png", "blurred.png")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	const cause = "gemini: secret internal cause"
	for i := 0; i < store.MaxJobAttempts; i++ {
		if _, err := rig.store.ClaimReceiptJob(ctx); err != nil {
			t.Fatalf("claim %d: %v", i+1, err)
		}
		if _, err := rig.store.RetryOrFailReceiptJob(ctx, jobID, cause, time.Now()); err != nil {
			t.Fatalf("retry or fail %d: %v", i+1, err)
		}
	}

	body := rig.do("GET", "/receipts", nil).Body.String()
	if !strings.Contains(body, "blurred.png") {
		t.Fatal("a failed receipt is not listed on /receipts")
	}
	if !strings.Contains(body, "could not be read automatically") {
		t.Error("the failed receipt is not explained")
	}
	if strings.Contains(body, cause) {
		t.Error("the worker's internal error leaked into the page")
	}

	rec := rig.do("GET", fmt.Sprintf("/transactions/new?type=expense&receipt=%d", jobID), nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), fmt.Sprintf(`name="receipt_job" value="%d"`, jobID)) {
		t.Errorf("the failed receipt cannot be entered by hand (status %d)", rec.Code)
	}
}
