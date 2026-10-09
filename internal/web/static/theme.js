/* Applies a remembered light/dark override before the stylesheet paints
 * anything, so a user who has picked one does not see the system default flash
 * first. Loaded synchronously from <head> on purpose: a deferred file would run
 * too late. It lives in a file, not inline, so the Content-Security-Policy can
 * forbid inline scripts. See initThemeToggle in ui.js for the other half.
 */
(function () {
  try {
    var t = localStorage.getItem("yaba-theme");
    if (t === "light" || t === "dark") {
      document.documentElement.setAttribute("data-theme", t);
    }
  } catch (e) {}
})();
