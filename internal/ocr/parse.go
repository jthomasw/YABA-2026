// Package ocr turns a photographed receipt into a draft expense.
//
// It is deliberately split in two. Everything in this file is pure text
// processing with no dependency beyond the standard library and internal/money,
// so the hard part -- deciding which number on a receipt is the total -- can be
// tested exhaustively without an image, a temporary file or the tesseract
// binary. The parts that shell out live in tesseract.go and preprocess.go.
package ocr

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jthomasw/YABA-2026/internal/money"
)

// Item is one product line read off a receipt.
type Item struct {
	Description string
	Amount      money.Cents
}

// Receipt is everything the parser managed to recover. Every field is optional:
// a receipt photographed badly enough yields a Receipt with nothing but Text,
// and that is a valid result rather than an error -- the user is asked to fill
// in the rest, which is what would have happened anyway without OCR.
type Receipt struct {
	Merchant string
	Date     string // YYYY-MM-DD, or "" when no date could be read
	Total    money.Cents
	Subtotal money.Cents
	Tax      money.Cents
	Tip      money.Cents
	Items    []Item

	// Category is a guess at the expense label, from the merchant name.
	Category string

	// Confidence is 0..1, combining how sure tesseract was about the characters
	// with how well the numbers hang together arithmetically. It is shown to the
	// user as a hint about how carefully to check, and never used to skip the
	// confirmation step.
	Confidence float64

	// Reasons records how Confidence was arrived at, for the log. A receipt that
	// parsed badly is much easier to diagnose from these than from the score.
	Reasons []string

	// Text is the raw OCR output, kept so a user can see what was actually read
	// and so a parser bug can be reproduced from the database alone.
	Text string
}

// HasTotal reports whether an amount worth prefilling was found.
func (r Receipt) HasTotal() bool { return r.Total > 0 }

// Reconciles reports whether the parts add up to the whole. When they do, the
// total is almost certainly right: three independently OCR'd numbers agreeing by
// chance is vanishingly unlikely.
func (r Receipt) Reconciles() bool {
	if r.Total <= 0 || r.Subtotal <= 0 {
		return false
	}
	return abs(r.Subtotal+r.Tax+r.Tip-r.Total) <= reconcileSlack
}

// reconcileSlack absorbs a rounding cent, and no more. A wider tolerance would
// start accepting genuinely wrong readings.
const reconcileSlack = money.Cents(2)

func abs(c money.Cents) money.Cents {
	if c < 0 {
		return -c
	}
	return c
}

// ── money on a line ───────────────────────────────────────────────────────────

// moneyRe finds anything shaped like an amount. The fractional part is matched
// greedily and checked afterwards rather than pinned to two digits here, because
// RE2 has no lookahead: without the check, a fuel price of 3.399 would match its
// first two decimals and be read as $3.39.
var moneyRe = regexp.MustCompile(`(-?)\$?\s?(\d[\d,]*)\.(\d+)`)

// parseAmounts returns every genuine money value on a line, with the byte offset
// each began at so the label can be taken from the text in front of it.
type amountAt struct {
	value money.Cents
	start int
	end   int
}

func parseAmounts(line string) []amountAt {
	var out []amountAt
	for _, m := range moneyRe.FindAllStringSubmatchIndex(line, -1) {
		neg := line[m[2]:m[3]] == "-"
		whole := line[m[4]:m[5]]
		frac := line[m[6]:m[7]]

		// Exactly two decimal places, or it is not a price. This one rule
		// rejects fuel prices (3.399), litre and gallon quantities (11.482)
		// and version-like noise, all of which sit next to real amounts on
		// real receipts.
		if len(frac) != 2 {
			continue
		}

		// A comma may only group thousands. "11,482" never reaches here (no
		// decimal part) but "1,2345.00" would, and it is not a number anyone
		// meant to write.
		if !validThousands(whole) {
			continue
		}

		digits := strings.ReplaceAll(whole, ",", "")
		// A receipt line with a fifteen-digit number on it is a card number or a
		// barcode that happened to acquire a dot, not a price.
		if len(digits) > 9 {
			continue
		}

		w, err := strconv.ParseInt(digits, 10, 64)
		if err != nil {
			continue
		}
		f, err := strconv.ParseInt(frac, 10, 64)
		if err != nil {
			continue
		}

		v := money.Cents(w*100 + f)
		if neg {
			v = -v
		}
		out = append(out, amountAt{value: v, start: m[0], end: m[1]})
	}
	return out
}

// validThousands checks a comma-grouped integer is grouped in threes.
func validThousands(s string) bool {
	if !strings.Contains(s, ",") {
		return true
	}
	parts := strings.Split(s, ",")
	if len(parts[0]) < 1 || len(parts[0]) > 3 {
		return false
	}
	for _, p := range parts[1:] {
		if len(p) != 3 {
			return false
		}
	}
	return true
}

// ── labels ────────────────────────────────────────────────────────────────────

// normalise reduces a label to comparable letters: lowercase, no punctuation,
// single spaces. "SALES TAX:" and "Sales  Tax" both become "sales tax", and the
// stray bracket tesseract puts in front of "(CHANGE" disappears.
func normalise(s string) string {
	var b strings.Builder
	lastSpace := true
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
			lastSpace = false
		case r == ' ' || r == '\t':
			if !lastSpace {
				b.WriteRune(' ')
				lastSpace = true
			}
		default:
			// Digits and punctuation are dropped, not turned into spaces, so
			// "2 x AA BATTERY 4PK" normalises to "x aa battery pk" and still
			// reads as a product rather than as a keyword.
		}
	}
	return strings.TrimSpace(b.String())
}

// containsWord reports whether phrase contains term as whole words, so "tax"
// matches "sales tax" but not "taxi".
func containsWord(phrase, term string) bool {
	if phrase == term {
		return true
	}
	return strings.HasPrefix(phrase, term+" ") ||
		strings.HasSuffix(phrase, " "+term) ||
		strings.Contains(phrase, " "+term+" ")
}

func containsAnyWord(phrase string, terms []string) bool {
	for _, t := range terms {
		if containsWord(phrase, t) {
			return true
		}
	}
	return false
}

// hasTerm reports whether a term list contains an exact entry.
func hasTerm(terms []string, want string) bool {
	for _, t := range terms {
		if t == want {
			return true
		}
	}
	return false
}

// totalTiers rank the labels that can name the amount actually charged. A
// receipt often carries several plausible candidates, so picking the largest
// number is not good enough: a hardware receipt shows CASH 60.00 against a
// 58.79 total, and a pharmacy receipt shows both "Total 30.93" and a
// "BALANCE DUE" of nil. Rank decides, and size never does.
var totalTiers = []struct {
	rank  int
	terms []string
}{
	{100, []string{"grand total", "total due", "amount due", "total amount",
		"total sale", "order total", "purchase total", "amount charged"}},
	{90, []string{"total"}},
	{80, []string{"balance due", "to pay", "amount", "balance", "due", "charged"}},
}

// neverTotal are labels whose amount is never what the expense cost. Note what
// is absent: "due" and "balance", which appear in tier 80 above.
var neverTotal = []string{
	"subtotal", "sub total", "sub", "tax", "vat", "gst", "hst", "pst",
	"tip", "gratuity", "service charge",
	"change", "cash", "tender", "tendered", "cash back", "cashback",
	"card", "credit", "debit", "visa", "mastercard", "amex", "discover",
	"auth", "approved", "ref", "reference", "account", "acct",
	"savings", "saved", "discount", "coupon", "points", "rewards",
	"gallons", "gal", "price", "unit", "qty", "quantity", "items", "item count",
	"previous", "prior", "forward", "deposit", "refund", "return",
}

// subtotalTerms name the pre-tax figure. Some receipts never use the word:
// a hardware shop says MERCHANDISE and a restaurant says "Food & Bev".
var subtotalTerms = []string{
	"subtotal", "sub total", "merchandise", "merch", "net", "net sale",
	"food bev", "food and bev", "items total", "item total", "goods",
}

var taxTerms = []string{"tax", "vat", "gst", "hst", "pst", "sales tax"}
var tipTerms = []string{"tip", "gratuity"}

// summaryTerms mark the end of the product list and the start of the arithmetic
// about it. This is deliberately NOT neverTotal: that list exists to stop a
// number being mistaken for the amount charged, and it is far too eager to be
// reused here. "gal" belongs in it, because a fuel quantity is not a total --
// but a carton of milk labelled "WHOLE MILK 1GAL" normalises to "whole milk
// gal", and reusing the list ended the item scan at the dairy aisle.
var summaryTerms = []string{
	"subtotal", "sub total", "merchandise", "merch", "net sale", "goods",
	"items total", "item total", "food bev", "food and bev",
	"tax", "vat", "gst", "hst", "pst",
	"tip", "gratuity", "service charge",
	"total", "grand total", "total due", "amount due", "total amount",
	"total sale", "order total", "purchase total", "amount charged",
	"balance due", "balance", "due", "to pay", "charged",
	"change", "cash", "tender", "tendered", "cash back", "cashback",
	"card", "credit", "debit", "visa", "mastercard", "amex", "discover",
	"auth", "approved", "savings", "saved", "discount", "coupon",
	"points", "rewards",
}

// ── the parse ─────────────────────────────────────────────────────────────────

// line is one OCR'd row, already split into a label and the amount at its end.
type line struct {
	raw    string
	label  string // normalised text before the amount
	amount money.Cents
	hasAmt bool
	index  int
}

// Parse turns raw OCR text into a draft. ocrConfidence is tesseract's own mean
// word confidence in 0..1; pass 0 when it is unknown.
func Parse(text string, ocrConfidence float64) Receipt {
	r := Receipt{Text: text}

	lines := splitLines(text)

	r.Merchant = findMerchant(lines)
	r.Category = guessCategory(r.Merchant, text)
	r.Date = findDate(text, time.Now())

	r.Subtotal = findLabelled(lines, subtotalTerms)
	r.Tax = findLabelled(lines, taxTerms)
	r.Tip = findLabelled(lines, tipTerms)
	r.Total = findTotal(lines)

	// A total that is missing but derivable is better than no total at all: a
	// receipt whose TOTAL line was creased away still has its parts.
	if r.Total <= 0 && r.Subtotal > 0 {
		r.Total = r.Subtotal + r.Tax + r.Tip
		r.Reasons = append(r.Reasons, "total derived from subtotal plus tax")
	}

	r.Items = findItems(lines, r)
	r.Confidence, r.Reasons = score(r, ocrConfidence, r.Reasons)
	return r
}

// splitLines prepares each OCR row for inspection.
func splitLines(text string) []line {
	var out []line
	for i, raw := range strings.Split(text, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		l := line{raw: raw, index: i}

		// The amount is the last one on the row: receipts right-align prices,
		// and a row like "2 x AA BATTERY 4PK 7.98" has digits earlier that are
		// quantities rather than money.
		if amts := parseAmounts(raw); len(amts) > 0 {
			last := amts[len(amts)-1]
			l.amount = last.value
			l.hasAmt = true
			l.label = normalise(raw[:last.start])
		} else {
			l.label = normalise(raw)
		}
		out = append(out, l)
	}
	return out
}

// findTotal picks the amount actually charged, by label rank rather than size.
func findTotal(lines []line) money.Cents {
	type cand struct {
		rank  int
		value money.Cents
		index int
	}
	var cands []cand

	for _, l := range lines {
		if !l.hasAmt || l.label == "" {
			continue
		}
		if containsAnyWord(l.label, neverTotal) {
			continue
		}
		for _, tier := range totalTiers {
			if containsAnyWord(l.label, tier.terms) {
				cands = append(cands, cand{tier.rank, l.amount, l.index})
				break
			}
		}
	}
	if len(cands) == 0 {
		return 0
	}

	// Highest rank wins. Within a rank the later line wins, because a receipt
	// that prints a running total repeats it and the final one is the one that
	// was charged. A zero is only ever chosen if nothing positive exists: a
	// "BALANCE DUE 0.00" printed under the real total means the bill is settled,
	// not that it was free.
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if (a.value > 0) != (b.value > 0) {
			return a.value > 0
		}
		if a.rank != b.rank {
			return a.rank > b.rank
		}
		return a.index > b.index
	})
	return cands[0].value
}

// findLabelled returns the amount on the first line whose label matches, and
// skips lines that also carry a disqualifying word so that "Subtotal" is never
// read as the tax just because it contains neither.
func findLabelled(lines []line, terms []string) money.Cents {
	for _, l := range lines {
		if !l.hasAmt || l.label == "" {
			continue
		}
		if !containsAnyWord(l.label, terms) {
			continue
		}
		// A line labelled "subtotal" is only ever the subtotal. Without this,
		// a receipt reading "SUBTOTAL AFTER TAX" would be picked up as the tax.
		if containsWord(l.label, "subtotal") && !hasTerm(terms, "subtotal") {
			continue
		}
		return l.amount
	}
	return 0
}

// findItems recovers the product lines. It stops at the first summary line,
// because everything below that is arithmetic about the items rather than more
// items -- and a receipt that repeats its total in a footer would otherwise
// contribute it as a product.
func findItems(lines []line, r Receipt) []Item {
	var out []Item
	for _, l := range lines {
		if !l.hasAmt {
			continue
		}
		if containsAnyWord(l.label, summaryTerms) {
			// The summary block has begun. Anything already collected stands.
			break
		}
		desc := cleanItemDescription(l.raw, l.amount)
		if desc == "" || l.amount <= 0 {
			continue
		}
		out = append(out, Item{Description: desc, Amount: l.amount})
	}

	// Items are only offered to the user when they reconcile, because the
	// transaction form requires line items to sum exactly to the amount. Handing
	// back a set that cannot be saved would turn a helpful prefill into a form
	// the user has to repair before it will submit.
	if len(out) == 0 {
		return nil
	}
	var sum money.Cents
	for _, it := range out {
		sum += it.Amount
	}

	switch {
	case r.Total > 0 && abs(sum-r.Total) <= reconcileSlack:
		return out
	case r.Total > 0 && r.Subtotal > 0 && abs(sum-r.Subtotal) <= reconcileSlack:
		// The items make up the pre-tax total, so tax and tip become lines of
		// their own and the set adds up to what was charged.
		if r.Tax > 0 {
			out = append(out, Item{Description: "Tax", Amount: r.Tax})
		}
		if r.Tip > 0 {
			out = append(out, Item{Description: "Tip", Amount: r.Tip})
		}
		var withExtras money.Cents
		for _, it := range out {
			withExtras += it.Amount
		}
		if abs(withExtras-r.Total) <= reconcileSlack {
			return out
		}
		return nil
	default:
		return nil
	}
}

// itemQtyRe strips a leading quantity. "2 x" is often OCR'd as "2%" or "2 «",
// so the separator is matched loosely rather than as a literal x.
var itemQtyRe = regexp.MustCompile(`^\s*\d+\s*[xX%*«@]\s*`)

// cleanItemDescription takes the text before the amount and tidies it.
func cleanItemDescription(raw string, amount money.Cents) string {
	amts := parseAmounts(raw)
	if len(amts) == 0 {
		return ""
	}
	desc := raw[:amts[len(amts)-1].start]
	desc = itemQtyRe.ReplaceAllString(desc, "")
	desc = strings.TrimSpace(desc)
	desc = strings.Trim(desc, ".-–—:*|()[]{}")
	desc = strings.Join(strings.Fields(desc), " ")

	// A description needs real words. A row of dots or a lone code is noise
	// that happened to sit beside a number.
	letters := 0
	for _, r := range desc {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			letters++
		}
	}
	if letters < 3 {
		return ""
	}
	if len([]rune(desc)) > 60 {
		desc = string([]rune(desc)[:60])
	}
	return desc
}

// ── merchant, category, date ──────────────────────────────────────────────────

// findMerchant takes the shop name from the top of the receipt, which is where
// every receipt puts it.
func findMerchant(lines []line) string {
	for i, l := range lines {
		if i > 5 {
			break // past the header; this is the body of the receipt
		}
		if l.hasAmt {
			continue
		}
		raw := strings.TrimSpace(l.raw)
		if looksLikeContact(raw) {
			continue
		}
		letters := 0
		for _, r := range raw {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				letters++
			}
		}
		if letters < 3 {
			continue
		}
		raw = strings.Join(strings.Fields(raw), " ")
		if len([]rune(raw)) > 60 {
			raw = string([]rune(raw)[:60])
		}
		return raw
	}
	return ""
}

// streetSuffixes end a street address. Matched as whole words, so "Drive" is a
// suffix but "Driveshaft Auto" is a shop.
var streetSuffixes = []string{
	"street", "st", "avenue", "ave", "road", "rd", "boulevard", "blvd",
	"lane", "ln", "drive", "dr", "court", "ct", "place", "pl", "way",
	"highway", "hwy", "parkway", "pkwy", "suite", "ste", "floor", "unit",
}

// houseNumberRe matches a street number at the start of a line: digits, then a
// space, then a word. "7-ELEVEN" does not match (no space) and neither does a
// shop whose name merely opens with a number.
var houseNumberRe = regexp.MustCompile(`^\d{1,6}\s+\p{L}`)

// looksLikeContact rejects the address, phone number and website printed around
// the shop name. It has to be right in both directions: rejecting the name
// leaves the payee blank, and accepting the address puts a street into it.
func looksLikeContact(s string) bool {
	low := strings.ToLower(s)
	for _, hint := range []string{"www.", "http", ".com", ".net", ".org", "@"} {
		if strings.Contains(low, hint) {
			return true
		}
	}

	// A house number followed by a street suffix is an address, wherever it
	// appears. Some receipts print the address above the name, because the name
	// itself was a logo the scanner never turned into text.
	norm := normalise(low)
	if houseNumberRe.MatchString(strings.TrimSpace(s)) && containsAnyWord(norm, streetSuffixes) {
		return true
	}

	digits := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	// A phone number or a bare postcode line is mostly digits; a shop name that
	// happens to carry a branch number is not.
	return digits > 0 && digits*2 >= len(strings.ReplaceAll(s, " ", ""))
}

// categories maps a hint that may appear in the merchant name or anywhere in the
// receipt onto the expense label YABA would use. It is a convenience, not a
// classification: the field stays editable and the guess is often blank.
var categories = []struct {
	label string
	hints []string
}{
	{"Groceries", []string{"grocery", "market", "foods", "supermarket", "aldi",
		"kroger", "safeway", "trader joe", "whole foods", "publix", "wegmans",
		"jewel", "mariano", "costco", "sams club", "walmart"}},
	{"Coffee", []string{"coffee", "espresso", "cafe", "café", "starbucks",
		"dunkin", "peet", "grind", "roasters"}},
	{"Dining", []string{"restaurant", "pizzeria", "pizza", "grill", "kitchen",
		"diner", "bistro", "taqueria", "sushi", "burger", "server", "table",
		"food bev", "gratuity"}},
	{"Fuel", []string{"shell", "bp", "exxon", "chevron", "mobil", "citgo",
		"speedway", "gallons", "price/gal", "pump", "unleaded", "regular fuel"}},
	{"Pharmacy", []string{"pharmacy", "walgreens", "cvs", "rite aid",
		"drug store", "drugstore"}},
	{"Household", []string{"hardware", "home depot", "lowe", "ace hardware",
		"menards", "true value"}},
	{"Transport", []string{"uber", "lyft", "transit", "metro", "parking",
		"toll", "amtrak"}},
}

// guessCategory picks an expense label from the merchant name, falling back to
// the body of the receipt for the cases where the name gives nothing away --
// a fuel receipt headed only "SHELL" is obvious, but one headed with a
// franchisee's name is not, and "PRICE/GAL" further down still gives it away.
func guessCategory(merchant, text string) string {
	m := strings.ToLower(merchant)
	for _, c := range categories {
		for _, h := range c.hints {
			if strings.Contains(m, h) {
				return c.label
			}
		}
	}
	body := strings.ToLower(text)
	for _, c := range categories {
		for _, h := range c.hints {
			if strings.Contains(body, h) {
				return c.label
			}
		}
	}
	return ""
}

// dateLayouts are tried in order against every candidate the scanner finds.
// Two-digit years come last: "12/05/25" is a date, but so is "12/05/2025", and
// the four-digit reading must win when both parse.
var dateLayouts = []string{
	"2006-01-02", "2006/01/02",
	"01/02/2006", "1/2/2006", "01-02-2006", "1-2-2006", "01.02.2006",
	"02 Jan 2006", "2 Jan 2006", "02 January 2006", "2 January 2006",
	"Jan 2, 2006", "Jan 02, 2006", "January 2, 2006", "January 02, 2006",
	"Jan 2 2006", "Jan 02 2006",
	"01/02/06", "1/2/06", "01-02-06",
}

// dateCandidateRe finds the shapes a date can take, so that time.Parse is only
// asked about substrings that could plausibly be one.
var dateCandidateRe = regexp.MustCompile(
	`(?i)\d{4}[-/]\d{1,2}[-/]\d{1,2}` +
		`|\d{1,2}[-/.]\d{1,2}[-/.]\d{2,4}` +
		`|\d{1,2}\s+[a-z]{3,9}\.?\s+\d{4}` +
		`|[a-z]{3,9}\.?\s+\d{1,2},?\s+\d{4}`)

// findDate returns the first plausible date on the receipt in storage form.
func findDate(text string, now time.Time) string {
	// Month names are title-cased before parsing because time.Parse is
	// case-sensitive and receipts shout: "14 JAN 2026" will not parse as
	// "02 Jan 2006" until the month is written the way the layout expects.
	for _, raw := range dateCandidateRe.FindAllString(text, -1) {
		cand := strings.Join(strings.Fields(raw), " ")
		cand = strings.ReplaceAll(cand, ".", "")
		cand = titleMonth(cand)

		for _, layout := range dateLayouts {
			t, err := time.Parse(layout, cand)
			if err != nil {
				continue
			}
			if !plausibleDate(t, now) {
				break // parsed, but not a date this receipt could carry
			}
			return t.Format("2006-01-02")
		}
	}
	return ""
}

// titleMonth rewrites an all-caps or lowercase month name as Jan / January.
func titleMonth(s string) string {
	fields := strings.Fields(s)
	for i, f := range fields {
		letters := true
		for _, r := range f {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == ',') {
				letters = false
				break
			}
		}
		if !letters || len(f) < 3 {
			continue
		}
		trailing := ""
		if strings.HasSuffix(f, ",") {
			f, trailing = strings.TrimSuffix(f, ","), ","
		}
		fields[i] = strings.ToUpper(f[:1]) + strings.ToLower(f[1:]) + trailing
	}
	return strings.Join(fields, " ")
}

// plausibleDate rejects a parse that succeeded on nonsense. A receipt cannot be
// from the future beyond a day of clock skew, and one from before 2000 is a
// misread rather than a genuinely ancient purchase.
func plausibleDate(t, now time.Time) bool {
	if t.After(now.AddDate(0, 0, 1)) {
		return false
	}
	return t.Year() >= 2000
}

// ── confidence ────────────────────────────────────────────────────────────────

// score combines how sure tesseract was with how well the numbers agree. The
// arithmetic check carries the most weight of any single signal: three numbers
// read independently do not add up by accident.
func score(r Receipt, ocrConfidence float64, reasons []string) (float64, []string) {
	structural := 0.0

	switch {
	case r.Total > 0 && r.Reconciles():
		structural += 0.55
		reasons = append(reasons, "subtotal plus tax equals the total")
	case r.Total > 0:
		structural += 0.25
		reasons = append(reasons, "a total was found but nothing cross-checks it")
	default:
		reasons = append(reasons, "no total could be identified")
	}

	if r.Date != "" {
		structural += 0.15
		reasons = append(reasons, "a date was read")
	}
	if r.Merchant != "" {
		structural += 0.10
		reasons = append(reasons, "a merchant name was read")
	}
	if len(r.Items) > 0 {
		structural += 0.20
		reasons = append(reasons, fmt.Sprintf("%d line items reconcile to the total", len(r.Items)))
	}
	if structural > 1 {
		structural = 1
	}

	// Without a tesseract confidence the structure has to speak for itself,
	// rather than being halved by a zero it did not earn.
	conf := structural
	if ocrConfidence > 0 {
		conf = 0.45*clamp01(ocrConfidence) + 0.55*structural
	}

	// No total means no useful prefill, whatever the characters looked like.
	if r.Total <= 0 && conf > 0.35 {
		conf = 0.35
	}
	return clamp01(conf), reasons
}

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// Summary is a one-line description of what was read, for the worker's log and
// the notification the user receives.
func (r Receipt) Summary() string {
	switch {
	case r.Total <= 0:
		return "no amount could be read"
	case r.Merchant == "":
		return fmt.Sprintf("%s", r.Total.Display())
	default:
		return fmt.Sprintf("%s at %s", r.Total.Display(), r.Merchant)
	}
}
