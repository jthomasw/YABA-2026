package store

import (
	"database/sql"
	"time"
)

// SetClockForTest pins Now, Today and everything built on them to a fixed
// instant read in loc, and returns a function that restores the real clock.
// It lives in a _test.go file so it can never be called from production code.
func SetClockForTest(now time.Time, loc *time.Location) (restore func()) {
	prevNow, prevLoc := timeNow, clockLocation
	timeNow = func() time.Time { return now }
	clockLocation = loc
	return func() { timeNow, clockLocation = prevNow, prevLoc }
}

// AdvanceRecurringDate exposes the schedule date arithmetic so it can be
// tested directly across leap years without driving a database through years
// of catch-up.
var AdvanceRecurringDate = advanceRecurringDate

// DBForTest exposes the handle so a test can age rows directly.
func DBForTest(s *Store) *sql.DB { return s.db }
