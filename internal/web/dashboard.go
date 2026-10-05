package web

import (
	"context"
	"net/http"
	"time"

	"github.com/jthomasw/YABA-2026/internal/insight"
	"github.com/jthomasw/YABA-2026/internal/money"
	"github.com/jthomasw/YABA-2026/internal/store"
)

// dashboardView is the four-card dashboard the mockup draws.
type dashboardView struct {
	view

	// Period.
	Month      string // "" means all time
	MonthLabel string
	Months     []string
	ThisMonth  string

	// Which of the four cards is open. "current" is the mockup's default.
	Tab string

	// Whether the recurring-expense planner <details> should render expanded.
	// True right after a bucket action redirects back here, so the section the
	// user was just working in does not appear to have collapsed on them.
	PlannerOpen bool

	// Same idea as PlannerOpen, for the "New savings fund" <details> on the
	// Current Funds tab.
	FundAddOpen bool

	// Card 1 — Current Funds.
	Cash    money.Cents
	Balance []store.Point
	Trend   insight.Trend

	// Card 2 — Emergency Fund: the emergency fund's runway and target.
	EmergencyFund store.Fund
	Runway        insight.Runway

	// Every open fund with its projection. The emergency one is drawn on the
	// Emergency Fund tab; the rest are drawn on Current Funds.
	Funds  []fundCard
	Totals store.Totals

	// Card 3 — Monthly Income. Actual first, the forecast range second.
	IncomeRange    insight.IncomeRange
	Monthly        []store.MonthPoint
	IncomeBySource []store.LabelTotal
	IncomeTotal    money.Cents

	// SpendTotal and IncomeTotal are what actually happened in the period.
	SpendTotal money.Cents

	// The needs/wants split, shown under the expenses total.
	Essential money.Cents
	NonEssent money.Cents

	// Card 4 — Expected Monthly Expenses.
	Buckets         []store.Bucket
	ExpenseRange    insight.ExpenseRange
	Allocation      store.AllocationSummary
	SpendByCategory []store.LabelTotal

	PendingReceipts int
	Charts          chartData
}

type chartData struct {
	BalanceLabels []string
	BalanceValues []float64
	MonthLabels   []string
	MonthIncome   []float64
	MonthExpense  []float64

	// The two doughnuts. They sit on the Expected Monthly Income and Expected
	// Monthly Expenses tabs, which is where a breakdown belongs -- the Add
	// pages are for entry, not analysis.
	IncomeLabels []string
	IncomeValues []float64
	SpendLabels  []string
	SpendValues  []float64
}

// validTabs guards the tab name arriving from a query string or fragment.
var validTabs = map[string]bool{
	"current": true, "emergency": true, "income": true, "expenses": true,
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	ctx := r.Context()

	month := parseMonth(r.URL.Query().Get("month"))

	tab := r.URL.Query().Get("tab")
	if !validTabs[tab] {
		tab = "current"
	}

	v := dashboardView{
		view:        s.baseView(w, r, "Dashboard", "dashboard"),
		Month:       month,
		Tab:         tab,
		PlannerOpen: r.URL.Query().Get("planner") == "open",
		FundAddOpen: r.URL.Query().Get("fund") == "open",
		ThisMonth:   store.Today()[:7],
	}
	v.MonthLabel = "All time"
	if month != "" {
		if t, err := time.Parse(store.MonthLayout, month); err == nil {
			v.MonthLabel = t.Format("January 2006")
		}
	}

	// Anything monthly falls back to the current calendar month, because a
	// recurring-expense budget for "all time" is not a meaningful thing.
	budgetMonth := month
	if budgetMonth == "" {
		budgetMonth = v.ThisMonth
	}

	if err := s.loadDashboard(ctx, sc, month, budgetMonth, &v); err != nil {
		s.serverError(w, r, err)
		return
	}

	v.Charts = buildDashboardCharts(v)
	s.render(w, r, "dashboard.html", v)
}

// loadDashboard fills the view, one card at a time and in order: the emergency
// fund's runway is measured against the buckets the expenses card reads, so it
// cannot run before it. Straight-line calls rather than a list of loaders,
// because that ordering is real and a slice would hide it.
func (s *Server) loadDashboard(ctx context.Context, sc store.Scope, month, budgetMonth string, v *dashboardView) error {
	var err error
	if v.Months, err = s.store.Months(ctx, sc); err != nil {
		return err
	}
	if err := s.loadCurrentFunds(ctx, sc, v); err != nil {
		return err
	}
	if err := s.loadIncome(ctx, sc, month, v); err != nil {
		return err
	}
	if err := s.loadExpenses(ctx, sc, month, budgetMonth, v); err != nil {
		return err
	}
	if err := s.loadEmergencyFund(ctx, sc, v); err != nil {
		return err
	}
	if err := s.loadSavingsGrid(ctx, sc, v); err != nil {
		return err
	}
	v.PendingReceipts, err = s.store.PendingReceiptCount(ctx, sc)
	return err
}

// loadCurrentFunds fills card 1. Cash is a balance, not a flow, so it is
// all-time however the period filter is set: scoping a balance to one month
// would simply be wrong.
func (s *Server) loadCurrentFunds(ctx context.Context, sc store.Scope, v *dashboardView) error {
	var err error
	if v.Cash, err = s.store.Cash(ctx, sc); err != nil {
		return err
	}
	if v.Balance, err = s.store.BalanceSeries(ctx, sc); err != nil {
		return err
	}
	v.Trend = insight.FitTrend(v.Balance)
	return nil
}

// loadIncome fills card 3, plus the actual totals for the selected period.
func (s *Server) loadIncome(ctx context.Context, sc store.Scope, month string, v *dashboardView) error {
	var err error
	if v.Monthly, err = s.store.MonthlySeries(ctx, sc, 12); err != nil {
		return err
	}
	v.IncomeRange = insight.ExpectedIncome(v.Monthly)
	if v.IncomeBySource, err = s.store.Breakdown(ctx, sc, store.KindIncome, month); err != nil {
		return err
	}

	periodTotals, err := s.store.Totals(ctx, sc, month)
	if err != nil {
		return err
	}
	v.IncomeTotal, v.SpendTotal = periodTotals.Income, periodTotals.Expense

	v.Essential, v.NonEssent, err = s.store.EssentialSplit(ctx, sc, month)
	return err
}

// loadExpenses fills card 4. Buckets is the most expensive read on the page and
// is taken exactly once here: everything else that needs it -- the estimate, the
// allocation summary, and the emergency fund's essential cost -- is handed the
// slice rather than querying again.
func (s *Server) loadExpenses(ctx context.Context, sc store.Scope, month, budgetMonth string, v *dashboardView) error {
	var err error
	if v.Buckets, err = s.store.Buckets(ctx, sc, budgetMonth); err != nil {
		return err
	}
	v.ExpenseRange = insight.EstimateMonthlyExpenses(v.Buckets)
	// CategoryBreakdown rather than Breakdown, so a split transaction reports
	// each of its line items under its own category.
	if v.SpendByCategory, err = s.store.CategoryBreakdown(ctx, sc, month); err != nil {
		return err
	}
	v.Allocation, err = s.store.AllocationsFor(ctx, sc, budgetMonth, v.Buckets)
	return err
}

// loadEmergencyFund fills card 2. Runs after loadExpenses, whose buckets decide
// how much a month of essentials costs and therefore how long the fund lasts.
func (s *Server) loadEmergencyFund(ctx context.Context, sc store.Scope, v *dashboardView) error {
	var err error
	if v.EmergencyFund, err = s.store.EmergencyFund(ctx, sc); err != nil {
		return err
	}
	withdrawals, err := s.store.FundWithdrawalHistory(ctx, sc, v.EmergencyFund.ID)
	if err != nil {
		return err
	}
	v.Runway = insight.AssessEmergencyFund(v.EmergencyFund, withdrawals, store.EssentialCost(v.Buckets))
	return nil
}

// loadSavingsGrid fills every fund with its own projection. The template splits
// them: the emergency fund on its own tab, every other fund on Current Funds.
func (s *Server) loadSavingsGrid(ctx context.Context, sc store.Scope, v *dashboardView) error {
	var err error
	if v.Totals, err = s.store.Totals(ctx, sc, ""); err != nil {
		return err
	}
	funds, err := s.store.ListFunds(ctx, sc)
	if err != nil {
		return err
	}
	rates, err := s.store.DepositRates(ctx, sc)
	if err != nil {
		return err
	}
	now := store.Now()
	for _, f := range funds {
		rate := rates[f.ID]
		v.Funds = append(v.Funds, fundCard{
			Fund:       f,
			Projection: insight.ProjectFund(f, insight.AverageMonthlyDeposit(rate.Total, rate.Months), now),
		})
	}
	return nil
}

func buildDashboardCharts(v dashboardView) chartData {
	// No trend series: the dashed trend line was removed from the chart on
	// purpose, and the fitted trend now survives only as the sentence under it
	// (v.Trend.Note), so shipping its points to the page would be dead weight.
	c := chartData{BalanceLabels: []string{}, BalanceValues: []float64{}}
	for _, p := range v.Balance {
		c.BalanceLabels = append(c.BalanceLabels, p.Date)
		c.BalanceValues = append(c.BalanceValues, p.Balance.Float())
	}
	c.MonthLabels, c.MonthIncome, c.MonthExpense = monthAxis(v.Monthly)
	// Blank labels are grouped rather than drawn as an unnamed slice.
	c.IncomeLabels, c.IncomeValues = labelAxis(v.IncomeBySource, "Unlabelled")
	c.SpendLabels, c.SpendValues = labelAxis(v.SpendByCategory, "Unlabelled")
	return c
}

// monthAxis and labelAxis turn a series into the parallel arrays Chart.js wants.
// The slices are never nil: encoding/json renders nil as null, which makes
// Chart.js throw, whereas an empty array draws nothing.
func monthAxis(months []store.MonthPoint) (labels []string, income, expense []float64) {
	labels, income, expense = []string{}, []float64{}, []float64{}
	for _, m := range months {
		label := m.Month
		if t, err := time.Parse(store.MonthLayout, m.Month); err == nil {
			label = t.Format("Jan 06")
		}
		labels = append(labels, label)
		income = append(income, m.Income.Float())
		expense = append(expense, m.Expense.Float())
	}
	return labels, income, expense
}

func labelAxis(totals []store.LabelTotal, blank string) (labels []string, values []float64) {
	labels, values = []string{}, []float64{}
	for _, lt := range totals {
		label := lt.Label
		if label == "" {
			label = blank
		}
		labels = append(labels, label)
		values = append(values, lt.Total.Float())
	}
	return labels, values
}

// fundCard pairs a savings fund with its projection.
type fundCard struct {
	store.Fund
	Projection insight.Projection
}

// handleSavingsRedirect keeps the old /savings path working, sending a bookmark to the
// tab that now lists the savings funds rather than 404ing it.
func (s *Server) handleSavingsRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/dashboard?tab=current", http.StatusMovedPermanently)
}
