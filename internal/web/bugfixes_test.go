package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// Regression tests for the bugs fixed in the November review. Each drives the
// app the way a browser does: a form is read off the rendered page and posted
// back with whatever hidden fields the page gave it.

// hiddenValue reads a hidden field's value out of a rendered page.
func hiddenValue(t *testing.T, body, name string) string {
	t.Helper()
	m := regexp.MustCompile(`name="` + regexp.QuoteMeta(name) + `" value="([^"]*)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("page has no hidden %s field", name)
	}
	return m[1]
}

// registerUser creates an account with no budgets shared with it yet.
func (r *testRig) registerUser(email string) int64 {
	r.t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		r.t.Fatal(err)
	}
	uid, err := r.store.CreateUser(context.Background(), email, string(hash))
	if err != nil {
		r.t.Fatalf("create %s: %v", email, err)
	}
	return uid
}

func householdExists(t *testing.T, rig *testRig, id int64) bool {
	t.Helper()
	var n int
	if err := rig.db.QueryRow(`SELECT COUNT(*) FROM households WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// ── 1 & 5: setup ──────────────────────────────────────────────────────────────

// A viewer of a shared budget cannot put money into it through /setup; setup
// only ever fills the user's own, still-empty budget, and only once.
func TestSetupOnlyFillsYourOwnEmptyBudget(t *testing.T) {
	rig, shared, _, viewer := sharedRig(t)
	rig.loginAs(viewer)
	ctx := context.Background()
	viewerID := mustUserID(t, rig, viewer)

	if m, _ := rig.store.ActiveHousehold(ctx, store.User{ID: viewerID}); m.ID != shared {
		t.Fatalf("viewer should be working in the shared budget, is in %d", m.ID)
	}
	rec := rig.post("/setup", url.Values{
		"starting_balance": {"50000.00"}, "expense_count": {"0"}, "income_count": {"0"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("setup: %d", rec.Code)
	}
	sharedTotals, _ := rig.store.Totals(ctx, store.Scope{HouseholdID: shared, UserID: viewerID}, "")
	if sharedTotals.Income != 0 {
		t.Errorf("setup put %s into the shared budget", sharedTotals.Income.Display())
	}

	// It went to the viewer's own budget instead...
	var personal int64
	if err := rig.db.QueryRow(`SELECT id FROM households WHERE personal_for = ?`, viewerID).Scan(&personal); err != nil {
		t.Fatal(err)
	}
	own, _ := rig.store.Totals(ctx, store.Scope{HouseholdID: personal, UserID: viewerID}, "")
	if own.Income != 5000000 {
		t.Errorf("own budget income = %s, want $50,000.00", own.Income.Display())
	}

	// ...and only once.
	rig.post("/setup", url.Values{
		"starting_balance": {"50000.00"}, "expense_count": {"0"}, "income_count": {"0"},
	})
	own, _ = rig.store.Totals(ctx, store.Scope{HouseholdID: personal, UserID: viewerID}, "")
	if own.Income != 5000000 {
		t.Errorf("a second setup changed the starting balance to %s", own.Income.Display())
	}
	if rec := rig.do("GET", "/setup", nil); rec.Code != http.StatusSeeOther {
		t.Errorf("/setup on a budget with entries should redirect, got %d", rec.Code)
	}
}

// ── 2: Sharing page acts on the budget it shows ──────────────────────────────

func TestSharingPageFromAnotherBudgetChangesNothing(t *testing.T) {
	rig := newRig(t)
	rig.login()
	ctx := context.Background()

	a, err := rig.store.CreateSharedHousehold(ctx, rig.userID, "A")
	if err != nil {
		t.Fatal(err)
	}
	b, err := rig.store.CreateSharedHousehold(ctx, rig.userID, "B") // now active
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.store.SwitchHousehold(ctx, rig.userID, a); err != nil {
		t.Fatal(err)
	}
	pageA := rig.do("GET", "/household", nil).Body.String()
	shownFor := hiddenValue(t, pageA, "form_household")
	if shownFor != fmt.Sprint(a) {
		t.Fatalf("A's sharing page carries form_household=%s, want %d", shownFor, a)
	}

	// Switch to B elsewhere, then press Delete on the page still showing A.
	if err := rig.store.SwitchHousehold(ctx, rig.userID, b); err != nil {
		t.Fatal(err)
	}
	rec := rig.post("/household/delete", url.Values{"form_household": {shownFor}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/household" {
		t.Errorf("stale delete: %d -> %q", rec.Code, rec.Header().Get("Location"))
	}
	if !householdExists(t, rig, a) || !householdExists(t, rig, b) {
		t.Fatalf("a stale Sharing page deleted a budget: A=%v B=%v",
			householdExists(t, rig, a), householdExists(t, rig, b))
	}

	// From B's own page it works.
	pageB := rig.do("GET", "/household", nil).Body.String()
	rig.post("/household/delete", url.Values{"form_household": {hiddenValue(t, pageB, "form_household")}})
	if householdExists(t, rig, b) {
		t.Error("deleting from B's own page did not delete B")
	}
	if !householdExists(t, rig, a) {
		t.Error("deleting B also deleted A")
	}
}

// ── 3: an expired form token saves, it does not claim "already saved" ─────────

func TestFormLeftOpenOvernightStillSaves(t *testing.T) {
	rig := newRig(t)
	rig.login()

	page := rig.do("GET", "/expense?step=manual", nil).Body.String()
	token := hiddenValue(t, page, "form_token")
	if _, err := rig.db.Exec(`UPDATE form_tokens SET created_at = datetime('now', '-13 hours') WHERE token = ?`, token); err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"form_token": {token}, "form_household": {hiddenValue(t, page, "form_household")},
		"expense_type": {"one_time"}, "label": {"Food"}, "amount": {"12.00"},
		"date": {store.Today()}, "essential": {"yes"},
	}
	rig.post("/expense", form)
	var n int
	rig.db.QueryRow(`SELECT COUNT(*) FROM transactions WHERE label = 'Food'`).Scan(&n)
	if n != 1 {
		t.Fatalf("an overnight form saved %d entries, want 1", n)
	}

	// A genuine resubmit of the same form is still caught.
	rec := rig.post("/expense", form)
	rig.db.QueryRow(`SELECT COUNT(*) FROM transactions WHERE label = 'Food'`).Scan(&n)
	if n != 1 || !strings.Contains(rig.do("GET", "/dashboard", nil).Body.String(), "already saved") {
		t.Errorf("a real double submit saved %d entries (status %d)", n, rec.Code)
	}
}

// ── 14: an invitation needs its email link ────────────────────────────────────

func TestInvitationNeedsItsEmailLink(t *testing.T) {
	rig := newRig(t) // no mail: the owner is shown the link to pass on
	rig.login()
	ctx := context.Background()
	shared, err := rig.store.CreateSharedHousehold(ctx, rig.userID, "Family")
	if err != nil {
		t.Fatal(err)
	}
	page := rig.do("GET", "/household", nil).Body.String()
	rig.post("/household/invite", url.Values{
		"email": {"spouse@example.com"}, "role": {"editor"},
		"form_household": {hiddenValue(t, page, "form_household")},
	})
	flash := rig.do("GET", "/household", nil).Body.String()
	link := regexp.MustCompile(`/invites/open\?token=([A-Za-z0-9_\-%=]+)`).FindStringSubmatch(flash)
	if link == nil {
		t.Fatal("without email, the owner was not shown the invitation link")
	}
	token, _ := url.QueryUnescape(link[1])

	// Somebody registers the invited address. Owning an account with it is
	// not owning the mailbox: they see the invitation but cannot accept.
	other := rig.registerUser("spouse@example.com")
	rig.loginAs("spouse@example.com")
	dash := rig.do("GET", "/dashboard", nil).Body.String()
	if !strings.Contains(dash, "open the link in the invitation email") {
		t.Error("the banner offers Accept without the email link")
	}
	invites, _ := rig.store.InvitesFor(ctx, other, "spouse@example.com")
	if len(invites) != 1 {
		t.Fatalf("invites for spouse: %d", len(invites))
	}
	rig.post(fmt.Sprintf("/invites/%d/accept", invites[0].ID), nil)
	if m, _ := rig.store.ActiveHousehold(ctx, store.User{ID: other}); m.ID == shared {
		t.Fatal("accepted an invitation without its email link")
	}

	// Opening the link proves the mailbox; then Accept works.
	if rec := rig.do("GET", "/invites/open?token="+url.QueryEscape(token), nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("open link: %d", rec.Code)
	}
	if !strings.Contains(rig.do("GET", "/dashboard", nil).Body.String(), fmt.Sprintf(`action="/invites/%d/accept"`, invites[0].ID)) {
		t.Fatal("after opening the link, the banner should offer Accept")
	}
	rig.post(fmt.Sprintf("/invites/%d/accept", invites[0].ID), nil)
	if m, _ := rig.store.ActiveHousehold(ctx, store.User{ID: other}); m.ID != shared {
		t.Error("accepting after opening the link did not join the budget")
	}
}

// An invitation link opened before signing in still counts afterwards.
func TestInvitationLinkSurvivesSignIn(t *testing.T) {
	rig := newRig(t)
	rig.login()
	ctx := context.Background()
	shared, _ := rig.store.CreateSharedHousehold(ctx, rig.userID, "Family")
	token, err := rig.store.InviteMember(ctx, shared, rig.userID, "late@example.com", store.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	other := rig.registerUser("late@example.com")

	rig.cookies = nil // signed out
	rec := rig.do("GET", "/invites/open?token="+url.QueryEscape(token), nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("open while signed out: %d -> %q", rec.Code, rec.Header().Get("Location"))
	}
	// Sign in on the same browser, keeping its cookies.
	signIn := rig.do("POST", "/auth", url.Values{
		"csrf_token": {rig.csrf("/")}, "email": {"late@example.com"}, "password": {testPassword},
	})
	if signIn.Code != http.StatusSeeOther {
		t.Fatalf("sign in: %d", signIn.Code)
	}
	invites, _ := rig.store.InvitesFor(ctx, other, "late@example.com")
	rig.post(fmt.Sprintf("/invites/%d/accept", invites[0].ID), nil)
	if m, _ := rig.store.ActiveHousehold(ctx, store.User{ID: other}); m.ID != shared {
		t.Error("the link opened before signing in was lost")
	}
}

// ── 17: recurring entries check the budget the form came from ────────────────

func TestRecurringFromAnotherBudgetsFormIsRefused(t *testing.T) {
	rig := newRig(t)
	rig.login()
	ctx := context.Background()
	page := rig.do("GET", "/expense?step=manual", nil).Body.String()
	personalForm := hiddenValue(t, page, "form_household")

	shared, _ := rig.store.CreateSharedHousehold(ctx, rig.userID, "Flat") // switches to it
	rec := rig.post("/expense", url.Values{
		"form_token": {hiddenValue(t, page, "form_token")}, "form_household": {personalForm},
		"expense_type": {"recurring"}, "label": {"Rent"}, "amount": {"900"},
		"date": {store.Today()}, "frequency_n": {"1"}, "frequency_unit": {"month"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("recurring from a stale form: %d, want 400", rec.Code)
	}
	list, _ := rig.store.ListRecurringExpense(ctx, store.Scope{HouseholdID: shared, UserID: rig.userID})
	if len(list) != 0 {
		t.Error("a schedule from another budget's form was saved into the shared one")
	}
}

// ── 18: archived bills ────────────────────────────────────────────────────────

func TestArchivedBillLinksAreKeptAndDetached(t *testing.T) {
	rig := newRig(t)
	rig.login()
	ctx := context.Background()

	bill, _ := rig.store.CreateBucket(ctx, rig.scope, store.NewBucket{Name: "Power", CostKind: store.CostFixed, Fixed: 5000})
	sched, _ := rig.store.CreateRecurringExpense(ctx, rig.scope, "Power co", 5000, &bill, true, 1, "month", "2099-01-01")
	id, _ := rig.store.Add(ctx, rig.scope, store.NewTransaction{
		Kind: store.KindExpense, Label: "Electric", Amount: 5000, OccurredOn: store.Today(), BucketID: &bill,
	})
	if err := rig.store.ArchiveBucket(ctx, rig.scope, bill); err != nil {
		t.Fatal(err)
	}

	// The schedule no longer charges the removed bill.
	got, _ := rig.store.RecurringExpenseByID(ctx, rig.scope, sched)
	if got.BucketID != nil {
		t.Error("archiving a bill left a schedule charging it")
	}

	// Editing the expense shows the removed bill selected and keeps it.
	form := rig.do("GET", fmt.Sprintf("/transactions/%d/edit", id), nil).Body.String()
	if !strings.Contains(form, fmt.Sprintf(`<option value="%d" selected`, bill)) || !strings.Contains(form, "Power (removed)") {
		t.Fatal("the edit form does not show the removed bill as selected")
	}
	rec := rig.post(fmt.Sprintf("/transactions/%d/edit", id), url.Values{
		"kind": {"expense"}, "label": {"Electric"}, "amount": {"50.00"}, "date": {store.Today()},
		"note": {"typo fixed"}, "bucket_id": {fmt.Sprint(bill)}, "essential": {"yes"},
		"version": {hiddenValue(t, form, "version")},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("edit: %d", rec.Code)
	}
	tx, _ := rig.store.ByID(ctx, rig.scope, id)
	if tx.BucketID == nil || *tx.BucketID != bill {
		t.Error("editing the note dropped the link to the removed bill")
	}

	// A new recurring expense pointed at the removed bill is a 400, not a 500.
	page := rig.do("GET", "/expense?step=manual", nil).Body.String()
	rec = rig.post("/expense", url.Values{
		"form_token": {hiddenValue(t, page, "form_token")}, "expense_type": {"recurring"},
		"label": {"Power"}, "amount": {"50"}, "date": {store.Today()},
		"frequency_n": {"1"}, "frequency_unit": {"month"}, "bucket_id": {fmt.Sprint(bill)},
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "has been removed") {
		t.Errorf("recurring to a removed bill: %d", rec.Code)
	}
}

// ── 20: Add Expense keeps typed item rows when a save is refused ─────────────

func TestAddExpenseKeepsItemsOnRefusal(t *testing.T) {
	rig := newRig(t)
	rig.login()
	page := rig.do("GET", "/expense?step=manual", nil).Body.String()
	rec := rig.post("/expense", url.Values{
		"form_token": {hiddenValue(t, page, "form_token")}, "expense_type": {"one_time"},
		"label": {"Groceries"}, "amount": {"10.00"}, "date": {store.Today()},
		"item_description": {"Milk", "Bread", ""}, "item_category": {"Food", "Food", ""},
		"item_amount": {"3.00", "4.00", ""},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unbalanced items: %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`value="Milk"`, `value="Bread"`, `value="4.00"`} {
		if !strings.Contains(body, want) {
			t.Errorf("refused page lost %s", want)
		}
	}
}

// ── 6, 7, 8: the emergency fund ───────────────────────────────────────────────

func TestEmergencyTargetCountsEssentialSchedulesAndIgnoresPeriod(t *testing.T) {
	rig := newRig(t)
	rig.login()
	ctx := context.Background()

	// An essential $500/month insurance schedule with no bill.
	if _, err := rig.store.CreateRecurringExpense(ctx, rig.scope, "Insurance", 50000, nil, true, 1, "month", "2099-01-01"); err != nil {
		t.Fatal(err)
	}
	body := rig.do("GET", "/dashboard", nil).Body.String()
	if !strings.Contains(body, "$1,500.00 target (3 months of essentials)") {
		t.Error("an essential automatic expense did not size the emergency target")
	}

	// The emergency fund's form asks for months of essentials, not a deadline.
	if !strings.Contains(body, "Months of essential expenses to aim for") {
		t.Error("the emergency fund's form does not ask for months of essentials")
	}

	// Picking an old month does not change today's target.
	last := time.Now().AddDate(0, -2, 0).Format(store.MonthLayout)
	if _, err := rig.store.Add(ctx, rig.scope, store.NewTransaction{
		Kind: store.KindExpense, Label: "Old", Amount: 100, OccurredOn: last + "-10",
	}); err != nil {
		t.Fatal(err)
	}
	old := rig.do("GET", "/dashboard?month="+last, nil).Body.String()
	if !strings.Contains(old, "$1,500.00 target (3 months of essentials)") {
		t.Error("the emergency target changed with the Period menu")
	}
}

// ── 10: future-dated entries ──────────────────────────────────────────────────

func TestFutureDatedEntryIsNotInTodaysFunds(t *testing.T) {
	rig := newRig(t)
	rig.login()
	ctx := context.Background()
	today, _ := time.Parse(store.DateLayout, store.Today())
	rig.store.Add(ctx, rig.scope, store.NewTransaction{Kind: store.KindIncome, Label: "Pay", Amount: 100000, OccurredOn: store.Today()})
	future := today.AddDate(0, 0, 5).Format(store.DateLayout)
	rig.store.Add(ctx, rig.scope, store.NewTransaction{Kind: store.KindExpense, Label: "Later", Amount: 30000, OccurredOn: future})

	body := rig.do("GET", "/dashboard", nil).Body.String()
	m := regexp.MustCompile(`(?s)tab-current.*?<span class="tab-value[^"]*">([^<]+)<`).FindStringSubmatch(body)
	if m == nil || !strings.Contains(m[1], "$1,000.00") {
		got := ""
		if m != nil {
			got = m[1]
		}
		t.Errorf("Current Funds shows %q, want $1,000.00 (the expense dated %s has not happened)", got, future)
	}
}

// ── 4, 9, 10: the forecast ───────────────────────────────────────────────────

func TestForecastUsesFullMonthsAndKnowsWhatIsPaid(t *testing.T) {
	today, _ := time.Parse(store.DateLayout, store.Today())
	day := func(n int) string { return today.AddDate(0, 0, n).Format(store.DateLayout) }
	thisMonth := today.Format(store.MonthLayout)
	prev := func(n int) string { return today.AddDate(0, -n, -today.Day()+1).Format(store.MonthLayout) }

	// $3,000 a month for three full months; $50 so far this month.
	monthly := []store.MonthPoint{
		{Month: prev(3), Expense: 300000}, {Month: prev(2), Expense: 300000},
		{Month: prev(1), Expense: 300000}, {Month: thisMonth, Expense: 5000},
	}
	daily := dailySpendingEstimate(monthly, today, 0, 0)
	if daily < 95 || daily > 105 {
		t.Errorf("daily spending = %.2f, want about 98.56 ($3,000 / 30.44)", daily)
	}

	// A brand-new account falls back to this month's pace so far.
	fresh := []store.MonthPoint{{Month: thisMonth, Expense: int64ToCents(int64(today.Day()) * 1000)}}
	if got := dailySpendingEstimate(fresh, today, 0, 0); got < 9.99 || got > 10.01 {
		t.Errorf("new account daily spending = %.2f, want 10.00", got)
	}

	// A bill already paid this month is not taken off again on its due date.
	dueDay := today.AddDate(0, 0, 3)
	paid := store.Bucket{ID: 1, Name: "Rent", DueDay: dueDay.Day(), Due: 100000, Spent: 100000}
	if dueDay.Month() == today.Month() {
		if when, amt := nextBillPayment(paid, store.Today()); when == day(3) || amt != 100000 {
			t.Errorf("paid bill next payment = %s %s, want next month in full", when, amt.Display())
		}
	}
	// A bill due today and unpaid is applied tomorrow rather than dropped.
	changes := map[string]float64{}
	addOccurrences(changes, store.Today(), 0, 1, "month", -500, today, today.AddDate(0, 0, 5))
	if changes[day(1)] != -500 {
		t.Errorf("due-today occurrence: %v, want -500 tomorrow", changes)
	}
}

func int64ToCents(v int64) store.Cents { return store.Cents(v) }

// ── 15: the invitation limit holds under a burst ─────────────────────────────

func TestInviteLimitHoldsUnderABurst(t *testing.T) {
	rig := newRig(t)
	ctx := context.Background()
	done := make(chan bool, 100)
	for i := 0; i < 100; i++ {
		go func() {
			ok, err := rig.store.RateTake(ctx, "invite|from|burst", store.InviteLimit)
			done <- ok && err == nil
		}()
	}
	taken := 0
	for i := 0; i < 100; i++ {
		if <-done {
			taken++
		}
	}
	if taken != int(store.InviteLimit.Max) {
		t.Errorf("a burst of 100 took %d slots, want exactly %d", taken, store.InviteLimit.Max)
	}
}

// ── 19 is covered in the worker package (illegible totals). ─────────────────
