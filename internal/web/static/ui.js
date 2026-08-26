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

  function init() {
    initConfirmDialogs();
    initHelp();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
