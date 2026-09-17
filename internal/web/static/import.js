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

  // The panel ships visible and this hides it, rather than the template
  // shipping it hidden and this revealing it. The difference is the whole
  // point: the opener is a <button type="button">, so with scripting off
  // nothing could remove a server-rendered hidden attribute and receipt
  // upload -- one of the two things the page exists to offer -- could not be
  // reached at all.
  panel.hidden = true;

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


/* Receipt progress.
 *
 * Uploading a receipt used to end in a redirect to the dashboard and a promise
 * that a notification would arrive. This keeps the user where they were and
 * shows the work instead: the server reports which stage the worker has reached
 * and the ring eases towards that stage's ceiling.
 *
 * The percentage is a stage, not a measurement. Tesseract reports nothing at all
 * while it runs, so there is no real fraction to draw -- which is why the ring
 * never reaches 100 until the server actually says the receipt is done. A bar
 * that sits full while the work continues is a lie every user catches.
 *
 * Everything here is an upgrade of markup that already says something truthful:
 * with the script blocked the card still reads "Queued" and still links to the
 * receipts page.
 */
(function () {
  "use strict";

  var card = document.querySelector("[data-receipt-progress]");
  if (!card) return;

  var ring = card.querySelector("[data-ring]");
  var figure = card.querySelector("[data-percent]");
  var label = card.querySelector("[data-label]");
  var detail = card.querySelector("[data-detail]");
  var next = card.querySelector("[data-next]");
  var statusURL = card.getAttribute("data-status-url");
  if (!statusURL) return;

  // The circumference of r=52, so stroke-dashoffset can be driven as a fraction.
  var LENGTH = 2 * Math.PI * 52;
  if (ring) {
    ring.style.strokeDasharray = LENGTH.toFixed(1);
    ring.style.strokeDashoffset = LENGTH.toFixed(1);
  }

  var shown = 0;     // what the ring currently displays
  var target = 20;   // what the server last said the stage is worth
  var settled = false;

  // Someone who prefers less motion gets the numbers without the animation.
  var still = window.matchMedia &&
              window.matchMedia("(prefers-reduced-motion: reduce)").matches;

  function paint(pct) {
    if (ring) {
      ring.style.strokeDashoffset = (LENGTH * (1 - pct / 100)).toFixed(1);
    }
    if (figure) figure.textContent = Math.round(pct) + "%";
  }

  /* The ring closes the gap to the target a fraction at a time, so it keeps
     drifting forward between polls instead of sitting still and then jumping.
     It approaches the ceiling without crossing it: waiting at 68% looks like
     work in progress, while waiting at 100% looks broken. */
  function animate() {
    if (still) {
      shown = target;
      paint(shown);
      return;
    }
    shown += (target - shown) * 0.08;
    if (Math.abs(target - shown) < 0.4) shown = target;
    paint(shown);
    if (!settled || shown !== target) requestAnimationFrame(animate);
  }
  paint(shown);
  requestAnimationFrame(animate);

  function finish(data) {
    settled = true;
    target = 100;
    card.classList.add(data.failed ? "is-failed" : "is-done");
    if (label) label.textContent = data.label || "Ready";
    if (detail && data.detail) detail.textContent = data.detail;
    if (next) {
      next.hidden = false;
      if (data.next) next.setAttribute("href", data.next);
      // Focus moves to the thing to do next, so a keyboard user is not left on
      // a card that has finished changing.
      next.focus({ preventScroll: true });
    }
  }

  var delay = 700;
  var attempts = 0;

  function poll() {
    attempts++;
    fetch(statusURL, { headers: { Accept: "application/json" }, credentials: "same-origin" })
      .then(function (r) {
        if (!r.ok) throw new Error("status " + r.status);
        return r.json();
      })
      .then(function (data) {
        if (typeof data.percent === "number") target = data.percent;
        if (label && data.label) label.textContent = data.label;
        if (detail && data.detail && !data.done && !data.failed) {
          detail.textContent = data.detail;
        }
        if (data.done || data.failed) {
          finish(data);
          return;
        }
        // Ease off gradually: a receipt that is taking a while is not helped by
        // being asked about three times a second.
        delay = Math.min(delay * 1.25, 4000);
        setTimeout(poll, delay);
      })
      .catch(function () {
        // A dropped connection is not a failed receipt -- the worker carries on
        // regardless of whether this page is watching. Retry a few times, then
        // say plainly where the result can be found rather than spinning on.
        if (attempts < 8) {
          setTimeout(poll, 2000);
          return;
        }
        settled = true;
        if (label) label.textContent = "Still working";
        if (detail) {
          detail.textContent =
            "We lost contact with the server while watching this receipt. " +
            "It is still being processed — open Receipts to pick it up.";
        }
      });
  }

  setTimeout(poll, 400);
})();
