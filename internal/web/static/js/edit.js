(function () {
  "use strict";

  document.addEventListener("keydown", function (event) {
    var form = event.target.closest("[data-editable-field] form");
    if (!form) return;

    if (event.key === "Escape") {
      event.preventDefault();
      var cancel = form.querySelector("[data-edit-cancel]");
      if (cancel) cancel.click();
      return;
    }

    // Cancel is an <a> and Save is a <button>, both inside the form, so
    // both would otherwise be caught by the Enter-saves rule below —
    // making a keyboard Cancel save the edit it was meant to discard.
    // Let the browser activate them itself.
    if (event.key === "Enter" && event.target.closest("[data-edit-cancel], button")) return;

    if (event.key === "Enter" && event.target.tagName !== "TEXTAREA") {
      event.preventDefault();
      form.requestSubmit();
      return;
    }

    if (event.key === "Enter" && event.target.tagName === "TEXTAREA" && (event.metaKey || event.ctrlKey)) {
      event.preventDefault();
      form.requestSubmit();
    }
  });

  document.addEventListener("htmx:afterSwap", function (event) {
    var target = event.detail.target;
    var field = target.id ? document.getElementById(target.id) : target.closest("[data-editable-field]");
    if (!field) return;

    var autofocus = field.querySelector("[autofocus]");
    if (autofocus) {
      autofocus.focus();
      if (autofocus.select) autofocus.select();
      return;
    }

    var read = field.querySelector(".editable__read");
    if (read) read.focus();
    if (field.dataset.field === "title") {
      var title = field.querySelector(".detail__title span");
      if (title) document.title = title.textContent + " · Bookshelf";
    }
  });
})();
