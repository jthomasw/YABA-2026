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

type setupView struct {
	view
	Error           string
	StartingBalance string
	Expenses        []setupExpenseView
	ExpenseCount    int
	Incomes         []setupIncomeView
	IncomeCount     int
	FormToken       string
}

type setupExpenseView struct {
	Index         int
	Name          string
	Amount        string
	Type          string
	Date          string
	FrequencyN    string
	FrequencyUnit string
	Essential     bool
}

type setupIncomeView struct {
	Index         int
	Source        string
	Amount        string
	Type          string
	Date          string
	FrequencyN    string
	FrequencyUnit string
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	s.renderSetup(w, r, http.StatusOK, setupView{
		Expenses:     defaultSetupExpenses(),
		ExpenseCount: 2,
		Incomes:      defaultSetupIncomes(),
		IncomeCount:  1,
		FormToken:    s.issueFormToken(r, "setup"),
	})
}

func (s *Server) handleSetupSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.parseForm(w, r) {
		return
	}

	user := mustUser(r)
	v := setupView{
		StartingBalance: strings.TrimSpace(r.PostFormValue("starting_balance")),
		FormToken:       strings.TrimSpace(r.PostFormValue("form_token")),
	}
	count, err := setupCount(r.PostFormValue("expense_count"))
	incomeCount, incomeCountErr := setupCount(r.PostFormValue("income_count"))
	if err != nil || incomeCountErr != nil {
		v.Error = "Please reload setup and try again."
		v.Expenses = defaultSetupExpenses()
		v.ExpenseCount = len(v.Expenses)
		v.Incomes = defaultSetupIncomes()
		v.IncomeCount = len(v.Incomes)
		s.renderSetup(w, r, http.StatusBadRequest, v)
		return
	}
	v.ExpenseCount = count
	v.IncomeCount = incomeCount
	showError := func(message string) {
		v.Error = message
		s.renderSetup(w, r, http.StatusBadRequest, v)
	}

	startingBalance, err := setupAmount(v.StartingBalance)
	if err != nil {
		showError("Enter a valid starting balance, or leave it blank.")
		return
	}
	expenses := make([]store.SetupExpense, 0, count)
	for i := 0; i < count; i++ {
		expense := setupExpenseView{
			Index:         i,
			Name:          strings.TrimSpace(r.PostFormValue(fmt.Sprintf("expense_name_%d", i))),
			Amount:        strings.TrimSpace(r.PostFormValue(fmt.Sprintf("expense_amount_%d", i))),
			Type:          r.PostFormValue(fmt.Sprintf("expense_type_%d", i)),
			Date:          strings.TrimSpace(r.PostFormValue(fmt.Sprintf("expense_date_%d", i))),
			FrequencyN:    strings.TrimSpace(r.PostFormValue(fmt.Sprintf("expense_frequency_n_%d", i))),
			FrequencyUnit: strings.TrimSpace(r.PostFormValue(fmt.Sprintf("expense_frequency_unit_%d", i))),
			Essential:     r.PostFormValue(fmt.Sprintf("expense_essential_%d", i)) != "no",
		}
		if expense.Type != "recurring" {
			expense.Type = "one_time"
		}
		if expense.FrequencyN == "" {
			expense.FrequencyN = "1"
		}
		if expense.FrequencyUnit == "" {
			expense.FrequencyUnit = "month"
		}
		if expense.Date == "" {
			expense.Date = store.Today()
		}
		v.Expenses = append(v.Expenses, expense)
	}

	incomes := make([]store.SetupIncome, 0, incomeCount)
	for i := 0; i < incomeCount; i++ {
		income := setupIncomeView{
			Index:         i,
			Source:        strings.TrimSpace(r.PostFormValue(fmt.Sprintf("income_source_%d", i))),
			Amount:        strings.TrimSpace(r.PostFormValue(fmt.Sprintf("income_amount_%d", i))),
			Type:          r.PostFormValue(fmt.Sprintf("income_type_%d", i)),
			Date:          strings.TrimSpace(r.PostFormValue(fmt.Sprintf("income_date_%d", i))),
			FrequencyN:    strings.TrimSpace(r.PostFormValue(fmt.Sprintf("income_frequency_n_%d", i))),
			FrequencyUnit: strings.TrimSpace(r.PostFormValue(fmt.Sprintf("income_frequency_unit_%d", i))),
		}
		if income.Type != "recurring" {
			income.Type = "one_time"
		}
		if income.FrequencyN == "" {
			income.FrequencyN = "1"
		}
		if income.FrequencyUnit == "" {
			income.FrequencyUnit = "month"
		}
		if income.Date == "" {
			income.Date = store.Today()
		}
		v.Incomes = append(v.Incomes, income)
	}

	for _, expense := range v.Expenses {
		if expense.Name == "" && expense.Amount == "" {
			continue
		}
		if expense.Name == "" {
			showError(fmt.Sprintf("Enter a name for initial expense %d.", expense.Index+1))
			return
		}
		amount, err := money.ParsePositive(expense.Amount)
		if err != nil {
			showError(fmt.Sprintf("Enter a valid amount for %q.", expense.Name))
			return
		}
		occurredOn, err := store.ParseDate(expense.Date)
		if err != nil {
			showError(fmt.Sprintf("Enter a valid date for %q.", expense.Name))
			return
		}
		if expense.Type == "recurring" {
			frequencyN, frequencyUnit, errMsg := setupRecurringFrequency(expense.FrequencyN, expense.FrequencyUnit)
			if errMsg != "" {
				showError(fmt.Sprintf("%s (%s)", errMsg, expense.Name))
				return
			}
			expenses = append(expenses, store.SetupExpense{
				Name: expense.Name, Amount: amount, OccurredOn: occurredOn,
				Recurring: true, FrequencyN: frequencyN, FrequencyUnit: frequencyUnit,
				Essential: expense.Essential,
			})
			continue
		}
		expenses = append(expenses, store.SetupExpense{
			Name: expense.Name, Amount: amount, OccurredOn: occurredOn,
			Essential: expense.Essential,
		})
	}

	for _, income := range v.Incomes {
		if income.Source == "" && income.Amount == "" {
			continue
		}
		if income.Source == "" {
			showError(fmt.Sprintf("Enter a source for initial income %d.", income.Index+1))
			return
		}
		amount, err := money.ParsePositive(income.Amount)
		if err != nil {
			showError(fmt.Sprintf("Enter a valid amount for %q.", income.Source))
			return
		}
		occurredOn, err := store.ParseDate(income.Date)
		if err != nil {
			showError(fmt.Sprintf("Enter a valid date for %q.", income.Source))
			return
		}
		setupIncome := store.SetupIncome{
			Source: income.Source, Amount: amount, OccurredOn: occurredOn,
		}
		if income.Type == "recurring" {
			frequencyN, frequencyUnit, errMsg := setupRecurringFrequency(income.FrequencyN, income.FrequencyUnit)
			if errMsg != "" {
				showError(fmt.Sprintf("%s (%s)", errMsg, income.Source))
				return
			}
			setupIncome.Recurring = true
			setupIncome.FrequencyN = frequencyN
			setupIncome.FrequencyUnit = frequencyUnit
		}
		incomes = append(incomes, setupIncome)
	}

	if s.duplicateSubmit(w, r, user.ID, "/dashboard", "Welcome to YABA.") {
		return
	}
	if err := s.store.InitializeBudget(r.Context(), scopeOf(r), startingBalance, expenses, incomes); err != nil {
		s.restoreSubmissionToken(r, user.ID)
		s.serverError(w, r, err)
		return
	}
	s.redirectSuccess(w, r, "/dashboard", "Welcome to YABA.")
}

func defaultSetupExpenses() []setupExpenseView {
	return []setupExpenseView{
		{Index: 0, Type: "one_time", Date: store.Today(), FrequencyN: "1", FrequencyUnit: "month", Essential: true},
		{Index: 1, Type: "one_time", Date: store.Today(), FrequencyN: "1", FrequencyUnit: "month", Essential: true},
	}
}

func defaultSetupIncomes() []setupIncomeView {
	return []setupIncomeView{
		{Index: 0, Type: "one_time", Date: store.Today(), FrequencyN: "1", FrequencyUnit: "month"},
	}
}

func setupCount(raw string) (int, error) {
	count, err := strconv.Atoi(raw)
	if err != nil || count < 0 || count > 20 {
		return 0, errors.New("invalid setup entry count")
	}
	return count, nil
}

func setupRecurringFrequency(rawN, unit string) (int, string, string) {
	n, err := strconv.Atoi(rawN)
	if err != nil {
		return 0, "", "Enter a valid recurring frequency."
	}
	if ok, message := store.ValidFrequency(n, unit); !ok {
		return 0, "", message
	}
	return n, unit, ""
}

func setupAmount(raw string) (money.Cents, error) {
	if raw == "" {
		return 0, nil
	}
	amount, err := money.Parse(raw)
	if err != nil || amount < 0 {
		return 0, money.ErrInvalidAmount
	}
	return amount, nil
}

func (s *Server) renderSetup(w http.ResponseWriter, r *http.Request, status int, v setupView) {
	v.view = s.baseView(w, r, "Set up your budget", "setup")
	s.renderStatus(w, r, status, "setup.html", v)
}
