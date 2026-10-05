package web

import (
	"net/http"
	"time"

	"github.com/jthomasw/YABA-2026/internal/insight"
	"github.com/jthomasw/YABA-2026/internal/money"
	"github.com/jthomasw/YABA-2026/internal/store"
)

// ── /reports ──────────────────────────────────────────────────────────────────

type reportsView struct {
	view

	Month      string
	MonthLabel string
	Months     []string

	Totals    store.Totals
	AllTime   store.Totals
	Essential money.Cents
	NonEssent money.Cents

	IncomeBySource  []store.LabelTotal
	SpendByCategory []store.LabelTotal
	Monthly         []store.MonthPoint

	Budgets      []store.Budget
	Categories   []string
	Observations []insight.Observation

	Charts reportCharts
}

type reportCharts struct {
	IncomeLabels    []string
	IncomeValues    []float64
	SpendLabels     []string
	SpendValues     []float64
	MonthLabels     []string
	MonthIncome     []float64
	MonthExpense    []float64
	EssentialLabels []string
	EssentialValues []float64
}

// handleReports holds everything that used to be stacked under the dashboard
// tabs: the plain-language observations, the category and source breakdowns, the
// month-by-month table and the per-category budget caps.
func (s *Server) handleReports(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	ctx := r.Context()

	month := parseMonth(r.URL.Query().Get("month"))

	v := reportsView{
		view:       s.baseView(w, r, "Reports", "reports"),
		Month:      month,
		MonthLabel: "All time",
	}
	if month != "" {
		if t, err := time.Parse(store.MonthLayout, month); err == nil {
			v.MonthLabel = t.Format("January 2006")
		}
	}

	var err error
	if v.Months, err = s.store.Months(ctx, sc); err != nil {
		s.serverError(w, r, err)
		return
	}
	if v.Totals, err = s.store.Totals(ctx, sc, month); err != nil {
		s.serverError(w, r, err)
		return
	}
	if v.AllTime, err = s.store.Totals(ctx, sc, ""); err != nil {
		s.serverError(w, r, err)
		return
	}
	if v.Essential, v.NonEssent, err = s.store.EssentialSplit(ctx, sc, month); err != nil {
		s.serverError(w, r, err)
		return
	}
	if v.IncomeBySource, err = s.store.Breakdown(ctx, sc, store.KindIncome, month); err != nil {
		s.serverError(w, r, err)
		return
	}
	// CategoryBreakdown rather than Breakdown, so a split transaction reports
	// each line item under its own category.
	if v.SpendByCategory, err = s.store.CategoryBreakdown(ctx, sc, month); err != nil {
		s.serverError(w, r, err)
		return
	}
	if v.Monthly, err = s.store.MonthlySeries(ctx, sc, 12); err != nil {
		s.serverError(w, r, err)
		return
	}
	if v.Budgets, err = s.store.ListBudgets(ctx, sc, month); err != nil {
		s.serverError(w, r, err)
		return
	}
	if v.Categories, err = s.store.SpendCategories(ctx, sc); err != nil {
		s.serverError(w, r, err)
		return
	}

	buckets, err := s.store.Buckets(ctx, sc, store.Today()[:7])
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	alloc, err := s.store.AllocationsFor(ctx, sc, store.Today()[:7], buckets)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	ef, err := s.store.EmergencyFund(ctx, sc)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	wd, err := s.store.FundWithdrawalHistory(ctx, sc, ef.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	runway := insight.AssessEmergencyFund(ef, wd, store.EssentialCost(buckets))

	v.Observations = insight.ObservationsWithHoldingsAsOf(v.Totals, v.AllTime,
		v.Essential, v.NonEssent, v.Budgets, v.Monthly, store.Today()[:7])
	v.Observations = append(v.Observations, bucketObservations(alloc, buckets, runway)...)

	v.Charts = buildReportCharts(v)
	s.render(w, r, "reports.html", v)
}

func buildReportCharts(v reportsView) reportCharts {
	c := reportCharts{EssentialLabels: []string{}, EssentialValues: []float64{}}
	c.IncomeLabels, c.IncomeValues = labelAxis(v.IncomeBySource, "")
	c.SpendLabels, c.SpendValues = labelAxis(v.SpendByCategory, "")
	c.MonthLabels, c.MonthIncome, c.MonthExpense = monthAxis(v.Monthly)
	if v.Essential > 0 || v.NonEssent > 0 {
		c.EssentialLabels = append(c.EssentialLabels, "Essential", "Non-essential")
		c.EssentialValues = append(c.EssentialValues, v.Essential.Float(), v.NonEssent.Float())
	}
	return c
}

// bucketObservations turns the funding position into the same kind of plain
// sentence the rest of the insight panel uses.
func bucketObservations(a store.AllocationSummary, buckets []store.Bucket, r insight.Runway) []insight.Observation {
	var out []insight.Observation

	if r.Warning != "" {
		out = append(out, insight.Observation{Severity: insight.Alert, Text: r.Warning})
	}

	if a.Required > 0 && a.Shortfall > 0 {
		// Name the highest-priority unfunded bucket: that is the one to act on,
		// and it is the whole point of ranking them.
		first := ""
		for _, b := range buckets {
			if b.Shortfall() > 0 {
				first = b.Name
				break
			}
		}
		text := "Recurring expenses are short by " + a.Shortfall.Display() + " this month."
		if first != "" {
			text = first + " is the highest priority expense still unfunded. " + text
		}
		out = append(out, insight.Observation{Severity: insight.Alert, Text: text})
	}

	if a.Required > 0 && a.Shortfall == 0 {
		out = append(out, insight.Observation{Severity: insight.Good,
			Text: "Every recurring expense is funded for this month."})
	}

	if a.Unassigned > 0 && a.Shortfall == 0 {
		out = append(out, insight.Observation{Severity: insight.Info,
			Text: a.Unassigned.Display() + " of income is not committed to a recurring expense."})
	}

	return out
}

// ── notifications ─────────────────────────────────────────────────────────────

// handleNotifications is polled by the page to deliver toasts.
func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)

	notes, err := s.store.TakeNotifications(r.Context(), user.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	pending, err := s.store.PendingReceiptCount(r.Context(), scopeOf(r))
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	type payload struct {
		Kind string `json:"kind"`
		Text string `json:"text"`
		Link string `json:"link"`
	}
	out := struct {
		Notifications []payload `json:"notifications"`
		Pending       int       `json:"pending"`
	}{Notifications: []payload{}, Pending: pending}

	for _, n := range notes {
		out.Notifications = append(out.Notifications, payload{Kind: n.Kind, Text: n.Text, Link: n.Link})
	}

	s.writeJSON(w, r, out)
}
