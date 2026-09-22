// A link pasted on the library page is confirmed in a dialog and posted
// natively to /import/url, so the answer is the Import page's own: a redirect
// to the preview, or the page carrying the refusal. A pasted file is the
// drop's, whose preview is the confirmation
(function () {
  "use strict";

  var dialog = document.querySelector("[data-paste-link]");
  var drop = window.importDrop;
  if (!dialog || !drop) return;

  var form = dialog.querySelector("form");
  var input = form.querySelector("input[name=url]");
  var host = dialog.querySelector("[data-paste-host]");
  var importButton = dialog.querySelector("[data-paste-import]");
  var downloading = dialog.querySelector("[data-paste-downloading]");
  var button = document.querySelector("[data-paste-button]");

  function parseLink(text) {
    text = text.trim();
    if (!text || /\s/.test(text)) return null;
    try {
      var url = new URL(text);
      return url.protocol === "http:" || url.protocol === "https:" ? url : null;
    } catch (_) {
      return null;
    }
  }

  // A copied image rides on the clipboard as a file too, and handing it to
  // the drop would navigate to a refusal for a paste nobody meant as an
  // import. Anything else goes through: the server decides by content, and
  // an FB2 archive can arrive as a plain .zip
  function mayBeABook(file) {
    return !/^image\//i.test(file.type);
  }

  // The host line is the one part of the question worth reading, so it
  // follows the input as the link is edited
  function showHost() {
    var url = parseLink(input.value);
    host.textContent = url ? url.host : "";
    host.parentElement.hidden = !url;
  }

  // Only the dialog's own UI: drop.js lets go of its hold itself on Escape
  // and on a bfcache restore
  function reset() {
    downloading.hidden = true;
    importButton.removeAttribute("aria-disabled");
    if (button) button.removeAttribute("aria-busy");
  }

  function openDialog(link) {
    if (drop.busy()) return;
    input.value = link;
    showHost();
    if (!dialog.open) dialog.showModal();
    (link ? importButton : input).focus();
  }

  function isEditable(target) {
    return (
      target instanceof Element &&
      (target.isContentEditable || target.closest("input, textarea, [contenteditable]") !== null)
    );
  }

  document.addEventListener("paste", function (event) {
    if (isEditable(event.target) || drop.busy()) return;
    var clipboard = event.clipboardData;
    if (!clipboard) return;

    var files = clipboard.files ? Array.prototype.slice.call(clipboard.files) : [];
    if (files.some(mayBeABook)) {
      event.preventDefault();
      drop.submitFiles(clipboard);
      return;
    }
    var url = parseLink(clipboard.getData("text/plain"));
    if (!url) return;
    event.preventDefault();
    openDialog(url.href);
  });

  input.addEventListener("input", showHost);

  // A download under way is stopped rather than left to land on the preview
  // behind a dialog that looked cancelled. The dialog's own downloading line
  // is the test, not drop.busy(): drop.js lets go of its hold on the Escape
  // keydown, which fires before the dialog's cancel event
  function abort() {
    if (!downloading.hidden) {
      window.stop();
      drop.release();
    }
    reset();
  }

  // close() fires no cancel event, so the button aborts for itself
  dialog.querySelector("[data-paste-cancel]").addEventListener("click", function () {
    abort();
    dialog.close();
  });

  // Validation has passed by the time submit fires. A second Enter while the
  // download is under way would start a second one the server refuses as busy
  form.addEventListener("submit", function (event) {
    event.preventDefault();
    if (drop.busy()) return;
    drop.hold();
    downloading.hidden = false;
    importButton.setAttribute("aria-disabled", "true");
    if (button) button.setAttribute("aria-busy", "true");
    form.submit();
  });

  // Escape and a platform close gesture both arrive here, and neither
  // unloads the page, so no pageshow follows to clean up after them
  dialog.addEventListener("cancel", abort);

  window.addEventListener("pageshow", function (event) {
    if (!event.persisted) return;
    reset();
    if (dialog.open) dialog.close();
  });

  if (!button) return;

  // readText() is refused outside a secure context and whenever the person
  // declines, and neither is worth more than an empty dialog to type into
  button.addEventListener("click", function () {
    if (drop.busy()) return;
    var read =
      navigator.clipboard && navigator.clipboard.readText
        ? navigator.clipboard.readText()
        : Promise.reject(new Error("clipboard unavailable"));
    read.then(
      function (text) {
        var url = parseLink(text);
        openDialog(url ? url.href : "");
      },
      function () {
        openDialog("");
      },
    );
  });

  var platform = (navigator.userAgentData && navigator.userAgentData.platform) || navigator.platform || "";
  button.querySelector("[data-paste-hint]").textContent = /mac|iphone|ipad|ipod/i.test(platform) ? "⌘V" : "Ctrl+V";
  button.hidden = false;
})();
