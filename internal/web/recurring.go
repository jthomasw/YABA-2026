package web

import (
	"context"
	"net/http"

	"github.com/jthomasw/YABA-2026/internal/insight"
	"github.com/jthomasw/YABA-2026/internal/store"
)

// The recurring sections.
//
// Recurring expenses and recurring income are each shown in exactly two places,
// and both places draw the same section from the same template
// (partials/recurring.html):
//
//	expenses: the dashboard's Expenses tab, and Add Expense -> Manual
//	income:   the dashboard's Income tab,   and Add Income
//
// Each section is built by one function below, so the two pages cannot drift
// apart: what the dashboard shows is what the entry page shows.

// Where a form inside a section was submitted from. The forms post a
// return_to field so the action lands the user back on the page they were
// using; only these fixed values are understood, so the field cannot be used
// to redirect anywhere else.
const (
	returnDashboard = "dashboard"
	returnEntry     = "entry"
)

// recurringExpensesSection is everything the shared recurring-expenses section
// draws: the monthly bill planner (bills funded in priority order) and the
// expenses YABA records automatically on a schedule.
type recurringExpensesSection struct {
	// Copied from the page, because a template called with this struct as its
	// data cannot reach the page's own fields.
	CSRFToken string
	Role      store.Role

	// ReturnTo is returnDashboard or returnEntry; the forms post it back.
	ReturnTo string

	// Open renders the section expanded, used right after an action in it so
	// the list the user was working in does not snap shut.
	Open bool

	// Month is the month the planner's funding is worked out for.
	Month string

	// The planner.
	Buckets     []store.Bucket
	Allocation  store.AllocationSummary
	ExpenseNote string

	// Schedules is every automatic recurring expense, cancelled ones included,
	// so a user can see what used to repeat.
	Schedules []store.RecurringExpense

	// NextDue maps a bill's id to the next date it falls due (YYYY-MM-DD). A
	// bill with no date of its own is missing from it.
	NextDue map[int64]string
}

// Count is the number shown in the section heading.
func (s recurringExpensesSection) Count() int { return len(s.Buckets) + len(s.Schedules) }

// recurringIncomeSection is the shared recurring-income section.
type recurringIncomeSection struct {
	CSRFToken string
	Role      store.Role
	ReturnTo  string
	Open      bool
	Schedules []store.RecurringIncome
}

// loadRecurringExpenses builds the expenses section. buckets and schedules may
// be passed in when the caller has already read them (the dashboard needs both
// for other cards); nil means read them here.
func (s *Server) loadRecurringExpenses(ctx context.Context, sc store.Scope, v view, returnTo, month string,
	buckets []store.Bucket, alloc *store.AllocationSummary, schedules []store.RecurringExpense,
) (recurringExpensesSection, error) {
	sec := recurringExpensesSection{
		CSRFToken: v.CSRFToken,
		Role:      v.Role,
		ReturnTo:  returnTo,
		Month:     month,
		Buckets:   buckets,
		Schedules: schedules,
	}
	var err error
	if sec.Buckets == nil {
		if sec.Buckets, err = s.store.Buckets(ctx, sc, month); err != nil {
			return sec, err
		}
	}
	if alloc != nil {
		sec.Allocation = *alloc
	} else if sec.Allocation, err = s.store.AllocationsFor(ctx, sc, month, sec.Buckets); err != nil {
		return sec, err
	}
	if sec.Schedules == nil {
		if sec.Schedules, err = s.store.ListRecurringExpense(ctx, sc); err != nil {
			return sec, err
		}
	}
	sec.ExpenseNote = insight.EstimateMonthlyExpenses(sec.Buckets).Note
	sec.NextDue = billDueDates(sec.Buckets, sec.Schedules)
	return sec, nil
}

// loadRecurringIncome builds the income section.
func (s *Server) loadRecurringIncome(ctx context.Context, sc store.Scope, v view, returnTo string,
	schedules []store.RecurringIncome,
) (recurringIncomeSection, error) {
	sec := recurringIncomeSection{
		CSRFToken: v.CSRFToken,
		Role:      v.Role,
		ReturnTo:  returnTo,
		Schedules: schedules,
	}
	if sec.Schedules == nil {
		var err error
		if sec.Schedules, err = s.store.ListRecurringIncome(ctx, sc); err != nil {
			return sec, err
		}
	}
	return sec, nil
}

// Where each section lives on each page. The ?..=open parameter re-opens the
// section after an action; the fragment scrolls to it.
const (
	expensesTab         = "/dashboard?tab=expenses&planner=open#panel-expenses"
	expensesEntryAnchor = "/expense?step=manual&planner=open#recurring-expenses"
	incomeTab           = "/dashboard?tab=income&recurring=open#panel-income"
	incomeEntryAnchor   = "/income?recurring=open#recurring-income"
)

// recurringExpensesReturn is where an action in the recurring-expenses section
// sends the user: back to Add Expense if it came from there, otherwise the
// dashboard.
func recurringExpensesReturn(r *http.Request) string {
	if r.PostFormValue("return_to") == returnEntry {
		return expensesEntryAnchor
	}
	return expensesTab
}

// recurringIncomeReturn is the income section's counterpart.
func recurringIncomeReturn(r *http.Request) string {
	if r.PostFormValue("return_to") == returnEntry {
		return incomeEntryAnchor
	}
	return incomeTab
}

// billDueDates is when each monthly bill is next due: the bill's own due day
// when it has one, otherwise the soonest active automatic expense that pays
// towards it -- a car payment recorded on the 15th by a schedule is due on the
// 15th even if nobody typed that day into the bill. A bill with neither has no
// entry.
func billDueDates(buckets []store.Bucket, schedules []store.RecurringExpense) map[int64]string {
	due := make(map[int64]string, len(buckets))
	own := make(map[int64]bool, len(buckets))
	for _, b := range buckets {
		if d := b.NextDueDate(); d != "" {
			due[b.ID], own[b.ID] = d, true
		}
	}
	for _, sch := range schedules {
		ref := sch.BucketRef()
		if !sch.Active || sch.NextDueDate == "" || ref == 0 || own[ref] {
			continue
		}
		if cur, ok := due[ref]; !ok || sch.NextDueDate < cur {
			due[ref] = sch.NextDueDate
		}
	}
	return due
}
