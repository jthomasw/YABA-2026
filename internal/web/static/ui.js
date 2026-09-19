/* Shared interface behaviour: confirmation dialogs and contextual help.
 *
 * Both are progressive enhancements. Without this file the log-out button still
 * logs you out (it is a plain form POST) and the help button simply does nothing.
 */
(function () {
  "use strict";

  /* ── confirmation dialogs ───────────────────────────────────────────────
   * A form marked data-confirm-dialog="some-id" has its submit intercepted and
   * the matching <dialog> shown instead. Confirming calls form.submit(), which
   * deliberately does NOT fire another submit event — so the handler cannot
   * recurse and the form posts exactly once.
   */
  function initConfirmDialogs() {
    document.querySelectorAll("form[data-confirm-dialog]").forEach(function (form) {
      var dialog = document.getElementById(form.getAttribute("data-confirm-dialog"));
      if (!dialog || typeof dialog.showModal !== "function") return;

      form.addEventListener("submit", function (e) {
        e.preventDefault();
        dialog.showModal();
      });

      dialog.querySelectorAll("[data-modal-close]").forEach(function (btn) {
        btn.addEventListener("click", function () { dialog.close(); });
      });

      var confirm = dialog.querySelector("[data-modal-confirm]");
      if (confirm) {
        confirm.addEventListener("click", function () {
          dialog.close();
          form.submit();
        });
      }

      // Clicking the backdrop closes it. The dialog element itself fills only the
      // panel, so a click whose target IS the dialog landed outside the content.
      dialog.addEventListener("click", function (e) {
        if (e.target === dialog) dialog.close();
      });
    });
  }

  /* ── contextual help ────────────────────────────────────────────────────
   * The panel holds every topic for the page; only the one matching the current
   * context is shown. On the dashboard that context is the selected tab, so the
   * same button explains Current Funds or Emergency Fund depending on where you
   * are — which is the whole point of it being contextual.
   */
  function initHelp() {
    var panel = document.getElementById("help-panel");
    var open = document.querySelector("[data-help-open]");
    if (!panel || !open || typeof panel.showModal !== "function") return;

    var titleEl = panel.querySelector("[data-help-title]");
    var topics = panel.querySelectorAll(".help-topic");
    if (!topics.length) {
      // Nothing to say about this page: hide the button rather than offering an
      // empty panel.
      open.hidden = true;
      return;
    }

    function currentTopicKey() {
      var tab = document.querySelector('[role="tab"][aria-selected="true"]');
      if (tab) {
        var panelID = tab.getAttribute("aria-controls") || "";
        return panelID.replace(/^panel-/, "");
      }
      return "";
    }

    function show() {
      var key = currentTopicKey();
      var chosen = null;

      topics.forEach(function (t) {
        var match = t.getAttribute("data-topic") === key;
        t.hidden = !match;
        if (match) chosen = t;
      });

      // No keyed match (an ordinary page, or a tab with no topic written for it):
      // fall back to the first topic so the button is never a dead end.
      if (!chosen) {
        topics.forEach(function (t, i) { t.hidden = i !== 0; });
        chosen = topics[0];
      }

      if (titleEl && chosen) {
        var h = chosen.querySelector("h3");
        titleEl.textContent = h ? h.textContent.trim() : "About this page";
      }

      panel.showModal();
    }

    open.addEventListener("click", show);

    panel.querySelectorAll("[data-help-close]").forEach(function (btn) {
      btn.addEventListener("click", function () { panel.close(); });
    });
    panel.addEventListener("click", function (e) {
      if (e.target === panel) panel.close();
    });

    // Returning focus to the button after closing keeps keyboard users where
    // they were; <dialog> restores it for showModal, but not if the panel was
    // closed by the backdrop click above in every browser.
    panel.addEventListener("close", function () { open.focus(); });
  }

  /* ── plain confirmations ─────────────────────────────────────────
   * A form marked data-confirm="..." asks before submitting.
   *
   * This lived in dashboard.js, which only the dashboard and reports pages
   * load -- so on Sharing, Active devices and Transactions the attribute was
   * markup and nothing else. "Permanently delete this budget and everything in
   * it" went through on one click, for every member, with no prompt. ui.js is
   * loaded by the layout on every signed-in page, which is where a guard that
   * every page relies on belongs.
   *
   * Still an enhancement, not the safety mechanism: with scripting off the form
   * posts unconfirmed, exactly as it did before. The server is what enforces
   * who may delete what.
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

  /* ── theme toggle ────────────────────────────────────────────────────────
   * The header button that overrides the system's light/dark preference. The
   * inline script in layout.html's <head> already applied whatever was saved
   * last time, before this file even loaded (that is why this is inline and
   * synchronous, and this one is deferred) -- so on load this only has to
   * read the CURRENT effective theme and make the button's icon and label
   * agree with it, then flip both on click.
   */
  function initThemeToggle() {
    var btn = document.querySelector("[data-theme-toggle]");
    if (!btn) return;

    var STORAGE_KEY = "yaba-theme";
    var darkQuery = window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)");

    function effectiveTheme() {
      var forced = document.documentElement.getAttribute("data-theme");
      if (forced === "light" || forced === "dark") return forced;
      return darkQuery && darkQuery.matches ? "dark" : "light";
    }

    function reflect(theme) {
      var label = theme === "dark" ? "Switch to light mode" : "Switch to dark mode";
      btn.setAttribute("aria-label", label);
      btn.setAttribute("title", label);
      btn.setAttribute("aria-pressed", theme === "dark" ? "true" : "false");
    }

    function apply(theme) {
      document.documentElement.setAttribute("data-theme", theme);
      try {
        localStorage.setItem(STORAGE_KEY, theme);
      } catch (e) {
        // Private browsing, or storage disabled -- the toggle still works for
        // the rest of this page view, it just won't be remembered.
      }
      reflect(theme);
    }

    reflect(effectiveTheme());

    btn.addEventListener("click", function () {
      apply(effectiveTheme() === "dark" ? "light" : "dark");
    });
  }

  function init() {
    initConfirmDialogs();
    initConfirms();
    initHelp();
    initThemeToggle();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
