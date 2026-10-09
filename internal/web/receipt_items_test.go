package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/store"
)

var (
	itemDescRE = regexp.MustCompile(`name="item_description"[^>]*value="([^"]*)"`)
	itemCatRE  = regexp.MustCompile(`name="item_category"[^>]*value="([^"]*)"`)
	itemAmtRE  = regexp.MustCompile(`name="item_amount"[^>]*value="([^"]*)"`)
	amountRE   = regexp.MustCompile(`id="amount" name="amount"[^>]*value="([^"]*)"`)
)

// TestAReadReceiptSavesAsPrefilled walks the path every scanned receipt takes:
// open the prefilled form and press Save without touching anything. The items
// read off a receipt never include tax, and here one price was unreadable and
// one line is a coupon -- the form must still save on the first try.
func TestAReadReceiptSavesAsPrefilled(t *testing.T) {
	rig := newRig(t)
	rig.login()
	jobID := rig.waitingReceipt("tesco.png")

	// Stored the way an older version saved it: not yet balanced.
	if err := rig.store.SaveReceiptDraft(context.Background(), jobID, store.ReceiptDraft{
		Merchant: "Tesco", Category: "Groceries", Date: "2026-08-01",
		Total: 1030, Subtotal: 950, Tax: 80,
		Items: []store.DraftItem{
			{Description: "Milk", Amount: 400},
			{Description: "Bread", Amount: 650},
			{Description: "Coupon", Amount: -100},
			{Description: "Smudged line", Amount: 0},
		},
	}); err != nil {
		t.Fatal(err)
	}

	page := rig.do("GET", fmt.Sprintf("/transactions/new?type=expense&receipt=%d", jobID), nil).Body.String()
	descs := itemDescRE.FindAllStringSubmatch(page, -1)
	cats := itemCatRE.FindAllStringSubmatch(page, -1)
	amts := itemAmtRE.FindAllStringSubmatch(page, -1)
	amount := amountRE.FindStringSubmatch(page)
	if amount == nil || len(descs) == 0 || len(descs) != len(amts) || len(cats) != len(amts) {
		t.Fatalf("could not read the prefilled form: amount=%v rows=%d/%d/%d", amount, len(descs), len(cats), len(amts))
	}

	form := url.Values{
		"kind": {"expense"}, "label": {"Groceries"}, "amount": {amount[1]},
		"date": {"2026-08-01"}, "receipt_job": {fmt.Sprint(jobID)},
	}
	for i := range amts {
		form.Add("item_description", descs[i][1])
		form.Add("item_category", cats[i][1])
		form.Add("item_amount", amts[i][1])
	}
	if got := amts[3][1]; got != "" {
		t.Errorf("an unreadable price is prefilled as %q; it must be blank", got)
	}

	rec := rig.post("/transactions/new", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("the prefilled form was refused (%d): %s", rec.Code, extractError(rec.Body.String()))
	}

	var n int
	var sum int64
	if err := rig.db.QueryRow(`
		SELECT COUNT(*), SUM(li.amount_cents) FROM line_items li
		JOIN transactions t ON t.id = li.transaction_id WHERE t.label = 'Groceries'`).Scan(&n, &sum); err != nil {
		t.Fatal(err)
	}
	// Milk, Bread, Coupon and the added Tax line; the smudged line has no
	// price to store.
	if n != 4 || sum != 1030 {
		t.Errorf("stored %d line(s) totalling %d, want 4 totalling 1030", n, sum)
	}
}

func TestZeroLineItemIsRefusedButDiscountIsAccepted(t *testing.T) {
	if _, msg := parseLineItems([]formItem{{Amount: "0.00"}}, 100); msg == "" {
		t.Error("a zero line item was accepted")
	}
	items, msg := parseLineItems([]formItem{{Amount: "3.00"}, {Amount: "-2.00"}}, 100)
	if msg != "" || len(items) != 2 {
		t.Errorf("a discount line was refused: %q", msg)
	}
}
