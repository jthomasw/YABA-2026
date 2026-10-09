package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// join puts a new user into household hh with role.
func join(t *testing.T, st *store.Store, owner store.Scope, hh int64, email string, role store.Role) store.Scope {
	t.Helper()
	ctx := context.Background()
	u := newSecondUser(t, st, email)
	if err := st.InviteMember(ctx, hh, owner.UserID, email, store.RoleEditor); err != nil {
		t.Fatalf("invite: %v", err)
	}
	inv, _ := st.InvitesFor(ctx, u.UserID, email)
	if err := st.AcceptInvite(ctx, inv[0].ID, u.UserID, email); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if role != store.RoleEditor {
		if err := st.SetRole(ctx, hh, owner.UserID, u.UserID, role); err != nil {
			t.Fatalf("set role: %v", err)
		}
	}
	return store.Scope{HouseholdID: hh, UserID: u.UserID}
}

// TestTransferOwnershipWithACoOwner: a budget that already has a second owner
// must still be able to hand ownership to a third member.
func TestTransferOwnershipWithACoOwner(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()
	hh, _ := st.CreateSharedHousehold(ctx, sc.UserID, "Flat")
	join(t, st, sc, hh, "co@example.com", store.RoleOwner)
	third := join(t, st, sc, hh, "third@example.com", store.RoleEditor)

	if err := st.TransferOwnership(ctx, hh, sc.UserID, third.UserID); err != nil {
		t.Fatalf("transfer with a co-owner present: %v", err)
	}
	roles := map[int64]store.Role{}
	members, _ := st.Members(ctx, hh, sc.UserID)
	for _, m := range members {
		roles[m.UserID] = m.Role
	}
	if roles[third.UserID] != store.RoleOwner || roles[sc.UserID] != store.RoleEditor {
		t.Errorf("roles after transfer = %v", roles)
	}
}

// TestVariableBucketNeedsItsUsualAmountUntilSpendingPassesIt: a small charge
// early in the month must not shrink the month's requirement to that charge.
func TestVariableBucketNeedsItsUsualAmountUntilSpendingPassesIt(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()
	month := store.Today()[:7]
	prev := store.MonthsBack(store.Now(), 2)[0]

	id, err := st.CreateBucket(ctx, sc, store.NewBucket{Name: "Power", CostKind: store.CostVariable})
	if err != nil {
		t.Fatal(err)
	}
	spend := func(cents store.Cents, date string) {
		if _, err := st.Add(ctx, sc, store.NewTransaction{Kind: store.KindExpense, Label: "Power",
			Amount: cents, OccurredOn: date, BucketID: &id}); err != nil {
			t.Fatal(err)
		}
	}
	due := func() store.Cents {
		bs, err := st.Buckets(ctx, sc, month)
		if err != nil || len(bs) != 1 {
			t.Fatalf("buckets: %v %v", bs, err)
		}
		return bs[0].Due
	}

	spend(12000, prev+"-15") // last month's bill: $120
	if got := due(); got != 12000 {
		t.Fatalf("due before any spending = %d, want the usual 12000", got)
	}
	spend(500, month+"-01")
	if got := due(); got != 12000 {
		t.Errorf("due after a $5 charge = %d, want still 12000", got)
	}
	spend(13000, month+"-01")
	if got := due(); got != 13500 {
		t.Errorf("due once spending passed the usual = %d, want 13500", got)
	}
}

func TestHousekeepingPurgesOldNotificationsAndAudit(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()
	if err := st.Notify(ctx, sc.UserID, "info", "fresh", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Notify(ctx, sc.UserID, "info", "old", ""); err != nil {
		t.Fatal(err)
	}
	db := store.DBForTest(st)
	db.Exec(`UPDATE notifications SET created_at = datetime('now', '-100 days') WHERE text = 'old'`)
	db.Exec(`INSERT INTO audit_log(household_id, user_id, action, entity, summary, created_at)
	         VALUES (?, ?, 'created', 'transaction', 'ancient', datetime('now', '-500 days'))`, sc.HouseholdID, sc.UserID)

	if n, err := st.PurgeOldNotifications(ctx); err != nil || n != 1 {
		t.Errorf("purged %d notification(s), err %v; want 1", n, err)
	}
	if n, err := st.PurgeOldAudit(ctx); err != nil || n != 1 {
		t.Errorf("purged %d audit row(s), err %v; want 1", n, err)
	}
	if left, _ := st.TakeNotifications(ctx, sc.UserID); len(left) != 1 || left[0].Text != "fresh" {
		t.Errorf("left = %+v", left)
	}
}

// TestBackdatedDepositChecksTheCashThen: $50 in March and $1,000 in June does
// not mean $500 could have gone into savings in March.
func TestBackdatedDepositChecksTheCashThen(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()
	addIncome(t, st, sc, 5000, "Gift", "2026-03-01")
	addIncome(t, st, sc, 100000, "Pay", "2026-06-01")
	fund, err := st.CreateFund(ctx, sc, "Holiday", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	if err := st.Deposit(ctx, sc, fund, 50000, "2026-03-15"); !errors.Is(err, store.ErrInsufficientCash) {
		t.Errorf("a $500 deposit in March, with $50 then: err = %v", err)
	}
	if err := st.Deposit(ctx, sc, fund, 5000, "2026-03-15"); err != nil {
		t.Errorf("a $50 deposit in March: %v", err)
	}
	if err := st.Deposit(ctx, sc, fund, 50000, "2026-06-15"); err != nil {
		t.Errorf("a $500 deposit in June: %v", err)
	}
	// $500 is now in the fund, but only $50 of it was there in April.
	if err := st.Withdraw(ctx, sc, fund, 10000, "2026-04-01"); !errors.Is(err, store.ErrInsufficientFund) {
		t.Errorf("a $100 withdrawal in April, with $50 in the fund then: err = %v", err)
	}
	if err := st.Withdraw(ctx, sc, fund, 10000, "2026-07-01"); err != nil {
		t.Errorf("a $100 withdrawal in July: %v", err)
	}
}

func TestSavingsTransfersCannotBeFutureDated(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()
	addIncome(t, st, sc, 10000, "Pay", store.Today())
	fund, _ := st.CreateFund(ctx, sc, "Car", 0, 0)
	if err := st.Deposit(ctx, sc, fund, 1000, "2199-01-01"); !errors.Is(err, store.ErrFutureDated) {
		t.Errorf("future deposit: err = %v", err)
	}
	if err := st.Deposit(ctx, sc, fund, 1000, store.Today()); err != nil {
		t.Fatal(err)
	}
	if err := st.Withdraw(ctx, sc, fund, 500, "2199-01-01"); !errors.Is(err, store.ErrFutureDated) {
		t.Errorf("future withdrawal: err = %v", err)
	}
}
