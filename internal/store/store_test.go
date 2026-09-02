package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jthomasw/YABA-2026/internal/db"
	"github.com/jthomasw/YABA-2026/internal/money"
	"github.com/jthomasw/YABA-2026/internal/store"
)

// newTestStore builds a real, migrated SQLite database in the test's temp
// directory.
//
// A real database rather than a mock, because the behaviour worth testing here
// lives in the SQL and the CHECK constraints, and a mock would assert only that
// the code calls the functions the test author expected.
func newTestStore(t *testing.T) (*store.Store, store.Scope) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "test.db")
	sqlDB, err := db.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })

	if err := db.Migrate(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	st := store.New(sqlDB)
	uid, err := st.CreateUser(context.Background(), "tester@example.com", "not-a-real-hash")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Resolve the household CreateUser made rather than assuming its id. Doing
	// it through ActiveHousehold means every test also exercises the resolution
	// path the middleware uses on each request.
	m, err := st.ActiveHousehold(context.Background(), store.User{ID: uid})
	if err != nil {
		t.Fatalf("active household: %v", err)
	}
	return st, store.Scope{HouseholdID: m.ID, UserID: uid}
}

// newSecondUser adds another account with its own personal household, for tests
// that need to prove one household cannot reach another's rows.
func newSecondUser(t *testing.T, st *store.Store, email string) store.Scope {
	t.Helper()
	uid, err := st.CreateUser(context.Background(), email, "not-a-real-hash")
	if err != nil {
		t.Fatalf("create second user: %v", err)
	}
	m, err := st.ActiveHousehold(context.Background(), store.User{ID: uid})
	if err != nil {
		t.Fatalf("active household: %v", err)
	}
	return store.Scope{HouseholdID: m.ID, UserID: uid}
}

func addIncome(t *testing.T, st *store.Store, sc store.Scope, amount money.Cents, label, date string) int64 {
	t.Helper()
	id, err := st.Add(context.Background(), sc, store.NewTransaction{
		Kind: store.KindIncome, Label: label, Amount: amount, OccurredOn: date,
	})
	if err != nil {
		t.Fatalf("add income: %v", err)
	}
	return id
}

func addExpense(t *testing.T, st *store.Store, sc store.Scope, amount money.Cents, label, date string, essential bool) int64 {
	t.Helper()
	id, err := st.Add(context.Background(), sc, store.NewTransaction{
		Kind: store.KindExpense, Label: label, Amount: amount,
		OccurredOn: date, Essential: &essential,
	})
	if err != nil {
		t.Fatalf("add expense: %v", err)
	}
	return id
}

// ── the exploit ───────────────────────────────────────────────────────────────

// TestCloseFundCannotBeToldTheBalance is the regression test for the old
// /delete-fund handler, which credited a balance read from a hidden form field.
// The API here gives a caller no way to supply one: CloseFund takes only ids.
func TestCloseFundReturnsOnlyTheDerivedBalance(t *testing.T) {
	ctx := context.Background()
	st, sc := newTestStore(t)

	addIncome(t, st, sc, 100000, "Salary", "2026-01-01") // $1,000
	fundID, err := st.CreateFund(ctx, sc, "Rainy day", 50000, 0)
	if err != nil {
		t.Fatalf("create fund: %v", err)
	}
	if err := st.Deposit(ctx, sc, fundID, 20000, "2026-01-02"); err != nil {
		t.Fatalf("deposit: %v", err)
	}

	returned, err := st.CloseFund(ctx, sc, fundID)
	if err != nil {
		t.Fatalf("close fund: %v", err)
	}
	if returned != 20000 {
		t.Errorf("returned %s, want $200.00", returned.Display())
	}

	// Net worth is unchanged by the whole deposit-and-close cycle.
	totals, err := st.Totals(ctx, sc, "")
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	if totals.NetWorth() != 100000 {
		t.Errorf("net worth = %s, want $1,000.00", totals.NetWorth().Display())
	}
	if totals.Cash() != 100000 {
		t.Errorf("cash = %s, want $1,000.00 back in full", totals.Cash().Display())
	}

	// Closing a fund must not look like earnings: income is untouched.
	if totals.Income != 100000 {
		t.Errorf("income = %s; closing a fund must not create income", totals.Income.Display())
	}
}

func TestDepositRejectsMoreThanAvailableCash(t *testing.T) {
	ctx := context.Background()
	st, sc := newTestStore(t)

	addIncome(t, st, sc, 10000, "Salary", "2026-01-01") // $100
	fundID, err := st.CreateFund(ctx, sc, "Car", 0, 0)
	if err != nil {
		t.Fatalf("create fund: %v", err)
	}

	err = st.Deposit(ctx, sc, fundID, 15000, "2026-01-02") // $150
	if !errors.Is(err, store.ErrInsufficientCash) {
		t.Fatalf("want ErrInsufficientCash, got %v", err)
	}

	// The rejected transfer must leave nothing behind.
	f, err := st.FundByID(ctx, sc, fundID)
	if err != nil {
		t.Fatalf("fund: %v", err)
	}
	if f.Balance != 0 {
		t.Errorf("fund balance = %s after a rejected deposit", f.Balance.Display())
	}
	cash, _ := st.Cash(ctx, sc)
	if cash != 10000 {
		t.Errorf("cash = %s, want $100.00 untouched", cash.Display())
	}
}

func TestWithdrawRejectsMoreThanTheFundHolds(t *testing.T) {
	ctx := context.Background()
	st, sc := newTestStore(t)

	addIncome(t, st, sc, 100000, "Salary", "2026-01-01")
	fundID, _ := st.CreateFund(ctx, sc, "Trip", 0, 0)
	if err := st.Deposit(ctx, sc, fundID, 5000, "2026-01-02"); err != nil {
		t.Fatalf("deposit: %v", err)
	}

	err := st.Withdraw(ctx, sc, fundID, 9000, "2026-01-03")
	if !errors.Is(err, store.ErrInsufficientFund) {
		t.Fatalf("want ErrInsufficientFund, got %v", err)
	}

	f, _ := st.FundByID(ctx, sc, fundID)
	if f.Balance != 5000 {
		t.Errorf("fund balance = %s, want $50.00", f.Balance.Display())
	}
}

func TestFundBalanceIsDerivedNotStored(t *testing.T) {
	ctx := context.Background()
	st, sc := newTestStore(t)

	addIncome(t, st, sc, 100000, "Salary", "2026-01-01")
	fundID, _ := st.CreateFund(ctx, sc, "Medical", 100000, 10)

	for _, amt := range []money.Cents{1000, 2000, 3000} {
		if err := st.Deposit(ctx, sc, fundID, amt, "2026-01-02"); err != nil {
			t.Fatalf("deposit %d: %v", amt, err)
		}
	}
	if err := st.Withdraw(ctx, sc, fundID, 1500, "2026-01-03"); err != nil {
		t.Fatalf("withdraw: %v", err)
	}

	f, err := st.FundByID(ctx, sc, fundID)
	if err != nil {
		t.Fatalf("fund: %v", err)
	}
	// 10 + 20 + 30 - 15 = 45
	if f.Balance != 4500 {
		t.Errorf("balance = %s, want $45.00", f.Balance.Display())
	}
	if f.Remaining() != 95500 {
		t.Errorf("remaining = %s, want $955.00", f.Remaining().Display())
	}
}

// ── transfers are not spending ─────────────────────────────────────────────────

func TestDepositIsNotCountedAsSpending(t *testing.T) {
	ctx := context.Background()
	st, sc := newTestStore(t)

	addIncome(t, st, sc, 100000, "Salary", "2026-03-01")
	addExpense(t, st, sc, 20000, "Food", "2026-03-02", true)
	fundID, _ := st.CreateFund(ctx, sc, "Emergency", 0, 0)
	if err := st.Deposit(ctx, sc, fundID, 30000, "2026-03-03"); err != nil {
		t.Fatalf("deposit: %v", err)
	}

	totals, _ := st.Totals(ctx, sc, "")
	if totals.Expense != 20000 {
		t.Errorf("expense = %s, want $200.00; a savings transfer is not spending",
			totals.Expense.Display())
	}
	if totals.Cash() != 50000 {
		t.Errorf("cash = %s, want $500.00", totals.Cash().Display())
	}

	// The old code inserted an expense row labelled "Emergency Fund", which put
	// the transfer into the spending pie chart. It must not appear here.
	byCat, err := st.Breakdown(ctx, sc, store.KindExpense, "")
	if err != nil {
		t.Fatalf("breakdown: %v", err)
	}
	if len(byCat) != 1 || byCat[0].Label != "Food" {
		t.Errorf("spending breakdown = %+v, want only Food", byCat)
	}
}

// ── ownership ─────────────────────────────────────────────────────────────────

func TestOneUserCannotTouchAnothersRows(t *testing.T) {
	ctx := context.Background()
	st, alice := newTestStore(t)

	// Bob needs a Scope, not a bare user id: since shared budgeting, ownership
	// is the household and every store method takes both halves. A user id on
	// its own no longer identifies whose money it is.
	bob := newSecondUser(t, st, "bob@example.com")

	txID := addExpense(t, st, alice, 5000, "Food", "2026-01-01", true)
	fundID, _ := st.CreateFund(ctx, alice, "Alice fund", 0, 0)

	if err := st.Delete(ctx, bob, txID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bob deleting alice's transaction: got %v, want ErrNotFound", err)
	}
	if _, err := st.ByID(ctx, bob, txID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bob reading alice's transaction: got %v, want ErrNotFound", err)
	}
	if err := st.Update(ctx, bob, txID, store.NewTransaction{
		Kind: store.KindExpense, Label: "hacked", Amount: 1, OccurredOn: "2026-01-01",
	}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bob updating alice's transaction: got %v, want ErrNotFound", err)
	}
	if _, err := st.FundByID(ctx, bob, fundID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bob reading alice's fund: got %v, want ErrNotFound", err)
	}
	if _, err := st.CloseFund(ctx, bob, fundID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bob closing alice's fund: got %v, want ErrNotFound", err)
	}

	// Alice's row is intact after all of that.
	tx, err := st.ByID(ctx, alice, txID)
	if err != nil {
		t.Fatalf("alice reading her own row: %v", err)
	}
	if tx.Label != "Food" || tx.Amount != 5000 {
		t.Errorf("alice's row was modified: %+v", tx)
	}
}

// ── validation at the storage boundary ────────────────────────────────────────

func TestAddRejectsNonPositiveAmounts(t *testing.T) {
	ctx := context.Background()
	st, sc := newTestStore(t)

	for _, amt := range []money.Cents{0, -1, -10000} {
		_, err := st.Add(ctx, sc, store.NewTransaction{
			Kind: store.KindIncome, Label: "Bad", Amount: amt, OccurredOn: "2026-01-01",
		})
		if err == nil {
			t.Errorf("Add accepted amount %d; negative income was possible in the old code", amt)
		}
	}
}

func TestAddRefusesToCreateTransfersDirectly(t *testing.T) {
	ctx := context.Background()
	st, sc := newTestStore(t)

	for _, k := range []store.Kind{store.KindFundDeposit, store.KindFundWithdrawal} {
		_, err := st.Add(ctx, sc, store.NewTransaction{
			Kind: k, Label: "sneaky", Amount: 1000, OccurredOn: "2026-01-01",
		})
		if err == nil {
			t.Errorf("Add accepted %s; transfers must go through Deposit/Withdraw "+
				"so the balance rules are enforced", k)
		}
	}
}

func TestUpdateCannotConvertATransferIntoAnExpense(t *testing.T) {
	ctx := context.Background()
	st, sc := newTestStore(t)

	addIncome(t, st, sc, 100000, "Salary", "2026-01-01")
	fundID, _ := st.CreateFund(ctx, sc, "Fund", 0, 0)
	if err := st.Deposit(ctx, sc, fundID, 10000, "2026-01-02"); err != nil {
		t.Fatalf("deposit: %v", err)
	}

	// Find the transfer row.
	txs, _, err := st.List(ctx, sc, store.Filter{Kind: store.KindFundDeposit})
	if err != nil || len(txs) != 1 {
		t.Fatalf("list transfers: %v %+v", err, txs)
	}

	err = st.Update(ctx, sc, txs[0].ID, store.NewTransaction{
		Kind: store.KindExpense, Label: "converted", Amount: 10000, OccurredOn: "2026-01-02",
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("converting a transfer should not be possible, got %v", err)
	}

	// The fund still holds its money.
	f, _ := st.FundByID(ctx, sc, fundID)
	if f.Balance != 10000 {
		t.Errorf("fund balance = %s, want $100.00", f.Balance.Display())
	}
}

func TestParseDateRejectsGarbage(t *testing.T) {
	if _, err := store.ParseDate("tomorrow"); err == nil {
		t.Error("ParseDate accepted 'tomorrow'")
	}
	if _, err := store.ParseDate("2026-13-45"); err == nil {
		t.Error("ParseDate accepted an impossible date")
	}
	if _, err := store.ParseDate("20026-01-01"); err == nil {
		t.Error("ParseDate accepted a five-digit year")
	}
	if got, err := store.ParseDate(""); err != nil || got != store.Today() {
		t.Errorf("empty date should default to today, got %q %v", got, err)
	}
	if got, err := store.ParseDate("2026-04-18"); err != nil || got != "2026-04-18" {
		t.Errorf("valid date round trip failed: %q %v", got, err)
	}
}

func TestNormalizeEmailPreventsCaseDuplicates(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)

	if _, err := st.CreateUser(ctx, "Kushith@Example.com", "hash"); err != nil {
		t.Fatalf("create: %v", err)
	}
	// A COLLATE NOCASE unique index is what stops these becoming two accounts;
	// a plain UNIQUE would allow both, because SQLite compares TEXT
	// case-sensitively by default.
	_, err := st.CreateUser(ctx, "  KUSHITH@EXAMPLE.COM ", "hash")
	if !errors.Is(err, store.ErrEmailTaken) {
		t.Errorf("want ErrEmailTaken for a case/space variant, got %v", err)
	}
}

func TestCredentialsForMatchesLegacyUsername(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)

	// Migration 3 backfilled email from the old username column, so an account
	// created before the change signs in with the string it always used. Losing
	// that would lock every existing user out of their own data.
	if _, err := st.CreateUser(ctx, "legacyname", "hash"); err != nil {
		t.Fatalf("create: %v", err)
	}
	u, hash, err := st.CredentialsFor(ctx, "LegacyName")
	if err != nil {
		t.Fatalf("legacy lookup failed: %v", err)
	}
	if hash != "hash" || u.ID == 0 {
		t.Errorf("unexpected result: %+v %q", u, hash)
	}
}

func TestEmailExists(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)

	// The combined login/signup form branches on this, so a wrong answer either
	// asks an existing user to create an account or tries to log a new one in.
	ok, err := st.EmailExists(ctx, "nobody@example.com")
	if err != nil || ok {
		t.Errorf("unknown address: got %v, %v", ok, err)
	}
	if _, err := st.CreateUser(ctx, "someone@example.com", "hash"); err != nil {
		t.Fatalf("create: %v", err)
	}
	ok, err = st.EmailExists(ctx, "SOMEONE@EXAMPLE.COM")
	if err != nil || !ok {
		t.Errorf("known address, different case: got %v, %v", ok, err)
	}
}

// ── filtering, listing and aggregates ─────────────────────────────────────────

func TestMonthFilterUsesAHalfOpenRange(t *testing.T) {
	ctx := context.Background()
	st, sc := newTestStore(t)

	addExpense(t, st, sc, 1000, "Food", "2026-03-31", true) // last day of March
	addExpense(t, st, sc, 2000, "Food", "2026-04-01", true) // first day of April
	addExpense(t, st, sc, 3000, "Food", "2026-04-30", true) // last day of April
	addExpense(t, st, sc, 4000, "Food", "2026-05-01", true) // first day of May

	april, err := st.Totals(ctx, sc, "2026-04")
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	// Boundaries are the classic off-by-one in date range filters: April must
	// include the 1st and the 30th, and exclude March 31 and May 1.
	if april.Expense != 5000 {
		t.Errorf("April spending = %s, want $50.00", april.Expense.Display())
	}

	all, _ := st.Totals(ctx, sc, "")
	if all.Expense != 10000 {
		t.Errorf("all-time spending = %s, want $100.00", all.Expense.Display())
	}
}

func TestPaginationCoversEveryRowExactlyOnce(t *testing.T) {
	ctx := context.Background()
	st, sc := newTestStore(t)

	const n = 57
	for i := 0; i < n; i++ {
		addExpense(t, st, sc, money.Cents(100+i), "Item", "2026-01-01", true)
	}

	seen := map[int64]int{}
	pageSize := 10
	for offset := 0; offset < n; offset += pageSize {
		txs, total, err := st.List(ctx, sc, store.Filter{Limit: pageSize, Offset: offset})
		if err != nil {
			t.Fatalf("list offset %d: %v", offset, err)
		}
		if total != n {
			t.Fatalf("total = %d, want %d", total, n)
		}
		for _, tx := range txs {
			seen[tx.ID]++
		}
	}

	if len(seen) != n {
		t.Errorf("saw %d distinct rows across all pages, want %d", len(seen), n)
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("row %d appeared %d times; the ORDER BY is not a total order", id, count)
		}
	}
}

func TestSearchTreatsWildcardsAsLiterals(t *testing.T) {
	ctx := context.Background()
	st, sc := newTestStore(t)

	addExpense(t, st, sc, 1000, "100% cotton", "2026-01-01", true)
	addExpense(t, st, sc, 2000, "Food", "2026-01-01", true)

	// A bare "%" would match everything if it were passed through as a wildcard.
	txs, total, err := st.List(ctx, sc, store.Filter{Search: "%"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(txs) != 1 || txs[0].Label != "100% cotton" {
		t.Errorf("search for %% matched %d rows (%+v); it must be a literal", total, txs)
	}
}

func TestEssentialSplit(t *testing.T) {
	ctx := context.Background()
	st, sc := newTestStore(t)

	addExpense(t, st, sc, 10000, "Rent", "2026-02-01", true)
	addExpense(t, st, sc, 3000, "Games", "2026-02-02", false)
	addExpense(t, st, sc, 2000, "Food", "2026-02-03", true)
	addIncome(t, st, sc, 50000, "Salary", "2026-02-01")

	essential, other, err := st.EssentialSplit(ctx, sc, "2026-02")
	if err != nil {
		t.Fatalf("essential split: %v", err)
	}
	if essential != 12000 {
		t.Errorf("essential = %s, want $120.00", essential.Display())
	}
	if other != 3000 {
		t.Errorf("non-essential = %s, want $30.00", other.Display())
	}
}

func TestBreakdownGroupsBlankLabelsTogether(t *testing.T) {
	ctx := context.Background()
	st, sc := newTestStore(t)

	// Add returns an error for a blank label only at the handler layer, so a
	// row with whitespace can still reach the store; the breakdown must not
	// produce an unnamed slice.
	if _, err := st.Add(ctx, sc, store.NewTransaction{
		Kind: store.KindExpense, Label: "   ", Amount: 1000, OccurredOn: "2026-01-01",
	}); err != nil {
		t.Fatalf("add: %v", err)
	}

	byCat, err := st.Breakdown(ctx, sc, store.KindExpense, "")
	if err != nil {
		t.Fatalf("breakdown: %v", err)
	}
	if len(byCat) != 1 || byCat[0].Label != "Uncategorised" {
		t.Errorf("breakdown = %+v, want a single Uncategorised slice", byCat)
	}
}

func TestBalanceSeriesIsOnePointPerDay(t *testing.T) {
	ctx := context.Background()
	st, sc := newTestStore(t)

	// Five transactions across two days must produce two points, not five.
	addIncome(t, st, sc, 10000, "A", "2026-01-01")
	addIncome(t, st, sc, 10000, "B", "2026-01-01")
	addExpense(t, st, sc, 5000, "C", "2026-01-01", true)
	addIncome(t, st, sc, 20000, "D", "2026-01-02")
	addExpense(t, st, sc, 1000, "E", "2026-01-02", true)

	series, err := st.BalanceSeries(ctx, sc)
	if err != nil {
		t.Fatalf("balance series: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("got %d points, want 2 (one per day): %+v", len(series), series)
	}
	if series[0].Balance != 15000 {
		t.Errorf("day 1 running balance = %s, want $150.00", series[0].Balance.Display())
	}
	if series[1].Balance != 34000 {
		t.Errorf("day 2 running balance = %s, want $340.00", series[1].Balance.Display())
	}
}

func TestMonthlySeriesFillsEmptyMonths(t *testing.T) {
	ctx := context.Background()
	st, sc := newTestStore(t)

	series, err := st.MonthlySeries(ctx, sc, 6)
	if err != nil {
		t.Fatalf("monthly series: %v", err)
	}
	// A gap must render as a zero bar, not be skipped, or the chart's x-axis
	// silently compresses time.
	if len(series) != 6 {
		t.Errorf("got %d months, want 6 even with no data", len(series))
	}
}

// ── budgets ───────────────────────────────────────────────────────────────────

func TestBudgetSpendMatchesCaseInsensitively(t *testing.T) {
	ctx := context.Background()
	st, sc := newTestStore(t)

	month := store.Today()[:7]
	today := store.Today()

	addExpense(t, st, sc, 3000, "Food", today, true)
	addExpense(t, st, sc, 2000, "food", today, true)
	addExpense(t, st, sc, 1000, " FOOD ", today, true)
	addExpense(t, st, sc, 9900, "Rent", today, true)

	if err := st.SetBudget(ctx, sc, "food", 5000); err != nil {
		t.Fatalf("set budget: %v", err)
	}

	budgets, err := st.ListBudgets(ctx, sc, month)
	if err != nil {
		t.Fatalf("list budgets: %v", err)
	}
	if len(budgets) != 1 {
		t.Fatalf("got %d budgets, want 1", len(budgets))
	}
	// All three spellings of Food count, Rent does not.
	if budgets[0].Spent != 6000 {
		t.Errorf("spent = %s, want $60.00 across all spellings", budgets[0].Spent.Display())
	}
	if !budgets[0].Over() {
		t.Error("a $60 spend against a $50 budget should be over")
	}
}

func TestSetBudgetIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st, sc := newTestStore(t)

	if err := st.SetBudget(ctx, sc, "Food", 5000); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := st.SetBudget(ctx, sc, "Food", 9000); err != nil {
		t.Fatalf("second: %v", err)
	}

	budgets, _ := st.ListBudgets(ctx, sc, store.Today()[:7])
	if len(budgets) != 1 {
		t.Fatalf("got %d budgets, want 1 updated in place", len(budgets))
	}
	if budgets[0].Limit != 9000 {
		t.Errorf("limit = %s, want $90.00", budgets[0].Limit.Display())
	}
}

func TestSetBudgetRejectsNonPositiveLimit(t *testing.T) {
	ctx := context.Background()
	st, sc := newTestStore(t)

	for _, limit := range []money.Cents{0, -100} {
		if err := st.SetBudget(ctx, sc, "Food", limit); err == nil {
			t.Errorf("SetBudget accepted limit %d", limit)
		}
	}
}

// ── editing ───────────────────────────────────────────────────────────────────

func TestUpdatePreservesCreatedAtOrdering(t *testing.T) {
	ctx := context.Background()
	st, sc := newTestStore(t)

	first := addExpense(t, st, sc, 1000, "First", "2026-01-01", true)
	addExpense(t, st, sc, 2000, "Second", "2026-01-02", true)

	before, err := st.ByID(ctx, sc, first)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	essential := false
	if err := st.Update(ctx, sc, first, store.NewTransaction{
		Kind: store.KindExpense, Label: "First edited", Amount: 1500,
		OccurredOn: "2026-01-01", Essential: &essential,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	after, err := st.ByID(ctx, sc, first)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	// Editing must not reorder history, which is what delete-and-retype did.
	if after.CreatedAt != before.CreatedAt {
		t.Errorf("created_at changed on edit: %q -> %q", before.CreatedAt, after.CreatedAt)
	}
	if after.Label != "First edited" || after.Amount != 1500 {
		t.Errorf("edit did not apply: %+v", after)
	}
	if after.Essential == nil || *after.Essential {
		t.Errorf("essential flag not updated: %+v", after.Essential)
	}
}

func TestDeleteIsScopedAndReportsMissingRows(t *testing.T) {
	ctx := context.Background()
	st, sc := newTestStore(t)

	id := addExpense(t, st, sc, 1000, "Food", "2026-01-01", true)
	if err := st.Delete(ctx, sc, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Deleting twice reports not-found rather than silently succeeding, so the
	// handler can tell the user what happened.
	if err := st.Delete(ctx, sc, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second delete: got %v, want ErrNotFound", err)
	}
}

func TestClosedFundsDisappearFromListButKeepHistory(t *testing.T) {
	ctx := context.Background()
	st, sc := newTestStore(t)

	addIncome(t, st, sc, 100000, "Salary", "2026-01-01")
	fundID, _ := st.CreateFund(ctx, sc, "Temp", 0, 0)
	if err := st.Deposit(ctx, sc, fundID, 5000, "2026-01-02"); err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if _, err := st.CloseFund(ctx, sc, fundID); err != nil {
		t.Fatalf("close: %v", err)
	}

	funds, err := st.ListFunds(ctx, sc)
	if err != nil {
		t.Fatalf("list funds: %v", err)
	}
	if len(funds) != 0 {
		t.Errorf("closed fund still listed: %+v", funds)
	}

	// The deposit and the closing withdrawal both remain, so the ledger still
	// explains where the money went. The old code deleted the fund row.
	txs, total, err := st.List(ctx, sc, store.Filter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 3 {
		t.Errorf("got %d transactions, want 3 (income, deposit, closing withdrawal): %+v", total, txs)
	}
}

// ── attaching an uploaded receipt to an expense ───────────────────────────────
//
// Before this existed the queue ran, the file was stored, the notification was
// delivered, and then nothing could ever consume it: `transaction_id` stayed
// NULL forever. These tests cover the closing of that loop, and the two ways it
// could close wrongly -- twice, or across a household boundary.

func TestUnattachedReceiptIsFoundThenConsumed(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	jobID, err := st.EnqueueReceipt(ctx, sc, "uploads/1/abc.png", "lidl.png")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// The worker could not read the amount, so it completes with no transaction.
	if err := st.CompleteReceiptJob(ctx, jobID, nil); err != nil {
		t.Fatalf("complete: %v", err)
	}

	job, err := st.UnattachedReceipt(ctx, sc, jobID)
	if err != nil {
		t.Fatalf("an unread receipt should be available to attach: %v", err)
	}
	if job.OriginalName != "lidl.png" {
		t.Errorf("original name is %q", job.OriginalName)
	}

	txID := addExpense(t, st, sc, 1250, "Groceries", "2026-08-01", true)
	if err := st.AttachReceipt(ctx, sc, jobID, txID); err != nil {
		t.Fatalf("attach: %v", err)
	}

	// The expense now carries the file...
	tx, err := st.ByID(ctx, sc, txID)
	if err != nil {
		t.Fatalf("by id: %v", err)
	}
	if tx.ReceiptPath != "uploads/1/abc.png" || tx.ReceiptName != "lidl.png" {
		t.Errorf("receipt not copied onto the expense: %q / %q", tx.ReceiptPath, tx.ReceiptName)
	}

	// ...and the receipt is no longer on offer.
	if _, err := st.UnattachedReceipt(ctx, sc, jobID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an attached receipt is still offered: %v", err)
	}
}

// TestAReceiptCannotBeAttachedTwice: without the IS NULL condition in the write,
// a resubmitted form or a double-tapped notification would put the same file on
// two expenses.
func TestAReceiptCannotBeAttachedTwice(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	jobID, err := st.EnqueueReceipt(ctx, sc, "uploads/1/abc.png", "lidl.png")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := st.CompleteReceiptJob(ctx, jobID, nil); err != nil {
		t.Fatalf("complete: %v", err)
	}

	first := addExpense(t, st, sc, 1250, "Groceries", "2026-08-01", true)
	if err := st.AttachReceipt(ctx, sc, jobID, first); err != nil {
		t.Fatalf("first attach: %v", err)
	}

	second := addExpense(t, st, sc, 999, "Groceries again", "2026-08-02", true)
	if err := st.AttachReceipt(ctx, sc, jobID, second); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the same receipt attached twice: %v", err)
	}

	tx, err := st.ByID(ctx, sc, second)
	if err != nil {
		t.Fatalf("by id: %v", err)
	}
	if tx.ReceiptPath != "" {
		t.Errorf("the second expense picked up the receipt anyway: %q", tx.ReceiptPath)
	}
}

// TestOneHouseholdCannotAttachAnothersReceipt is the insecure-direct-object-
// reference case: the job id is a small integer in a URL, so guessing one must
// not be enough.
func TestOneHouseholdCannotAttachAnothersReceipt(t *testing.T) {
	st, alice := newTestStore(t)
	bob := newSecondUser(t, st, "bob@example.com")
	ctx := context.Background()

	jobID, err := st.EnqueueReceipt(ctx, alice, "uploads/1/abc.png", "alice-receipt.png")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := st.CompleteReceiptJob(ctx, jobID, nil); err != nil {
		t.Fatalf("complete: %v", err)
	}

	if _, err := st.UnattachedReceipt(ctx, bob, jobID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bob can see alice's receipt: %v", err)
	}

	bobsExpense := addExpense(t, st, bob, 500, "Nothing to do with alice", "2026-08-01", true)
	if err := st.AttachReceipt(ctx, bob, jobID, bobsExpense); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bob attached alice's receipt: %v", err)
	}

	// And alice's receipt is untouched, still available to her.
	if _, err := st.UnattachedReceipt(ctx, alice, jobID); err != nil {
		t.Errorf("alice's receipt was consumed by bob's attempt: %v", err)
	}
}

// TestUnattachedReceiptsListsOnlyWhatIsWaiting keeps the Add Expense list honest:
// queued work is not ready, and an imported receipt is finished.
func TestUnattachedReceiptsListsOnlyWhatIsWaiting(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	// Still queued: not ready to be entered.
	if _, err := st.EnqueueReceipt(ctx, sc, "uploads/1/queued.png", "queued.png"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Processed, amount unreadable: this is the one to show.
	waiting, err := st.EnqueueReceipt(ctx, sc, "uploads/1/waiting.png", "waiting.png")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := st.CompleteReceiptJob(ctx, waiting, nil); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Already became an expense: finished, so hidden.
	done, err := st.EnqueueReceipt(ctx, sc, "uploads/1/done.png", "done.png")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	txID := addExpense(t, st, sc, 100, "Done", "2026-08-01", true)
	if err := st.CompleteReceiptJob(ctx, done, &txID); err != nil {
		t.Fatalf("complete: %v", err)
	}

	list, err := st.UnattachedReceipts(ctx, sc, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != waiting {
		names := []string{}
		for _, j := range list {
			names = append(names, j.OriginalName)
		}
		t.Fatalf("want only waiting.png, got %v", names)
	}
}

// TestUnattachedReceiptsIsScopedToTheHousehold: the list drives buttons that
// attach, so leaking a row here would leak an attachable id.
func TestUnattachedReceiptsIsScopedToTheHousehold(t *testing.T) {
	st, alice := newTestStore(t)
	bob := newSecondUser(t, st, "bob@example.com")
	ctx := context.Background()

	jobID, err := st.EnqueueReceipt(ctx, alice, "uploads/1/abc.png", "alice.png")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := st.CompleteReceiptJob(ctx, jobID, nil); err != nil {
		t.Fatalf("complete: %v", err)
	}

	list, err := st.UnattachedReceipts(ctx, bob, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("bob sees %d of alice's receipts", len(list))
	}
}

// ── invitations expire ────────────────────────────────────────────────────────

func TestInvitationExpiresAfter24Hours(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	shared, err := st.CreateSharedHousehold(ctx, sc.UserID, "Flat 4B")
	if err != nil {
		t.Fatalf("create household: %v", err)
	}
	if err := st.InviteMember(ctx, shared, sc.UserID, "guest@example.com", store.RoleEditor); err != nil {
		t.Fatalf("invite: %v", err)
	}

	guest := newSecondUser(t, st, "guest@example.com")

	// Fresh: offered.
	mine, err := st.InvitesFor(ctx, guest.UserID, "guest@example.com")
	if err != nil {
		t.Fatalf("invites for: %v", err)
	}
	if len(mine) != 1 {
		t.Fatalf("a fresh invitation is not offered: %d found", len(mine))
	}
	if mine[0].ExpiresAt == "" {
		t.Error("no expiry recorded on the invitation")
	}

	// Aged past its window: not offered any more.
	expireInvite(t, st, mine[0].ID)
	mine2, err := st.InvitesFor(ctx, guest.UserID, "guest@example.com")
	if err != nil {
		t.Fatalf("invites for: %v", err)
	}
	if len(mine2) != 0 {
		t.Errorf("an expired invitation is still offered: %d found", len(mine2))
	}

	// And accepting it is refused with a distinguishable error, so the page can
	// say "expired, ask for another" rather than "no longer available".
	err = st.AcceptInvite(ctx, mine[0].ID, guest.UserID, "guest@example.com")
	if !errors.Is(err, store.ErrInviteExpired) {
		t.Errorf("accept returned %v, want ErrInviteExpired", err)
	}
}

// TestOwnerStillSeesAnExpiredInvitation: it has to be visible, or the owner
// cannot tell the difference between "lapsed" and "I never sent it".
func TestOwnerStillSeesAnExpiredInvitation(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	shared, _ := st.CreateSharedHousehold(ctx, sc.UserID, "Flat 4B")
	if err := st.InviteMember(ctx, shared, sc.UserID, "guest@example.com", store.RoleViewer); err != nil {
		t.Fatalf("invite: %v", err)
	}
	pending, err := st.PendingInvites(ctx, shared)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending: %v (%d)", err, len(pending))
	}
	if pending[0].Expired {
		t.Error("a fresh invitation is flagged expired")
	}

	expireInvite(t, st, pending[0].ID)
	pending, err = st.PendingInvites(ctx, shared)
	if err != nil || len(pending) != 1 {
		t.Fatalf("an expired invitation vanished from the owner's list: %v (%d)", err, len(pending))
	}
	if !pending[0].Expired {
		t.Error("an expired invitation is not flagged")
	}
}

func TestResendGivesAnotherWindow(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	shared, _ := st.CreateSharedHousehold(ctx, sc.UserID, "Flat 4B")
	if err := st.InviteMember(ctx, shared, sc.UserID, "guest@example.com", store.RoleEditor); err != nil {
		t.Fatalf("invite: %v", err)
	}
	pending, _ := st.PendingInvites(ctx, shared)
	expireInvite(t, st, pending[0].ID)

	inv, err := st.ResendInvite(ctx, shared, pending[0].ID)
	if err != nil {
		t.Fatalf("resend: %v", err)
	}
	if inv.Email != "guest@example.com" {
		t.Errorf("resend returned the wrong invitation: %+v", inv)
	}

	guest := newSecondUser(t, st, "guest@example.com")
	mine, _ := st.InvitesFor(ctx, guest.UserID, "guest@example.com")
	if len(mine) != 1 {
		t.Errorf("after resending, the recipient is offered %d invitations", len(mine))
	}

	// A household cannot refresh an invitation belonging to another.
	other, _ := st.CreateSharedHousehold(ctx, sc.UserID, "Somewhere else")
	if _, err := st.ResendInvite(ctx, other, pending[0].ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("resend crossed a household boundary: %v", err)
	}
}

// ── password reset ────────────────────────────────────────────────────────────

func TestResetTokenIsSingleUse(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	token, err := st.CreateReset(ctx, sc.UserID)
	if err != nil {
		t.Fatalf("create reset: %v", err)
	}
	if len(token) < 20 {
		t.Errorf("token looks too short to be unguessable: %q", token)
	}

	if _, err := st.ResetUser(ctx, token); err != nil {
		t.Fatalf("a fresh token should resolve: %v", err)
	}
	if _, err := st.ConsumeReset(ctx, token, "new-hash"); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if _, err := st.ConsumeReset(ctx, token, "newer-hash"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the same token was accepted twice: %v", err)
	}
	if _, err := st.ResetUser(ctx, token); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a used token still resolves: %v", err)
	}
}

func TestRequestingASecondLinkInvalidatesTheFirst(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	first, _ := st.CreateReset(ctx, sc.UserID)
	second, _ := st.CreateReset(ctx, sc.UserID)
	if first == second {
		t.Fatal("two requests produced the same token")
	}
	if _, err := st.ResetUser(ctx, first); !errors.Is(err, store.ErrNotFound) {
		t.Error("the earlier link still works, so an attacker's request survives the owner's")
	}
	if _, err := st.ResetUser(ctx, second); err != nil {
		t.Errorf("the latest link does not work: %v", err)
	}
}

func TestUnknownResetTokenIsRefused(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	for _, tok := range []string{"", "nope", "0", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		if _, err := st.ResetUser(ctx, tok); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("token %q resolved: %v", tok, err)
		}
	}
}

// ── login attempts survive a restart ─────────────────────────────────────────

func TestRateLimitCounts(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	key := "1.2.3.4|someone@example.com"

	retry, err := st.RateRetryIn(ctx, key)
	if err != nil || retry != 0 {
		t.Fatalf("blocked before any failure: %v %v", retry, err)
	}
	for i := 0; i < store.RateMaxTries-1; i++ {
		if err := st.RateFail(ctx, key); err != nil {
			t.Fatalf("fail %d: %v", i, err)
		}
	}
	if retry, _ := st.RateRetryIn(ctx, key); retry != 0 {
		t.Errorf("blocked one attempt early, retry in %v", retry)
	}
	if err := st.RateFail(ctx, key); err != nil {
		t.Fatal(err)
	}

	// Blocked, and the wait is the whole window rather than an unknown amount:
	// the counter was started by the first failure moments ago.
	retry, _ = st.RateRetryIn(ctx, key)
	if retry <= 0 {
		t.Fatalf("not blocked after %d failures", store.RateMaxTries)
	}
	if retry > store.RateWindow {
		t.Errorf("retry in %v, which is longer than the whole window (%v)",
			retry, store.RateWindow)
	}
	if retry < store.RateWindow-time.Minute {
		t.Errorf("retry in %v, want close to the full %v", retry, store.RateWindow)
	}

	// A successful sign-in clears it.
	if err := st.RateReset(ctx, key); err != nil {
		t.Fatal(err)
	}
	if retry, _ := st.RateRetryIn(ctx, key); retry != 0 {
		t.Error("still blocked after a successful sign-in")
	}
}

// TestRateLimitCountsDown is what the message on the page depends on: the wait
// has to shrink as the window ages, or the countdown is decoration.
func TestRateLimitCountsDown(t *testing.T) {
	ctx := context.Background()
	key := "1.2.3.4|counting@example.com"

	// Its own database handle, because ageing the window without waiting ten real
	// minutes needs raw SQL, and Store deliberately does not expose its
	// connection. Widening the store's API for one test's convenience is the
	// worse trade of the two.
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "rate.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.Migrate(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st := store.New(sqlDB)

	for i := 0; i < store.RateMaxTries; i++ {
		st.RateFail(ctx, key)
	}
	full, _ := st.RateRetryIn(ctx, key)
	if full <= 0 {
		t.Fatal("not blocked")
	}

	// Age the window by four minutes without waiting four minutes.
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE login_attempts SET window_start = datetime(window_start, '-4 minutes')
		 WHERE key = ?`, key); err != nil {
		t.Fatalf("age the window: %v", err)
	}

	later, _ := st.RateRetryIn(ctx, key)
	if later <= 0 {
		t.Fatal("the lockout vanished after four of ten minutes")
	}
	if later >= full {
		t.Errorf("the wait did not shrink: %v then %v", full, later)
	}
	if diff := full - later; diff < 3*time.Minute || diff > 5*time.Minute {
		t.Errorf("wait fell by %v after ageing four minutes", diff)
	}

	// And once the whole window has passed, the key is free again.
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE login_attempts SET window_start = datetime('now', '-1 hour') WHERE key = ?`,
		key); err != nil {
		t.Fatal(err)
	}
	if retry, _ := st.RateRetryIn(ctx, key); retry != 0 {
		t.Errorf("still blocked %v after the window lapsed", retry)
	}
}

func TestRateLimitIsPerKey(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	for i := 0; i < store.RateMaxTries; i++ {
		st.RateFail(ctx, "1.2.3.4|victim@example.com")
	}
	if retry, _ := st.RateRetryIn(ctx, "1.2.3.4|victim@example.com"); retry <= 0 {
		t.Fatal("the exhausted key is not blocked")
	}
	// Another address from the same IP, and the same address from another IP, are
	// separate counters -- otherwise one attacker locks out a whole network, or
	// anyone can lock a known user out of their own account.
	if retry, _ := st.RateRetryIn(ctx, "1.2.3.4|other@example.com"); retry > 0 {
		t.Error("a different address on the same IP is blocked")
	}
	if retry, _ := st.RateRetryIn(ctx, "9.9.9.9|victim@example.com"); retry > 0 {
		t.Error("the same address from another IP is blocked")
	}
}

// ── optimistic concurrency ───────────────────────────────────────────────────

// TestSecondWriterIsRefused is the bug shared budgeting introduced: two members
// editing one entry, last write silently wins.
func TestSecondWriterIsRefused(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	id := addExpense(t, st, sc, 1000, "Rent", "2026-08-01", true)
	first, err := st.ByID(ctx, sc, id)
	if err != nil {
		t.Fatalf("by id: %v", err)
	}
	second := first // both members opened the same row, so both hold version 1

	if err := st.Update(ctx, sc, id, store.NewTransaction{
		Kind: store.KindExpense, Label: "Rent (Alice)", Amount: 1100,
		OccurredOn: "2026-08-01", Version: first.Version,
	}); err != nil {
		t.Fatalf("the first writer should succeed: %v", err)
	}

	err = st.Update(ctx, sc, id, store.NewTransaction{
		Kind: store.KindExpense, Label: "Rent (Bob)", Amount: 1200,
		OccurredOn: "2026-08-01", Version: second.Version,
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("the second writer got %v, want ErrConflict", err)
	}

	// Alice's value stands; Bob's was not silently applied.
	now, _ := st.ByID(ctx, sc, id)
	if now.Label != "Rent (Alice)" || now.Amount != 1100 {
		t.Errorf("the losing write was applied anyway: %q %s", now.Label, now.Amount.Display())
	}
	if now.Version != first.Version+1 {
		t.Errorf("version is %d, want %d", now.Version, first.Version+1)
	}

	// Bob can retry against the current version.
	if err := st.Update(ctx, sc, id, store.NewTransaction{
		Kind: store.KindExpense, Label: "Rent (Bob)", Amount: 1200,
		OccurredOn: "2026-08-01", Version: now.Version,
	}); err != nil {
		t.Errorf("retrying with the current version failed: %v", err)
	}
}

// TestVersionZeroSkipsTheCheck keeps every existing caller working: only the edit
// form has a version to offer.
func TestVersionZeroSkipsTheCheck(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	id := addExpense(t, st, sc, 1000, "Rent", "2026-08-01", true)
	if err := st.Update(ctx, sc, id, store.NewTransaction{
		Kind: store.KindExpense, Label: "Changed", Amount: 1000,
		OccurredOn: "2026-08-01", // Version left at zero
	}); err != nil {
		t.Fatalf("an update with no version should still work: %v", err)
	}
}

// TestAMissingRowIsStillNotFound: the conflict error must not swallow the case
// where the entry genuinely went away.
func TestAMissingRowIsStillNotFound(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	err := st.Update(ctx, sc, 987654, store.NewTransaction{
		Kind: store.KindExpense, Label: "Ghost", Amount: 100,
		OccurredOn: "2026-08-01", Version: 1,
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// ── ownership transfer ───────────────────────────────────────────────────────

func TestTransferOwnershipIsAtomic(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	shared, _ := st.CreateSharedHousehold(ctx, sc.UserID, "Flat 4B")
	guest := newSecondUser(t, st, "guest@example.com")
	if err := st.InviteMember(ctx, shared, sc.UserID, "guest@example.com", store.RoleEditor); err != nil {
		t.Fatalf("invite: %v", err)
	}
	invites, _ := st.InvitesFor(ctx, guest.UserID, "guest@example.com")
	if err := st.AcceptInvite(ctx, invites[0].ID, guest.UserID, "guest@example.com"); err != nil {
		t.Fatalf("accept: %v", err)
	}

	if err := st.TransferOwnership(ctx, shared, sc.UserID, guest.UserID); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	members, err := st.Members(ctx, shared, sc.UserID)
	if err != nil {
		t.Fatalf("members: %v", err)
	}
	owners := 0
	for _, m := range members {
		if m.Role == store.RoleOwner {
			owners++
			if m.UserID != guest.UserID {
				t.Errorf("the wrong person owns it: %d", m.UserID)
			}
		}
		if m.UserID == sc.UserID && m.Role != store.RoleEditor {
			t.Errorf("the previous owner is %q, want editor", m.Role)
		}
	}
	if owners != 1 {
		t.Errorf("%d owners after the transfer, want exactly 1", owners)
	}
}

func TestOnlyAnOwnerCanTransfer(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	shared, _ := st.CreateSharedHousehold(ctx, sc.UserID, "Flat 4B")
	guest := newSecondUser(t, st, "guest@example.com")
	if err := st.InviteMember(ctx, shared, sc.UserID, "guest@example.com", store.RoleEditor); err != nil {
		t.Fatal(err)
	}
	invites, _ := st.InvitesFor(ctx, guest.UserID, "guest@example.com")
	st.AcceptInvite(ctx, invites[0].ID, guest.UserID, "guest@example.com")

	// The editor tries to take it.
	if err := st.TransferOwnership(ctx, shared, guest.UserID, guest.UserID); err == nil {
		t.Error("an editor transferred ownership to themselves")
	}
	// And cannot hand it to the owner either, since they are not an owner.
	if err := st.TransferOwnership(ctx, shared, guest.UserID, sc.UserID); !errors.Is(err, store.ErrForbidden) {
		t.Errorf("an editor's transfer returned %v, want ErrForbidden", err)
	}
}

func TestCannotTransferToANonMember(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	shared, _ := st.CreateSharedHousehold(ctx, sc.UserID, "Flat 4B")
	outsider := newSecondUser(t, st, "outsider@example.com")

	if err := st.TransferOwnership(ctx, shared, sc.UserID, outsider.UserID); !errors.Is(err, store.ErrNotMember) {
		t.Errorf("got %v, want ErrNotMember", err)
	}
	// Ownership is unchanged.
	members, _ := st.Members(ctx, shared, sc.UserID)
	for _, m := range members {
		if m.UserID == sc.UserID && m.Role != store.RoleOwner {
			t.Error("the failed transfer demoted the owner anyway")
		}
	}
}

// expireInvite ages an invitation past its window without waiting 24 hours.
func expireInvite(t *testing.T, st *store.Store, id int64) {
	t.Helper()
	if err := st.TestOnlyExpireInvite(context.Background(), id); err != nil {
		t.Fatalf("expire invite %d: %v", id, err)
	}
}

// TestDiscardRemovesTheReceiptAndReturnsItsPath: the waiting list needs an exit
// that is not "invent an expense for it".
func TestDiscardRemovesTheReceiptAndReturnsItsPath(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	jobID, err := st.EnqueueReceipt(ctx, sc, "uploads/1/dupe.png", "dupe.png")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := st.CompleteReceiptJob(ctx, jobID, nil); err != nil {
		t.Fatalf("complete: %v", err)
	}

	path, err := st.DiscardReceipt(ctx, sc, jobID)
	if err != nil {
		t.Fatalf("discard: %v", err)
	}
	if path != "uploads/1/dupe.png" {
		t.Errorf("path returned %q, so the caller cannot delete the file", path)
	}

	list, err := st.UnattachedReceipts(ctx, sc, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("a discarded receipt is still waiting: %+v", list)
	}
	if _, err := st.DiscardReceipt(ctx, sc, jobID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("discarding twice: got %v, want ErrNotFound", err)
	}
}

// TestDiscardRefusesAnAttachedReceipt is the one that protects real data:
// deleting the row there would leave an expense pointing at a file the handler
// is about to remove from disk.
func TestDiscardRefusesAnAttachedReceipt(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	jobID, err := st.EnqueueReceipt(ctx, sc, "uploads/1/real.png", "real.png")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := st.CompleteReceiptJob(ctx, jobID, nil); err != nil {
		t.Fatalf("complete: %v", err)
	}
	txID := addExpense(t, st, sc, 1250, "Groceries", "2026-08-01", true)
	if err := st.AttachReceipt(ctx, sc, jobID, txID); err != nil {
		t.Fatalf("attach: %v", err)
	}

	if _, err := st.DiscardReceipt(ctx, sc, jobID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("discard of an attached receipt: got %v, want ErrNotFound", err)
	}

	// And the expense still has its file.
	tx, err := st.ByID(ctx, sc, txID)
	if err != nil {
		t.Fatal(err)
	}
	if tx.ReceiptPath == "" {
		t.Error("the expense lost its receipt")
	}
}

// TestDiscardIsScopedToTheHousehold: the id travels in a URL, so guessing one
// must not delete somebody else's upload.
func TestDiscardIsScopedToTheHousehold(t *testing.T) {
	st, alice := newTestStore(t)
	bob := newSecondUser(t, st, "bob@example.com")
	ctx := context.Background()

	jobID, err := st.EnqueueReceipt(ctx, alice, "uploads/1/hers.png", "hers.png")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := st.CompleteReceiptJob(ctx, jobID, nil); err != nil {
		t.Fatalf("complete: %v", err)
	}

	if _, err := st.DiscardReceipt(ctx, bob, jobID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bob discarded alice's receipt: %v", err)
	}
	if _, err := st.UnattachedReceipt(ctx, alice, jobID); err != nil {
		t.Errorf("alice's receipt was removed by bob's attempt: %v", err)
	}
}

// ── one-time form tokens ──────────────────────────────────────────────────────

func TestFormTokenIsSpentExactlyOnce(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	tok, err := st.NewFormToken(ctx, sc.UserID, "entry")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if tok == "" {
		t.Fatal("empty token")
	}

	ok, err := st.ConsumeFormToken(ctx, sc.UserID, tok)
	if err != nil || !ok {
		t.Fatalf("first use: ok=%v err=%v", ok, err)
	}
	ok, err = st.ConsumeFormToken(ctx, sc.UserID, tok)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("the same token was spent twice — a double submit would duplicate")
	}
}

// TestFormTokenBelongsToItsUser: tokens are unguessable, but the query is scoped
// anyway, so a leaked one cannot be used to cancel somebody else's pending form.
func TestFormTokenBelongsToItsUser(t *testing.T) {
	st, alice := newTestStore(t)
	bob := newSecondUser(t, st, "bob@example.com")
	ctx := context.Background()

	tok, err := st.NewFormToken(ctx, alice.UserID, "entry")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.ConsumeFormToken(ctx, bob.UserID, tok); ok {
		t.Error("bob spent alice's token")
	}
	if ok, _ := st.ConsumeFormToken(ctx, alice.UserID, tok); !ok {
		t.Error("alice's own token was consumed by bob's attempt")
	}
}

func TestEmptyFormTokenIsNotAccepted(t *testing.T) {
	st, sc := newTestStore(t)
	if ok, err := st.ConsumeFormToken(context.Background(), sc.UserID, "  "); ok || err != nil {
		t.Errorf("blank token: ok=%v err=%v", ok, err)
	}
}

// ── audit log ─────────────────────────────────────────────────────────────────

// TestAuditRecordsTheThingsThatMatter walks the events the log exists for.
func TestAuditRecordsTheThingsThatMatter(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	id := addIncome(t, st, sc, 100000, "Salary", "2026-01-01")
	if err := st.Update(ctx, sc, id, store.NewTransaction{
		Kind: store.KindIncome, Label: "Salary (corrected)", Amount: 110000,
		OccurredOn: "2026-01-01",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := st.Delete(ctx, sc, id); err != nil {
		t.Fatalf("delete: %v", err)
	}

	entries, err := st.AuditLog(ctx, sc, 10)
	if err != nil {
		t.Fatalf("audit log: %v", err)
	}
	var actions []string
	for _, e := range entries {
		actions = append(actions, e.Action)
	}
	// Newest first.
	want := []string{"deleted", "edited", "created"}
	if len(actions) != 3 || actions[0] != want[0] || actions[1] != want[1] || actions[2] != want[2] {
		t.Fatalf("actions = %v, want %v", actions, want)
	}

	// The deletion is the one that has to carry detail: after it, the row is
	// gone and nothing else can say what it was.
	if !strings.Contains(entries[0].Summary, "Salary (corrected)") ||
		!strings.Contains(entries[0].Summary, "$1,100.00") {
		t.Errorf("deletion summary does not describe what was deleted: %q", entries[0].Summary)
	}
	// And the edit should show the before and after.
	if !strings.Contains(entries[1].Summary, "→") {
		t.Errorf("edit summary does not show the change: %q", entries[1].Summary)
	}
}

// TestAuditIsRolledBackWithItsChange is the property that makes the log
// trustworthy: it cannot record something that did not happen.
func TestAuditIsRolledBackWithItsChange(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	addIncome(t, st, sc, 10000, "Salary", "2026-01-01")
	fundID, _ := st.CreateFund(ctx, sc, "Car", 0, 0)

	before, _ := st.AuditLog(ctx, sc, 100)

	// More than the household holds, so the whole transaction rolls back.
	if err := st.Deposit(ctx, sc, fundID, 999999, "2026-01-02"); !errors.Is(err, store.ErrInsufficientCash) {
		t.Fatalf("want ErrInsufficientCash, got %v", err)
	}

	after, _ := st.AuditLog(ctx, sc, 100)
	if len(after) != len(before) {
		t.Errorf("a refused deposit left %d audit entries behind", len(after)-len(before))
	}
}

// TestAuditIsScopedToTheHousehold: one budget's history is not another's.
func TestAuditIsScopedToTheHousehold(t *testing.T) {
	st, alice := newTestStore(t)
	bob := newSecondUser(t, st, "bob@example.com")
	ctx := context.Background()

	addIncome(t, st, alice, 50000, "Alice salary", "2026-01-01")
	entries, err := st.AuditLog(ctx, bob, 50)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Summary, "Alice salary") {
			t.Fatalf("bob can read alice's history: %+v", e)
		}
	}
}

// TestMembershipChangesAreRecorded covers the question the log was built for.
func TestMembershipChangesAreRecorded(t *testing.T) {
	st, alice := newTestStore(t)
	ctx := context.Background()

	hh, err := st.CreateSharedHousehold(ctx, alice.UserID, "Flat 4B")
	if err != nil {
		t.Fatalf("create shared: %v", err)
	}
	shared := store.Scope{HouseholdID: hh, UserID: alice.UserID}

	bobID, err := st.CreateUser(ctx, "bob@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InviteMember(ctx, hh, alice.UserID, "bob@example.com", store.RoleEditor); err != nil {
		t.Fatalf("invite: %v", err)
	}
	invites, err := st.InvitesFor(ctx, bobID, "bob@example.com")
	if err != nil || len(invites) != 1 {
		t.Fatalf("invites: %v (%d)", err, len(invites))
	}
	if err := st.AcceptInvite(ctx, invites[0].ID, bobID, "bob@example.com"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := st.SetRole(ctx, hh, alice.UserID, bobID, store.RoleViewer); err != nil {
		t.Fatalf("set role: %v", err)
	}
	if err := st.RemoveMember(ctx, hh, alice.UserID, bobID); err != nil {
		t.Fatalf("remove: %v", err)
	}

	entries, err := st.AuditLog(ctx, shared, 50)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Action] = true
	}
	for _, want := range []string{"invited", "joined", "changed a role", "removed a member"} {
		if !seen[want] {
			t.Errorf("%q was not recorded; actions seen: %v", want, seen)
		}
	}
}

// ── device names ──────────────────────────────────────────────────────────────

// TestDeviceNameIsReadable pins the parsing down, because the ordering of the
// checks is not obvious and a plausible-looking rearrangement silently breaks it:
// every Chromium browser claims "Safari", Edge also claims "Chrome", and Chrome on
// iOS claims both. Android user agents also contain "Linux", and iPhone ones
// contain "Mac OS X".
func TestDeviceNameIsReadable(t *testing.T) {
	tests := []struct {
		name string
		ua   string
		want string
	}{
		{
			"chrome on windows",
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
				"(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
			"Chrome on Windows",
		},
		{
			"edge is not chrome, though it says so",
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
				"(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 Edg/120.0.2210.91",
			"Edge on Windows",
		},
		{
			"safari on iphone, not a mac",
			"Mozilla/5.0 (iPhone; CPU iPhone OS 17_1 like Mac OS X) " +
				"AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.1 Mobile/15E148 Safari/604.1",
			"Safari on iPhone",
		},
		{
			"chrome on ios is CriOS, and still not safari",
			"Mozilla/5.0 (iPhone; CPU iPhone OS 17_1 like Mac OS X) " +
				"AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/120.0.6099.119 Mobile/15E148 Safari/604.1",
			"Chrome on iPhone",
		},
		{
			"chrome on android, not linux",
			"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 " +
				"(KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36",
			"Chrome on Android",
		},
		{
			"firefox on a mac",
			"Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:121.0) Gecko/20100101 Firefox/121.0",
			"Firefox on Mac",
		},
		{"nothing at all", "", "Unknown device"},
		{"something that is not a browser", "curl/8.4.0", "Unknown device"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := store.Session{UserAgent: tc.ua}.DeviceName()
			if got != tc.want {
				t.Errorf("DeviceName() = %q, want %q", got, tc.want)
			}
		})
	}
}
