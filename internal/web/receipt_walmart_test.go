package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// walmartDraft is the Walmart receipt as the worker stores it: sixteen items,
// the printed tax lines, and the balancing applied when it is saved.
func walmartDraft() store.ReceiptDraft {
	items := []store.DraftItem{}
	for _, it := range []struct {
		d string
		c store.Cents
	}{
		{"STRAWB DONU", 298}, {"SBUX SLCR 1", 897}, {"DOWNY", 797}, {"GRANULATED", 324},
		{"GV FLOUR", 132}, {"MM 6.3LB", 984}, {"LAC GEL LLA", 198}, {"WM SEA SALT", 223},
		{"SLVR GLITTE", 124}, {"PNK PASTEL", 124}, {"GRN MONSTER", 244}, {"ELF SHADOW", 300},
		{"RED CREAM M", 57}, {"CRAYON", 99}, {"LIP STICK", 100}, {"CLOWN", 2144},
	} {
		items = append(items, store.DraftItem{Description: it.d, Amount: it.c})
	}
	return store.ReceiptDraft{
		Merchant: "Walmart", Category: "Groceries", Date: "2023-10-31",
		Total: 7353, Subtotal: 7045, Tax: 308, Confidence: 0.97, Items: items,
		TaxLines: []store.DraftItem{
			{Description: "TAX 1 5.5 %", Amount: 284},
			{Description: "TAX 12 0 %", Amount: 0},
			{Description: "TAX 4 8 %", Amount: 24},
		},
	}.Balanced()
}

// TestTheWalmartReceiptShowsEveryItemAndSavesAsIs: all sixteen products and
// both tax lines are on the form, and pressing Save without touching anything
// stores a breakdown that adds up to the $73.53 paid.
func TestTheWalmartReceiptShowsEveryItemAndSavesAsIs(t *testing.T) {
	rig := newRig(t)
	rig.login()
	jobID := rig.waitingReceipt("walmart.png")
	if err := rig.store.SaveReceiptDraft(context.Background(), jobID, walmartDraft()); err != nil {
		t.Fatal(err)
	}

	page := rig.do("GET", fmt.Sprintf("/transactions/new?type=expense&receipt=%d", jobID), nil).Body.String()
	descs := itemDescRE.FindAllStringSubmatch(page, -1)
	cats := itemCatRE.FindAllStringSubmatch(page, -1)
	amts := itemAmtRE.FindAllStringSubmatch(page, -1)
	if len(descs) != 18 || len(amts) != 18 {
		t.Fatalf("the form shows %d rows, want 16 items + 2 tax lines", len(descs))
	}
	for _, want := range []string{"STRAWB DONU", "CLOWN", "TAX 1 5.5 %", "TAX 4 8 %"} {
		if !strings.Contains(page, `value="`+want+`"`) {
			t.Errorf("the form is missing %q", want)
		}
	}
	if strings.Contains(page, `value="TAX 12 0 %"`) {
		t.Error("the 0% tax line was added as a row; it charges nothing")
	}
	if strings.Count(page, "item-row-added") != 2 {
		t.Error("the two tax rows are not marked as added")
	}
	amount := amountRE.FindStringSubmatch(page)
	if amount == nil || amount[1] != "73.53" {
		t.Fatalf("amount prefilled as %v, want 73.53", amount)
	}

	form := url.Values{
		"kind": {"expense"}, "label": {"Groceries"}, "amount": {amount[1]},
		"date": {"2023-10-31"}, "receipt_job": {fmt.Sprint(jobID)},
	}
	for i := range amts {
		form.Add("item_description", descs[i][1])
		form.Add("item_category", cats[i][1])
		form.Add("item_amount", amts[i][1])
	}
	if rec := rig.post("/transactions/new", form); rec.Code != http.StatusSeeOther {
		t.Fatalf("the prefilled form was refused (%d): %s", rec.Code, extractError(rec.Body.String()))
	}

	var n int
	var sum int64
	if err := rig.db.QueryRow(`SELECT COUNT(*), IFNULL(SUM(amount_cents), 0) FROM line_items`).Scan(&n, &sum); err != nil {
		t.Fatal(err)
	}
	if n != 18 || sum != 7353 {
		t.Errorf("stored %d line(s) totalling %d, want 18 totalling 7353", n, sum)
	}
}

// TestStatusSaysWhenReadingIsUnavailable: the progress card must not blame
// the photo when the server has no reader.
func TestStatusSaysWhenReadingIsUnavailable(t *testing.T) {
	rig := newRig(t)
	rig.login()
	jobID, err := rig.store.EnqueueReceipt(context.Background(), rig.scope, "uploads/x.png", "x.png")
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.store.CompleteReceiptJobUnread(context.Background(), jobID, store.UnreadReaderUnavailable); err != nil {
		t.Fatal(err)
	}
	var st receiptStatus
	json.Unmarshal(rig.do("GET", fmt.Sprintf("/receipts/%d/status", jobID), nil).Body.Bytes(), &st)
	if !st.Done || !strings.Contains(st.Detail, "isn't available") || strings.Contains(st.Detail, "could be read") {
		t.Errorf("status = %+v", st)
	}
}

// TestUploadPanelWarnsWhenReadingIsOff: say it before the upload, not after.
func TestUploadPanelWarnsWhenReadingIsOff(t *testing.T) {
	rig := newRig(t) // the rig's server has no reader configured
	rig.login()
	if page := rig.do("GET", "/expense", nil).Body.String(); !strings.Contains(page, "Automatic reading is off") {
		t.Error("the upload panel does not say reading is off")
	}
}
