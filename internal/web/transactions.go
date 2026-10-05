package web

import (
	"encoding/csv"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/jthomasw/YABA-2026/internal/money"
	"github.com/jthomasw/YABA-2026/internal/store"
)

// transactionsView backs the transaction list.
type transactionsView struct {
	view

	Transactions []store.Transaction
	Filter       store.Filter

	Page       int
	TotalPages int
	Total      int
	From       int
	To         int
	// PrevURL and NextURL are complete, same-site links ("/transactions?..."),
	// written straight into an href. They used to be bare query strings
	// placed after a literal "?" in the template, which is a query-value
	// context: html/template percent-encoded every "=" and "&", so Next sent
	// "page%3D2" -- one key with no value -- and paging silently stayed on
	// page one.
	PrevURL string
	NextURL string

	Months     []string
	Kinds      []kindOption
	SearchText string

	// BackPath is this filtered, paginated list's own URL, so Edit and Delete
	// can return here instead of resetting to page one with no filters.
	//
	// The Edit link puts it after "?back=" unescaped and lets html/template
	// encode it, exactly once. There used to be a pre-escaped copy for that
	// href, which the template then escaped a second time ("%252F..."), so
	// Cancel on the edit page led to a 404 and a save fell back to the bare
	// list with the filters gone.
	BackPath string
}

type kindOption struct {
	Value    string
	Label    string
	Selected bool
}

func (s *Server) handleTransactions(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	q := r.URL.Query()

	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}

	f := listFilter(q)
	f.Limit = store.DefaultPageSize
	f.Offset = (page - 1) * store.DefaultPageSize

	txs, total, err := s.store.List(r.Context(), sc, f)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	totalPages := (total + store.DefaultPageSize - 1) / store.DefaultPageSize
	if totalPages == 0 {
		totalPages = 1
	}
	// A page number past the end (from a stale bookmark, or after deleting
	// rows) would otherwise render an empty table with no explanation.
	if page > totalPages {
		http.Redirect(w, r, transactionsURL(q, totalPages), http.StatusSeeOther)
		return
	}

	v := transactionsView{
		view:         s.baseView(w, r, "Transactions", "transactions"),
		Transactions: txs,
		Filter:       f,
		Page:         page,
		TotalPages:   totalPages,
		Total:        total,
		SearchText:   q.Get("q"),
	}
	if total > 0 {
		v.From = f.Offset + 1
		v.To = f.Offset + len(txs)
	}
	if page > 1 {
		v.PrevURL = transactionsURL(q, page-1)
	}
	if page < totalPages {
		v.NextURL = transactionsURL(q, page+1)
	}
	v.BackPath = transactionsURL(q, page)

	if v.Months, err = s.store.Months(r.Context(), sc); err != nil {
		s.serverError(w, r, err)
		return
	}

	for _, k := range []struct{ v, l string }{
		{"", "All types"},
		{string(store.KindIncome), "Income"},
		{string(store.KindExpense), "Expenses"},
		{string(store.KindFundDeposit), "To savings"},
		{string(store.KindFundWithdrawal), "From savings"},
	} {
		v.Kinds = append(v.Kinds, kindOption{
			Value: k.v, Label: k.l, Selected: k.v == string(f.Kind),
		})
	}

	s.render(w, r, "transactions.html", v)
}

// transactionsURL is the transaction list's URL with a different page number,
// preserving the active filters so paging does not reset them. url.Values does
// the encoding, so a search for "a&b" or "50%" survives the round trip; a
// hand-rolled replacer here once had to remember every reserved character.
func transactionsURL(q url.Values, page int) string {
	out := url.Values{}
	for _, key := range []string{"type", "month", "q"} {
		if v := q.Get(key); v != "" {
			out.Set(key, v)
		}
	}
	out.Set("page", strconv.Itoa(page))
	return "/transactions?" + out.Encode()
}

// backFallback is where an edit returns when it was not given anywhere
// trustworthy to go back to.
const backFallback = "/transactions"

// safeBack vets a "back" value -- the list URL an edit or delete should return
// to -- before it is used in a redirect or printed into an href.
//
// A prefix check on "/transactions" was not enough. http.Redirect cleans the
// path it is given, so "/transactions/../\evil.com" left as "/\evil.com",
// which browsers resolve as the host evil.com; and the Cancel link printed the
// value with no check at all, so "//evil.example" worked there directly. This
// accepts only a plain same-site path on the transactions list, already in
// clean form, with no scheme, host, backslash or dot segments, and keeps its
// query so the filters survive. Anything else is replaced, not repaired:
// guessing what a malformed value meant is how open redirects are born.
func safeBack(raw string) string {
	if raw == "" || strings.Contains(raw, "\\") || strings.HasPrefix(raw, "//") {
		return backFallback
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
		return backFallback
	}
	p := u.Path
	if p != backFallback && !strings.HasPrefix(p, backFallback+"/") {
		// "/transactionsX" is not the list.
		return backFallback
	}
	// u.Path is decoded, so "%2e%2e" is caught here as well as a literal "..".
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." {
			return backFallback
		}
	}
	if path.Clean(p) != p || u.RawPath != "" {
		return backFallback
	}
	if u.RawQuery == "" {
		return p
	}
	if _, err := url.ParseQuery(u.RawQuery); err != nil {
		// A query that does not parse cannot be a filter this app wrote.
		return p
	}
	return p + "?" + u.RawQuery
}

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
				v.Draft = d
				if d.Total > 0 {
					v.Amount = d.Total.Input()
				}
				if d.Date != "" {
					v.OccurredOn = d.Date
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
	if v.Buckets, err = s.store.BucketOptions(r.Context(), sc); err != nil {
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
	if kind != store.KindIncome {
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

func (s *Server) handleTransactionCreate(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)
	sc := scopeOf(r)
	if !s.parseForm(w, r) {
		return
	}

	in, msg := parseTransactionForm(r)
	if msg != "" {
		s.rerenderTransactionForm(w, r, in, msg, false, 0)
		return
	}
	if !formBelongsToHousehold(r, sc.HouseholdID) {
		s.rerenderTransactionForm(w, r, in, "This form belongs to a different budget. Reload it before saving.", false, 0)
		return
	}

	if s.duplicateSubmit(w, r, user.ID, "/dashboard", "That entry was already saved.") {
		return
	}

	n := in.toNewTransaction()

	// An expense may arrive with a receipt attached in the same submission.
	if in.kind == store.KindExpense {
		path, name, err := s.saveReceipt(r, user.ID)
		if err != nil {
			s.restoreSubmissionToken(r, user.ID)
			s.rerenderTransactionForm(w, r, in,
				s.safeMessage(r, err, "That receipt could not be stored. Try again."), false, 0)
			return
		}
		n.ReceiptPath, n.ReceiptName = path, name
	}

	// Or it may be finishing a receipt uploaded earlier: validated before the insert, so a
	// stolen or already-used id fails without leaving a stray expense behind.
	var attachJobID int64
	if in.kind == store.KindExpense && n.ReceiptPath == "" {
		if raw := r.PostFormValue("receipt_job"); raw != "" {
			jobID, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				s.restoreSubmissionToken(r, user.ID)
				s.badRequest(w, "That receipt reference was not valid.")
				return
			}
			if _, err := s.store.UnattachedReceipt(r.Context(), sc, jobID); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					s.restoreSubmissionToken(r, user.ID)
					s.rerenderTransactionForm(w, r, in,
						"That receipt has already been used, or is no longer available.", false, 0)
					return
				}
				s.restoreSubmissionToken(r, user.ID)
				s.serverError(w, r, err)
				return
			}
			attachJobID = jobID
		}
	}

	txID, err := s.store.AddWithReceiptJob(r.Context(), sc, n, attachJobID)
	if errors.Is(err, store.ErrNotFound) {
		s.restoreSubmissionToken(r, user.ID)
		s.rerenderTransactionForm(w, r, in, "That recurring expense no longer exists.", false, 0)
		return
	}
	if err != nil {
		s.removeReceiptFiles([]string{n.ReceiptPath})
		s.restoreSubmissionToken(r, user.ID)
		s.serverError(w, r, err)
		return
	}

	ws := s.finishSave(r, sc, txID, in, in.occurredOn)
	s.redirectSaved(w, r, "/dashboard", ws,
		in.kind.Label()+" of "+in.amount.Display()+" saved.")
}

// warnings are the things that went wrong after the row was already written.
//
// They are collected rather than flashed as they happen because there is one
// flash slot per redirect: a warning set here would be overwritten by the
// success message the handler sets a moment later, and the user would be told
// everything worked while the line items, the receipt or the month's funding
// were quietly missing.
type warnings []string

func (ws *warnings) add(format string, a ...any) {
	*ws = append(*ws, fmt.Sprintf(format, a...))
}

// fold combines the warnings with the message the handler wanted to show. A
// partial failure is reported as an error, because it is one.
func (ws warnings) fold(success string) (kind, text string) {
	if len(ws) == 0 {
		return "success", success
	}
	return "error", success + " " + strings.Join(ws, " ")
}

// redirectSaved sends the user on with a single message covering both what was
// saved and anything that did not survive the save.
func (s *Server) redirectSaved(w http.ResponseWriter, r *http.Request, to string, ws warnings, success string) {
	kind, text := ws.fold(success)
	s.setFlash(w, r, kind, text)
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// finishSave stores the optional line-item breakdown and re-pours the funding
// waterfall: income funds the priority list, and a bucket-attributed expense
// changes what a variable bucket costs. The transaction is already saved, so a
// failure here is reported alongside the success rather than instead of it.
//
// Every month named is recalculated. An edit that moves an entry passes two --
// the month it left and the month it joined -- because the waterfall is stored
// per month and re-pouring only the new one leaves the old holding allocations
// against money that is no longer in it.
func (s *Server) finishSave(r *http.Request, sc store.Scope, txID int64, in transactionInput, months ...string) warnings {
	var ws warnings
	if len(in.items) > 0 {
		if err := s.store.SetLineItems(r.Context(), sc, txID, in.items); err != nil {
			// safeMessage logs the real cause and only shows store validation
			// text; a driver error used to be printed on the page verbatim.
			ws.add("The line items could not be stored: %s",
				s.safeMessage(r, err, "please add them again from Edit."))
		}
	}
	ws.addReallocations(r, s.store, sc, months...)
	return ws
}

// addReallocations re-pours each distinct month named, ignoring blanks.
func (ws *warnings) addReallocations(r *http.Request, st *store.Store, sc store.Scope, months ...string) {
	done := map[string]bool{}
	for _, m := range months {
		if len(m) < 7 || done[m[:7]] {
			continue
		}
		done[m[:7]] = true
		if err := st.ReallocateMonthOf(r.Context(), sc, m); err != nil {
			ws.add("The expense funding for %s could not be recalculated.", m[:7])
		}
	}
}

func (s *Server) handleTransactionUpdate(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	if !s.parseForm(w, r) {
		return
	}

	in, msg := parseTransactionForm(r)
	if msg != "" {
		s.rerenderTransactionForm(w, r, in, msg, true, id)
		return
	}

	// The month the entry is leaving, read before the write. An edit can move an
	// entry between months and the funding waterfall is stored per month, so
	// both have to be re-poured -- handleTransactionDelete reads the date first
	// for the same reason.
	wasOn := ""
	if t, e := s.store.ByID(r.Context(), sc, id); e == nil {
		wasOn = t.OccurredOn
	}

	// The amount and its breakdown go in together. Saving them separately let a
	// cancelled request commit a new amount over the old line items, leaving the
	// dashboard's headline spend and its own category chart disagreeing with
	// nothing in the schema to catch it.
	err := s.store.UpdateWithItems(r.Context(), sc, id, in.toNewTransaction(), in.items)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, store.ErrConflict) {
		// Somebody else saved this row while the form was open.
		s.conflictOnUpdate(w, r, sc, id, in)
		return
	}
	// A breakdown that does not add up is a correctable typing mistake, not a
	// server fault. The form already checks this, so reaching here means a
	// hand-built POST -- which still deserves the sentence rather than a 500.
	if errors.Is(err, store.ErrItemsDoNotBalance) {
		s.rerenderTransactionFormAt(w, r, in,
			"The items must add up to the amount above. Nothing was saved.",
			true, id, in.version)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	var ws warnings
	ws.addReallocations(r, s.store, sc, wasOn, in.occurredOn)

	// Vetted by safeBack, so this cannot be turned into an open redirect to
	// another site -- the same guard handleTransactionDelete applies to its
	// own "back" field.
	s.redirectSaved(w, r, safeBack(r.PostFormValue("back")), ws, "Entry updated.")
}

func (s *Server) handleTransactionDelete(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}

	// Read the date before deleting, so the right month can be recalculated.
	month := ""
	if t, e := s.store.ByID(r.Context(), sc, id); e == nil {
		month = t.OccurredOn
	}

	orphaned, err := s.store.Delete(r.Context(), sc, id)
	s.removeReceiptFiles(orphaned)
	if errors.Is(err, store.ErrNotFound) {
		// Covers both no such row and belongs to another household.
		s.flashError(w, r, "That entry no longer exists.")
	} else if err != nil {
		s.serverError(w, r, err)
		return
	} else {
		// Deleting an income unwinds its allocations through the foreign key,
		// but the remaining income has to be re-poured over the priority list.
		// One flash slot per redirect: setting the warning and then the
		// success message used to overwrite the warning, so a failed
		// recalculation was reported as a plain success.
		if err := s.store.ReallocateMonthOf(r.Context(), sc, month); err != nil {
			log.Printf("ERROR %s %s %s: reallocation after delete failed: %v",
				requestID(r.Context()), r.Method, r.URL.Path, err)
			s.flashError(w, r, "Entry deleted, but the expense funding could not be recalculated. Try the Recalculate button.")
		} else {
			s.flashSuccess(w, r, "Entry deleted.")
		}
	}

	// Only a vetted same-site list URL is honoured, so the redirect cannot be
	// turned into an open redirect to another site.
	http.Redirect(w, r, safeBack(r.PostFormValue("back")), http.StatusSeeOther)
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
				v.Draft = job.Draft
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
	if buckets, err := s.store.BucketOptions(r.Context(), sc); err == nil {
		v.Buckets = buckets
	}

	s.renderStatus(w, r, http.StatusBadRequest, "transaction_form.html", v)
}

// ── CSV export ────────────────────────────────────────────────────────────────

// handleExportCSV streams the filtered transactions as a spreadsheet.
func (s *Server) handleExportCSV(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	q := r.URL.Query()
	month := parseMonth(q.Get("month"))

	txs, err := s.store.All(r.Context(), sc, listFilter(q))
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	filename := "yaba-transactions"
	if month != "" {
		filename += "-" + month
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`.csv"`)

	// csv.Writer errors are sticky: after the first failure every later row is
	// dropped and Flush says nothing, so a user exporting a year of history
	// over a flaky connection downloaded a file that stopped halfway, with
	// HTTP 200 and no reason to doubt it. The header is already sent by the
	// time this can happen, so the response cannot become an error -- but it
	// can stop being silent.
	cw := csv.NewWriter(w)
	defer func() {
		cw.Flush()
		if err := cw.Error(); err != nil {
			log.Printf("ERROR %s %s: CSV export truncated: %v", r.Method, r.URL.Path, err)
		}
	}()

	// "Added by" is always a column here, unlike in the on-screen table where it is hidden
	// for a solo user.
	_ = cw.Write([]string{
		"Date", "Type", "Description", "Amount", "Effect on cash", "Essential",
		"Fund", "Added by",
	})
	for _, t := range txs {
		essential := ""
		if t.Essential != nil {
			if *t.Essential {
				essential = "Essential"
			} else {
				essential = "Non-essential"
			}
		}
		// Amounts are written as plain decimals with no currency symbol or thousands
		// separator, because a spreadsheet will not parse "$1,234.56" as a number.
		_ = cw.Write([]string{
			t.OccurredOn,
			t.Kind.Label(),
			csvSafe(t.Label),
			t.Amount.Input(),
			t.SignedAmount().Input(),
			essential,
			csvSafe(t.FundName),
			csvSafe(t.AddedBy),
		})
	}
}

// csvSafe neutralises spreadsheet formula injection: a label like =1+1 or @SUM(A1:A9)
// is executed as a formula when the file is opened in Excel or Sheets.
//
// Leading whitespace is looked past: some spreadsheets trim it before deciding
// whether a cell is a formula, so " =cmd" is as dangerous as "=cmd".
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	t := strings.TrimLeft(s, " \t\r\n")
	if s[0] == '\t' || s[0] == '\r' || (t != "" && strings.ContainsRune("=+-@", rune(t[0]))) {
		return "'" + s
	}
	return s
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

// conflictOnUpdate re-renders the edit form after a losing race, showing what the other
// person saved and carrying the new version forward so that saving again succeeds.
func (s *Server) conflictOnUpdate(w http.ResponseWriter, r *http.Request,
	sc store.Scope, id int64, in transactionInput) {
	current, err := s.store.ByID(r.Context(), sc, id)
	if err != nil {
		// It was there a moment ago -- the row has since gone entirely.
		http.NotFound(w, r)
		return
	}

	msg := fmt.Sprintf(
		"Somebody else changed this entry while you had it open. "+
			"It now reads %s — %s on %s%s. "+
			"Your version is still in the form below; save again to replace theirs.",
		current.Label, current.Amount.Display(), current.OccurredOn,
		addedByPhrase(current.AddedBy))

	s.rerenderTransactionFormAt(w, r, in, msg, true, id, current.Version)
}

func addedByPhrase(who string) string {
	if strings.TrimSpace(who) == "" {
		return ""
	}
	return ", entered by " + who
}
