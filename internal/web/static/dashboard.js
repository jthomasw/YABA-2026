/* Dashboard behaviour: accessible tabs, confirm-before-destroy, and charts.
 *
 * Everything here degrades: with JavaScript off, all tab panels render (the
 * hidden attribute is only added by this script), forms submit without a
 * confirmation prompt, and the tables beside each chart still carry the numbers.
 */
(function () {
  "use strict";

  /* ── tabs ───────────────────────────────────────────────────────────────
   * Implements the ARIA tabs pattern: arrow keys move between tabs, Home and
   * End jump to the ends, and only the selected tab is in the tab order so a
   * keyboard user tabs *into* the panel rather than through five buttons.
   *
   * The old dashboard used onclick="showSection('x')" on <div> and <a href="#">
   * elements, which cannot be reached by keyboard and announce nothing.
   */
  function initTabs(root) {
    var tabs = Array.prototype.slice.call(root.querySelectorAll('[role="tab"]'));
    if (!tabs.length) return;

    function panelFor(tab) {
      return document.getElementById(tab.getAttribute("aria-controls"));
    }

    function select(tab, focus) {
      tabs.forEach(function (t) {
        var selected = t === tab;
        t.setAttribute("aria-selected", selected ? "true" : "false");
        // tabindex -1 keeps unselected tabs out of the tab order.
        t.setAttribute("tabindex", selected ? "0" : "-1");
        var panel = panelFor(t);
        if (panel) panel.hidden = !selected;
      });
      if (focus) tab.focus();
      // Record the tab in the URL fragment so a reload, a bookmark, or a
      // redirect from a fund action lands back on the right section.
      var id = tab.getAttribute("aria-controls").replace(/^panel-/, "");
      if (history.replaceState) {
        history.replaceState(null, "", "#" + id);
      }
    }

    tabs.forEach(function (tab, i) {
      tab.addEventListener("click", function () { select(tab, false); });

      tab.addEventListener("keydown", function (e) {
        var next = null;
        switch (e.key) {
          case "ArrowRight": next = tabs[(i + 1) % tabs.length]; break;
          case "ArrowLeft":  next = tabs[(i - 1 + tabs.length) % tabs.length]; break;
          case "Home":       next = tabs[0]; break;
          case "End":        next = tabs[tabs.length - 1]; break;
          default: return;
        }
        e.preventDefault();
        select(next, true);
      });
    });

    // Which tab to open: the URL fragment wins, then ?tab= as rendered by the
    // server into data-default, then the first tab. The server default matters
    // because a redirect after a form post cannot set a fragment reliably.
    function find(id) {
      return tabs.filter(function (t) {
        return t.getAttribute("aria-controls") === id;
      })[0] || null;
    }

    var wanted = null;
    if (window.location.hash) {
      wanted = find("panel-" + window.location.hash.slice(1).replace(/^panel-/, ""));
    }
    if (!wanted) {
      var def = root.getAttribute("data-default");
      if (def) wanted = find("panel-" + def);
    }
    select(wanted || tabs[0], false);
  }

  /* ── confirm before destructive actions ─────────────────────────────────
   * The old delete links fired immediately, with no confirmation and no undo.
   */
  function initConfirms() {
    document.querySelectorAll("form[data-confirm]").forEach(function (form) {
      form.addEventListener("submit", function (e) {
        if (!window.confirm(form.getAttribute("data-confirm"))) {
          e.preventDefault();
        }
      });
    });
  }

  /* ── charts ─────────────────────────────────────────────────────────────
   * Data arrives as JSON in data- attributes and is parsed here, rather than
   * being interpolated into a script body. The old templates emitted
   * template.JS(...) directly into JavaScript source, which switches Go's
   * escaping off: an expense category containing a quote or a </script> broke
   * the page at best and executed as script at worst.
   */
  function readJSON(el, attr) {
    var raw = el.getAttribute(attr);
    if (!raw) return null;
    try {
      return JSON.parse(raw);
    } catch (err) {
      // A malformed payload must not take the whole dashboard down with it.
      console.warn("chart data unreadable", attr, err);
      return null;
    }
  }

  // Colours are read from the stylesheet so the charts follow dark mode.
  function cssVar(name, fallback) {
    var v = getComputedStyle(document.documentElement).getPropertyValue(name);
    return (v && v.trim()) || fallback;
  }

  // Three palettes. The mixed one is for charts that are not about a single
  // direction of money; the green and red ramps are for the income and expense
  // doughnuts, so a glance at the colour already tells you which page you are on.
  var PALETTES = {
    mixed: ["#2563eb", "#dc2626", "#16a34a", "#d97706", "#7c3aed",
            "#0891b2", "#db2777", "#65a30d", "#c2410c", "#4f46e5"],
    income: ["#128a4d", "#1aa862", "#34c07a", "#5bd096", "#84dcb1",
             "#0d6f3d", "#0a5730", "#a8e8c8", "#22b06a", "#166b41"],
    expense: ["#c62d2d", "#dc4545", "#e86a6a", "#f08d8d", "#f6b0b0",
              "#a92323", "#8a1c1c", "#fad2d2", "#d13b3b", "#b52a2a"]
  };
  var PALETTE = PALETTES.mixed;

  function money(value) {
    return "$" + Number(value).toLocaleString(undefined, {
      minimumFractionDigits: 2,
      maximumFractionDigits: 2
    });
  }

  function baseOptions() {
    var text = cssVar("--text-muted", "#6b7280");
    var grid = cssVar("--border", "#e2e8f0");
    return {
      responsive: true,
      maintainAspectRatio: false,
      plugins: {
        legend: { labels: { color: text, boxWidth: 12 } },
        tooltip: {
          callbacks: {
            label: function (ctx) {
              var label = ctx.dataset.label || ctx.label || "";
              return (label ? label + ": " : "") + money(ctx.parsed.y != null ? ctx.parsed.y : ctx.parsed);
            }
          }
        }
      },
      scales: {
        x: { ticks: { color: text }, grid: { color: grid } },
        y: {
          ticks: {
            color: text,
            callback: function (v) { return money(v); }
          },
          grid: { color: grid }
        }
      }
    };
  }

  function lineChart(el) {
    var labels = readJSON(el, "data-labels");
    var values = readJSON(el, "data-values");
    if (!labels || !values) return;

    var datasets = [{
      label: "Cash balance",
      data: values,
      borderColor: cssVar("--brand", "#2563eb"),
      backgroundColor: "rgba(37, 99, 235, .12)",
      fill: true,
      tension: .25,
      pointRadius: labels.length > 40 ? 0 : 3
    }];

    // The trend line the wireframe asks for. Dashed and point-less so it reads
    // as a fitted line rather than as more data.
    var trend = readJSON(el, "data-trend");
    if (trend && trend.length === values.length) {
      datasets.push({
        label: "Trend",
        data: trend,
        borderColor: cssVar("--text-muted", "#6b7280"),
        borderDash: [6, 4],
        borderWidth: 2,
        pointRadius: 0,
        fill: false,
        tension: 0
      });
    }

    new Chart(el, { type: "line", data: { labels: labels, datasets: datasets },
                    options: baseOptions() });
  }

  function pieChart(el) {
    var labels = readJSON(el, "data-labels");
    var values = readJSON(el, "data-values");
    if (!labels || !values) return;

    var palette = PALETTES[el.getAttribute("data-palette")] || PALETTE;
    var opts = baseOptions();
    // A doughnut has no x/y axes; leaving the scales in place makes Chart.js
    // warn on every render.
    delete opts.scales;
    opts.plugins.legend.position = "bottom";
    opts.cutout = "58%";
    // A slice is far more informative with its share of the total alongside the
    // amount, and the total is only known here.
    opts.plugins.tooltip.callbacks.label = function (ctx) {
      var total = ctx.dataset.data.reduce(function (a, b) { return a + Number(b); }, 0);
      var pct = total > 0 ? Math.round((Number(ctx.parsed) / total) * 100) : 0;
      return ctx.label + ": " + money(ctx.parsed) + " (" + pct + "%)";
    };

    new Chart(el, {
      type: "doughnut",
      data: {
        labels: labels,
        datasets: [{
          data: values,
          backgroundColor: labels.map(function (_, i) { return palette[i % palette.length]; }),
          borderColor: cssVar("--surface", "#fff"),
          borderWidth: 2,
          hoverOffset: 6
        }]
      },
      options: opts
    });
  }

  function monthlyChart(el) {
    var labels = readJSON(el, "data-labels");
    var income = readJSON(el, "data-income");
    var expense = readJSON(el, "data-expense");
    if (!labels || !income || !expense) return;

    new Chart(el, {
      type: "bar",
      data: {
        labels: labels,
        datasets: [
          { label: "Income",  data: income,  backgroundColor: cssVar("--positive", "#15803d") },
          { label: "Spending", data: expense, backgroundColor: cssVar("--negative", "#b91c1c") }
        ]
      },
      options: baseOptions()
    });
  }

  function initCharts() {
    if (typeof Chart === "undefined") {
      // The CDN is unreachable. The tables beside each chart still carry the
      // numbers, so this is a degraded page rather than a broken one.
      console.warn("Chart.js did not load; charts are unavailable.");
      return;
    }

    var balance = document.getElementById("chart-balance");
    if (balance) lineChart(balance);

    ["chart-spend", "chart-income", "chart-essential", "chart-entry"].forEach(function (id) {
      var el = document.getElementById(id);
      if (el) pieChart(el);
    });

    var monthly = document.getElementById("chart-monthly");
    if (monthly) monthlyChart(monthly);
  }

  /* A recurring expense whose cost varies has its amount derived from the
   * transactions recorded against it, so the amount field does not apply. It is
   * disabled rather than hidden, which stops the form jumping as you switch. */
  function initBucketKind() {
    var select = document.querySelector("[data-bucket-kind]");
    var amountField = document.querySelector("[data-bucket-amount]");
    if (!select || !amountField) return;

    var input = amountField.querySelector("input");

    function apply() {
      var variable = select.value === "variable";
      amountField.classList.toggle("is-muted", variable);
      if (input) {
        input.disabled = variable;
        input.required = !variable;
      }
    }

    select.addEventListener("change", apply);
    apply();
  }

  function init() {
    document.querySelectorAll("[data-tabs]").forEach(initTabs);
    initConfirms();
    initBucketKind();
    initCharts();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
