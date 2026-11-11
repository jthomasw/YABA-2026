package web

import (
	"encoding/csv"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

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

func addedByPhrase(who string) string {
	if strings.TrimSpace(who) == "" {
		return ""
	}
	return ", entered by " + who
}
