package store

import "testing"

func item(d string, c Cents) DraftItem      { return DraftItem{Description: d, Amount: c} }
func addedItem(d string, c Cents) DraftItem { return DraftItem{Description: d, Amount: c, Added: true} }

func TestBalancedAddsTaxTipAndOther(t *testing.T) {
	d := ReceiptDraft{Total: 1500, Tax: 80, Tip: 200,
		Items: []DraftItem{item("Milk", 400), item("Bread", 600), item("Smudged", 0)}}
	b := d.Balanced()
	if b.ItemsTotal() != b.Total || !b.ItemsBalanced {
		t.Fatalf("not balanced: %+v", b.Items)
	}
	want := []DraftItem{item("Milk", 400), item("Bread", 600), item("Smudged", 0),
		addedItem(DraftLineTax, 80), addedItem(DraftLineTip, 200), addedItem(DraftLineOther, 220)}
	if len(b.Items) != len(want) {
		t.Fatalf("items = %+v, want %+v", b.Items, want)
	}
	for i := range want {
		if b.Items[i] != want[i] {
			t.Errorf("item %d = %+v, want %+v", i, b.Items[i], want[i])
		}
	}
	if len(d.Items) != 3 {
		t.Error("Balanced modified the original draft's items")
	}
	if b.ReadItemCount() != 3 {
		t.Errorf("ReadItemCount = %d, want 3 (added lines are not read items)", b.ReadItemCount())
	}
}

// TestBalancedListsEachPrintedTaxLine is the Walmart receipt: two tax lines
// with an amount and one at 0%, and items that add up to the subtotal.
func TestBalancedListsEachPrintedTaxLine(t *testing.T) {
	d := ReceiptDraft{Total: 7353, Subtotal: 7045, Tax: 308,
		Items: []DraftItem{item("Groceries", 7045)},
		TaxLines: []DraftItem{
			item("TAX 1 5.5 %", 284), item("TAX 12 0 %", 0), item("TAX 4 8 %", 24),
		}}
	b := d.Balanced()
	want := []DraftItem{item("Groceries", 7045),
		addedItem("TAX 1 5.5 %", 284), addedItem("TAX 4 8 %", 24)}
	if len(b.Items) != len(want) {
		t.Fatalf("items = %+v, want %+v", b.Items, want)
	}
	for i := range want {
		if b.Items[i] != want[i] {
			t.Errorf("item %d = %+v, want %+v", i, b.Items[i], want[i])
		}
	}
	if b.ItemsTotal() != 7353 {
		t.Errorf("items total %d, want 7353", b.ItemsTotal())
	}
}

func TestBalancedLabelsAnUnlabelledTaxLine(t *testing.T) {
	d := ReceiptDraft{Total: 1100, Items: []DraftItem{item("Food", 1000)},
		TaxLines: []DraftItem{item("", 60), item("State 4%", 40)}}
	b := d.Balanced()
	if b.Items[1].Description != "Tax" || b.Items[2].Description != "Tax (State 4%)" {
		t.Errorf("items = %+v", b.Items)
	}
}

func TestBalancedDoesNotDoubleCountListedTax(t *testing.T) {
	// The model already listed tax as an item: no second Tax line.
	d := ReceiptDraft{Total: 1080, Tax: 80, Items: []DraftItem{item("Food", 1000), item("Tax", 80)}}
	if b := d.Balanced(); len(b.Items) != 2 || b.ItemsBalanced {
		t.Errorf("already balanced draft changed: %+v", b.Items)
	}
}

func TestBalancedAddsNegativeAdjustmentForUnreadDiscount(t *testing.T) {
	d := ReceiptDraft{Total: 900, Items: []DraftItem{item("Shoes", 1000)}}
	b := d.Balanced()
	last := b.Items[len(b.Items)-1]
	if last.Description != DraftLineAdjustment || last.Amount != -100 || b.ItemsTotal() != 900 {
		t.Errorf("items = %+v", b.Items)
	}
	if again := b.Balanced(); len(again.Items) != len(b.Items) {
		t.Error("Balanced is not idempotent")
	}
}

func TestBalancedLeavesDraftsWithoutItemsOrTotalAlone(t *testing.T) {
	for _, d := range []ReceiptDraft{{Total: 500}, {Items: []DraftItem{item("x", 100)}}} {
		if b := d.Balanced(); b.ItemsBalanced || len(b.Items) != len(d.Items) {
			t.Errorf("Balanced(%+v) = %+v", d, b)
		}
	}
}
