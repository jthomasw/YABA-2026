package web

import (
	"math"
	"time"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// forecastBalance predicts available cash from tomorrow to end (YYYY-MM-DD),
// one point per day. end is the last day of the period chosen in the Period
// menu, so the prediction never runs past what the user asked to see. When end
// is today or earlier -- a past month -- there is nothing to predict and the
// forecast is empty. It combines estimated daily spending with scheduled income
// and bills.
func forecastBalance(
	v dashboardView,
	labels []string,
	actual []*float64,
	endDate string,
) ([]string, []*float64, []*float64) {

	if len(labels) == 0 || len(actual) == 0 {
		return labels, actual, []*float64{}
	}

	today, err := time.Parse("2006-01-02", store.Today())
	if err != nil {
		return labels, actual, []*float64{}
	}
	end, err := time.Parse("2006-01-02", endDate)
	if err != nil || !end.After(today) {
		return labels, actual, []*float64{}
	}

	// Anchor predictions to today's actual cash balance.
	todayKey := today.Format("2006-01-02")

	if labels[len(labels)-1] != todayKey {
		labels = append(labels, todayKey)
		cash := v.Cash.Float()
		actual = append(actual, &cash)
	}

	// Null values hide the forecast before today.
	forecast := make([]*float64, len(labels))
	cash := v.Cash.Float()
	forecast[len(forecast)-1] = &cash

	// Scheduled expenses are applied on their own dates below, so they are
	// taken out of the everyday-spending baseline.
	recurringMonthly := 0.0
	scheduledBuckets := map[int64]bool{}

	for _, r := range v.RecurringExpenses {
		if !r.Active {
			continue
		}

		recurringMonthly += monthlyEquivalent(
			r.Amount.Float(),
			r.FrequencyN,
			r.FrequencyUnit,
		)

		if id := r.BucketRef(); id != 0 {
			scheduledBuckets[id] = true
		}
	}

	for _, b := range v.NowBuckets {
		if b.Due > 0 &&
			b.NextDueDate() != "" &&
			!scheduledBuckets[b.ID] {
			recurringMonthly += b.Due.Float()
		}
	}

	// Spending dated later this month is in this month's total but has not
	// happened; it is applied on its own date below, so it must not also
	// count towards the pace so far.
	var futureSpend float64
	thisMonth := today.Format(store.MonthLayout)
	for date, net := range v.FutureCash {
		if len(date) >= 7 && date[:7] == thisMonth && net < 0 {
			futureSpend += -net.Float()
		}
	}
	dailySpending := dailySpendingEstimate(v.Monthly, today, recurringMonthly, futureSpend)

	// Store expected financial changes by date.
	changes := map[string]float64{}

	// Add future recurring income.
	for _, r := range v.RecurringIncome {
		if !r.Active {
			continue
		}

		addOccurrences(
			changes,
			r.NextDueDate,
			anchorDay(r.StartDate),
			r.FrequencyN,
			r.FrequencyUnit,
			r.Amount.Float(),
			today,
			end,
		)
	}

	// Subtract future recurring expenses.
	for _, r := range v.RecurringExpenses {
		if !r.Active {
			continue
		}

		addOccurrences(
			changes,
			r.NextDueDate,
			anchorDay(r.StartDate),
			r.FrequencyN,
			r.FrequencyUnit,
			-r.Amount.Float(),
			today,
			end,
		)
	}

	// Include manually scheduled bills that aren't already
	// represented by active recurring expenses. The first payment is only
	// what is still owed on it: a bill already paid this month is not taken
	// off again on its due date. Later months are the full amount, on the
	// bill's own due day (so a bill due on the 31st lands on the 31st, not
	// on the 30th it was clamped to in a shorter month).
	for _, b := range v.NowBuckets {
		if b.Due <= 0 || scheduledBuckets[b.ID] {
			continue
		}

		first, owed := nextBillPayment(b, today.Format(store.DateLayout))
		if first == "" {
			continue
		}
		firstDate, err := time.Parse(store.DateLayout, first)
		if err != nil {
			continue
		}
		if !firstDate.After(end) {
			addOccurrences(changes, first, b.DueDay, 1, "month", -owed.Float(), today, firstDate)
		}

		next := store.NextMonthlyDue(b.DueDay, firstDate.AddDate(0, 0, 1).Format(store.DateLayout))
		if next != "" {
			addOccurrences(changes, next, b.DueDay, 1, "month", -b.Due.Float(), today, end)
		}
	}

	// Entries already recorded for a future date: left out of today's cash,
	// so they are applied on the day they are dated.
	for date, net := range v.FutureCash {
		if d, err := time.Parse(store.DateLayout, date); err == nil && d.After(today) && !d.After(end) {
			changes[date] += net.Float()
		}
	}

	// Calculate the predicted balance for each future day.
	balance := v.Cash.Float()

	// One point per day from tomorrow to the end of the period. At most a
	// month, so a daily point is cheap and shows exactly when a bill lands.
	for day := today.AddDate(0, 0, 1); !day.After(end); day = day.AddDate(0, 0, 1) {
		date := day.Format("2006-01-02")
		balance += changes[date] - dailySpending
		predicted := math.Round(balance*100) / 100

		labels = append(labels, date)
		actual = append(actual, nil)
		forecast = append(forecast, &predicted)
	}

	return labels, actual, forecast
}

// chartPeriod is the stretch of time the Current Funds chart covers, set by
// the Period menu.
type chartPeriod struct {
	// From is the first day shown, or "" for all history.
	From string
	// To is the last day: the prediction runs up to it.
	To string
}

// chartPeriodFor maps the Period menu to the chart's dates. A month covers
// that month, 1st to last day. "All time" covers all history and predicts to
// the end of the current month -- never into the next one.
func chartPeriodFor(month, today string) chartPeriod {
	lastDay := func(t time.Time) string {
		return time.Date(t.Year(), t.Month()+1, 0, 0, 0, 0, 0, time.UTC).Format(store.DateLayout)
	}
	if month != "" {
		if t, err := time.Parse(store.MonthLayout, month); err == nil {
			return chartPeriod{From: t.Format(store.DateLayout), To: lastDay(t)}
		}
	}
	t, err := time.Parse(store.DateLayout, today)
	if err != nil {
		return chartPeriod{}
	}
	return chartPeriod{To: lastDay(t)}
}

// periodBalance trims the running-balance history to the period, ending no
// later than today. A month that does not open with a transaction on the 1st
// starts with the balance carried over from before it, so the line begins on
// the 1st at the right height rather than at the month's first entry; a past
// month that does not close with one is carried to its last day the same way.
// An account with no history at all stays empty.
func periodBalance(points []store.Point, p chartPeriod, today string) []store.Point {
	if len(points) == 0 {
		return points
	}
	limit := today
	if p.To != "" && p.To < limit {
		limit = p.To
	}

	var opening store.Cents
	out := []store.Point{}
	for _, pt := range points {
		switch {
		case p.From != "" && pt.Date < p.From:
			opening = pt.Balance
		case pt.Date <= limit:
			out = append(out, pt)
		}
	}

	if p.From != "" && p.From <= limit && (len(out) == 0 || out[0].Date != p.From) {
		out = append([]store.Point{{Date: p.From, Balance: opening}}, out...)
	}
	// Close a finished month on its last day. The current month needs no
	// closing point: the forecast adds today's balance itself.
	if limit < today && len(out) > 0 && out[len(out)-1].Date < limit {
		out = append(out, store.Point{Date: limit, Balance: out[len(out)-1].Balance})
	}
	return out
}

// forecastLabel names the prediction in the chart legend: "Predicted balance
// (to Oct 31)".
func forecastLabel(end string) string {
	t, err := time.Parse(store.DateLayout, end)
	if err != nil {
		return "Predicted balance"
	}
	return "Predicted balance (to " + t.Format("Jan 2") + ")"
}

// monthlyEquivalent converts recurring expenses into an
// approximate monthly amount for spending calculations.
func monthlyEquivalent(amount float64, n int, unit string) float64 {
	if n <= 0 {
		return 0
	}

	switch unit {
	case "day":
		return amount * 30 / float64(n)
	case "week":
		return amount * 30 / (7 * float64(n))
	case "month":
		return amount / float64(n)
	}

	return 0
}

// addOccurrences adds scheduled income or expenses to future dates.
//
// An occurrence dated today or earlier has not been recorded yet -- a
// schedule is posted when it is processed, which advances it past today --
// so its money is not in today's cash. It is applied tomorrow rather than
// dropped: a bill due today and still unpaid used to vanish from the
// forecast. day is the day of the month a monthly schedule is anchored to;
// 0 means the first due date's own day.
func addOccurrences(
	changes map[string]float64,
	due string,
	day int,
	n int,
	unit string,
	amount float64,
	start time.Time,
	end time.Time,
) {
	if n <= 0 || amount == 0 {
		return
	}

	d, err := time.Parse("2006-01-02", due)
	if err != nil {
		return
	}
	if day <= 0 {
		day = d.Day()
	}
	tomorrow := start.AddDate(0, 0, 1).Format("2006-01-02")

	for count := 0; count < 1000 && !d.After(end); count++ {
		if d.After(start) {
			changes[d.Format("2006-01-02")] += amount
		} else {
			changes[tomorrow] += amount
		}

		switch unit {
		case "day":
			d = d.AddDate(0, 0, n)

		case "week":
			d = d.AddDate(0, 0, 7*n)

		case "month":
			target := time.Date(
				d.Year(),
				d.Month()+time.Month(n),
				1, 0, 0, 0, 0, time.UTC,
			)

			last := target.AddDate(0, 1, -1).Day()
			on := day
			if on > last {
				on = last
			}

			d = time.Date(
				target.Year(),
				target.Month(),
				on, 0, 0, 0, 0, time.UTC,
			)

		default:
			return
		}
	}
}

// anchorDay is the day of the month a schedule started on, which its monthly
// occurrences keep to; 0 when the date cannot be read.
func anchorDay(start string) int {
	t, err := time.Parse(store.DateLayout, start)
	if err != nil {
		return 0
	}
	return t.Day()
}

// nextBillPayment is when a monthly bill is next paid and how much: its next
// due date and what is still owed on it this month. A bill already paid in
// full this month is next paid on next month's due day, in full. "" when the
// bill has no due day. b must be the current month's bill (Spent is this
// month's).
func nextBillPayment(b store.Bucket, today string) (string, store.Cents) {
	due := b.NextDueDate()
	if due == "" || b.Due <= 0 || len(today) < 7 {
		return "", 0
	}
	if due[:7] != today[:7] {
		return due, b.Due // already past this month's day: next month, in full
	}
	if owed := b.Due - b.Spent; owed > 0 {
		return due, owed
	}
	// Paid this month: the next payment is next month's.
	t, err := time.Parse(store.DateLayout, today)
	if err != nil {
		return "", 0
	}
	firstOfNext := time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	return store.NextMonthlyDue(b.DueDay, firstOfNext.Format(store.DateLayout)), b.Due
}

// daysPerMonth is the average month, for turning a monthly amount into a daily one.
const daysPerMonth = 365.25 / 12

// dailySpendingEstimate is the everyday spending the forecast assumes per day:
// the average of the last three full months that had any activity, less what
// is paid on a schedule (applied on its own dates). The current month is not
// used while there is any history: two days into November, November's
// spending so far divided by a whole month made the forecast assume almost
// nothing would be spent. A brand-new account with no full month yet falls
// back to this month's pace so far.
func dailySpendingEstimate(monthly []store.MonthPoint, today time.Time, recurringMonthly, futureSpend float64) float64 {
	if len(monthly) == 0 {
		return 0
	}
	thisMonth := today.Format(store.MonthLayout)
	complete := monthly
	var current *store.MonthPoint
	if last := monthly[len(monthly)-1]; last.Month == thisMonth {
		complete = monthly[:len(monthly)-1]
		current = &monthly[len(monthly)-1]
	}

	// Skip the empty months before the account had any activity.
	first := len(complete)
	for i, m := range complete {
		if m.Income > 0 || m.Expense > 0 {
			first = i
			break
		}
	}
	window := complete[first:]
	if len(window) > 3 {
		window = window[len(window)-3:]
	}
	if len(window) > 0 {
		var total float64
		for _, m := range window {
			total += m.Expense.Float()
		}
		return math.Max(0, total/float64(len(window))-recurringMonthly) / daysPerMonth
	}

	if current == nil || today.Day() <= 0 {
		return 0
	}
	return math.Max(0, current.Expense.Float()-futureSpend-recurringMonthly) / float64(today.Day())
}
