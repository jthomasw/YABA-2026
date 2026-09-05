package insight

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jthomasw/YABA-2026/internal/money"
	"github.com/jthomasw/YABA-2026/internal/store"
)

var now = time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)

func TestProjectFundNoGoal(t *testing.T) {
	p := ProjectFund(store.Fund{Name: "Car", Balance: 5000}, 1000, now)
	if p.Reachable {
		t.Error("a fund with no goal is not reachable")
	}
	if p.Note == "" {
		t.Error("note should still explain the situation")
	}
}

func TestProjectFundAlreadyMet(t *testing.T) {
	f := store.Fund{Name: "Medical", Goal: 100000, Balance: 100000}
	p := ProjectFund(f, 5000, now)
	if !p.Reachable || p.Months != 0 || !p.OnTrack {
		t.Fatalf("met goal should be reachable in 0 months and on track, got %+v", p)
	}
	if !strings.Contains(p.Note, "Goal reached") {
		t.Errorf("note = %q", p.Note)
	}
}

func TestProjectFundRoundsMonthsUp(t *testing.T) {
	// $100 remaining at $30/month is 3.33 months, which must present as 4:
	// three months of deposits leaves the user short.
	f := store.Fund{Name: "Trip", Goal: 10000, Balance: 0}
	p := ProjectFund(f, 3000, now)
	if p.Months != 4 {
		t.Errorf("months = %d, want 4", p.Months)
	}
	if got, want := p.ETA.Format("2006-01"), "2026-12"; got != want {
		t.Errorf("ETA = %s, want %s", got, want)
	}
}

func TestProjectFundOnTrackVsBehind(t *testing.T) {
	// Needs 10 months at the current rate, target is 12 -> on track.
	onTrack := ProjectFund(store.Fund{Name: "A", Goal: 100000, TargetMonths: 12}, 10000, now)
	if !onTrack.OnTrack {
		t.Errorf("10 months against a 12 month target should be on track: %+v", onTrack)
	}
	if !strings.Contains(onTrack.Note, "On track") {
		t.Errorf("note = %q", onTrack.Note)
	}

	// Needs 20 months, target is 6 -> behind, and must name the catch-up rate.
	behind := ProjectFund(store.Fund{Name: "B", Goal: 100000, TargetMonths: 6}, 5000, now)
	if behind.OnTrack {
		t.Errorf("20 months against a 6 month target is behind: %+v", behind)
	}
	if !strings.Contains(behind.Note, "Behind") {
		t.Errorf("note = %q", behind.Note)
	}
}

func TestProjectFundZeroRateFallsBackToRequiredRate(t *testing.T) {
	f := store.Fund{Name: "Rainy", Goal: 60000, Balance: 0, TargetMonths: 6}
	p := ProjectFund(f, 0, now)
	if p.Reachable {
		t.Error("no contributions means no projection")
	}
	// $600 over 6 months is $100/month.
	if !strings.Contains(p.Note, "$100.00 a month") {
		t.Errorf("note should state the required rate, got %q", p.Note)
	}
}

func TestProjectFundAbsurdHorizonIsSuppressed(t *testing.T) {
	// One cent a month against a $1m goal is not a useful estimate.
	f := store.Fund{Name: "Slow", Goal: 100000000, Balance: 0}
	p := ProjectFund(f, 1, now)
	if p.Reachable {
		t.Error("century-long projections should not be presented as reachable")
	}
	if !strings.Contains(p.Note, "century") {
		t.Errorf("note = %q", p.Note)
	}
}

func TestMonthlyNeededRoundsUp(t *testing.T) {
	// $100 over 3 months is 33.33 -> 3334 cents, so three payments clear it.
	f := store.Fund{Goal: 10000, Balance: 0, TargetMonths: 3}
	got := f.MonthlyNeeded()
	if got != 3334 {
		t.Errorf("MonthlyNeeded = %d, want 3334", got)
	}
	if got*3 < f.Remaining() {
		t.Errorf("three payments of %d do not cover %d", got, f.Remaining())
	}
}

func TestAverageMonthlyDeposit(t *testing.T) {
	if got := AverageMonthlyDeposit(30000, 3); got != 10000 {
		t.Errorf("got %d, want 10000", got)
	}
	if got := AverageMonthlyDeposit(30000, 0); got != 0 {
		t.Errorf("zero active months must not divide by zero, got %d", got)
	}
	if got := AverageMonthlyDeposit(0, 5); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

func TestObservationsEmptyAccountStillGuidesTheUser(t *testing.T) {
	obs := Observations(store.Totals{}, 0, 0, nil, nil)
	if len(obs) != 1 {
		t.Fatalf("want exactly one guiding observation, got %d: %+v", len(obs), obs)
	}
	if obs[0].Severity != Info {
		t.Errorf("severity = %s, want info", obs[0].Severity)
	}
}

func TestObservationsPutsAlertsFirst(t *testing.T) {
	totals := store.Totals{Income: 100000, Expense: 150000}
	budgets := []store.Budget{
		{Category: "Food", Limit: 10000, Spent: 15000}, // over  -> alert
		{Category: "Fuel", Limit: 10000, Spent: 8500},  // 85%   -> warn
	}
	obs := Observations(totals, 100000, 50000, budgets, nil)

	if len(obs) < 3 {
		t.Fatalf("expected several observations, got %+v", obs)
	}
	if obs[0].Severity != Alert {
		t.Errorf("first observation should be an alert, got %s (%q)", obs[0].Severity, obs[0].Text)
	}

	// Once an alert appears, no later entry may be a higher priority.
	rank := map[Severity]int{Alert: 0, Warn: 1, Good: 2, Info: 3}
	for i := 1; i < len(obs); i++ {
		if rank[obs[i].Severity] < rank[obs[i-1].Severity] {
			t.Errorf("severity order broken at %d: %s after %s", i, obs[i].Severity, obs[i-1].Severity)
		}
	}
}

func TestObservationsReportsOverspend(t *testing.T) {
	obs := Observations(store.Totals{Income: 50000, Expense: 80000}, 80000, 0, nil, nil)
	if !containsText(obs, "exceeded income by $300.00") {
		t.Errorf("missing overspend line: %+v", obs)
	}
}

func TestObservationsReportsNegativeCash(t *testing.T) {
	// Income 100, spent 50, but 200 moved to a fund -> cash is -150.
	totals := store.Totals{Income: 10000, Expense: 5000, Deposits: 20000}
	if totals.Cash() >= 0 {
		t.Fatalf("fixture should produce negative cash, got %d", totals.Cash())
	}
	obs := Observations(totals, 5000, 0, nil, nil)
	if !containsText(obs, "Available cash is -$150.00") {
		t.Errorf("missing negative cash alert: %+v", obs)
	}
}

func TestObservationsSurfacesEssentialSplit(t *testing.T) {
	// 60% non-essential should warn.
	obs := Observations(store.Totals{Income: 100000, Expense: 100000}, 40000, 60000, nil, nil)
	if !containsText(obs, "60% of spending") {
		t.Errorf("missing essential split: %+v", obs)
	}

	// All essential should be positive, not a warning.
	obs = Observations(store.Totals{Income: 100000, Expense: 50000}, 50000, 0, nil, nil)
	if !containsText(obs, "All recorded spending was marked essential") {
		t.Errorf("missing all-essential line: %+v", obs)
	}
}

func TestObservationsMonthOverMonth(t *testing.T) {
	months := []store.MonthPoint{
		{Month: "2026-07", Income: 100000, Expense: 40000},
		{Month: "2026-08", Income: 100000, Expense: 60000}, // +50%
	}
	obs := Observations(store.Totals{Income: 100000, Expense: 60000}, 60000, 0, nil, months)
	if !containsText(obs, "up 50%") {
		t.Errorf("missing month-over-month rise: %+v", obs)
	}

	months[1].Expense = 20000 // -50%
	obs = Observations(store.Totals{Income: 100000, Expense: 20000}, 20000, 0, nil, months)
	if !containsText(obs, "down 50%") {
		t.Errorf("missing month-over-month fall: %+v", obs)
	}
}

func TestObservationsNeverDividesByZero(t *testing.T) {
	// A month with zero previous spending must not produce Inf or NaN text.
	months := []store.MonthPoint{
		{Month: "2026-07", Income: 0, Expense: 0},
		{Month: "2026-08", Income: 0, Expense: 5000},
	}
	for _, o := range Observations(store.Totals{}, 0, 0, nil, months) {
		if strings.Contains(o.Text, "Inf") || strings.Contains(o.Text, "NaN") {
			t.Errorf("bad arithmetic leaked into text: %q", o.Text)
		}
	}
}

func TestTotalsNetWorthIgnoresTransfers(t *testing.T) {
	// The property the old code violated: moving money into a fund must not
	// change net worth.
	before := store.Totals{Income: 100000, Expense: 20000}
	after := store.Totals{Income: 100000, Expense: 20000, Deposits: 50000}

	if before.NetWorth() != after.NetWorth() {
		t.Errorf("a transfer changed net worth: %d -> %d", before.NetWorth(), after.NetWorth())
	}
	if after.Cash() != before.Cash()-50000 {
		t.Errorf("cash should fall by the deposit, got %d", after.Cash())
	}
	if after.Saved() != 50000 {
		t.Errorf("saved should equal the deposit, got %d", after.Saved())
	}
	if after.Cash()+after.Saved() != after.NetWorth() {
		t.Errorf("cash + saved must equal net worth")
	}
}

func TestBudgetStatusThresholds(t *testing.T) {
	tests := []struct {
		spent money.Cents
		limit money.Cents
		want  string
	}{
		{0, 10000, "ok"},
		{7999, 10000, "ok"},
		{8000, 10000, "warn"},
		{10000, 10000, "warn"}, // exactly at the cap is not yet over
		{10001, 10000, "over"},
	}
	for _, tc := range tests {
		b := store.Budget{Limit: tc.limit, Spent: tc.spent}
		if got := b.Status(); got != tc.want {
			t.Errorf("Budget{spent:%d limit:%d}.Status() = %q, want %q", tc.spent, tc.limit, got, tc.want)
		}
	}
}

func TestBudgetProgressClampsButOverStillReported(t *testing.T) {
	b := store.Budget{Limit: 10000, Spent: 30000}
	if got := b.Progress(); got != 100 {
		t.Errorf("Progress should clamp to 100, got %v", got)
	}
	if !b.Over() {
		t.Error("Over() must still be true when the bar is clamped")
	}
	if b.Remaining() != -20000 {
		t.Errorf("Remaining = %d, want -20000", b.Remaining())
	}
}

func TestFundProgressAndRemaining(t *testing.T) {
	f := store.Fund{Goal: 100000, Balance: 25000}
	if got := f.Progress(); got != 25 {
		t.Errorf("Progress = %v, want 25", got)
	}
	if got := f.Remaining(); got != 75000 {
		t.Errorf("Remaining = %d, want 75000", got)
	}

	over := store.Fund{Goal: 100000, Balance: 120000}
	if over.Remaining() != 0 {
		t.Errorf("Remaining should floor at 0, got %d", over.Remaining())
	}
	if !over.Complete() {
		t.Error("Complete() should be true")
	}

	none := store.Fund{Goal: 0, Balance: 5000}
	if none.Progress() != 0 {
		t.Errorf("no goal must not divide by zero, got %v", none.Progress())
	}
	if none.HasGoal() {
		t.Error("HasGoal() should be false")
	}
}

func TestMonthPointSavingsRate(t *testing.T) {
	m := store.MonthPoint{Income: 100000, Expense: 75000}
	if got := m.SavingsRate(); got != 25 {
		t.Errorf("SavingsRate = %v, want 25", got)
	}
	if got := (store.MonthPoint{}).SavingsRate(); got != 0 {
		t.Errorf("zero income must not divide by zero, got %v", got)
	}
	if got := m.Net(); got != 25000 {
		t.Errorf("Net = %d, want 25000", got)
	}
}

func containsText(obs []Observation, sub string) bool {
	for _, o := range obs {
		if strings.Contains(o.Text, sub) {
			return true
		}
	}
	return false
}

// ═════════════════════════════════════════════════════════════════════════════
// forecast_test.go
// ═════════════════════════════════════════════════════════════════════════════

func months(vals ...money.Cents) []store.MonthPoint {
	out := make([]store.MonthPoint, len(vals))
	for i, v := range vals {
		out[i] = store.MonthPoint{Month: "2026-01", Income: v}
	}
	return out
}

func TestEstimateMonthlyIncomeNoHistory(t *testing.T) {
	r := EstimateMonthlyIncome(nil)
	if r.Reliable || r.Mean != 0 || r.Months != 0 {
		t.Errorf("empty history should produce nothing: %+v", r)
	}
	if r.Note == "" {
		t.Error("note should explain why there is no estimate")
	}
}

func TestEstimateMonthlyIncomeIgnoresZeroMonths(t *testing.T) {
	// Two real months of 1000 plus four months of nothing must average 1000,
	// not 333. A user who joined recently should not see their salary diluted.
	series := []store.MonthPoint{
		{Month: "2026-01"}, {Month: "2026-02"},
		{Month: "2026-03", Income: 100000},
		{Month: "2026-04", Income: 100000},
		{Month: "2026-05", Income: 100000},
		{Month: "2026-06"},
	}
	r := EstimateMonthlyIncome(series)
	if r.Months != 3 {
		t.Errorf("Months = %d, want 3 (zero months excluded)", r.Months)
	}
	if r.Mean != 100000 {
		t.Errorf("Mean = %s, want $1,000.00", r.Mean.Display())
	}
}

func TestEstimateMonthlyIncomeSingleMonthHasNoRange(t *testing.T) {
	r := EstimateMonthlyIncome(months(50000))
	if r.Reliable {
		t.Error("one month cannot support an interval")
	}
	if r.Low != r.High || r.Low != r.Mean {
		t.Errorf("bounds should collapse to the mean: %+v", r)
	}
}

func TestEstimateMonthlyIncomeBelowThresholdHasNoRange(t *testing.T) {
	r := EstimateMonthlyIncome(months(50000, 70000))
	if r.Reliable {
		t.Errorf("two months is below the %d month floor", minMonthsForInterval)
	}
	if r.Spread() != 0 {
		t.Errorf("spread should be zero, got %s", r.Spread().Display())
	}
	if !strings.Contains(r.Note, "at least") && !strings.Contains(r.Note, "At least") {
		t.Errorf("note should say more months are needed: %q", r.Note)
	}
}

func TestEstimateMonthlyIncomeConstantIncomeHasZeroSpread(t *testing.T) {
	r := EstimateMonthlyIncome(months(100000, 100000, 100000, 100000))
	if !r.Reliable {
		t.Fatal("four months should produce an interval")
	}
	if r.Spread() != 0 {
		t.Errorf("identical income should have no spread, got %s", r.Spread().Display())
	}
	if r.Low != 100000 || r.High != 100000 {
		t.Errorf("bounds = %s..%s, want both $1,000.00", r.Low.Display(), r.High.Display())
	}
	if !strings.Contains(r.Note, "exactly") {
		t.Errorf("note = %q", r.Note)
	}
}

func TestEstimateMonthlyIncomeIntervalBracketsTheMean(t *testing.T) {
	r := EstimateMonthlyIncome(months(80000, 100000, 120000, 90000, 110000))
	if !r.Reliable {
		t.Fatal("five months should produce an interval")
	}
	if r.Mean != 100000 {
		t.Errorf("Mean = %s, want $1,000.00", r.Mean.Display())
	}
	if r.Low >= r.Mean || r.High <= r.Mean {
		t.Errorf("interval %s..%s does not bracket mean %s",
			r.Low.Display(), r.High.Display(), r.Mean.Display())
	}
	if r.Confidence != 90 {
		t.Errorf("Confidence = %d, want 90", r.Confidence)
	}
}

func TestEstimateMonthlyIncomeWiderSpreadGivesWiderInterval(t *testing.T) {
	tight := EstimateMonthlyIncome(months(99000, 100000, 101000, 100000))
	wide := EstimateMonthlyIncome(months(20000, 100000, 180000, 100000))

	if tight.Mean != wide.Mean {
		t.Fatalf("fixtures should share a mean: %s vs %s", tight.Mean.Display(), wide.Mean.Display())
	}
	if wide.Spread() <= tight.Spread() {
		t.Errorf("volatile income should give a wider interval: tight=%s wide=%s",
			tight.Spread().Display(), wide.Spread().Display())
	}
}

func TestEstimateMonthlyIncomeNeverGoesNegative(t *testing.T) {
	// Highly volatile low income would push the normal lower bound below zero.
	r := EstimateMonthlyIncome(months(100, 50000, 100, 90000, 100))
	if r.Low < 0 {
		t.Errorf("Low = %s, must clamp at zero", r.Low.Display())
	}
}

// ── trend line ────────────────────────────────────────────────────────────────

func points(vals ...money.Cents) []store.Point {
	out := make([]store.Point, len(vals))
	for i, v := range vals {
		out[i] = store.Point{Date: "2026-01-01", Balance: v}
	}
	return out
}

func TestFitTrendNeedsTwoPoints(t *testing.T) {
	if FitTrend(nil).OK {
		t.Error("no points should not fit")
	}
	if FitTrend(points(1000)).OK {
		t.Error("one point should not fit")
	}
}

func TestFitTrendPerfectlyLinearSeries(t *testing.T) {
	// 100, 200, 300, 400 -> slope of exactly 100 cents per step, and the fitted
	// line should reproduce the input.
	tr := FitTrend(points(10000, 20000, 30000, 40000))
	if !tr.OK {
		t.Fatal("should fit")
	}
	if math.Abs(tr.PerStep-10000) > 0.001 {
		t.Errorf("PerStep = %v, want 10000", tr.PerStep)
	}
	if !tr.Rising {
		t.Error("Rising should be true")
	}
	want := []float64{100, 200, 300, 400} // dollars
	for i, w := range want {
		if math.Abs(tr.Values[i]-w) > 0.001 {
			t.Errorf("Values[%d] = %v, want %v", i, tr.Values[i], w)
		}
	}
	if !strings.Contains(tr.Note, "up") {
		t.Errorf("Note = %q", tr.Note)
	}
}

func TestFitTrendFallingSeries(t *testing.T) {
	tr := FitTrend(points(40000, 30000, 20000, 10000))
	if tr.Rising {
		t.Error("Rising should be false")
	}
	if tr.PerStep >= 0 {
		t.Errorf("PerStep = %v, want negative", tr.PerStep)
	}
	if !strings.Contains(tr.Note, "down") {
		t.Errorf("Note = %q", tr.Note)
	}
	// The note reports magnitude, so it must not contain a minus sign.
	if strings.Contains(tr.Note, "-$") {
		t.Errorf("Note should state magnitude, not a negative: %q", tr.Note)
	}
}

func TestFitTrendFlatSeries(t *testing.T) {
	tr := FitTrend(points(50000, 50000, 50000))
	if tr.PerStep != 0 {
		t.Errorf("PerStep = %v, want 0", tr.PerStep)
	}
	if !strings.Contains(tr.Note, "Flat") {
		t.Errorf("Note = %q", tr.Note)
	}
}

func TestFitTrendProducesOneValuePerPoint(t *testing.T) {
	in := points(1000, 5000, 2000, 8000, 3000)
	tr := FitTrend(in)
	if len(tr.Values) != len(in) {
		t.Errorf("got %d fitted values for %d points", len(tr.Values), len(in))
	}
	for i, v := range tr.Values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			t.Errorf("Values[%d] is not a number: %v", i, v)
		}
	}
}

// ── emergency fund ────────────────────────────────────────────────────────────

func TestAssessEmergencyFundEmpty(t *testing.T) {
	r := AssessEmergencyFund(store.Fund{}, nil, 150000)
	if r.Warning == "" {
		t.Error("no fund with essential expenses should warn")
	}
	// 3 months of $1,500 is the default target.
	if r.Target != 450000 {
		t.Errorf("Target = %s, want $4,500.00", r.Target.Display())
	}
	if r.TargetMonths != DefaultEmergencyMonths {
		t.Errorf("TargetMonths = %d, want %d", r.TargetMonths, DefaultEmergencyMonths)
	}
}

func TestAssessEmergencyFundUntouchedHasNoRunwayButHasCover(t *testing.T) {
	fund := store.Fund{Balance: 300000} // $3,000
	r := AssessEmergencyFund(fund, nil, 100000)

	if r.Depleting {
		t.Error("no withdrawals means not depleting")
	}
	if r.Months != 0 {
		t.Errorf("Months = %v; an untouched fund has no depletion rate", r.Months)
	}
	if math.Abs(r.CoverMonths-3) > 0.001 {
		t.Errorf("CoverMonths = %v, want 3", r.CoverMonths)
	}
	if !strings.Contains(r.Note, "no depletion rate") {
		t.Errorf("Note = %q", r.Note)
	}
}

func TestAssessEmergencyFundRunwayFromBurnRate(t *testing.T) {
	fund := store.Fund{Balance: 300000} // $3,000
	// Two months with withdrawals of $500 each -> burn rate $500.
	wd := []store.MonthPoint{
		{Month: "2026-05", Expense: 50000},
		{Month: "2026-06", Expense: 50000},
	}
	r := AssessEmergencyFund(fund, wd, 100000)

	if !r.Depleting {
		t.Fatal("withdrawals mean depleting")
	}
	if r.BurnRate != 50000 {
		t.Errorf("BurnRate = %s, want $500.00", r.BurnRate.Display())
	}
	if math.Abs(r.Months-6) > 0.001 {
		t.Errorf("Months = %v, want 6", r.Months)
	}
}

func TestAssessEmergencyFundBurnRateIgnoresQuietMonths(t *testing.T) {
	fund := store.Fund{Balance: 100000}
	// One big withdrawal, three quiet months. Averaging over all four would
	// report a comfortable runway for someone who just drained the fund.
	wd := []store.MonthPoint{
		{Month: "2026-03"}, {Month: "2026-04"},
		{Month: "2026-05", Expense: 100000},
		{Month: "2026-06"},
	}
	r := AssessEmergencyFund(fund, wd, 0)

	if r.BurnRate != 100000 {
		t.Errorf("BurnRate = %s, want $1,000.00 (quiet months excluded)", r.BurnRate.Display())
	}
	if math.Abs(r.Months-1) > 0.001 {
		t.Errorf("Months = %v, want 1", r.Months)
	}
	if r.Warning == "" {
		t.Error("a one month runway should warn")
	}
}

func TestAssessEmergencyFundWarnsBelowOneMonthOfCover(t *testing.T) {
	fund := store.Fund{Balance: 50000}          // $500
	r := AssessEmergencyFund(fund, nil, 200000) // $2,000 essential
	if r.CoverMonths >= 1 {
		t.Fatalf("fixture should be under one month: %v", r.CoverMonths)
	}
	if !strings.Contains(r.Warning, "one month") {
		t.Errorf("Warning = %q", r.Warning)
	}
}

func TestAssessEmergencyFundRespectsUserTargetMonths(t *testing.T) {
	fund := store.Fund{Balance: 100000, TargetMonths: 6}
	r := AssessEmergencyFund(fund, nil, 100000)
	if r.TargetMonths != 6 {
		t.Errorf("TargetMonths = %d, want 6", r.TargetMonths)
	}
	if r.Target != 600000 {
		t.Errorf("Target = %s, want $6,000.00", r.Target.Display())
	}
	if r.Adequate() {
		t.Error("$1,000 against a $6,000 target is not adequate")
	}
	if p := r.TargetProgress(); math.Abs(p-16.666) > 0.01 {
		t.Errorf("TargetProgress = %v, want about 16.67", p)
	}
}

func TestAssessEmergencyFundNoDivideByZero(t *testing.T) {
	// No essential expenses and no withdrawals: every derived figure must stay
	// finite rather than becoming NaN or Inf.
	r := AssessEmergencyFund(store.Fund{Balance: 100000}, nil, 0)
	for name, v := range map[string]float64{
		"Months": r.Months, "CoverMonths": r.CoverMonths, "TargetProgress": r.TargetProgress(),
	} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			t.Errorf("%s = %v", name, v)
		}
	}
	if strings.Contains(r.Note, "NaN") || strings.Contains(r.Note, "Inf") {
		t.Errorf("bad arithmetic leaked into the note: %q", r.Note)
	}
}

// ── bucket funding ────────────────────────────────────────────────────────────

func TestBucketStatusAndShortfall(t *testing.T) {
	tests := []struct {
		due, allocated money.Cents
		wantStatus     string
		wantShortfall  money.Cents
	}{
		{0, 0, "empty", 0},
		{10000, 0, "unfunded", 10000},
		{10000, 4000, "partial", 6000},
		{10000, 10000, "funded", 0},
		{10000, 12000, "funded", 0},
	}
	for _, tc := range tests {
		b := store.Bucket{Due: tc.due, Allocated: tc.allocated}
		if got := b.Status(); got != tc.wantStatus {
			t.Errorf("Bucket{due:%d alloc:%d}.Status() = %q, want %q",
				tc.due, tc.allocated, got, tc.wantStatus)
		}
		if got := b.Shortfall(); got != tc.wantShortfall {
			t.Errorf("Bucket{due:%d alloc:%d}.Shortfall() = %d, want %d",
				tc.due, tc.allocated, got, tc.wantShortfall)
		}
	}
}

func TestAllocationSummaryClampsAndReports(t *testing.T) {
	s := store.AllocationSummary{Required: 100000, Allocated: 100000, Income: 150000, Unassigned: 50000}
	if !s.FullyFunded() {
		t.Error("FullyFunded should be true when nothing is short")
	}
	if p := s.Progress(); p != 100 {
		t.Errorf("Progress = %v, want 100", p)
	}

	empty := store.AllocationSummary{}
	if empty.FullyFunded() {
		t.Error("no requirement is not the same as fully funded")
	}
	if p := empty.Progress(); p != 0 {
		t.Errorf("Progress with no requirement = %v, want 0", p)
	}
}
