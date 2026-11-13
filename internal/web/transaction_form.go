package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jthomasw/YABA-2026/internal/money"
	"github.com/jthomasw/YABA-2026/internal/store"
)

// ── create and edit ───────────────────────────────────────────────────────────

// formFields is the state of an income or expense form: the wireframe's five Ws
// (What, When, Who, Where, Why) plus amount, essential flag and recurring
// bucket. Shared by the dedicated entry pages and the add/edit form, and echoed
// back after a validation failure so nothing typed is lost.
type formFields struct {
	Label      string // What?
	Amount     string
	OccurredOn string // When?
	Essential  bool
	Payee      string // Who?
	Place      string // Where?
	Note       string // Why?
	BucketID   int64

	// The repeat choice on /income and /expense. Echoed like everything else:
	// when these were hard-coded in the templates, a recurring submission
	// refused for a typo came back as One-time, and the corrected resubmit
	// silently saved a single entry instead of the schedule the user chose.
	// FrequencyN is text so whatever was typed is shown back as typed.
	Recurring       bool
	FrequencyPreset string
	FrequencyN      string
	FrequencyUnit   string
}

// recurringTypeField is the radio that says whether an entry page's form is a
// one-off or a schedule; the two pages name it after themselves.
func recurringTypeField(kind store.Kind) string {
	if kind == store.KindIncome {
		return "income_type"
	}
	return "expense_type"
}

// withRecurringDefaults fills in the schedule an entry page offers before the
// user has chosen one: paydays are commonly fortnightly, bills monthly.
func (f formFields) withRecurringDefaults(kind store.Kind) formFields {
	if kind == store.KindIncome {
		f.FrequencyPreset, f.FrequencyN, f.FrequencyUnit = "biweekly", "2", "week"
	} else {
		f.FrequencyPreset, f.FrequencyN, f.FrequencyUnit = "monthly", "1", "month"
	}
	return f
}

// echoForm reads the submitted values back for re-rendering.
func echoForm(r *http.Request, in transactionInput) formFields {
	f := formFields{
		Label:      r.PostFormValue("label"),
		Amount:     r.PostFormValue("amount"),
		OccurredOn: r.PostFormValue("date"),
		Essential:  r.PostFormValue("essential") != "no",
		Payee:      r.PostFormValue("payee"),
		Place:      r.PostFormValue("place"),
		Note:       r.PostFormValue("note"),
	}
	if in.bucketID != nil {
		f.BucketID = *in.bucketID
	}
	if f.OccurredOn == "" {
		f.OccurredOn = store.Today()
	}

	// The schedule, starting from the page's defaults so a field that was not
	// submitted (or not recognised) still renders a sensible choice. Only
	// values the form itself offers are taken for the two selects: anything
	// else would match no option and the browser would quietly pick the first.
	f = f.withRecurringDefaults(in.kind)
	f.Recurring = r.PostFormValue(recurringTypeField(in.kind)) == "recurring"
	if p := r.PostFormValue("frequency_preset"); p == "custom" {
		f.FrequencyPreset = p
	} else if _, ok := recurringPresets[p]; ok {
		f.FrequencyPreset = p
	}
	if n := strings.TrimSpace(r.PostFormValue("frequency_n")); n != "" {
		f.FrequencyN = n
	}
	switch u := r.PostFormValue("frequency_unit"); u {
	case "day", "week", "month":
		f.FrequencyUnit = u
	}
	return f
}

// formItem is one line-item row as the form shows it. The fields are strings
// rather than parsed values so a refused submission can be shown back exactly
// as typed -- "12.345" stays "12.345" for the user to correct, instead of the
// row vanishing because it could not be parsed.
type formItem struct {
	Description string
	Category    string
	Amount      string
	Position    int

	// Added marks a line the app put in so the breakdown adds up (tax, tip,
	// "Other"), so the form can show it is not a product from the receipt.
	Added bool
}

// formItemsFrom converts stored line items for the edit form.
func formItemsFrom(items []store.LineItem) []formItem {
	out := make([]formItem, 0, len(items))
	for i, it := range items {
		out = append(out, formItem{
			Description: it.Description, Category: it.Category,
			Amount: it.Amount.Input(), Position: i,
		})
	}
	return out
}

// draftFormItems turns the lines OCR read off a receipt into form rows.
//
// A DraftItem carries no category of its own, and an item saved without one is
// charted as Uncategorised -- so confirming a Groceries receipt used to file
// every one of its lines outside Groceries. Each row starts in the receipt's
// category instead (or, failing that, the label the form opens with), which is
// the bucket the user is confirming anyway; any row can still be changed.
func draftFormItems(d *store.ReceiptDraft, label string) []formItem {
	category := strings.TrimSpace(d.Category)
	if category == "" {
		category = strings.TrimSpace(label)
	}
	out := make([]formItem, 0, len(d.Items))
	for i, it := range d.Items {
		// A price that could not be read stays blank rather than "0.00": blank
		// is skipped on save, while "0.00" was refused and blocked the form.
		amount := ""
		if it.Amount != 0 {
			amount = it.Amount.Input()
		}
		out = append(out, formItem{
			Description: it.Description, Category: category,
			Amount: amount, Position: i, Added: it.Added,
		})
	}
	return out
}

// transactionFormView backs the shared add/edit form.
type transactionFormView struct {
	view

	// Editing is false for a new entry.
	Editing bool
	ID      int64
	Action  string

	Kind store.Kind
	formFields
	Buckets []store.Bucket

	// Items backs the optional line-item breakdown, as text: after a failed
	// submission these are the rows exactly as typed, invalid ones included.
	Items []formItem

	// Echoed is set when the page is a re-render of a refused submission, so
	// the template knows Items are the user's rows rather than a fresh reading
	// of the receipt.
	Echoed bool

	Categories []string
	Error      string

	// FormToken is a one-time token for the create path; empty when editing,
	// because an edit is already protected by the version check.
	FormToken string

	// ReceiptJobID is a receipt already uploaded and waiting to be described, carried in a
	// hidden field so it survives a validation error: otherwise correcting a typo would
	// silently detach the receipt the user came here to attach.
	ReceiptJobID   int64
	ReceiptName    string
	ReceiptMissing bool

	// Draft is what OCR read off that receipt, or nil. The template uses it to
	// say where the prefilled figures came from and how sure the reading was, so
	// the user knows which fields to check rather than trusting the form.
	Draft *store.ReceiptDraft

	// Version is the row version this form was built from, submitted back as a
	// hidden field so a save can be refused if the row moved on meanwhile.
	Version int64

	// Back is where Cancel and, after a successful save, the redirect should
	// return to -- the filtered, paginated transactions list the user actually
	// came from, echoed through as a hidden field. Always the output of
	// safeBack, on the way in and on the way out, because it is printed into
	// the Cancel href as well as used for the redirect.
	Back string
}

func (s *Server) handleTransactionForm(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)

	kind := store.Kind(r.URL.Query().Get("type"))
	if kind != store.KindIncome && kind != store.KindExpense {
		kind = store.KindExpense
	}

	v := transactionFormView{
		view:       s.baseView(w, r, "Add entry", "transactions"),
		Kind:       kind,
		formFields: formFields{OccurredOn: store.Today(), Essential: true},
		Action:     "/transactions/new",
		Back:       safeBack(r.URL.Query().Get("back")),
	}
	// Only the create path needs one. An edit cannot duplicate anything: it
	// carries a version, and the second save of the same version is refused.
	if r.PathValue("id") == "" {
		v.FormToken = s.issueFormToken(r, "transaction")
	}

	// A receipt uploaded earlier that could not be read automatically, whose notification
	// sent the user here to finish it by hand.
	if raw := r.URL.Query().Get("receipt"); raw != "" && kind == store.KindExpense {
		jobID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		job, err := s.store.UnattachedReceipt(r.Context(), sc, jobID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			// Either it belongs to another budget, or somebody already turned it into an
			// expense.
			v.ReceiptMissing = true
		case err != nil:
			s.serverError(w, r, err)
			return
		default:
			v.ReceiptJobID = job.ID
			v.ReceiptName = job.OriginalName
			// Seed the description from the filename. Wrong often enough that it
			// stays editable, useful often enough to save typing.
			if v.Label == "" {
				v.Label = receiptLabel(job.OriginalName)
			}
			// Everything OCR read. Each field is only filled in when it was
			// actually recovered, so a partial reading prefills what it found and
			// leaves the rest blank rather than inventing a plausible default.
			if job.Draft != nil {
				// Balanced again here as well as when it was saved, so a draft
				// stored before balancing existed still opens ready to save.
				balanced := job.Draft.Balanced()
				d := &balanced
				d.CheckDate()
				v.Draft = d
				if d.Total > 0 {
					v.Amount = d.Total.Input()
				}
				if d.Date != "" {
					v.OccurredOn = d.Date
				} else if d.BadDate != "" {
					// Not today: an empty box the user has to fill in, with the
					// callout saying why.
					v.OccurredOn = ""
				}
				if d.Merchant != "" {
					v.Payee = d.Merchant
				}
				if d.Category != "" {
					// The guessed category beats the filename guess above: it
					// came from the receipt's contents rather than from whatever
					// the camera happened to name the file.
					v.Label = d.Category
				}
				v.Items = draftFormItems(d, v.Label)
			}
		}
	}

	var err error

	// The edit route shares this handler, populated from the existing row.
	if idStr := r.PathValue("id"); idStr != "" {
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		t, err := s.store.ByID(r.Context(), sc, id)
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if t.Kind.IsTransfer() {
			// Transfers are not editable here: changing one would desynchronise
			// the fund balance it contributes to.
			s.redirectError(w, r, safeBack(r.URL.Query().Get("back")),
				"Savings transfers can't be edited. Withdraw from the fund instead.")
			return
		}

		v.Editing = true
		v.ID = t.ID
		v.Kind = t.Kind
		v.Label = t.Label
		v.Amount = t.Amount.Input()
		v.OccurredOn = t.OccurredOn
		v.Essential = t.Essential == nil || *t.Essential
		v.Payee, v.Place, v.Note = t.Payee, t.Place, t.Note
		if t.BucketID != nil {
			v.BucketID = *t.BucketID
		}
		v.Action = fmt.Sprintf("/transactions/%d/edit", t.ID)
		v.Title = "Edit entry"
		v.Version = t.Version

		stored, err := s.store.LineItems(r.Context(), sc, t.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		v.Items = formItemsFrom(stored)
	}

	if v.Categories, err = s.store.SpendCategories(r.Context(), sc); err != nil {
		s.serverError(w, r, err)
		return
	}
	if v.Buckets, err = s.store.BucketOptionsKeeping(r.Context(), sc, v.BucketID); err != nil {
		s.serverError(w, r, err)
		return
	}

	s.render(w, r, "transaction_form.html", v)
}

// transactionInput is the parsed, validated result of the add/edit form.
type transactionInput struct {
	kind       store.Kind
	label      string
	amount     money.Cents
	occurredOn string
	essential  bool
	payee      string
	place      string
	note       string
	bucketID   *int64
	items      []store.NewLineItem

	// itemRows is every line-item row that was submitted, as typed, whether
	// or not the submission validated. It is what a re-render shows, so a
	// refusal anywhere in the form never costs the user their breakdown.
	itemRows []formItem

	// version is the row version the form was rendered from.
	version int64
}

// parseTransactionForm validates every field and returns a message fit to show the user,
// rather than redirecting to an empty form with the input discarded.
func parseTransactionForm(r *http.Request) (transactionInput, string) {
	kind := store.Kind(r.PostFormValue("kind"))
	if kind != store.KindIncome && kind != store.KindExpense {
		return transactionInput{}, "Choose whether this is income or an expense."
	}
	return parseTransactionFormFor(r, kind)
}

// parseTransactionFormFor validates with the kind imposed by the caller rather than read
// from the request, which is what makes /income and /expense genuinely separate: a
// hand-edited POST cannot store an expense as income.
func parseTransactionFormFor(r *http.Request, kind store.Kind) (transactionInput, string) {
	var in transactionInput
	in.kind = kind

	// Read before anything can fail. Every return below hands back in, and the
	// re-render shows these rows; parsing them only at the end meant a typo in
	// the date, or the items not adding up -- the commonest refusal of all --
	// sent a twelve-line receipt back with no lines at all.
	//
	// Income has no breakdown. The form hides the rows and disables them for
	// income, but anything that arrives anyway (no script, or a stale page) is
	// ignored rather than validated against a total the user cannot see.
	// A recurring expense has no breakdown either (a schedule stores none), so
	// rows left over from switching One-time to Recurring are ignored too.
	if kind != store.KindIncome && r.PostFormValue("expense_type") != "recurring" {
		in.itemRows = readItemRows(r)
	}

	in.label = strings.TrimSpace(r.PostFormValue("label"))
	if in.label == "" {
		if in.kind == store.KindIncome {
			return in, "Give the income a source, for example \"Salary\"."
		}
		return in, "Give the expense a category, for example \"Food\"."
	}

	amount, err := money.ParsePositive(r.PostFormValue("amount"))
	if err != nil {
		return in, "Enter an amount greater than zero, using at most two decimal places."
	}
	in.amount = amount

	date, err := store.ParseDate(strings.TrimSpace(r.PostFormValue("date")))
	if err != nil {
		return in, "Enter a date in YYYY-MM-DD form."
	}
	in.occurredOn = date

	in.essential = r.PostFormValue("essential") != "no"

	// The three optional Ws. Trimmed but not required: an expense with only a
	// category and an amount is still a valid expense, and refusing to save one
	// would make the form tedious to use for a coffee.
	in.payee = strings.TrimSpace(r.PostFormValue("payee"))
	in.place = strings.TrimSpace(r.PostFormValue("place"))
	in.note = strings.TrimSpace(r.PostFormValue("note"))

	if raw := strings.TrimSpace(r.PostFormValue("bucket_id")); raw != "" && raw != "0" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return in, "That recurring expense could not be recognised."
		}
		// Ownership is verified in the store, not here: a handler check would be
		// one more place to forget it.
		in.bucketID = &id
	}

	// The version the editor was shown, used as a compare-and-swap on save.
	if raw := strings.TrimSpace(r.PostFormValue("version")); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil && v > 0 {
			in.version = v
		}
	}

	items, msg := parseLineItems(in.itemRows, in.amount)
	if msg != "" {
		return in, msg
	}
	in.items = items

	return in, ""
}

// readItemRows collects the breakdown rows, which arrive as parallel arrays
// from repeated form fields, without judging them. A row with nothing typed in
// any of its three boxes is dropped -- the always-present blank row would
// otherwise come back as a second blank row on every re-render -- but a row
// with anything in it is kept exactly as typed.
func readItemRows(r *http.Request) []formItem {
	descs := r.PostForm["item_description"]
	cats := r.PostForm["item_category"]
	amounts := r.PostForm["item_amount"]

	at := func(vs []string, i int) string {
		if i < len(vs) {
			return vs[i]
		}
		return ""
	}

	n := max(len(descs), len(cats), len(amounts))
	var rows []formItem
	for i := 0; i < n; i++ {
		row := formItem{
			Description: at(descs, i), Category: at(cats, i), Amount: at(amounts, i),
		}
		if strings.TrimSpace(row.Description+row.Category+row.Amount) == "" {
			continue
		}
		row.Position = len(rows)
		rows = append(rows, row)
	}
	return rows
}

// parseLineItems validates the breakdown rows read by readItemRows.
func parseLineItems(rows []formItem, total money.Cents) ([]store.NewLineItem, string) {
	var items []store.NewLineItem
	var sum money.Cents

	for _, row := range rows {
		raw := strings.TrimSpace(row.Amount)
		if raw == "" {
			// A description with no amount has nothing to reconcile; it is
			// shown back on a re-render but not stored.
			continue
		}
		// Negative is a discount or coupon line; zero means nothing.
		amount, err := money.Parse(raw)
		if err != nil || amount == 0 {
			return nil, "Every line item needs an amount. Use a minus sign for a discount, for example -2.00."
		}
		items = append(items, store.NewLineItem{
			Description: strings.TrimSpace(row.Description),
			Category:    strings.TrimSpace(row.Category),
			Amount:      amount,
		})
		sum += amount
	}

	if len(items) == 0 {
		return nil, ""
	}
	// Requiring the lines to reconcile is what keeps the category breakdown and the
	// headline total telling the same story.
	if sum != total {
		return nil, fmt.Sprintf("The line items add up to %s but the total is %s.",
			sum.Display(), total.Display())
	}
	return items, ""
}

func (s *Server) rerenderTransactionForm(w http.ResponseWriter, r *http.Request, in transactionInput, msg string, editing bool, id int64) {
	// Echo back whatever version the submission carried, so an ordinary
	// validation error does not turn into a spurious conflict on the retry.
	s.rerenderTransactionFormAt(w, r, in, msg, editing, id, in.version)
}

// rerenderTransactionFormAt is rerenderTransactionForm with the row version
// stated explicitly, for the conflict case where the form must be re-armed with
// the version somebody else just created.
func (s *Server) rerenderTransactionFormAt(w http.ResponseWriter, r *http.Request,
	in transactionInput, msg string, editing bool, id, version int64) {
	sc := scopeOf(r)
	kind := in.kind
	if kind != store.KindIncome && kind != store.KindExpense {
		kind = store.KindExpense
	}

	v := transactionFormView{
		view:       s.baseView(w, r, "Add entry", "transactions"),
		Editing:    editing,
		ID:         id,
		Kind:       kind,
		formFields: echoForm(r, in),
		Error:      msg,
		Action:     "/transactions/new",
		Version:    version,
		Back:       safeBack(r.PostFormValue("back")),
	}
	// Echo the submitted line items back so a validation failure does not throw
	// away rows the user typed. The raw rows, not the parsed ones: those are
	// empty whenever the refusal came before or from the items themselves.
	v.Items = in.itemRows
	v.Echoed = true
	// A fresh token, not the one that was posted. Form validation does fail
	// before the token is spent, but three later refusals do not: an unusable
	// receipt, a receipt id already attached to something else, and a bucket
	// deleted since the page was opened. Echoing the spent token back meant the
	// user's corrected resubmit was answered with "That entry was already
	// saved." while recording nothing -- losing the entry and denying it in the
	// same breath. A new token per rendered page is the invariant that stops a
	// double submit anyway; reusing one was never what made it work.
	if !editing {
		v.FormToken = s.issueFormToken(r, "transaction")
	}

	// Keep the pending receipt across a validation failure, along with what OCR
	// read from it: losing the image and the reading because a date was mistyped
	// would send the user back to find the receipt again.
	if raw := r.PostFormValue("receipt_job"); raw != "" {
		if jobID, err := strconv.ParseInt(raw, 10, 64); err == nil {
			if job, err := s.store.UnattachedReceipt(r.Context(), sc, jobID); err == nil {
				v.ReceiptJobID = job.ID
				v.ReceiptName = job.OriginalName
				if job.Draft != nil {
					d := *job.Draft
					d.CheckDate()
					v.Draft = &d
				}
			}
		}
	}

	if editing {
		v.Action = fmt.Sprintf("/transactions/%d/edit", id)
		v.Title = "Edit entry"
	}
	if cats, err := s.store.SpendCategories(r.Context(), sc); err == nil {
		v.Categories = cats
	}
	keep := int64(0)
	if editing {
		keep = v.BucketID
	}
	if buckets, err := s.store.BucketOptionsKeeping(r.Context(), sc, keep); err == nil {
		v.Buckets = buckets
	}

	s.renderStatus(w, r, http.StatusBadRequest, "transaction_form.html", v)
}

// toNewTransaction converts validated form input into the store's input type.
func (in transactionInput) toNewTransaction() store.NewTransaction {
	n := store.NewTransaction{
		Kind:       in.kind,
		Label:      in.label,
		Amount:     in.amount,
		OccurredOn: in.occurredOn,
		Payee:      in.payee,
		Place:      in.place,
		Note:       in.note,
		BucketID:   in.bucketID,
		Version:    in.version,
	}
	if in.kind == store.KindExpense {
		e := in.essential
		n.Essential = &e
	} else {
		// Income has no essential flag and no recurring-expense bucket.
		n.BucketID = nil
	}
	return n
}
