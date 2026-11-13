package web

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/jthomasw/YABA-2026/internal/store"
)

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
			job, err := s.store.UnattachedReceipt(r.Context(), sc, jobID)
			if err == nil && strings.TrimSpace(r.PostFormValue("date")) == "" && job.Draft != nil {
				// The receipt's date could not be read, so the date box was
				// empty; saving it as today would put the expense in the wrong
				// month without anyone noticing.
				d := *job.Draft
				d.CheckDate()
				if d.BadDate != "" {
					s.restoreSubmissionToken(r, user.ID)
					s.rerenderTransactionForm(w, r, in,
						"The date on this receipt couldn't be read. Enter it from the picture.", false, 0)
					return
				}
			}
			if err != nil {
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
	if errors.Is(err, store.ErrNotFound) && wasOn != "" {
		// The entry exists, so what is missing is the bill it was pointed at:
		// removed since the page was opened. Say so and keep what was typed,
		// rather than a bare 404.
		s.rerenderTransactionFormAt(w, r, in,
			"That monthly bill has been removed. Choose another under Pays towards, or none. Nothing was saved.",
			true, id, in.version)
		return
	}
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
