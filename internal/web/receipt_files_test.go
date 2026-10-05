package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// storedReceipt writes a real file and queues it as a read receipt in sc.
func (r *testRig) storedReceipt(sc store.Scope, name string) (jobID int64, path string) {
	r.t.Helper()
	path = filepath.ToSlash(filepath.Join(r.uploadDir, name))
	if err := os.WriteFile(filepath.FromSlash(path), []byte("\xff\xd8\xff receipt"), 0o600); err != nil {
		r.t.Fatal(err)
	}
	jobID, err := r.store.EnqueueReceipt(context.Background(), sc, path, name)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.store.CompleteReceiptJob(context.Background(), jobID, nil); err != nil {
		r.t.Fatal(err)
	}
	return jobID, path
}

// TestDeletingAnExpenseDeletesItsReceipt: the file must not outlive the entry,
// and the receipt must not reappear as "waiting" once its expense is gone.
func TestDeletingAnExpenseDeletesItsReceipt(t *testing.T) {
	rig := newRig(t)
	rig.login()
	jobID, path := rig.storedReceipt(rig.scope, "shop.jpg")

	if rec := rig.post("/transactions/new", url.Values{
		"kind": {"expense"}, "label": {"Shop"}, "amount": {"12.00"},
		"date": {"2026-08-01"}, "receipt_job": {fmt.Sprint(jobID)},
	}); rec.Code != http.StatusSeeOther {
		t.Fatalf("create: %d", rec.Code)
	}
	var txID int64
	if err := rig.db.QueryRow(`SELECT id FROM transactions WHERE label = 'Shop'`).Scan(&txID); err != nil {
		t.Fatal(err)
	}

	if rec := rig.post(fmt.Sprintf("/transactions/%d/delete", txID), nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d", rec.Code)
	}
	if _, err := os.Stat(filepath.FromSlash(path)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the receipt file survived its expense: %v", err)
	}
	if waiting, _ := rig.store.UnattachedReceipts(context.Background(), rig.scope, 50); len(waiting) != 0 {
		t.Errorf("the deleted expense's receipt is back on the waiting list: %+v", waiting)
	}
}

// TestDeletingABudgetDeletesItsReceipts covers the shared-budget cascade.
func TestDeletingABudgetDeletesItsReceipts(t *testing.T) {
	rig := newRig(t)
	rig.login()
	if rec := rig.post("/household", url.Values{"name": {"Trip"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("create: %d", rec.Code)
	}
	hh, err := rig.store.ActiveHousehold(context.Background(), store.User{ID: rig.userID})
	if err != nil {
		t.Fatal(err)
	}
	_, path := rig.storedReceipt(store.Scope{HouseholdID: hh.ID, UserID: rig.userID}, "trip.jpg")
	_, kept := rig.storedReceipt(rig.scope, "mine.jpg")

	if rec := rig.post("/household/delete", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete household: %d", rec.Code)
	}
	if _, err := os.Stat(filepath.FromSlash(path)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the deleted budget's receipt survived: %v", err)
	}
	if _, err := os.Stat(filepath.FromSlash(kept)); err != nil {
		t.Errorf("a receipt in another budget was deleted: %v", err)
	}
}
