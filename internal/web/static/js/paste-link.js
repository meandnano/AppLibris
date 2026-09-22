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
  var handingOff = false;

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

  // The host line is the one part of the question worth reading, so it
  // follows the input as the link is edited
  function showHost() {
    var url = parseLink(input.value);
    host.textContent = url ? url.host : "";
    host.parentElement.hidden = !url;
  }

  // A pasted file is the drop's upload, so the button answers for both
  function showBusy() {
    if (!button) return;
    if (handingOff || drop.busy()) button.setAttribute("aria-busy", "true");
    else button.removeAttribute("aria-busy");
  }

  function reset() {
    handingOff = false;
    downloading.hidden = true;
    importButton.removeAttribute("aria-disabled");
    showBusy();
  }

  function openDialog(link) {
    if (handingOff || drop.busy()) return;
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
    if (isEditable(event.target) || handingOff || drop.busy()) return;
    var clipboard = event.clipboardData;
    if (!clipboard) return;

    if (clipboard.files && clipboard.files.length > 0) {
      event.preventDefault();
      drop.submitFiles(clipboard);
      showBusy();
      return;
    }
    var url = parseLink(clipboard.getData("text/plain"));
    if (!url) return;
    event.preventDefault();
    openDialog(url.href);
  });

  input.addEventListener("input", showHost);

  dialog.querySelector("[data-paste-cancel]").addEventListener("click", function () {
    dialog.close();
  });

  // Validation has passed by the time submit fires. A second Enter while the
  // download is under way would start a second one the server refuses as busy
  form.addEventListener("submit", function (event) {
    event.preventDefault();
    if (handingOff) return;
    handingOff = true;
    drop.hold();
    downloading.hidden = false;
    importButton.setAttribute("aria-disabled", "true");
    showBusy();
    form.submit();
  });

  // Escape is the way back from a submit the person cancelled, which aborts
  // the navigation without unloading the page, so no pageshow follows it
  dialog.addEventListener("cancel", function () {
    if (!handingOff) return;
    reset();
    drop.release();
  });

  window.addEventListener("pageshow", function (event) {
    if (!event.persisted) return;
    reset();
    if (dialog.open) dialog.close();
  });

  // drop.js clears its own state on Escape and registered first, so by now
  // a cancelled pasted file has already let go
  document.addEventListener("keydown", function (event) {
    if (event.key === "Escape") showBusy();
  });

  if (!button) return;

  // readText() is refused outside a secure context and whenever the person
  // declines, and neither is worth more than an empty dialog to type into
  button.addEventListener("click", function () {
    if (handingOff || drop.busy()) return;
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
