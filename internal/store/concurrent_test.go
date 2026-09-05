package store_test

// Tests for the two read-decide-write sequences that used to run as separate
// statements. Both are reachable from an ordinary browser: two tabs, or a page
// and its own refresh, are enough. SetMaxOpenConns(1) serialises the individual
// statements but does nothing to make a sequence of them atomic, which is what
// made these look safe.

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// TestConcurrentFirstVisitsCreateOneEmergencyFund. The fund is created lazily
// on the first dashboard load. Two loads racing each other both found none, and
// the second INSERT hit the partial unique index on (household_id) WHERE
// is_emergency = 1 -- so a household's very first visit could answer with
// "UNIQUE constraint failed" instead of a dashboard.
func TestConcurrentFirstVisitsCreateOneEmergencyFund(t *testing.T) {
	st, sc := newTestStore(t)

	const racers = 8
	var wg sync.WaitGroup
	errs := make([]error, racers)
	ids := make([]int64, racers)
	start := make(chan struct{})

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			f, err := st.EmergencyFund(context.Background(), sc)
			errs[i], ids[i] = err, f.ID
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("racer %d: %v", i, err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	for i, id := range ids {
		if id != ids[0] {
			t.Errorf("racer %d got fund %d, racer 0 got %d: two funds were created", i, id, ids[0])
		}
	}

	funds, err := st.ListFunds(context.Background(), sc)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range funds {
		if f.IsEmergency {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d emergency funds after %d concurrent first visits, want 1", n, racers)
	}
}

// TestANotificationIsTakenExactlyOnce. "Take" means read and mark seen, and the
// two used to be separate statements: concurrent readers both saw the same
// unseen rows and the user got the same toast twice.
func TestANotificationIsTakenExactlyOnce(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	const messages = 12
	for i := 0; i < messages; i++ {
		if err := st.Notify(ctx, sc.UserID, "info", "receipt ready", "/transactions/new"); err != nil {
			t.Fatal(err)
		}
	}

	const readers = 6
	var wg sync.WaitGroup
	got := make([][]store.Notification, readers)
	start := make(chan struct{})
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ns, err := st.TakeNotifications(ctx, sc.UserID)
			if err != nil {
				t.Errorf("reader %d: %v", i, err)
				return
			}
			got[i] = ns
		}(i)
	}
	close(start)
	wg.Wait()

	seen := map[int64]int{}
	total := 0
	for _, batch := range got {
		for _, n := range batch {
			seen[n.ID]++
			total++
		}
	}
	if total != messages {
		t.Errorf("%d notifications delivered in total, want %d", total, messages)
	}
	var twice []int64
	for id, count := range seen {
		if count > 1 {
			twice = append(twice, id)
		}
	}
	if len(twice) > 0 {
		t.Errorf("notifications %v were delivered more than once", twice)
	}
	if len(seen) != messages {
		t.Errorf("%d distinct notifications delivered, want %d", len(seen), messages)
	}

	// And nothing is left behind.
	if rest, err := st.TakeNotifications(ctx, sc.UserID); err != nil {
		t.Fatal(err)
	} else if len(rest) != 0 {
		t.Errorf("%d notifications still unseen", len(rest))
	}
}

// TestEmergencyFundAdoptsAnObviouslyIntendedFund pins the behaviour the
// transaction had to preserve: a fund the user already named "Emergency" is
// promoted rather than shadowed by a second one.
func TestEmergencyFundAdoptsAnObviouslyIntendedFund(t *testing.T) {
	st, sc := newTestStore(t)
	ctx := context.Background()

	mine, err := st.CreateFund(ctx, sc, "My emergency savings", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	f, err := st.EmergencyFund(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	if f.ID != mine {
		t.Errorf("emergency fund is %d, want the existing %q fund %d", f.ID, "My emergency savings", mine)
	}
	if !strings.Contains(strings.ToLower(f.Name), "emergency") {
		t.Errorf("adopted the wrong fund: %q", f.Name)
	}
}
