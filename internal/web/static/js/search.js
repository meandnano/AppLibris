// Focus the search input on "/" — plate 01 draws the hint box next to the
// input, and the hint stays hidden until this runs, so it never advertises
// a key that does nothing (with JS off, or with the control dimmed because
// the library is empty).
(function () {
  var input = document.querySelector("[data-search-input]");
  if (!input || input.disabled) return;

  var hint = document.querySelector("[data-search-shortcut]");
  if (hint) hint.hidden = false;

  document.addEventListener("keydown", function (event) {
    if (event.key !== "/" || event.ctrlKey || event.metaKey || event.altKey) return;

    var target = event.target;
    if (target && (target.isContentEditable ||
        target.tagName === "INPUT" || target.tagName === "TEXTAREA" || target.tagName === "SELECT")) {
      return;
    }

    // Only prevent the browser's own quick-find once we're sure we're
    // taking the key.
    event.preventDefault();
    input.focus();
    input.select();
  });
})();
