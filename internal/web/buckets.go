package web

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/jthomasw/YABA-2026/internal/money"
	"github.com/jthomasw/YABA-2026/internal/store"
)

// expensesTab is where every bucket action returns the user, so they land back
// on the list they were editing rather than at the top of the dashboard.
const expensesTab = "/dashboard?tab=expenses&planner=open#panel-expenses"

// parseBucketForm reads the recurring-expense form.
func parseBucketForm(r *http.Request) (store.NewBucket, string) {
	var n store.NewBucket

	n.Name = strings.TrimSpace(r.PostFormValue("name"))
	if n.Name == "" {
		return n, "Give the expense a name, for example \"Rent\"."
	}

	n.CostKind = store.CostKind(r.PostFormValue("cost_kind"))
	if !n.CostKind.Valid() {
		n.CostKind = store.CostFixed
	}
	n.Essential = r.PostFormValue("essential") == "yes"

	if n.CostKind == store.CostFixed {
		amount, err := money.ParsePositive(r.PostFormValue("amount"))
		if err != nil {
			return n, "A fixed monthly expense needs an amount greater than zero."
		}
		n.Fixed = amount
	}
	// A variable bucket's amount is derived from the transactions entered
	// against it, so any figure typed here is ignored rather than rejected --
	// the field stays on screen when the user switches kind.

	return n, ""
}

func (s *Server) handleBucketCreate(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	if !s.parseForm(w, r) {
		return
	}

	n, msg := parseBucketForm(r)
	if msg != "" {
		s.redirectError(w, r, expensesTab, msg)
		return
	}

	if _, err := s.store.CreateBucket(r.Context(), sc, n); err != nil {
		s.redirectError(w, r, expensesTab,
			s.safeMessage(r, err, "That recurring expense could not be created. Try again."))
		return
	}

	// A new expense changes what the month requires, so the waterfall is re-poured.
	// Its failure is folded into the one message rather than flashed first,
	// where the success message used to overwrite it.
	s.redirectSaved(w, r, expensesTab, s.reallocateNow(r),
		fmt.Sprintf("%q added to your recurring expenses.", n.Name))
}

// redirectAfterStoreOp is the "edit or remove one row, then bounce back to the
// list" shape that repeats across the bucket and recurring-income handlers: a
// missing row gets its own message (someone else archived or deleted it since
// the page was loaded), any other error is shown safely rather than leaking
// its raw text, and success runs an optional follow-up (reallocating, say)
// whose warnings are folded into the success flash -- there is one flash slot,
// so a follow-up that flashed its own failure was simply overwritten by the
// success message. Centralised so the three-way switch is written once.
func (s *Server) redirectAfterStoreOp(
	w http.ResponseWriter,
	r *http.Request,
	err error,
	path string,
	notFoundMsg string,
	failMsg string,
	successMsg string,
	onSuccess func() warnings,
) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.flashError(w, r, notFoundMsg)
	case err != nil:
		s.flashError(w, r, s.safeMessage(r, err, failMsg))
	default:
		var ws warnings
		if onSuccess != nil {
			ws = onSuccess()
		}
		kind, text := ws.fold(successMsg)
		s.setFlash(w, r, kind, text)
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

func (s *Server) handleBucketUpdate(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	if !s.parseForm(w, r) {
		return
	}

	n, msg := parseBucketForm(r)
	if msg != "" {
		s.redirectError(w, r, expensesTab, msg)
		return
	}

	err := s.store.UpdateBucket(r.Context(), sc, id, n)
	s.redirectAfterStoreOp(w, r, err, expensesTab,
		"That expense no longer exists.",
		"That change could not be saved. Try again.",
		"Recurring expense updated.",
		func() warnings { return s.reallocateNow(r) },
	)
}

func (s *Server) handleBucketUp(w http.ResponseWriter, r *http.Request) {
	s.moveBucket(w, r, true)
}

func (s *Server) handleBucketDown(w http.ResponseWriter, r *http.Request) {
	s.moveBucket(w, r, false)
}

// moveBucket reorders the priority list. Not cosmetic: priority decides which expense
// is funded first when income arrives, so a move has to re-run the allocation.
func (s *Server) moveBucket(w http.ResponseWriter, r *http.Request, up bool) {
	sc := scopeOf(r)
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}

	err := s.store.MoveBucket(r.Context(), sc, id, up)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.flashError(w, r, "That expense no longer exists.")
	case err != nil:
		s.serverError(w, r, err)
		return
	default:
		// A move has no success message of its own, so only a failure is
		// reported.
		if ws := s.reallocateNow(r); len(ws) > 0 {
			kind, text := ws.fold("Priority changed.")
			s.setFlash(w, r, kind, text)
		}
	}
	http.Redirect(w, r, expensesTab, http.StatusSeeOther)
}

func (s *Server) handleBucketArchive(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}

	err := s.store.ArchiveBucket(r.Context(), sc, id)
	s.redirectAfterStoreOp(w, r, err, expensesTab,
		"That expense no longer exists.",
		"That could not be removed. Try again.",
		"Recurring expense removed. Its history has been kept.",
		func() warnings { return s.reallocateNow(r) },
	)
}

// handleReallocate lets the user force a recalculation.
func (s *Server) handleReallocate(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	month := strings.TrimSpace(r.PostFormValue("month"))

	if err := s.store.Reallocate(r.Context(), sc, month); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.redirectSuccess(w, r, expensesTab, "Income re-allocated down the priority list.")
}

// reallocateNow re-pours the waterfall for the current month and returns a
// warning if that failed, rather than leaving the page silently stale.
//
// It returns the warning instead of flashing it: every caller goes on to set a
// success flash, and with one flash slot per redirect that used to overwrite
// this one, so a failed recalculation after creating, editing or archiving a
// bucket was reported as a plain success. Callers fold it into their message.
func (s *Server) reallocateNow(r *http.Request) warnings {
	var ws warnings
	if err := s.store.Reallocate(r.Context(), scopeOf(r), ""); err != nil {
		// Not fatal: the change the user asked for did happen. But the cause was
		// dropped here entirely, so a user who pressed Recalculate and watched it
		// fail again left no trace at all for anybody to diagnose from.
		log.Printf("ERROR %s %s %s: reallocation failed: %v",
			requestID(r.Context()), r.Method, r.URL.Path, err)
		ws.add("But the expense funding could not be recalculated. Try the Recalculate button.")
	}
	return ws
}
