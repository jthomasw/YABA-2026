package web

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// itemValues returns the value of every rendered line-item input with the
// given name, in page order. The always-present blank template row carries no
// value attribute, so it is not counted.
func itemValues(body, name string) []string {
	re := regexp.MustCompile(`name="` + name + `"[^>]*?value="([^"]*)"`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		out = append(out, html.UnescapeString(m[1]))
	}
	return out
}

// summaryText is the line-item section's <summary>, whitespace collapsed.
func summaryText(t *testing.T, body string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<summary>(.*?)</summary>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("the form has no line-item summary")
	}
	return strings.Join(strings.Fields(m[1]), " ")
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ── 1. typed line items survive a refusal ─────────────────────────────────────

// TestLineItemsAreEchoedBackOnAnyRefusal: the items used to be parsed last, and
// discarded when they did not add up, so any refusal -- above all the items
// not reconciling, the commonest one -- sent the form back with no rows.
func TestLineItemsAreEchoedBackOnAnyRefusal(t *testing.T) {
	cases := []struct {
		name    string
		amount  string
		date    string
		amounts []string
		wantErr string
	}{
		{"items do not add up", "30.00", "2026-08-01", []string{"12.00", "10.00"}, "add up to"},
		{"bad date", "22.00", "not a date", []string{"12.00", "10.00"}, "YYYY-MM-DD"},
		{"bad total", "lots", "2026-08-01", []string{"12.00", "10.00"}, "greater than zero"},
		{"an item amount is invalid", "22.00", "2026-08-01", []string{"12.00", "1.234"}, "Every line item"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rig := newRig(t)
			rig.login()

			rec := rig.post("/transactions/new", url.Values{
				"kind": {"expense"}, "label": {"Shopping"}, "amount": {c.amount}, "date": {c.date},
				// The trailing blank row is the form's always-present template
				// row; it must not come back as an extra empty row.
				"item_description": {"Milk & eggs", "Bread", ""},
				"item_category":    {"Dairy", "Bakery", ""},
				"item_amount":      append(append([]string{}, c.amounts...), ""),
			})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", rec.Code)
			}
			body := rec.Body.String()
			if got := extractError(body); !strings.Contains(got, c.wantErr) {
				t.Errorf("error %q, want it to mention %q", got, c.wantErr)
			}
			if got := itemValues(body, "item_description"); !equalStrings(got, []string{"Milk & eggs", "Bread"}) {
				t.Errorf("descriptions echoed %q", got)
			}
			if got := itemValues(body, "item_category"); !equalStrings(got, []string{"Dairy", "Bakery"}) {
				t.Errorf("categories echoed %q", got)
			}
			if got := itemValues(body, "item_amount"); !equalStrings(got, c.amounts) {
				t.Errorf("amounts echoed %q, want exactly what was typed, %q", got, c.amounts)
			}
			if got := summaryText(t, body); got != "Break this into items (2)" {
				t.Errorf("summary %q", got)
			}
		})
	}
}

// ── 2. receipt drafts: categories, and the echo ───────────────────────────────

// draftReceipt is a processed receipt with an OCR reading attached.
func (r *testRig) draftReceipt(name string, d store.ReceiptDraft) int64 {
	r.t.Helper()
	jobID := r.waitingReceipt(name)
	if err := r.store.SaveReceiptDraft(context.Background(), jobID, d); err != nil {
		r.t.Fatalf("save draft: %v", err)
	}
	return jobID
}

var groceryDraft = store.ReceiptDraft{
	Merchant: "Lidl", Category: "Groceries", Date: "2026-08-01", Total: 900,
	Items: []store.DraftItem{
		{Description: "Milk", Amount: 200},
		{Description: "Bread", Amount: 300},
		{Description: "Eggs", Amount: 400},
	},
}

// TestDraftItemsStartInTheReceiptsCategory: a DraftItem has no category, and an
// item saved without one is charted as Uncategorised, so a confirmed Groceries
// receipt used to file every line outside Groceries.
func TestDraftItemsStartInTheReceiptsCategory(t *testing.T) {
	rig := newRig(t)
	rig.login()
	jobID := rig.draftReceipt("lidl.png", groceryDraft)

	body := rig.do("GET", fmt.Sprintf("/transactions/new?type=expense&receipt=%d", jobID), nil).Body.String()
	if got := itemValues(body, "item_category"); !equalStrings(got, []string{"Groceries", "Groceries", "Groceries"}) {
		t.Errorf("draft item categories = %q, want each in Groceries", got)
	}
	if got := summaryText(t, body); got != "Items read from the receipt (3)" {
		t.Errorf("summary %q", got)
	}
}

// With no category read off the receipt, the items follow the label the form
// opens with, so they still land in the bucket the user confirms.
func TestDraftItemsWithoutACategoryFollowTheLabel(t *testing.T) {
	rig := newRig(t)
	rig.login()
	d := groceryDraft
	d.Category = ""
	jobID := rig.draftReceipt("corner-shop.png", d)

	body := rig.do("GET", fmt.Sprintf("/transactions/new?type=expense&receipt=%d", jobID), nil).Body.String()
	m := regexp.MustCompile(`id="label"[^>]*?value="([^"]*)"`).FindStringSubmatch(body)
	if m == nil || m[1] == "" {
		t.Fatal("the form opened with no label")
	}
	label := html.UnescapeString(m[1])
	for _, got := range itemValues(body, "item_category") {
		if got != label {
			t.Errorf("draft item category %q, want the label %q", got, label)
		}
	}
}

// TestADraftWithOneTypoKeepsItsItems is the receipt case end to end: a typo in
// one line used to bring back an empty breakdown under a summary claiming that
// nothing was read off the receipt.
func TestADraftWithOneTypoKeepsItsItems(t *testing.T) {
	rig := newRig(t)
	rig.login()
	jobID := rig.draftReceipt("lidl.png", groceryDraft)

	post := func(amounts ...string) string {
		t.Helper()
		descs := []string{"Milk", "Bread", "Eggs"}[:len(amounts)]
		cats := []string{"Groceries", "Groceries", "Groceries"}[:len(amounts)]
		rec := rig.post("/transactions/new", url.Values{
			"kind": {"expense"}, "label": {"Groceries"}, "amount": {"9.00"},
			"date": {"2026-08-01"}, "receipt_job": {fmt.Sprint(jobID)},
			"item_description": descs, "item_category": cats, "item_amount": amounts,
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", rec.Code)
		}
		return rec.Body.String()
	}

	body := post("2.00", "3.00", "4.50")
	if got := itemValues(body, "item_amount"); !equalStrings(got, []string{"2.00", "3.00", "4.50"}) {
		t.Errorf("amounts echoed %q", got)
	}
	if got := summaryText(t, body); got != "Items from the receipt (3)" {
		t.Errorf("summary %q", got)
	}

	// Every row deleted but the totals still wrong elsewhere: the draft did
	// have lines, so the summary must not say none were read.
	rec := rig.post("/transactions/new", url.Values{
		"kind": {"expense"}, "label": {"Groceries"}, "amount": {"9.00"},
		"date": {"bad"}, "receipt_job": {fmt.Sprint(jobID)},
	})
	if got := summaryText(t, rec.Body.String()); strings.Contains(got, "none were read") {
		t.Errorf("summary %q claims the receipt had no lines", got)
	}
}

// A draft that genuinely had no lines still says so.
func TestADraftWithNoItemsSaysSo(t *testing.T) {
	rig := newRig(t)
	rig.login()
	d := groceryDraft
	d.Items = nil
	jobID := rig.draftReceipt("lidl.png", d)

	body := rig.do("GET", fmt.Sprintf("/transactions/new?type=expense&receipt=%d", jobID), nil).Body.String()
	if got := summaryText(t, body); got != "Add items (none were read off the receipt)" {
		t.Errorf("summary %q", got)
	}
}

// ── 3. the recurring choice survives a refusal ────────────────────────────────

func TestRecurringChoiceIsEchoedBack(t *testing.T) {
	cases := []struct {
		page, typeField string
		form            url.Values
		want            []string
		notWant         []string
	}{
		{
			page: "/expense", typeField: "expense_type",
			form: url.Values{"frequency_preset": {"quarterly"}, "frequency_n": {"1"}, "frequency_unit": {"month"}},
			want: []string{
				`name="expense_type" value="recurring" checked`,
				`<option value="quarterly" selected>`,
			},
			notWant: []string{`name="expense_type" value="one_time" checked`, `<option value="monthly" selected>`},
		},
		{
			page: "/income", typeField: "income_type",
			form: url.Values{"frequency_preset": {"custom"}, "frequency_n": {"3"}, "frequency_unit": {"month"}},
			want: []string{
				`name="income_type" value="recurring" checked`,
				`<option value="custom" selected>`,
				`<option value="month" selected>`,
				`value="3"`,
			},
			notWant: []string{`name="income_type" value="one_time" checked`, `<option value="biweekly" selected>`},
		},
	}
	for _, c := range cases {
		t.Run(c.page, func(t *testing.T) {
			rig := newRig(t)
			rig.login()

			form := c.form
			form.Set(c.typeField, "recurring")
			form.Set("label", "Rent")
			form.Set("amount", "12OO") // the typo
			form.Set("date", "2026-08-01")

			rec := rig.post(c.page, form)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", rec.Code)
			}
			body := rec.Body.String()
			for _, w := range c.want {
				if !strings.Contains(body, w) {
					t.Errorf("re-rendered page lacks %s", w)
				}
			}
			for _, w := range c.notWant {
				if strings.Contains(body, w) {
					t.Errorf("re-rendered page still has the default %s", w)
				}
			}
		})
	}
}

// Before anything is submitted, each page offers its own default schedule.
func TestRecurringDefaultsOnAFreshPage(t *testing.T) {
	rig := newRig(t)
	rig.login()

	for page, want := range map[string][]string{
		"/expense?step=manual": {`name="expense_type" value="one_time" checked`, `<option value="monthly" selected>`},
		"/income":              {`name="income_type" value="one_time" checked`, `<option value="biweekly" selected>`, `<option value="week" selected>`},
	} {
		body := rig.do("GET", page, nil).Body.String()
		for _, w := range want {
			if !strings.Contains(body, w) {
				t.Errorf("%s lacks %s", page, w)
			}
		}
	}
}

// ── 4. back links ─────────────────────────────────────────────────────────────

func TestSafeBack(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "/transactions"},
		{"/transactions", "/transactions"},
		{"/transactions?type=expense&month=2026-08&q=a%26b&page=2", "/transactions?type=expense&month=2026-08&q=a%26b&page=2"},
		{"/transactions/5/edit", "/transactions/5/edit"},
		{"/transactions/../\\evil.com", "/transactions"},
		{"/transactions/..", "/transactions"},
		{"/transactions/%2e%2e/evil", "/transactions"},
		{"/transactions/./x", "/transactions"},
		{"/transactions//evil.com", "/transactions"},
		{"/transactions/", "/transactions"},
		{"//evil.com", "/transactions"},
		{"//evil.com/transactions", "/transactions"},
		{"/\\evil.com", "/transactions"},
		{"\\\\evil.com", "/transactions"},
		{"https://evil.com", "/transactions"},
		{"https://evil.com/transactions", "/transactions"},
		{"javascript:alert(1)", "/transactions"},
		{"/transactionsX", "/transactions"},
		{"/dashboard", "/transactions"},
		{"transactions", "/transactions"},
		{"/transactions\x00", "/transactions"},
		{"/transactions?q=%zz", "/transactions"},
		{"/transactions?type=expense#frag", "/transactions?type=expense"},
	}
	for _, c := range cases {
		if got := safeBack(c.in); got != c.want {
			t.Errorf("safeBack(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

var (
	editHrefRE   = regexp.MustCompile(`href="(/transactions/\d+/edit\?back=[^"]*)"`)
	cancelHrefRE = regexp.MustCompile(`<a class="btn btn-quiet" href="([^"]*)">Cancel</a>`)
	backFieldRE  = regexp.MustCompile(`name="back" value="([^"]*)"`)
)

// TestEditKeepsTheFilteredListItCameFrom follows the links a browser would:
// the Edit link on a filtered list, the Cancel link on the edit page, and the
// redirect after saving. The back value was escaped twice on the way in, so
// Cancel was a 404 and the save lost the filters.
func TestEditKeepsTheFilteredListItCameFrom(t *testing.T) {
	rig := newRig(t)
	rig.login()
	id := rig.addExpenseVia(t, "Fish & chips", "8.00")

	list := rig.do("GET", "/transactions?type=expense&q=Fish+%26+chips", nil).Body.String()
	m := editHrefRE.FindStringSubmatch(list)
	if m == nil {
		t.Fatal("no Edit link on the filtered list")
	}
	edit := html.UnescapeString(m[1])
	// Escaped exactly once: a single decode must give back a list URL. The
	// double-escaped link decoded to "%2Ftransactions%3F...", which is not.
	if strings.Contains(edit, "back=%252f") || strings.Contains(edit, "back=%252F") {
		t.Errorf("the Edit link is escaped twice: %s", edit)
	}
	u, err := url.Parse(edit)
	if err != nil {
		t.Fatal(err)
	}
	back := u.Query().Get("back")
	q, err := url.Parse(back)
	if err != nil || q.Path != "/transactions" || q.Query().Get("q") != "Fish & chips" || q.Query().Get("type") != "expense" {
		t.Fatalf("the Edit link's back value decodes to %q", back)
	}

	form := rig.do("GET", edit, nil)
	if form.Code != http.StatusOK {
		t.Fatalf("edit page: %d", form.Code)
	}
	body := form.Body.String()
	c := cancelHrefRE.FindStringSubmatch(body)
	if c == nil {
		t.Fatal("no Cancel link")
	}
	if got := html.UnescapeString(c[1]); got != back {
		t.Errorf("Cancel href = %q, want %q", got, back)
	}
	// And it resolves to the list, not a 404.
	if rec := rig.do("GET", html.UnescapeString(c[1]), nil); rec.Code != http.StatusOK {
		t.Errorf("following Cancel: %d", rec.Code)
	}

	hidden := backFieldRE.FindStringSubmatch(body)
	if hidden == nil {
		t.Fatal("the edit form does not carry back")
	}
	version := regexp.MustCompile(`name="version" value="(\d+)"`).FindStringSubmatch(body)
	rec := rig.post(fmt.Sprintf("/transactions/%d/edit", id), url.Values{
		"kind": {"expense"}, "label": {"Fish & chips"}, "amount": {"9.00"}, "date": {"2026-08-01"},
		"version": {version[1]}, "back": {html.UnescapeString(hidden[1])},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", rec.Code, extractError(rec.Body.String()))
	}
	if got := rec.Header().Get("Location"); got != back {
		t.Errorf("after saving, redirected to %q, want %q", got, back)
	}
}

// TestBackCannotLeaveTheSite covers every read of back: the Cancel href on
// GET, the redirects after an update and a delete, and the transfer refusal.
func TestBackCannotLeaveTheSite(t *testing.T) {
	hostile := []string{"//evil.example", "/transactions/../\\evil.com", "https://evil.com", "/\\evil.com"}

	for _, bad := range hostile {
		t.Run(bad, func(t *testing.T) {
			rig := newRig(t)
			rig.login()
			id := rig.addExpenseVia(t, "Lunch", "8.00")

			body := rig.do("GET", fmt.Sprintf("/transactions/%d/edit?back=%s", id, url.QueryEscape(bad)), nil).Body.String()
			if c := cancelHrefRE.FindStringSubmatch(body); c == nil || c[1] != "/transactions" {
				t.Errorf("Cancel href = %v, want /transactions", c)
			}

			version := regexp.MustCompile(`name="version" value="(\d+)"`).FindStringSubmatch(body)
			rec := rig.post(fmt.Sprintf("/transactions/%d/edit", id), url.Values{
				"kind": {"expense"}, "label": {"Lunch"}, "amount": {"9.00"}, "date": {"2026-08-01"},
				"version": {version[1]}, "back": {bad},
			})
			if got := rec.Header().Get("Location"); got != "/transactions" {
				t.Errorf("update redirected to %q", got)
			}

			rec = rig.post(fmt.Sprintf("/transactions/%d/delete", id), url.Values{"back": {bad}})
			if got := rec.Header().Get("Location"); got != "/transactions" {
				t.Errorf("delete redirected to %q", got)
			}
		})
	}
}

// TestPagingLinksKeepTheirFilters: the Previous/Next hrefs were query strings
// written after a literal "?", where html/template encoded every "=" and "&",
// so Next sent one meaningless key and paging never left page one.
func TestPagingLinksKeepTheirFilters(t *testing.T) {
	rig := newRig(t)
	rig.login()
	for i := 0; i < store.DefaultPageSize+1; i++ {
		if _, err := rig.store.Add(context.Background(), rig.scope, store.NewTransaction{
			Kind: store.KindExpense, Label: "Coffee", Amount: 300, OccurredOn: "2026-08-01",
		}); err != nil {
			t.Fatal(err)
		}
	}

	body := rig.do("GET", "/transactions?type=expense", nil).Body.String()
	m := regexp.MustCompile(`href="([^"]*)" rel="next"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no Next link")
	}
	next := html.UnescapeString(m[1])
	if next != "/transactions?page=2&type=expense" {
		t.Errorf("Next href = %q", next)
	}
	page2 := rig.do("GET", next, nil).Body.String()
	if !strings.Contains(page2, `rel="prev"`) {
		t.Error("following Next did not reach page two")
	}
}

// ── 5. a failed recalculation is not reported as success ──────────────────────

// TestAFailedReallocationIsNotHiddenBySuccess makes the waterfall fail by
// refusing every write to allocations, then creates a bucket that has income
// to be funded from. The success flash used to overwrite the warning.
func TestAFailedReallocationIsNotHiddenBySuccess(t *testing.T) {
	rig := newRig(t)
	rig.login()

	if _, err := rig.store.Add(context.Background(), rig.scope, store.NewTransaction{
		Kind: store.KindIncome, Label: "Salary", Amount: 100000, OccurredOn: store.Today(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.db.Exec(`CREATE TRIGGER fail_allocations BEFORE INSERT ON allocations
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}

	rec := rig.post("/buckets", url.Values{
		"name": {"Rent"}, "cost_kind": {"fixed"}, "amount": {"500.00"}, "essential": {"yes"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d", rec.Code)
	}
	page := rig.do("GET", "/dashboard", nil).Body.String()
	m := regexp.MustCompile(`class="flash flash-(\w+)" role="status">([^<]*)<`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no flash after creating the bucket")
	}
	text := html.UnescapeString(m[2])
	if m[1] != "error" {
		t.Errorf("flash kind %q, want error: %q", m[1], text)
	}
	if !strings.Contains(text, "Rent") || !strings.Contains(text, "could not be recalculated") {
		t.Errorf("flash %q should say what was saved and what failed", text)
	}
}

// ── 6. oversized uploads ──────────────────────────────────────────────────────

// TestAnOversizedUploadSaysSo: a body over the limit failed to parse inside
// the CSRF check, and the user was told their form had expired.
func TestAnOversizedUploadSaysSo(t *testing.T) {
	rig := newRig(t)
	rig.login()
	token := rig.csrf("/expense")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("csrf_token", token)
	fw, _ := mw.CreateFormFile("receipt", "huge.jpg")
	fw.Write(bytes.Repeat([]byte{0xff}, 3<<20)) // the rig allows 1 MB
	mw.Close()

	req := httptest.NewRequest("POST", "/import/receipt", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for _, c := range rig.cookies {
		req.AddCookie(c)
	}
	rec := rig.serve(req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "expired") {
		t.Errorf("an oversized upload was reported as an expired form: %q", body)
	}
	if !strings.Contains(body, "1 MB") {
		t.Errorf("the message does not name the limit: %q", body)
	}
}

// The limit named to the user is the one in force, never an unset zero.
func TestTheUploadLimitIsNeverZero(t *testing.T) {
	s := &Server{}
	if got := s.maxUploadMB(); got != defaultMaxUploadMB || got == 0 {
		t.Errorf("unset limit = %d MB, want %d", got, defaultMaxUploadMB)
	}
	if !strings.Contains(s.tooLargeMessage("/import/receipt"), fmt.Sprintf("%d MB", defaultMaxUploadMB)) {
		t.Errorf("message %q", s.tooLargeMessage("/import/receipt"))
	}
	s.cfg.MaxUploadMB = 7
	if got := s.maxUploadBytes(); got != 7<<20 {
		t.Errorf("configured limit = %d bytes", got)
	}
}

// ── 8. income ignores line items ──────────────────────────────────────────────

// TestIncomeIgnoresLineItems is the server's backstop for a form switched from
// Expense to Income with its rows still filled in.
func TestIncomeIgnoresLineItems(t *testing.T) {
	rig := newRig(t)
	rig.login()

	rec := rig.post("/transactions/new", url.Values{
		"kind": {"income"}, "label": {"Salary"}, "amount": {"1000.00"}, "date": {"2026-08-01"},
		"item_description": {"Leftover"}, "item_category": {"Food"}, "item_amount": {"12.00"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rec.Code, extractError(rec.Body.String()))
	}
	var id int64
	if err := rig.db.QueryRow(`SELECT id FROM transactions WHERE label = 'Salary'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	items, err := rig.store.LineItems(context.Background(), rig.scope, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("an income stored %d line items", len(items))
	}
}

// ── 9. the budget switcher ────────────────────────────────────────────────────

// TestBudgetSwitcherSelectsByID: two budgets with the same name both matched
// a by-name comparison, and the first one won.
func TestBudgetSwitcherSelectsByID(t *testing.T) {
	rig := newRig(t)
	rig.login()

	first, err := rig.store.CreateSharedHousehold(context.Background(), rig.userID, "Home")
	if err != nil {
		t.Fatal(err)
	}
	second, err := rig.store.CreateSharedHousehold(context.Background(), rig.userID, "Home")
	if err != nil {
		t.Fatal(err)
	}

	for _, active := range []int64{first, second} {
		if rec := rig.post("/household/switch", url.Values{"household_id": {fmt.Sprint(active)}}); rec.Code != http.StatusSeeOther {
			t.Fatalf("switch: %d", rec.Code)
		}
		body := rig.do("GET", "/dashboard", nil).Body.String()
		picker := regexp.MustCompile(`(?s)<select id="hh-switch".*?</select>`).FindString(body)
		if picker == "" {
			t.Fatal("no budget picker")
		}
		selected := regexp.MustCompile(`<option value="(\d+)" selected>`).FindAllStringSubmatch(picker, -1)
		if len(selected) != 1 || selected[0][1] != fmt.Sprint(active) {
			t.Errorf("active %d: selected options %v", active, selected)
		}
	}
}
