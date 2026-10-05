package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jthomasw/YABA-2026/internal/money"
	"github.com/jthomasw/YABA-2026/internal/store"
)

// ── budgets ───────────────────────────────────────────────────────────────────

func (s *Server) handleBudgetSet(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	if !s.parseForm(w, r) {
		return
	}

	category := strings.TrimSpace(r.PostFormValue("category"))
	if category == "" {
		s.redirectError(w, r, "/reports#budgets", "Choose a category to budget.")
		return
	}

	limit, err := money.ParsePositive(r.PostFormValue("limit"))
	if err != nil {
		s.redirectError(w, r, "/reports#budgets", "Enter a monthly limit greater than zero.")
		return
	}

	if err := s.store.SetBudget(r.Context(), sc, category, limit); err != nil {
		s.flashError(w, r, s.safeMessage(r, err, "That change could not be saved. Try again."))
	} else {
		s.flashSuccess(w, r, fmt.Sprintf("%s budget set to %s a month.", category, limit.Display()))
	}
	http.Redirect(w, r, "/reports#budgets", http.StatusSeeOther)
}

func (s *Server) handleBudgetDelete(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}

	err := s.store.DeleteBudget(r.Context(), sc, id)
	if errors.Is(err, store.ErrNotFound) {
		s.flashError(w, r, "That budget no longer exists.")
	} else if err != nil {
		s.serverError(w, r, err)
		return
	} else {
		s.flashSuccess(w, r, "Budget removed.")
	}
	http.Redirect(w, r, "/reports#budgets", http.StatusSeeOther)
}
