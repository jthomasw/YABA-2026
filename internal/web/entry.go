package web

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/jthomasw/YABA-2026/internal/money"
	"github.com/jthomasw/YABA-2026/internal/store"
)

// entryView backs the two dedicated entry pages, /income and /expense.
type entryView struct {
	view

	// Kind is what this page adds. The template does not offer a choice.
	Kind store.Kind

	formFields
	Error string

	// Step is "choose" or "manual" on the expense page.
	Step string

	// Suggestions for the label field, and buckets an expense can pay towards, and
	// deliberately nothing else: the charts and recent lists live on the dashboard, so the
	// queries that fed them here are gone rather than run and discarded.
	Categories []string
	Buckets    []store.Bucket

	// RecurringIncomes lists this household's recurring income schedules, for
	// the income page's own management list. Empty (never nil) on the expense
	// page and on income pages with none set up yet.
	RecurringIncomes []store.RecurringIncome

	// RecurringExpenses is the expense page's counterpart: every recurring
	// expense schedule in this budget, with Edit and Cancel. Empty on the
	// income page.
	RecurringExpenses []store.RecurringExpense

	// FormToken is a one-time token, so a double click or a back-then-save does
	// not record the same money twice.
	FormToken string

	// Watching is the id of a receipt just uploaded from this page, or zero. The
	// page stays put and shows that receipt being read rather than throwing the
	// user back to the dashboard to wait for a notification; the list of every
	// pending receipt lives on /receipts instead, so this page offers a choice
	// between two things and nothing else.
	Watching int64

	// ReadingEnabled is whether uploaded receipts are read automatically.
	ReadingEnabled bool
}

// handleIncomePage is GET /income.
func (s *Server) handleIncomePage(w http.ResponseWriter, r *http.Request) {
	// Catching schedules up is a write, and this is a GET, so it happens only
	// for a navigation that came from this site. See sameSiteNavigation.
	if sameSiteNavigation(r) {
		if err := s.store.ProcessDueRecurringIncome(
			r.Context(),
			scopeOf(r),
			store.Today(),
		); err != nil {
			s.serverError(w, r, err)
			return
		}
	}

	v, ok := s.buildEntryView(w, r, store.KindIncome, "Add Income", "income")
	if !ok {
		return
	}

	var err error
	if v.RecurringIncomes, err = s.store.ListRecurringIncome(r.Context(), scopeOf(r)); err != nil {
		s.serverError(w, r, err)
		return
	}

	s.render(w, r, "income.html", v)
}

// handleExpensePage is GET /expense. It opens on the Manual-or-Upload chooser rather
// than a form: the user picks how to add the expense before seeing any fields.
func (s *Server) handleExpensePage(w http.ResponseWriter, r *http.Request) {
	// Schedules are caught up on the way in, the same way /income does it: there
	// is no background job in this deployment, so "what do I owe by now" is
	// answered the next time somebody opens the page that would show it.
	if sameSiteNavigation(r) {
		if err := s.store.ProcessDueRecurringExpenses(
			r.Context(),
			scopeOf(r),
			store.Today(),
		); err != nil {
			s.serverError(w, r, err)
			return
		}
	}

	v, ok := s.buildEntryView(w, r, store.KindExpense, "Add Expense", "expense")
	if !ok {
		return
	}

	v.Step = "choose"
	if r.URL.Query().Get("step") == "manual" {
		v.Step = "manual"
	}

	// ?receipt=N is set by the upload, which returns here rather than to the
	// dashboard. The id is checked against this household before the page will
	// watch it, so a guessed number shows nothing rather than telling a stranger
	// that somebody else's receipt exists.
	if raw := r.URL.Query().Get("receipt"); raw != "" {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
			// ReceiptInFlight, not UnattachedReceipt: the page has to watch a
			// receipt that is still queued or being read, and UnattachedReceipt
			// only admits settled ones.
			switch _, err := s.store.ReceiptInFlight(r.Context(), scopeOf(r), id); {
			case err == nil:
				v.Watching = id
			case errors.Is(err, store.ErrNotFound):
				// Somebody else's receipt, or one already turned into an
				// expense. Showing the plain chooser instead is correct, not
				// a bug, so this is not logged.
			default:
				// A real failure here used to be indistinguishable from the
				// two cases above: the page silently fell back to the plain
				// chooser either way, which looked exactly like "the upload
				// did nothing" from the chair the user is sitting in. Now it
				// is at least in the log.
				log.Printf("ERROR %s %s %s: checking receipt %d: %v",
					requestID(r.Context()), r.Method, r.URL.Path, id, err)
			}
		}
	}

	s.render(w, r, "expense.html", v)
}

// buildEntryView loads everything both pages need.
func (s *Server) buildEntryView(w http.ResponseWriter, r *http.Request, kind store.Kind, title, nav string) (entryView, bool) {
	sc := scopeOf(r)
	ctx := r.Context()

	v := entryView{
		view:       s.baseView(w, r, title, nav),
		Kind:       kind,
		formFields: formFields{OccurredOn: store.Today(), Essential: true}.withRecurringDefaults(kind),
		FormToken:  s.issueFormToken(r, "entry"),

		ReadingEnabled: s.cfg.ReceiptReading,
	}

	var err error
	if v.Categories, err = s.store.SpendCategories(ctx, sc); err != nil {
		s.serverError(w, r, err)
		return v, false
	}
	if kind == store.KindExpense {
		if v.Buckets, err = s.store.BucketOptions(ctx, sc); err != nil {
			s.serverError(w, r, err)
			return v, false
		}
		if v.RecurringExpenses, err = s.store.ListRecurringExpense(ctx, sc); err != nil {
			s.serverError(w, r, err)
			return v, false
		}
	}

	return v, true
}

// recurringPresets maps a friendly "repeat every" choice -- the value the
// frequency_preset select actually submits -- to the (n, unit) pair the
// schedule is stored as. "custom" is deliberately absent from this map: it,
// along with any blank or unrecognised value (which is also what a client
// with no JavaScript leaves behind, since the raw fields are what it can
// reach), means fall through to the raw frequency_n/frequency_unit fields.
var recurringPresets = map[string]struct {
	n    int
	unit string
}{
	"weekly":     {1, "week"},
	"biweekly":   {2, "week"},
	"monthly":    {1, "month"},
	"quarterly":  {3, "month"},
	"semiannual": {6, "month"},
	"yearly":     {12, "month"},
}

// parseRecurringFrequency reads the interval a recurring income or expense
// form submitted. A recognised frequency_preset wins outright; otherwise the
// raw frequency_n/frequency_unit fields are read and checked against the same
// ceilings the store enforces, exactly as before this preset existed -- an
// unbounded value here used to reach AddDate and overflow into a date that
// never advances, spinning the catch-up loop on the process's only database
// connection.
func parseRecurringFrequency(r *http.Request) (n int, unit string, errMsg string) {
	if p, ok := recurringPresets[r.PostFormValue("frequency_preset")]; ok {
		return p.n, p.unit, ""
	}

	n, err := strconv.Atoi(r.PostFormValue("frequency_n"))
	if err != nil {
		return 0, "", "Enter a valid recurring frequency."
	}
	unit = r.PostFormValue("frequency_unit")
	if ok, msg := store.ValidFrequency(n, unit); !ok {
		return 0, "", msg
	}
	return n, unit, ""
}

// handleIncomeCreate is POST /income.
func (s *Server) handleIncomeCreate(w http.ResponseWriter, r *http.Request) {
	if !s.parseForm(w, r) {
		return
	}

	// If this is not a recurring-income submission, use the normal
	// one-time income flow.
	if r.PostFormValue("income_type") != "recurring" {
		s.createEntry(w, r, store.KindIncome)
		return
	}

	user := mustUser(r)
	sc := scopeOf(r)

	in, msg := parseTransactionFormFor(r, store.KindIncome)
	if msg != "" {
		s.rerenderEntry(w, r, store.KindIncome, in, msg)
		return
	}

	frequencyN, frequencyUnit, freqMsg := parseRecurringFrequency(r)
	if freqMsg != "" {
		s.rerenderEntry(w, r, store.KindIncome, in, freqMsg)
		return
	}

	if s.duplicateSubmit(w, r, user.ID, "/income", "That recurring income was already saved.") {
		return
	}

	_, err := s.store.CreateRecurringIncome(
		r.Context(),
		sc,
		in.label,
		in.amount,
		frequencyN,
		frequencyUnit,
		in.occurredOn,
	)
	if err != nil {
		s.restoreSubmissionToken(r, user.ID)
		s.serverError(w, r, err)
		return
	}

	s.redirectSuccess(
		w,
		r,
		"/income",
		fmt.Sprintf("Recurring income of %s saved.", in.amount.Display()),
	)
}

// recurringIncomeEditForm is what /income/{id}/edit and /income/{id}/cancel
// share with handleIncomeCreate's own recurring path: a source, an amount, and
// a frequency, read the same way in both places so a preset picked here means
// exactly what it meant when the schedule was created.
func parseRecurringIncomeEditForm(r *http.Request) (source string, amount money.Cents, freqN int, freqUnit string, errMsg string) {
	source = strings.TrimSpace(r.PostFormValue("label"))
	amount, err := money.ParsePositive(r.PostFormValue("amount"))
	if err != nil {
		return "", 0, 0, "", "Enter an amount greater than zero."
	}
	freqN, freqUnit, errMsg = parseRecurringFrequency(r)
	return source, amount, freqN, freqUnit, errMsg
}

// handleRecurringIncomeUpdate is POST /income/{id}/edit. It changes what a
// schedule pays and how often, but never its next_due_date: whatever is
// already owed under the old terms is still owed, on the day it was already
// due, and only the cycle after that reflects the change -- the same rule
// ProcessDueRecurringIncome already applies when it advances the date itself.
func (s *Server) handleRecurringIncomeUpdate(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	if !s.parseForm(w, r) {
		return
	}

	existing, err := s.store.RecurringIncomeByID(r.Context(), sc, id)
	if errors.Is(err, store.ErrNotFound) {
		s.flashError(w, r, "That recurring income no longer exists.")
		http.Redirect(w, r, "/income", http.StatusSeeOther)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// A cancelled schedule is history, as it is for recurring expenses: the
	// store no longer updates one, and this says why instead of "not found".
	if !existing.Active {
		s.redirectError(w, r, "/income", "That recurring income has been cancelled, so it can no longer be changed.")
		return
	}

	source, amount, freqN, freqUnit, msg := parseRecurringIncomeEditForm(r)
	if msg != "" {
		s.flashError(w, r, msg)
		http.Redirect(w, r, "/income", http.StatusSeeOther)
		return
	}

	err = s.store.UpdateRecurringIncome(
		r.Context(), sc, id,
		source, amount, freqN, freqUnit,
		existing.StartDate, existing.NextDueDate,
	)
	s.redirectAfterStoreOp(w, r, err, "/income",
		"That recurring income no longer exists.",
		"That change could not be saved. Try again.",
		"Recurring income updated.",
		nil,
	)
}

// handleRecurringIncomeCancel is POST /income/{id}/cancel. It stops future
// occurrences without deleting the schedule or anything it already created --
// the same one-way, history-keeping shape as archiving a recurring expense,
// deliberately: undoing a delete on money that already happened is a much
// worse conversation than turning a schedule back on would ever be, so there
// is no resume path in this UI even though the store layer could support one.
func (s *Server) handleRecurringIncomeCancel(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}

	err := s.store.SetRecurringIncomeActive(r.Context(), sc, id, false)
	s.redirectAfterStoreOp(w, r, err, "/income",
		"That recurring income no longer exists.",
		"That could not be cancelled. Try again.",
		"Recurring income cancelled. Its history has been kept.",
		nil,
	)
}

// handleExpenseCreate is POST /expense.
//
// The manual form can save two different things: a single expense that happened,
// or a schedule that will keep creating them. They share every field except the
// frequency, so they share one form and one route -- expense_type says which was
// meant, and a missing or unrecognised value means the ordinary one-time flow.
// That is the safe default: a broken script cannot accidentally commit somebody
// to a repeating charge.
func (s *Server) handleExpenseCreate(w http.ResponseWriter, r *http.Request) {
	if !s.parseForm(w, r) {
		return
	}

	if r.PostFormValue("expense_type") != "recurring" {
		s.createEntry(w, r, store.KindExpense)
		return
	}

	user := mustUser(r)
	sc := scopeOf(r)

	in, msg := parseTransactionFormFor(r, store.KindExpense)
	if msg != "" {
		s.rerenderEntry(w, r, store.KindExpense, in, msg)
		return
	}

	frequencyN, frequencyUnit, freqMsg := parseRecurringFrequency(r)
	if freqMsg != "" {
		s.rerenderEntry(w, r, store.KindExpense, in, freqMsg)
		return
	}

	if s.duplicateSubmit(w, r, user.ID, "/expense", "That recurring expense was already saved.") {
		return
	}

	_, err := s.store.CreateRecurringExpense(
		r.Context(),
		sc,
		in.label,
		in.amount,
		in.bucketID,
		in.essential,
		frequencyN,
		frequencyUnit,
		in.occurredOn,
	)
	if err != nil {
		s.restoreSubmissionToken(r, user.ID)
		s.serverError(w, r, err)
		return
	}

	s.redirectSuccess(
		w,
		r,
		"/expense",
		fmt.Sprintf("Recurring expense of %s saved.", in.amount.Display()),
	)
}

// recurringExpensesAnchor is where the recurring-expense actions return the
// user: the list they were editing, on the Add Expense page.
const recurringExpensesAnchor = "/expense#recurring-expenses"

// handleRecurringExpenseUpdate is POST /expense/recurring/{id}/edit. Like its
// income counterpart it changes what the schedule charges and how often, but
// never its next_due_date: whatever is already owed stays owed on the day it
// fell due.
func (s *Server) handleRecurringExpenseUpdate(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	if !s.parseForm(w, r) {
		return
	}

	// Checked first, so a missing schedule and a missing bucket -- both
	// ErrNotFound from the update -- can be told apart in the message.
	if _, err := s.store.RecurringExpenseByID(r.Context(), sc, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.redirectError(w, r, recurringExpensesAnchor, "That recurring expense no longer exists.")
			return
		}
		s.serverError(w, r, err)
		return
	}

	label := strings.TrimSpace(r.PostFormValue("label"))
	if label == "" {
		s.redirectError(w, r, recurringExpensesAnchor, "Give the recurring expense a name.")
		return
	}
	amount, err := money.ParsePositive(r.PostFormValue("amount"))
	if err != nil {
		s.redirectError(w, r, recurringExpensesAnchor, "Enter an amount greater than zero.")
		return
	}
	freqN, freqUnit, msg := parseRecurringFrequency(r)
	if msg != "" {
		s.redirectError(w, r, recurringExpensesAnchor, msg)
		return
	}
	var bucketID *int64
	if raw := strings.TrimSpace(r.PostFormValue("bucket_id")); raw != "" && raw != "0" {
		b, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || b <= 0 {
			s.redirectError(w, r, recurringExpensesAnchor, "That recurring bill could not be recognised.")
			return
		}
		bucketID = &b
	}
	essential := r.PostFormValue("essential") != "no"

	err = s.store.UpdateRecurringExpense(r.Context(), sc, id,
		label, amount, bucketID, essential, freqN, freqUnit)
	s.redirectAfterStoreOp(w, r, err, recurringExpensesAnchor,
		"That recurring expense, or the bill it pays towards, no longer exists.",
		"That change could not be saved. Try again.",
		"Recurring expense updated.",
		nil,
	)
}

// handleRecurringExpenseCancel is POST /expense/recurring/{id}/cancel. It stops
// future charges and keeps every expense the schedule already recorded, the
// same one-way shape as cancelling recurring income.
func (s *Server) handleRecurringExpenseCancel(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}

	err := s.store.SetRecurringExpenseActive(r.Context(), sc, id, false)
	s.redirectAfterStoreOp(w, r, err, recurringExpensesAnchor,
		"That recurring expense no longer exists.",
		"That could not be cancelled. Try again.",
		"Recurring expense cancelled. The expenses it already recorded have been kept.",
		nil,
	)
}

// createEntry saves one entry of a fixed kind. The kind comes from the route, never from
// the form, so a hand-edited request cannot post an expense to the income page.
func (s *Server) createEntry(w http.ResponseWriter, r *http.Request, kind store.Kind) {
	user := mustUser(r)
	sc := scopeOf(r)

	if !s.parseForm(w, r) {
		return
	}

	in, msg := parseTransactionFormFor(r, kind)
	if msg != "" {
		s.rerenderEntry(w, r, kind, in, msg)
		return
	}
	if !formBelongsToHousehold(r, sc.HouseholdID) {
		s.rerenderEntry(w, r, kind, in, "This form belongs to a different budget. Reload it before saving.")
		return
	}

	// Validated, so this is a real submission -- spend the token.
	if s.duplicateSubmit(w, r, user.ID, "/dashboard", "That entry was already saved.") {
		return
	}

	n := in.toNewTransaction()

	// An expense may carry a receipt in the same submission.
	if kind == store.KindExpense {
		path, name, err := s.saveReceipt(r, user.ID)
		if err != nil {
			s.restoreSubmissionToken(r, user.ID)
			s.rerenderEntry(w, r, kind, in,
				s.safeMessage(r, err, "That receipt could not be stored. Try again."))
			return
		}
		n.ReceiptPath, n.ReceiptName = path, name
	}

	txID, err := s.store.Add(r.Context(), sc, n)
	if errors.Is(err, store.ErrNotFound) {
		s.restoreSubmissionToken(r, user.ID)
		s.rerenderEntry(w, r, kind, in, "That recurring expense no longer exists.")
		return
	}
	if err != nil {
		s.removeReceiptFiles([]string{n.ReceiptPath})
		s.restoreSubmissionToken(r, user.ID)
		s.serverError(w, r, err)
		return
	}

	ws := s.finishSave(r, sc, txID, in, in.occurredOn)

	// Back to the same page, so another entry can be added without navigating.
	back := "/expense?step=manual"
	if kind == store.KindIncome {
		back = "/income"
	}
	s.redirectSaved(w, r, back, ws, kind.Label()+" of "+in.amount.Display()+" saved.")
}

// rerenderEntry redisplays the page with an error and the submitted values.
func (s *Server) rerenderEntry(w http.ResponseWriter, r *http.Request, kind store.Kind, in transactionInput, msg string) {
	title, nav, page := "Add Income", "income", "income.html"
	if kind == store.KindExpense {
		title, nav, page = "Add Expense", "expense", "expense.html"
	}

	v, ok := s.buildEntryView(w, r, kind, title, nav)
	if !ok {
		return
	}

	v.Error = msg
	v.formFields = echoForm(r, in)
	if kind == store.KindExpense {
		v.Step = "manual"
	}

	s.renderStatus(w, r, http.StatusBadRequest, page, v)
}
