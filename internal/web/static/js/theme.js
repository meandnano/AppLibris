// Light/dark toggle. The preference is remembered in localStorage; with no
// stored preference the CSS follows prefers-color-scheme on its own.
(function () {
  var root = document.documentElement;
  var button = document.querySelector("[data-theme-toggle]");
  if (!button) return;

  function current() {
    var set = root.getAttribute("data-theme");
    if (set) return set;
    return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
  }

  function relabel() {
    button.textContent = current() === "dark" ? "Light mode" : "Dark mode";
  }

  relabel();

  button.addEventListener("click", function () {
    var next = current() === "dark" ? "light" : "dark";
    root.setAttribute("data-theme", next);
    try {
      localStorage.setItem("theme", next);
    } catch (e) {}
    relabel();
  });
})();
