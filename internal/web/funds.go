package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jthomasw/YABA-2026/internal/money"
	"github.com/jthomasw/YABA-2026/internal/store"
)

// savingsAnchor is where a fund action returns when its form did not say it
// came from Current Funds -- in practice, the emergency fund's own card.
const savingsAnchor = "/dashboard?tab=emergency"

// currentTab is the Current Funds tab, where every savings fund except the
// emergency one is listed; fundBack needs the literal value to recognise it.
const currentTab = "/dashboard?tab=current"

// fundBack says which of the two tabs a fund action's form was submitted
// from. Ordinary savings funds (and "New savings fund") are on Current Funds;
// the emergency fund's card is on Emergency Fund. Each form carries a hidden
// "back" field naming its own tab, so an action returns to where it was taken.
// Only the two exact, known values are ever honoured -- anything else
// (missing, tampered, pointed elsewhere) falls back to the Emergency Fund tab,
// so this can never become an open redirect.
func fundBack(r *http.Request) string {
	if r.PostFormValue("back") == currentTab {
		return currentTab
	}
	return savingsAnchor
}

// fundCreateTab is where handleFundCreate returns the user, same idea as
// expensesTab: it reopens the "New savings fund" details so a validation
// error -- or the confirmation of success -- appears next to the form that
// produced it, instead of the section collapsing shut on reload.
func fundCreateTab(r *http.Request) string {
	return fundBack(r) + "&fund=open"
}

func (s *Server) handleFundCreate(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	if !s.parseForm(w, r) {
		return
	}

	back := fundCreateTab(r)

	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		s.redirectError(w, r, back, "Give the fund a name.")
		return
	}

	// A goal is optional, so an empty field is zero rather than an error.
	goal, msg := optionalAmount(r.PostFormValue("goal"), "goal")
	if msg != "" {
		s.redirectError(w, r, back, msg)
		return
	}
	months, msg := optionalMonths(r.PostFormValue("target_months"))
	if msg != "" {
		s.redirectError(w, r, back, msg)
		return
	}

	if _, err := s.store.CreateFund(r.Context(), sc, name, goal, months); err != nil {
		s.redirectError(w, r, back,
			s.safeMessage(r, err, "That fund could not be created. Try again."))
		return
	}

	s.redirectSuccess(w, r, back, fmt.Sprintf("Fund %q created.", name))
}

func (s *Server) handleFundGoal(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	fundID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	if !s.parseForm(w, r) {
		return
	}
	back := fundBack(r)

	goal, msg := optionalAmount(r.PostFormValue("goal"), "goal")
	if msg != "" {
		s.redirectError(w, r, back, msg)
		return
	}
	months, msg := optionalMonths(r.PostFormValue("target_months"))
	if msg != "" {
		s.redirectError(w, r, back, msg)
		return
	}

	// A rename can arrive on the same form; an empty field leaves it alone.
	if name := strings.TrimSpace(r.PostFormValue("name")); name != "" {
		if err := s.store.RenameFund(r.Context(), sc, fundID, name); err != nil &&
			!errors.Is(err, store.ErrNotFound) {
			s.redirectError(w, r, back,
				s.safeMessage(r, err, "That fund could not be renamed. Try again."))
			return
		}
	}

	err := s.store.UpdateFundGoal(r.Context(), sc, fundID, goal, months)
	if errors.Is(err, store.ErrNotFound) {
		s.flashError(w, r, "That fund no longer exists.")
	} else if err != nil {
		s.flashError(w, r, s.safeMessage(r, err, "That change could not be saved. Try again."))
	} else {
		s.flashSuccess(w, r, "Fund target updated.")
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (s *Server) handleFundDeposit(w http.ResponseWriter, r *http.Request) {
	s.moveFund(w, r, true)
}

func (s *Server) handleFundWithdraw(w http.ResponseWriter, r *http.Request) {
	s.moveFund(w, r, false)
}

// moveFund is the shared body of deposit and withdrawal. Note what is NOT read
// from the form: the fund's balance. The store re-derives it inside the
// transaction, which is what makes an over-withdrawal impossible.
func (s *Server) moveFund(w http.ResponseWriter, r *http.Request, deposit bool) {
	sc := scopeOf(r)
	fundID, ok := s.pathID(w, r)
	if !ok || !s.parseForm(w, r) {
		return
	}

	back := fundBack(r)

	direction := "to take out of the fund"
	if deposit {
		direction = "to move into the fund"
	}
	amount, err := money.ParsePositive(r.PostFormValue("amount"))
	if err != nil {
		s.redirectError(w, r, back, "Enter an amount greater than zero "+direction+".")
		return
	}
	date, err := store.ParseDate(strings.TrimSpace(r.PostFormValue("date")))
	if err != nil {
		s.redirectError(w, r, back, "Enter a date in YYYY-MM-DD form.")
		return
	}
	if date > store.Today() {
		// Checked here for the friendly message; the store enforces it too.
		s.redirectError(w, r, back, "A savings transfer can't be dated in the future. Use today's date or an earlier one.")
		return
	}

	var short error
	var shortText, done string
	if deposit {
		err = s.store.Deposit(r.Context(), sc, fundID, amount, date)
		short, shortText = store.ErrInsufficientCash, "Not enough available cash. "
		done = fmt.Sprintf("%s moved into savings.", amount.Display())
	} else {
		err = s.store.Withdraw(r.Context(), sc, fundID, amount, date)
		short, shortText = store.ErrInsufficientFund, "Not enough in that fund. "
		done = fmt.Sprintf("%s returned to available cash.", amount.Display())
	}

	switch {
	case errors.Is(err, store.ErrNotFound):
		s.flashError(w, r, "That fund no longer exists.")
	case errors.Is(err, short):
		// The store's message already names the available balance.
		s.flashError(w, r, shortText+trimSentinel(err, short))
	case errors.Is(err, store.ErrFutureDated):
		s.flashError(w, r, "A savings transfer can't be dated in the future. Use today's date or an earlier one.")
	case err != nil:
		s.serverError(w, r, err)
		return
	default:
		s.flashSuccess(w, r, done)
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// handleFundClose replaces the old /delete-fund route.
func (s *Server) handleFundClose(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	fundID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	back := fundBack(r)

	returned, err := s.store.CloseFund(r.Context(), sc, fundID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.flashError(w, r, "That fund no longer exists.")
	case errors.Is(err, store.ErrFutureTransfers):
		// A reason, not a 500: the user can act on this.
		s.flashError(w, r, "This fund has a transfer dated after today, so it can't be closed until that date.")
	case err != nil:
		s.serverError(w, r, err)
		return
	case returned > 0:
		s.flashSuccess(w, r, fmt.Sprintf("Fund closed. %s returned to available cash.", returned.Display()))
	default:
		s.flashSuccess(w, r, "Fund closed.")
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
