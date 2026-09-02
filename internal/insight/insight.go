// Package insight turns stored figures into the sentences a user reads.
package insight

import (
	"fmt"
	"math"
	"time"

	"github.com/jthomasw/YABA-2026/internal/money"
	"github.com/jthomasw/YABA-2026/internal/store"
)

// Projection answers when a fund will reach its goal at the current rate.
type Projection struct {
	// Reachable is false when there is no goal, or no savings rate to extrapolate from.
	Reachable bool
	// Months is the whole number of months until the goal is met.
	Months int
	// ETA is the projected completion date.
	ETA time.Time
	// PerMonth is the average monthly contribution the projection assumes.
	PerMonth money.Cents
	// OnTrack compares the projection to the fund's own target horizon.
	OnTrack bool
	// Note is a one-line human summary, always safe to display.
	Note string
}

// ProjectFund estimates when a fund reaches its goal.
func ProjectFund(f store.Fund, avgPerMonth money.Cents, now time.Time) Projection {
	remaining := f.Remaining()

	if !f.HasGoal() {
		return Projection{Note: "No target set for this fund yet."}
	}
	if remaining <= 0 {
		return Projection{
			Reachable: true,
			Months:    0,
			ETA:       now,
			OnTrack:   true,
			Note:      fmt.Sprintf("Goal reached — %s saved of %s.", f.Balance.Display(), f.Goal.Display()),
		}
	}

	if avgPerMonth <= 0 {
		need := f.MonthlyNeeded()
		if need > 0 {
			return Projection{
				PerMonth: 0,
				Note: fmt.Sprintf("No deposits yet. Save %s a month to reach %s in %d months.",
					need.Display(), f.Goal.Display(), f.TargetMonths),
			}
		}
		return Projection{
			Note: fmt.Sprintf("%s still needed. Add a deposit to start a projection.",
				remaining.Display()),
		}
	}

	// Round up: a partial month still needs to be lived through.
	months := int(math.Ceil(float64(remaining) / float64(avgPerMonth)))
	if months < 0 || months > 1200 {
		// Guard against an absurd horizon from a tiny contribution rate; a
		// hundred-year estimate is noise, not information.
		return Projection{
			PerMonth: avgPerMonth,
			Note: fmt.Sprintf("At %s a month this goal is more than a century away.",
				avgPerMonth.Display()),
		}
	}

	eta := now.AddDate(0, months, 0)
	p := Projection{
		Reachable: true,
		Months:    months,
		ETA:       eta,
		PerMonth:  avgPerMonth,
		OnTrack:   f.TargetMonths == 0 || months <= f.TargetMonths,
	}

	switch {
	case f.TargetMonths == 0:
		p.Note = fmt.Sprintf("At %s a month, %s reaches %s in %s (%s).",
			avgPerMonth.Display(), f.Name, f.Goal.Display(), plural(months, "month"),
			eta.Format("Jan 2006"))
	case p.OnTrack:
		p.Note = fmt.Sprintf("On track: %s in %s, inside your %s target.",
			f.Goal.Display(), plural(months, "month"), plural(f.TargetMonths, "month"))
	default:
		p.Note = fmt.Sprintf("Behind: %s at this rate, %s later than your %s target. Save %s a month to catch up.",
			plural(months, "month"), plural(months-f.TargetMonths, "month"),
			plural(f.TargetMonths, "month"), f.MonthlyNeeded().Display())
	}
	return p
}

// AverageMonthlyDeposit is the mean contribution across months that had activity,
// which is fairer than dividing by the calendar span for someone who started late.
func AverageMonthlyDeposit(deposits money.Cents, activeMonths int) money.Cents {
	if activeMonths <= 0 || deposits <= 0 {
		return 0
	}
	return money.Cents(int64(deposits) / int64(activeMonths))
}

// Severity ranks an observation for styling and ordering.
type Severity string

const (
	// Alert is something the user should act on.
	Alert Severity = "alert"
	// Warn is worth noticing but not urgent.
	Warn Severity = "warn"
	// Good is positive reinforcement.
	Good Severity = "good"
	// Info is neutral context.
	Info Severity = "info"
)

// Observation is a single line of feedback for the dashboard.
type Observation struct {
	Severity Severity
	Text     string
}

// Observations derives the dashboard's feedback list, alerts first so the thing needing
// attention is not buried.
func Observations(t store.Totals, essential, nonEssential money.Cents, budgets []store.Budget, months []store.MonthPoint) []Observation {
	var alerts, warns, goods, infos []Observation

	// Budget breaches are the most actionable thing on the page.
	for _, b := range budgets {
		switch b.Status() {
		case "over":
			alerts = append(alerts, Observation{Alert, fmt.Sprintf(
				"%s is %s over its %s budget.",
				b.Category, (-b.Remaining()).Display(), b.Limit.Display())})
		case "warn":
			warns = append(warns, Observation{Warn, fmt.Sprintf(
				"%s has used %.0f%% of its %s budget, %s left.",
				b.Category, b.Progress(), b.Limit.Display(), b.Remaining().Display())})
		}
	}

	// Spending more than earning, this period.
	if t.Income > 0 && t.Expense > t.Income {
		alerts = append(alerts, Observation{Alert, fmt.Sprintf(
			"Spending exceeded income by %s this period.", (t.Expense - t.Income).Display())})
	}

	// Negative cash means committed money has been double-spent.
	if t.Cash() < 0 {
		alerts = append(alerts, Observation{Alert, fmt.Sprintf(
			"Available cash is %s. Check for a mistyped amount or an unrecorded income.",
			t.Cash().Display())})
	}

	// Savings rate, the single most informative number in personal finance.
	if t.Income > 0 {
		rate := float64(t.Income-t.Expense) / float64(t.Income) * 100
		switch {
		case rate >= 20:
			goods = append(goods, Observation{Good, fmt.Sprintf(
				"You kept %.0f%% of your income this period.", rate)})
		case rate >= 0:
			infos = append(infos, Observation{Info, fmt.Sprintf(
				"You kept %.0f%% of your income this period.", rate)})
		}
	}

	// The essential flag was collected on every expense form in the old app
	// and displayed nowhere. This is the payoff for having stored it.
	if spend := essential + nonEssential; spend > 0 {
		share := float64(nonEssential) / float64(spend) * 100
		switch {
		case share >= 50:
			warns = append(warns, Observation{Warn, fmt.Sprintf(
				"%.0f%% of spending (%s) was non-essential.", share, nonEssential.Display())})
		case nonEssential > 0:
			infos = append(infos, Observation{Info, fmt.Sprintf(
				"%.0f%% of spending (%s) was non-essential.", share, nonEssential.Display())})
		default:
			goods = append(goods, Observation{Good, "All recorded spending was marked essential."})
		}
	}

	// Month-on-month direction, using the two most recent complete data points.
	if n := len(months); n >= 2 {
		prev, cur := months[n-2], months[n-1]
		if prev.Expense > 0 && cur.Expense > 0 {
			delta := cur.Expense - prev.Expense
			pct := float64(delta) / float64(prev.Expense) * 100
			switch {
			case pct >= 25:
				warns = append(warns, Observation{Warn, fmt.Sprintf(
					"Spending is up %.0f%% (%s) on last month.", pct, delta.Display())})
			case pct <= -15:
				goods = append(goods, Observation{Good, fmt.Sprintf(
					"Spending is down %.0f%% (%s) on last month.", -pct, (-delta).Display())})
			}
		}
	}

	// Savings held.
	if t.Saved() > 0 {
		infos = append(infos, Observation{Info, fmt.Sprintf(
			"%s is set aside in savings funds.", t.Saved().Display())})
	}

	out := make([]Observation, 0, len(alerts)+len(warns)+len(goods)+len(infos))
	out = append(out, alerts...)
	out = append(out, warns...)
	out = append(out, goods...)
	out = append(out, infos...)

	if len(out) == 0 {
		out = append(out, Observation{Info,
			"Add some income and a few expenses and this panel will start explaining your numbers."})
	}
	return out
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// ── forecast ──────────────────────────────────────────────────────────────────

// IncomeRange estimates next month's income as a range, because a single figure
// is misleading when income varies.
type IncomeRange struct {
	// Months is how many months of history the estimate is based on.
	Months int
	// Mean is the average monthly income.
	Mean money.Cents
	// Low and High bound the interval.
	Low, High money.Cents
	// Confidence is the nominal confidence level, e.g. 90.
	Confidence int
	// Reliable is false when there is too little history for an interval, in
	// which case Low and High equal Mean.
	Reliable bool
	// Note explains the figure in words, always safe to display.
	Note string
}

// Spread is the width of the interval.
func (r IncomeRange) Spread() money.Cents { return r.High - r.Low }

// zFor90 is the two-sided normal critical value for 90% confidence.
const zFor90 = 1.645

// minMonthsForInterval is the fewest months that can produce a spread: one has no
// variance to measure, and with two a single unusual month dominates it.
const minMonthsForInterval = 3

// EstimateMonthlyIncome derives an expected-income range from monthly history.
func EstimateMonthlyIncome(months []store.MonthPoint) IncomeRange {
	var vals []float64
	for _, m := range months {
		if m.Income > 0 {
			vals = append(vals, float64(m.Income))
		}
	}

	r := IncomeRange{Confidence: 90, Months: len(vals)}

	switch len(vals) {
	case 0:
		r.Note = "No income recorded yet, so there is nothing to project from."
		return r
	case 1:
		r.Mean, r.Low, r.High = money.Cents(vals[0]), money.Cents(vals[0]), money.Cents(vals[0])
		r.Note = fmt.Sprintf("Based on a single month (%s). At least %d months are needed for a range.",
			r.Mean.Display(), minMonthsForInterval)
		return r
	}

	var sum float64
	for _, v := range vals {
		sum += v
	}
	n := float64(len(vals))
	mean := sum / n
	r.Mean = money.Cents(int64(mean + 0.5))

	if len(vals) < minMonthsForInterval {
		r.Low, r.High = r.Mean, r.Mean
		r.Note = fmt.Sprintf("Averaging %s over %d months. At least %d months are needed for a range.",
			r.Mean.Display(), len(vals), minMonthsForInterval)
		return r
	}

	// Sample variance divides by n-1: with a small sample the population formula
	// understates the spread and makes the interval look tighter than the data allows.
	var ss float64
	for _, v := range vals {
		d := v - mean
		ss += d * d
	}
	sd := math.Sqrt(ss / (n - 1))
	margin := zFor90 * sd / math.Sqrt(n)

	low := mean - margin
	if low < 0 {
		// Income cannot be negative, so a lower bound below zero is an artefact
		// of the normal approximation rather than information.
		low = 0
	}

	r.Low = money.Cents(int64(low + 0.5))
	r.High = money.Cents(int64(mean + margin + 0.5))
	r.Reliable = true

	switch {
	case sd == 0:
		r.Note = fmt.Sprintf("Income has been exactly %s for %d months running.",
			r.Mean.Display(), len(vals))
	default:
		r.Note = fmt.Sprintf("Expect roughly %s to %s next month, based on %d months of income (average %s).",
			r.Low.Display(), r.High.Display(), len(vals), r.Mean.Display())
	}
	return r
}

// ExpenseRange is what recurring expenses are expected to cost, as a range: rent is
// exact but a water bill is not, so a single number would be false precision.
type ExpenseRange struct {
	Low, High money.Cents
	// Expected is the figure the funding waterfall actually works to.
	Expected money.Cents
	// Fixed and Variable split the total by cost kind.
	Fixed, Variable money.Cents
	// VariableCount is how many buckets contribute uncertainty.
	VariableCount int
	// Note explains the figure in words.
	Note string
}

// Exact reports whether every bucket is a fixed cost, so the range is a point.
func (r ExpenseRange) Exact() bool { return r.Low == r.High }

// Spread is the width of the range.
func (r ExpenseRange) Spread() money.Cents { return r.High - r.Low }

// EstimateMonthlyExpenses brackets the monthly recurring cost.
func EstimateMonthlyExpenses(buckets []store.Bucket) ExpenseRange {
	var r ExpenseRange

	for _, b := range buckets {
		r.Expected += b.Due
		if b.CostKind == store.CostFixed {
			r.Fixed += b.Fixed
			r.Low += b.Fixed
			r.High += b.Fixed
			continue
		}
		r.Variable += b.Due
		r.VariableCount++
		r.Low += b.Low
		r.High += b.High
	}

	// The expected figure must sit inside the range.
	if r.Expected < r.Low {
		r.Low = r.Expected
	}
	if r.Expected > r.High {
		r.High = r.Expected
	}

	switch {
	case len(buckets) == 0:
		r.Note = "No recurring expenses set up yet."
	case r.VariableCount == 0:
		r.Note = fmt.Sprintf("%s a month, all of it fixed.", r.Expected.Display())
	case r.Low == r.High:
		r.Note = fmt.Sprintf("%s a month. Not enough history yet to show how much the %s vary.",
			r.Expected.Display(), plural(r.VariableCount, "variable bill"))
	default:
		r.Note = fmt.Sprintf("Between %s and %s a month: %s fixed, plus %s that vary.",
			r.Low.Display(), r.High.Display(), r.Fixed.Display(),
			plural(r.VariableCount, "bill"))
	}
	return r
}

// Trend is a least-squares straight line fitted through a series.
type Trend struct {
	// Values is the fitted line, one point per input point, so a chart can plot
	// it as a second dataset against the same labels.
	Values []float64
	// PerStep is the gradient in cents per point.
	PerStep float64
	// Rising is true when the line slopes upwards.
	Rising bool
	// Note describes the direction in words.
	Note string
	// OK is false when there were too few points to fit a line.
	OK bool
}

// FitTrend fits a least-squares line through a running-balance series.
func FitTrend(points []store.Point) Trend {
	n := len(points)
	if n < 2 {
		return Trend{Note: "Not enough history to show a trend yet."}
	}

	var sumX, sumY, sumXY, sumXX float64
	for i, p := range points {
		x := float64(i)
		y := float64(p.Balance)
		sumX += x
		sumY += y
		sumXY += x * y
		sumXX += x * x
	}

	fn := float64(n)
	denom := fn*sumXX - sumX*sumX
	if denom == 0 {
		// Every x identical, which cannot happen with distinct indices, but
		// dividing by zero here would produce NaN and poison the chart.
		return Trend{Note: "Not enough history to show a trend yet."}
	}

	slope := (fn*sumXY - sumX*sumY) / denom
	intercept := (sumY - slope*sumX) / fn

	t := Trend{
		Values:  make([]float64, n),
		PerStep: slope,
		Rising:  slope > 0,
		OK:      true,
	}
	for i := range points {
		// Divided by 100 because the chart plots dollars, while the series is
		// held in cents everywhere else.
		t.Values[i] = (intercept + slope*float64(i)) / 100
	}

	change := money.Cents(int64(slope*float64(n-1) + 0.5))
	switch {
	case slope > 0:
		t.Note = fmt.Sprintf("Trending up: about %s across this period.", change.Display())
	case slope < 0:
		t.Note = fmt.Sprintf("Trending down: about %s across this period.", (-change).Display())
	default:
		t.Note = "Flat across this period."
	}
	return t
}

// Runway answers how long the emergency fund will last.
type Runway struct {
	// Balance is what is in the fund now.
	Balance money.Cents
	// BurnRate is the average monthly amount withdrawn from it.
	BurnRate money.Cents
	// Months is how many months the balance covers at that burn rate.
	Months float64
	// EssentialMonthly is the total of the user's essential recurring expenses.
	EssentialMonthly money.Cents
	// CoverMonths is how many months of essential expenses the balance covers,
	// which is the conventional measure of an emergency fund's adequacy.
	CoverMonths float64
	// Target is the recommended balance: TargetMonths of essential expenses.
	Target money.Cents
	// TargetMonths is the number of months the target represents.
	TargetMonths int
	// Depleting is true when money is actually being taken out.
	Depleting bool
	// Note summarises the position in words.
	Note string
	// Warning is set when the fund is in trouble.
	Warning string
}

// DefaultEmergencyMonths is the conventional recommendation for an emergency
// fund, used when the user has not set their own target.
const DefaultEmergencyMonths = 3

// AssessEmergencyFund answers two different questions: how long the fund lasts at the
// observed withdrawal rate, and how many months of essential expenses it covers.
func AssessEmergencyFund(fund store.Fund, withdrawals []store.MonthPoint, essentialMonthly money.Cents) Runway {
	r := Runway{
		Balance:          fund.Balance,
		EssentialMonthly: essentialMonthly,
		TargetMonths:     DefaultEmergencyMonths,
	}
	if fund.TargetMonths > 0 {
		r.TargetMonths = fund.TargetMonths
	}
	r.Target = essentialMonthly * money.Cents(r.TargetMonths)

	// Burn rate is the mean of the months that saw a withdrawal.
	var total money.Cents
	active := 0
	for _, m := range withdrawals {
		if m.Expense > 0 {
			total += m.Expense
			active++
		}
	}
	if active > 0 {
		r.BurnRate = money.Cents(int64(total) / int64(active))
		r.Depleting = true
	}

	if essentialMonthly > 0 {
		r.CoverMonths = float64(fund.Balance) / float64(essentialMonthly)
	}

	switch {
	case fund.Balance <= 0 && essentialMonthly > 0:
		r.Note = fmt.Sprintf("Nothing set aside. %s a month of essential expenses means a %d month target of %s.",
			essentialMonthly.Display(), r.TargetMonths, r.Target.Display())
		r.Warning = "You have no emergency fund."

	case fund.Balance <= 0:
		r.Note = "Nothing set aside yet, and no essential expenses marked to size a target from."

	case !r.Depleting && essentialMonthly > 0:
		r.Note = fmt.Sprintf("%s saved, covering %.1f months of essential expenses (%s a month). Nothing has been withdrawn, so there is no depletion rate to project.",
			fund.Balance.Display(), r.CoverMonths, essentialMonthly.Display())

	case !r.Depleting:
		r.Note = fmt.Sprintf("%s saved. Mark some recurring expenses as essential and this will show how many months they are covered for.",
			fund.Balance.Display())

	default:
		r.Months = float64(fund.Balance) / float64(r.BurnRate)
		r.Note = fmt.Sprintf("Withdrawing about %s a month. At that rate %s lasts around %.1f more months.",
			r.BurnRate.Display(), fund.Balance.Display(), r.Months)
	}

	// Warnings, most serious last so the strongest one survives.
	if r.Depleting && r.Months > 0 && r.Months < 3 {
		r.Warning = fmt.Sprintf("At the current rate this fund runs out in under %.0f months.",
			math.Ceil(r.Months))
	}
	if essentialMonthly > 0 && fund.Balance > 0 && r.CoverMonths < 1 {
		r.Warning = "Less than one month of essential expenses is covered."
	}
	return r
}

// TargetProgress is how close the emergency fund is to its recommended size.
func (r Runway) TargetProgress() float64 { return money.Ratio(r.Balance, r.Target) }

// Adequate reports whether the fund meets its target.
func (r Runway) Adequate() bool { return r.Target > 0 && r.Balance >= r.Target }
