/* Add/edit form behaviour.
 *
 * One form serves both income and expenses, matching the wireframe's two
 * variants: Source/Date/Amount for income, and the five Ws for an expense.
 * Switching type relabels the description field and hides the expense-only ones.
 *
 * Everything degrades. With JavaScript off the server picks the right labels from
 * ?type=, all fields stay visible, the handler ignores an essential flag on an
 * income row, and the single blank line-item row still submits.
 */
(function () {
  "use strict";

  function initTypeToggle() {
    var expenseRadio = document.querySelector("[data-toggle-expense]");
    var incomeRadio = document.querySelector("[data-toggle-income]");
    if (!expenseRadio || !incomeRadio) return;

    var expenseOnly = document.querySelectorAll("[data-expense-only]");
    var labelEl = document.querySelector('label[for="label"]');
    var hintEl = document.querySelector("[data-hint-expense]");
    var labelInput = document.getElementById("label");
    var dateLabel = document.querySelector('label[for="date"]');

    function apply() {
      var isIncome = incomeRadio.checked;

      expenseOnly.forEach(function (el) { el.hidden = isIncome; });

      if (labelEl) {
        var t = isIncome
          ? labelEl.getAttribute("data-label-income")
          : labelEl.getAttribute("data-label-expense");
        if (t) labelEl.textContent = t;
      }
      if (hintEl) {
        var h = isIncome
          ? hintEl.getAttribute("data-hint-income")
          : hintEl.getAttribute("data-hint-expense");
        if (h) hintEl.textContent = h;
      }
      if (dateLabel) {
        dateLabel.textContent = isIncome ? "Date" : "When?";
      }
      if (labelInput) {
        labelInput.placeholder = isIncome ? "Salary" : "Food";
        // The suggestions are expense categories, so they are misleading on an
        // income entry.
        if (isIncome) {
          labelInput.removeAttribute("list");
        } else {
          labelInput.setAttribute("list", "category-options");
        }
      }
    }

    expenseRadio.addEventListener("change", apply);
    incomeRadio.addEventListener("change", apply);
    apply();
  }

  /* Line items.
   *
   * Rows are cloned from the blank template row, and the running total is shown
   * against the transaction amount. The server enforces that they reconcile; this
   * only tells the user before they submit, which is friendlier than a rejection.
   */
  function initLineItems() {
    var rows = document.querySelector("[data-item-rows]");
    var template = document.querySelector("[data-item-template]");
    var addBtn = document.querySelector("[data-add-item]");
    var totalEl = document.querySelector("[data-item-total]");
    var amountInput = document.getElementById("amount");
    if (!template) return;

    var counter = 0;

    function money(cents) {
      var sign = cents < 0 ? "-" : "";
      var n = Math.abs(cents);
      return sign + "$" + (n / 100).toLocaleString(undefined, {
        minimumFractionDigits: 2, maximumFractionDigits: 2
      });
    }

    // Parsing to integer cents rather than using parseFloat, so the running total
    // matches the server's arithmetic exactly instead of drifting on values like
    // 0.1 + 0.2.
    function cents(value) {
      var s = String(value == null ? "" : value).trim();
      if (s === "") return 0;
      var m = /^(-?)(\d*)(?:\.(\d{0,2}))?$/.exec(s);
      if (!m) return NaN;
      var whole = m[2] === "" ? 0 : parseInt(m[2], 10);
      var frac = (m[3] || "").padEnd(2, "0");
      var out = whole * 100 + parseInt(frac || "0", 10);
      return m[1] === "-" ? -out : out;
    }

    function recalc() {
      if (!totalEl) return;

      var sum = 0;
      var any = false;
      document.querySelectorAll('input[name="item_amount"]').forEach(function (el) {
        if (el.value.trim() === "") return;
        var c = cents(el.value);
        if (!isNaN(c)) { sum += c; any = true; }
      });

      if (!any) {
        totalEl.textContent = "";
        totalEl.classList.remove("is-negative", "is-positive");
        return;
      }

      var target = amountInput ? cents(amountInput.value) : NaN;
      if (isNaN(target) || target === 0) {
        totalEl.textContent = "Items total " + money(sum) + ".";
        totalEl.classList.remove("is-negative", "is-positive");
        return;
      }

      var diff = sum - target;
      if (diff === 0) {
        totalEl.textContent = "Items total " + money(sum) + " — matches the amount.";
        totalEl.classList.add("is-positive");
        totalEl.classList.remove("is-negative");
      } else {
        totalEl.textContent = "Items total " + money(sum) + ", which is " +
          money(Math.abs(diff)) + (diff > 0 ? " more" : " less") + " than the amount.";
        totalEl.classList.add("is-negative");
        totalEl.classList.remove("is-positive");
      }
    }

    if (addBtn && rows) {
      addBtn.addEventListener("click", function () {
        counter += 1;
        var clone = template.cloneNode(true);
        clone.removeAttribute("data-item-template");

        // Re-point every label/input id pair, otherwise the clones all share an
        // id and every label activates the first row's field.
        clone.querySelectorAll("input").forEach(function (input) {
          var old = input.id;
          if (!old) return;
          var fresh = old.replace(/-new$/, "-extra" + counter);
          input.id = fresh;
          input.value = "";
          var label = clone.querySelector('label[for="' + old + '"]');
          if (label) label.setAttribute("for", fresh);
        });

        rows.appendChild(clone);
        var first = clone.querySelector("input");
        if (first) first.focus();
        recalc();
      });
    }

    // Delegated, so rows added later are covered without rebinding.
    document.addEventListener("input", function (e) {
      if (e.target && (e.target.name === "item_amount" || e.target.id === "amount")) {
        recalc();
      }
    });

    recalc();
  }

  function init() {
    initTypeToggle();
    initLineItems();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
