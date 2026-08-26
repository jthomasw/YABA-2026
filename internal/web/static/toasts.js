/* Gentle notifications.
 *
 * The wireframe: "a gentle notification is given (gentle meaning it pops up,
 * lives for a few seconds, then makes itself go away)". And, for the case where
 * the user was not around: "If user is not logged in and there is an issue with
 * processing, then some other notification should be given, perhaps when they
 * log back in again."
 *
 * Both are the same mechanism. Messages live in a database table, so one written
 * while the user was signed out is simply still unseen when they return; this
 * script collects whatever is waiting and shows it.
 */
(function () {
  "use strict";

  var HOST = document.getElementById("toasts");
  if (!HOST) return;

  var LIFETIME = 6000;   // long enough to read, short enough not to nag
  var IDLE = 20000;      // polling interval when nothing is in flight
  var BUSY = 4000;       // faster while a receipt is being processed

  var timer = null;
  var stopped = false;

  function toast(kind, text, link) {
    var el = document.createElement("div");
    el.className = "toast toast-" + (kind || "info");
    el.setAttribute("role", kind === "error" ? "alert" : "status");

    var msg = document.createElement("span");
    msg.className = "toast-text";
    // textContent, not innerHTML: the message can contain a filename the user
    // chose, and that must never be parsed as markup.
    msg.textContent = text;
    el.appendChild(msg);

    if (link) {
      var a = document.createElement("a");
      a.className = "toast-link";
      a.href = link;
      a.textContent = "Open";
      el.appendChild(a);
    }

    var close = document.createElement("button");
    close.type = "button";
    close.className = "toast-close";
    close.setAttribute("aria-label", "Dismiss");
    close.textContent = "×";
    close.addEventListener("click", function () { remove(el); });
    el.appendChild(close);

    HOST.appendChild(el);

    // An error stays until dismissed. Auto-hiding the one message the user must
    // act on would defeat the requirement that they always find out.
    if (kind !== "error") {
      setTimeout(function () { remove(el); }, LIFETIME);
    }
  }

  function remove(el) {
    if (!el.parentNode) return;
    el.classList.add("is-leaving");
    // Let the fade finish before removing, but do not depend on the event:
    // prefers-reduced-motion disables the transition, and transitionend would
    // then never fire, leaving the toast on screen forever.
    setTimeout(function () {
      if (el.parentNode) el.parentNode.removeChild(el);
    }, 300);
  }

  function schedule(ms) {
    if (stopped) return;
    clearTimeout(timer);
    timer = setTimeout(poll, ms);
  }

  function poll() {
    if (document.hidden) {
      // No point polling a tab nobody is looking at; the visibility handler
      // below picks it straight back up.
      schedule(IDLE);
      return;
    }

    fetch("/notifications", {
      credentials: "same-origin",
      headers: { "Accept": "application/json" }
    })
      .then(function (res) {
        if (res.status === 303 || res.status === 401 || res.status === 403) {
          // Signed out. Stop rather than hammering the endpoint.
          stopped = true;
          return null;
        }
        if (!res.ok) throw new Error("status " + res.status);
        return res.json();
      })
      .then(function (data) {
        if (!data) return;
        (data.notifications || []).forEach(function (n) {
          toast(n.kind, n.text, n.link);
        });
        schedule(data.pending > 0 ? BUSY : IDLE);
      })
      .catch(function () {
        // A transient failure should back off, not give up: the pending receipt
        // this is waiting for still needs reporting.
        schedule(IDLE * 2);
      });
  }

  document.addEventListener("visibilitychange", function () {
    if (!document.hidden) schedule(500);
  });

  // First poll shortly after load, so anything that finished while the user was
  // away appears as soon as they arrive.
  schedule(1200);
})();
