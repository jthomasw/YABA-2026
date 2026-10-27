package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// TestBillNextDueDate pins "today" and checks the date a bill shows as next
// due: today itself counts, a passed day rolls to next month, and a day the
// month does not have falls back to its last day.
func TestBillNextDueDate(t *testing.T) {
	cases := []struct {
		today string
		day   int
		want  string
	}{
		{"2026-10-27", 1, "2026-11-01"},  // already passed this month
		{"2026-10-27", 27, "2026-10-27"}, // due today
		{"2026-10-27", 31, "2026-10-31"}, // later this month
		{"2026-11-05", 31, "2026-11-30"}, // November has no 31st
		{"2027-02-10", 30, "2027-02-28"}, // nor February a 30th
		{"2028-02-10", 30, "2028-02-29"}, // leap year
		{"2026-12-15", 3, "2027-01-03"},  // across the new year
		{"2026-10-27", 0, ""},            // no due day set
	}
	for _, c := range cases {
		now, _ := time.Parse(store.DateLayout, c.today)
		restore := store.SetClockForTest(now.Add(12*time.Hour), time.UTC)
		got := store.Bucket{DueDay: c.day}.NextDueDate()
		restore()
		if got != c.want {
			t.Errorf("day %d on %s: next due %q, want %q", c.day, c.today, got, c.want)
		}
	}
}

// TestBillDueDayIsSavedAndEdited checks the due day survives create, update,
// clearing, and both ways of reading bills back.
func TestBillDueDayIsSavedAndEdited(t *testing.T) {
	s, sc := newTestStore(t)
	ctx := context.Background()

	id, err := s.CreateBucket(ctx, sc, store.NewBucket{
		Name: "Rent", CostKind: store.CostFixed, Fixed: 100000, DueDay: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	dueDay := func() (fromBuckets, fromOptions int) {
		t.Helper()
		bs, err := s.Buckets(ctx, sc, "")
		if err != nil {
			t.Fatal(err)
		}
		opts, err := s.BucketOptions(ctx, sc)
		if err != nil {
			t.Fatal(err)
		}
		return bs[0].DueDay, opts[0].DueDay
	}
	if a, b := dueDay(); a != 1 || b != 1 {
		t.Fatalf("after create: due day %d / %d, want 1", a, b)
	}

	if err := s.UpdateBucket(ctx, sc, id, store.NewBucket{
		Name: "Rent", CostKind: store.CostFixed, Fixed: 100000, DueDay: 15,
	}); err != nil {
		t.Fatal(err)
	}
	if a, b := dueDay(); a != 15 || b != 15 {
		t.Fatalf("after edit: due day %d / %d, want 15", a, b)
	}

	if err := s.UpdateBucket(ctx, sc, id, store.NewBucket{
		Name: "Rent", CostKind: store.CostFixed, Fixed: 100000,
	}); err != nil {
		t.Fatal(err)
	}
	if a, _ := dueDay(); a != 0 {
		t.Fatalf("after clearing: due day %d, want 0", a)
	}

	if _, err := s.CreateBucket(ctx, sc, store.NewBucket{
		Name: "Bad", CostKind: store.CostFixed, Fixed: 100, DueDay: 32,
	}); err == nil {
		t.Error("a due day of 32 was accepted")
	}
}
