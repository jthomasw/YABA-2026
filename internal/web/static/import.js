/* Import chooser.
 *
 * Reveals the upload panel when "Receipt Picture" is chosen. Without JavaScript
 * the panel is still in the document, so removing the hidden attribute is the
 * only thing this adds — the form itself is a plain POST and works regardless.
 */
(function () {
  "use strict";

  var panel = document.getElementById("receipt-panel");
  if (!panel) return;

  // The Upload choice is now a .choice button on /expense rather than a card on
  // a separate /import page, but the data- hooks are unchanged so this still
  // finds it.
  var open = document.querySelector("[data-open-receipt]");
  var close = document.querySelector("[data-close-receipt]");
  var input = document.getElementById("receipt");

  if (open) {
    open.addEventListener("click", function () {
      panel.hidden = false;
      open.setAttribute("aria-expanded", "true");
      panel.scrollIntoView({ behavior: "smooth", block: "nearest" });
      // Move focus into the panel that just appeared, so a keyboard user is not
      // left where the button used to be.
      if (input) input.focus();
    });
    open.setAttribute("aria-expanded", "false");
    open.setAttribute("aria-controls", "receipt-panel");
  }

  if (close) {
    close.addEventListener("click", function () {
      panel.hidden = true;
      if (open) {
        open.setAttribute("aria-expanded", "false");
        open.focus();
      }
    });
  }
})();
