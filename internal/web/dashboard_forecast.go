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

	// Estimate daily spending from the latest month's expenses.
	monthlySpend := 0.0
	if len(v.Monthly) > 0 {
		monthlySpend = v.Monthly[len(v.Monthly)-1].Expense.Float()
	}

	// Exclude scheduled expenses from the spending baseline.
	// They will be applied separately on their due dates.
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

	for _, b := range v.Buckets {
		if b.Due > 0 &&
			b.NextDueDate() != "" &&
			!scheduledBuckets[b.ID] {
			recurringMonthly += b.Due.Float()
		}
	}

	dailySpending := math.Max(
		0,
		monthlySpend-recurringMonthly,
	) / 30.0

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
			r.StartDate,
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
			r.StartDate,
			r.FrequencyN,
			r.FrequencyUnit,
			-r.Amount.Float(),
			today,
			end,
		)
	}

	// Include manually scheduled bills that aren't already
	// represented by active recurring expenses.
	for _, b := range v.Buckets {
		if b.Due <= 0 || scheduledBuckets[b.ID] {
			continue
		}

		due := b.NextDueDate()
		if due == "" {
			continue
		}

		addOccurrences(
			changes,
			due,
			due,
			1,
			"month",
			-b.Due.Float(),
			today,
			end,
		)
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
func addOccurrences(
	changes map[string]float64,
	due string,
	anchor string,
	n int,
	unit string,
	amount float64,
	start time.Time,
	end time.Time,
) {
	if n <= 0 {
		return
	}

	d, err := time.Parse("2006-01-02", due)
	if err != nil {
		return
	}

	anchorDate, err := time.Parse("2006-01-02", anchor)
	if err != nil {
		anchorDate = d
	}

	for count := 0; count < 1000 && !d.After(end); count++ {
		if d.After(start) {
			changes[d.Format("2006-01-02")] += amount
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
			day := anchorDate.Day()

			if day > last {
				day = last
			}

			d = time.Date(
				target.Year(),
				target.Month(),
				day, 0, 0, 0, 0, time.UTC,
			)

		default:
			return
		}
	}
}
