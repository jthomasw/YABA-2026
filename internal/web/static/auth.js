/* Login page: reveal the password.
 *
 * Progressive enhancement — the button is in the markup but does nothing without
 * this file, and the form works either way. The type swap is all that is needed;
 * no value is ever copied to another element, so the password never leaves the
 * input it was typed into.
 */
(function () {
  "use strict";

  function initReveal() {
    document.querySelectorAll("[data-reveal]").forEach(function (btn) {
      var input = document.getElementById(btn.getAttribute("data-reveal"));
      if (!input) return;

      var use = btn.querySelector("use");

      btn.addEventListener("click", function () {
        var showing = input.type === "text";
        input.type = showing ? "password" : "text";

        btn.setAttribute("aria-pressed", showing ? "false" : "true");
        btn.setAttribute("aria-label", showing ? "Show password" : "Hide password");
        if (use) use.setAttribute("href", showing ? "#i-eye" : "#i-eye-off");

        // Keep the caret where it was: switching type moves it to the end in
        // some browsers, which is jarring mid-word.
        var pos = input.value.length;
        input.focus();
        try { input.setSelectionRange(pos, pos); } catch (e) { /* not supported */ }
      });
    });
  }

  function init() {
    initReveal();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
