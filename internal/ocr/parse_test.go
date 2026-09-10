package ocr

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jthomasw/YABA-2026/internal/money"
)

// The testdata files are real tesseract output, captured by running the OCR
// engine over generated receipt images at three degradation levels: a clean
// scan, a decent phone photo and a poor one. They are checked in rather than
// produced at test time so the suite needs no tesseract binary and cannot start
// failing because a different tesseract version reads a character differently.
//
// The misreadings in them are genuine. pharmacy_l2 really does contain
// "BALANCE DUE 8.00" where the receipt said 0.00, and that is precisely the
// case the total picker has to survive.

type expectation struct {
	Name     string          `json:"name"`
	Level    int             `json:"level"`
	Merchant string          `json:"merchant"`
	Date     string          `json:"date"`
	Total    money.Cents     `json:"total"`
	Subtotal money.Cents     `json:"subtotal"`
	Tax      money.Cents     `json:"tax"`
	Items    [][]interface{} `json:"items"`
}

func loadExpectations(t *testing.T) []expectation {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "expected.json"))
	if err != nil {
		t.Fatalf("read expectations: %v", err)
	}
	var out []expectation
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode expectations: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("no expectations loaded")
	}
	return out
}

// TestParseRealOCR is the test that matters: every sample must yield the right
// total. The total is the one number that becomes money in the ledger, so it is
// held to a higher standard than the merchant or the item list.
func TestParseRealOCR(t *testing.T) {
	for _, want := range loadExpectations(t) {
		t.Run(want.Name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", want.Name+".txt"))
			if err != nil {
				t.Fatalf("read sample: %v", err)
			}
			got := Parse(string(b), 0)

			if got.Total != want.Total {
				t.Errorf("total = %s, want %s\n--- ocr ---\n%s",
					got.Total.Display(), want.Total.Display(), b)
			}
			if want.Date != "" && got.Date != want.Date {
				t.Errorf("date = %q, want %q", got.Date, want.Date)
			}
			if want.Merchant != "" && got.Merchant == "" {
				t.Errorf("merchant not found, want something like %q", want.Merchant)
			}
			if got.Total > 0 && got.Confidence <= 0 {
				t.Errorf("a total was found but confidence is %v", got.Confidence)
			}
		})
	}
}

// TestItemsNeverFallShort guards what the transaction form depends on. Every
// product line read is now handed back, because a user who photographed ten
// things wants to see ten things -- but a set that falls short of the total is
// a set SetLineItems refuses, so the parser must never hand one back. It closes
// any shortfall with a balancing line instead.
//
// Overshooting is the one case left unresolved: a line item cannot be negative,
// so nothing can be added to bring the sum down, and the form asks the user to
// fix it. That is a visible, explicable state rather than a silent one.
func TestItemsNeverFallShort(t *testing.T) {
	for _, want := range loadExpectations(t) {
		t.Run(want.Name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", want.Name+".txt"))
			if err != nil {
				t.Fatalf("read sample: %v", err)
			}
			got := Parse(string(b), 0)
			if len(got.Items) == 0 {
				return // offering nothing is always allowed
			}
			var sum money.Cents
			for _, it := range got.Items {
				if it.Amount <= 0 {
					t.Errorf("item %q has non-positive amount %s", it.Description, it.Amount.Display())
				}
				if it.Description == "" {
					t.Errorf("item with amount %s has no description", it.Amount.Display())
				}
				sum += it.Amount
			}

			if got.Total <= 0 {
				return // nothing to reconcile against
			}
			if sum < got.Total {
				t.Errorf("items sum to %s, short of the total %s, and were not balanced;"+
					" they would be rejected on save", sum.Display(), got.Total.Display())
			}
			if got.ItemsBalanced && sum != got.Total {
				t.Errorf("balanced items sum to %s but the total is %s; balancing must be exact",
					sum.Display(), got.Total.Display())
			}
		})
	}
}

// TestBalancingLine drives the three outcomes directly, because the real samples
// happen to reconcile and would never exercise the interesting paths.
func TestBalancingLine(t *testing.T) {
	// Two items and a total three dollars above them: the third thing on the
	// receipt was cut off, misread, or is a bag charge.
	short := Parse(strings.Join([]string{
		"CORNER MARKET",
		"MILK 2.50",
		"BREAD 3.00",
		"TOTAL 8.50",
	}, "\n"), 0)

	if !short.ItemsBalanced {
		t.Fatalf("items short of the total were not balanced: %+v", short.Items)
	}
	if len(short.Items) != 3 {
		t.Fatalf("got %d items, want the 2 read plus 1 balancing line", len(short.Items))
	}
	last := short.Items[len(short.Items)-1]
	if last.Description != BalancingItemDescription {
		t.Errorf("balancing line is described as %q, want %q",
			last.Description, BalancingItemDescription)
	}
	if last.Amount != money.Cents(300) {
		t.Errorf("balancing line is %s, want $3.00", last.Amount.Display())
	}
	if sum := sumItems(short.Items); sum != short.Total {
		t.Errorf("balanced items sum to %s, want the total %s", sum.Display(), short.Total.Display())
	}

	// Items that already add up are left alone: no invented line, no flag.
	exact := Parse(strings.Join([]string{
		"CORNER MARKET",
		"MILK 2.50",
		"BREAD 3.00",
		"TOTAL 5.50",
	}, "\n"), 0)

	if exact.ItemsBalanced {
		t.Error("items that reconcile were flagged as balanced")
	}
	for _, it := range exact.Items {
		if it.Description == BalancingItemDescription {
			t.Error("a balancing line was added to items that already reconciled")
		}
	}

	// The user's actual case: ten things on one receipt, one price misread, and
	// all ten must still reach the form.
	var lines []string
	lines = append(lines, "BIG SHOP")
	for i := 1; i <= 10; i++ {
		lines = append(lines, fmt.Sprintf("ITEM NUMBER %d 1.00", i))
	}
	lines = append(lines, "TOTAL 14.00")
	ten := Parse(strings.Join(lines, "\n"), 0)

	if len(ten.Items) != 11 {
		t.Fatalf("got %d items, want the 10 read plus 1 balancing line: %+v",
			len(ten.Items), ten.Items)
	}
	if sum := sumItems(ten.Items); sum != ten.Total {
		t.Errorf("items sum to %s, want the total %s", sum.Display(), ten.Total.Display())
	}
}

// TestItemsWithoutTotal covers a receipt whose total was creased away: there is
// nothing to reconcile against, so the items stand as read and nothing is
// invented to pad them out to a number nobody knows.
func TestItemsWithoutTotal(t *testing.T) {
	got := Parse(strings.Join([]string{
		"CORNER MARKET",
		"MILK 2.50",
		"BREAD 3.00",
	}, "\n"), 0)

	if got.ItemsBalanced {
		t.Error("items were balanced against a total that was never found")
	}
	for _, it := range got.Items {
		if it.Description == BalancingItemDescription {
			t.Error("a balancing line was invented without a total to balance to")
		}
	}
}

// TestTotalPickerAdversarial covers the shapes that broke a size-based picker.
func TestTotalPickerAdversarial(t *testing.T) {
	cases := []struct {
		name string
		text string
		want money.Cents
	}{
		{
			// Cash tendered exceeds the total, and change is printed below it.
			name: "cash larger than total",
			text: "AMOUNT DUE 58.79\nCASH 60.00\nCHANGE 1.21",
			want: 5879,
		},
		{
			// A settled account prints a zero balance under the real total.
			name: "zero balance below real total",
			text: "Subtotal 28.77\nTax 2.16\nTotal 30.93\nBALANCE DUE 0.00",
			want: 3093,
		},
		{
			// Same, but OCR misread the zero as an eight. Rank must still win.
			name: "misread balance below real total",
			text: "Subtotal 28.77\nIl Tax 2.16\nTotal 30.93\nBALANCE DUE 8.00",
			want: 3093,
		},
		{
			// Fuel: three-decimal prices and a comma-grouped quantity must not
			// be read as money at all.
			name: "fuel quantities are not money",
			text: "GALLONS 11,482\nPRICE/GAL 3.399\nTOTAL $39.03",
			want: 3903,
		},
		{
			name: "balance due is the only total",
			text: "Food & Bev 41.45\nTax 4.25\nBALANCE DUE 45.70",
			want: 4570,
		},
		{
			name: "grand total outranks total",
			text: "TOTAL 10.00\nGRAND TOTAL 12.50",
			want: 1250,
		},
		{
			name: "subtotal is never the total",
			text: "SUBTOTAL 42.80\nTAX 4.39",
			want: 4719, // derived: subtotal + tax
		},
		{
			name: "tip is not the total",
			text: "Subtotal 10.00\nTax 0.88\nTip 2.00\nTOTAL $12.88",
			want: 1288,
		},
		{
			name: "thousands separator",
			text: "TOTAL $1,234.56",
			want: 123456,
		},
		{
			name: "card number is not an amount",
			text: "VISA ************4412\nTOTAL 47.19",
			want: 4719,
		},
		{
			name: "no total at all",
			text: "THANK YOU FOR SHOPPING\nHAVE A NICE DAY",
			want: 0,
		},
		{
			name: "running total repeated, last wins",
			text: "TOTAL 10.00\nTOTAL 25.50",
			want: 2550,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Parse(c.text, 0)
			if got.Total != c.want {
				t.Errorf("total = %s, want %s", got.Total.Display(), c.want.Display())
			}
		})
	}
}

func TestParseAmountsRejectsNonMoney(t *testing.T) {
	cases := []struct {
		in   string
		want []money.Cents
	}{
		{"PRICE/GAL 3.399", nil},
		{"GALLONS 11.482", nil},
		{"VERSION 1.2.3", nil},
		{"TOTAL 47.19", []money.Cents{4719}},
		{"TOTAL $1,234.56", []money.Cents{123456}},
		{"BAD 1,2345.00", nil},
		{"REFUND -12.50", []money.Cents{-1250}},
		{"TWO 1.00 AND 2.00", []money.Cents{100, 200}},
		{"TIME 08:14", nil},
		{"CARD 4111111111111111.00", nil},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			var got []money.Cents
			for _, a := range parseAmounts(c.in) {
				got = append(got, a.value)
			}
			if len(got) != len(c.want) {
				t.Fatalf("parseAmounts(%q) = %v, want %v", c.in, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("parseAmounts(%q)[%d] = %s, want %s",
						c.in, i, got[i].Display(), c.want[i].Display())
				}
			}
		})
	}
}

func TestFindDate(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct{ in, want string }{
		{"03/14/2026 14:22", "2026-03-14"},
		{"Date: 2026-02-28 Time: 08:14", "2026-02-28"},
		{"SALE 14 JAN 2026 16:45", "2026-01-14"},
		{"12/05/25 10:02 aM", "2025-12-05"},
		{"Mar 2, 2026 7:38 PM", "2026-03-02"},
		{"January 02, 2026", "2026-01-02"},
		{"(312) 555-0142", ""},    // a phone number is not a date
		{"Store 0421 Reg 03", ""}, // nor is a register number
		{"01/02/2099", ""},        // nor is a date far in the future
		{"01/02/1970", ""},        // nor one from before 2000
		{"no date here at all", ""},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := findDate(c.in, now); got != c.want {
				t.Errorf("findDate(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestMerchantAndCategory(t *testing.T) {
	cases := []struct{ text, merchant, category string }{
		{"WHOLE FOODS MARKET\n1234 Lake Street\nTOTAL 10.00", "WHOLE FOODS MARKET", "Groceries"},
		{"The Daily Grind\n88 Michigan Ave\nTOTAL 5.00", "The Daily Grind", "Coffee"},
		{"SHELL\n2200 W Fullerton Ave\nGALLONS 11.482", "SHELL", "Fuel"},
		{"ACE HARDWARE #2213\nwww.acehardware.com", "ACE HARDWARE #2213", "Household"},
		{"WALGREENS\nSTORE #4471 PHARMACY", "WALGREENS", "Pharmacy"},
		// The address must never be taken for the shop name.
		{"1234 Lake Street\nWHOLE FOODS MARKET", "WHOLE FOODS MARKET", "Groceries"},
	}
	for _, c := range cases {
		t.Run(c.merchant, func(t *testing.T) {
			got := Parse(c.text, 0)
			if got.Merchant != c.merchant {
				t.Errorf("merchant = %q, want %q", got.Merchant, c.merchant)
			}
			if got.Category != c.category {
				t.Errorf("category = %q, want %q", got.Category, c.category)
			}
		})
	}
}

// TestConfidenceOrdering checks the score behaves monotonically: a receipt whose
// numbers cross-check must never score below one whose numbers do not.
func TestConfidenceOrdering(t *testing.T) {
	full := Parse("WHOLE FOODS\n03/14/2026\nSUBTOTAL 42.80\nTAX 4.39\nTOTAL 47.19", 0)
	totalOnly := Parse("TOTAL 47.19", 0)
	nothing := Parse("THANK YOU FOR SHOPPING", 0)

	if !full.Reconciles() {
		t.Error("a receipt whose parts add up should reconcile")
	}
	if full.Confidence <= totalOnly.Confidence {
		t.Errorf("reconciling receipt scored %.2f, bare total scored %.2f",
			full.Confidence, totalOnly.Confidence)
	}
	if totalOnly.Confidence <= nothing.Confidence {
		t.Errorf("a total scored %.2f, no total scored %.2f",
			totalOnly.Confidence, nothing.Confidence)
	}
	if nothing.Confidence > 0.35 {
		t.Errorf("a receipt with no total scored %.2f; it should be capped", nothing.Confidence)
	}
	if nothing.HasTotal() {
		t.Error("HasTotal should be false when nothing was read")
	}
}

// TestParseNeverPanics feeds the parser the kind of debris a bad photo produces.
func TestParseNeverPanics(t *testing.T) {
	junk := []string{
		"", "\n\n\n", "....", "$", ".00", "0.00", "-", "\x00\x01",
		"999999999999999999999999.99", "1,,,,.00", "TOTAL", "TOTAL $",
		"总计 47.19", "\n\t \n", "aaaa" + string(make([]byte, 4096)),
	}
	for _, j := range junk {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					t.Errorf("Parse(%q) panicked: %v", j, rec)
				}
			}()
			_ = Parse(j, 0.5)
		}()
	}
}
