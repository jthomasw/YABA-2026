package web

import (
	"math"
	"time"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// forecastBalance predicts the next 30 days of available cash.
// It combines estimated daily spending with scheduled income and bills.
func forecastBalance(
	v dashboardView,
	labels []string,
	actual []*float64,
) ([]string, []*float64, []*float64) {

	if len(labels) == 0 || len(actual) == 0 {
		return labels, actual, []*float64{}
	}

	today, err := time.Parse("2006-01-02", store.Today())
	if err != nil {
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
	end := today.AddDate(0, 0, 28)

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

	// Forecast four weeks ahead, displaying one point per week.
	// Transactions between weekly points still affect the balance.
	for week := 1; week <= 4; week++ {
		for dayOffset := (week-1)*7 + 1; dayOffset <= week*7; dayOffset++ {
			day := today.AddDate(0, 0, dayOffset)
			date := day.Format("2006-01-02")

			balance += changes[date] - dailySpending
		}

		weekEnd := today.AddDate(0, 0, week*7)
		predicted := math.Round(balance*100) / 100

		labels = append(labels, weekEnd.Format("2006-01-02"))
		actual = append(actual, nil)
		forecast = append(forecast, &predicted)
	}

	return labels, actual, forecast
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
