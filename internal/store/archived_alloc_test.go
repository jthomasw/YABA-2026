package store_test

// Archiving a bucket re-pours only the CURRENT month, so allocations to it
// survive in every other month. Required is summed over active buckets only,
// so Allocated had to be as well -- otherwise a past month showed more income
// committed than there were budget lines to commit it to, and the difference
// simply vanished off the page.

import (
	"context"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/store"
)

func TestArchivingABucketReleasesItsAllocationsInEveryMonth(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	rent, err := st.CreateBucket(ctx, sc, store.NewBucket{
		Name: "Rent", CostKind: store.CostFixed, Fixed: 100000,
	})
	if err != nil {
		t.Fatal(err)
	}
	gym, err := st.CreateBucket(ctx, sc, store.NewBucket{
		Name: "Gym", CostKind: store.CostFixed, Fixed: 5000,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := st.Add(ctx, sc, store.NewTransaction{
		Kind: store.KindIncome, Label: "Salary", Amount: 120000,
		OccurredOn: "2026-08-01",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Reallocate(ctx, sc, "2026-08"); err != nil {
		t.Fatal(err)
	}

	buckets, err := st.Buckets(ctx, sc, "2026-08")
	if err != nil {
		t.Fatal(err)
	}
	before, err := st.AllocationsFor(ctx, sc, "2026-08", buckets)
	if err != nil {
		t.Fatal(err)
	}
	if before.Required != 105000 || before.Allocated != 105000 {
		t.Fatalf("baseline is wrong: required %s, allocated %s",
			before.Required.Display(), before.Allocated.Display())
	}

	if err := st.ArchiveBucket(ctx, sc, gym); err != nil {
		t.Fatalf("archive: %v", err)
	}

	buckets, err = st.Buckets(ctx, sc, "2026-08")
	if err != nil {
		t.Fatal(err)
	}
	after, err := st.AllocationsFor(ctx, sc, "2026-08", buckets)
	if err != nil {
		t.Fatal(err)
	}

	if after.Required != 100000 {
		t.Errorf("required = %s, want $1,000.00 (Rent alone)", after.Required.Display())
	}
	if after.Allocated > after.Required {
		t.Errorf("allocated %s exceeds required %s — money is committed to a bucket that is not on the page",
			after.Allocated.Display(), after.Required.Display())
	}
	// $1,200.00 income less $1,000.00 of surviving requirement.
	if want := store.Cents(20000); after.Unassigned != want {
		t.Errorf("unassigned = %s, want %s", after.Unassigned.Display(), want.Display())
	}

	_ = rent
}
