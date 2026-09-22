// A file dropped on the library page is handed to the hidden upload form and
// submitted natively, so the answer is the no-JS upload's own: a redirect to
// the preview, or the Import page carrying the refusal. Nothing here talks to
// the server itself.
(function () {
  "use strict";

  var root = document.querySelector("[data-import-drop]");
  if (!root) return;

  var form = root.querySelector("form");
  var input = form.querySelector("input[type=file]");
  var overlay = root.querySelector("[data-drop-overlay]");
  var maxBytes = Number(root.dataset.maxBytes);
  // dragenter and dragleave fire for every child crossed, so only the count
  // returning to zero means the drag has left the page
  var depth = 0;
  var uploading = false;

  function carriesFiles(event) {
    var types = event.dataTransfer && event.dataTransfer.types;
    return !!types && Array.prototype.indexOf.call(types, "Files") >= 0;
  }

  function showState(name) {
    overlay.querySelectorAll("[data-drop-state]").forEach(function (el) {
      el.hidden = el.dataset.dropState !== name;
    });
  }

  function showRefusal(name) {
    root.querySelectorAll("[data-drop-refusal]").forEach(function (el) {
      el.hidden = el.dataset.dropRefusal !== name;
    });
  }

  function reset() {
    depth = 0;
    uploading = false;
    overlay.hidden = true;
    showState("ready");
    showRefusal(null);
    input.value = "";
  }

  // A folder arrives as one File, so the count alone would upload it
  function isSingleFile(dataTransfer) {
    if (dataTransfer.files.length !== 1) return false;
    var item = dataTransfer.items && dataTransfer.items[0];
    var entry = item && item.webkitGetAsEntry && item.webkitGetAsEntry();
    return !(entry && entry.isDirectory);
  }

  document.addEventListener("dragenter", function (event) {
    if (!carriesFiles(event) || uploading) return;
    depth++;
    showRefusal(null);
    overlay.hidden = false;
  });

  document.addEventListener("dragleave", function (event) {
    if (!carriesFiles(event) || uploading) return;
    depth = Math.max(0, depth - 1);
    if (depth === 0) overlay.hidden = true;
  });

  document.addEventListener("dragover", function (event) {
    if (!carriesFiles(event)) return;
    event.preventDefault();
    event.dataTransfer.dropEffect = uploading ? "none" : "copy";
  });

  // Shared with a pasted file, so both gestures keep one set of refusals
  function submitFiles(dataTransfer) {
    depth = 0;
    showRefusal(null);
    if (!isSingleFile(dataTransfer)) {
      overlay.hidden = true;
      showRefusal("not-one");
      return;
    }
    // Stopped here rather than by the server, whose refusal of a body far
    // past the cap can be lost while the rest of it is still in flight
    if (dataTransfer.files[0].size > maxBytes) {
      overlay.hidden = true;
      showRefusal("too-large");
      return;
    }

    input.files = dataTransfer.files;
    uploading = true;
    overlay.hidden = false;
    showState("uploading");
    form.submit();
  }

  // Always prevented for a file drag, so a refused drop never navigates the
  // tab to the file itself
  document.addEventListener("drop", function (event) {
    if (!carriesFiles(event)) return;
    event.preventDefault();
    if (uploading) return;
    submitFiles(event.dataTransfer);
  });

  // paste-link.js hands a pasted file here, and holds drops off while a
  // pasted link is on its way to the server
  window.importDrop = {
    submitFiles: submitFiles,
    busy: function () {
      return uploading;
    },
    hold: function () {
      uploading = true;
    },
    release: reset,
  };

  // Back onto a page restored from the bfcache would otherwise still say
  // it is uploading
  window.addEventListener("pageshow", function (event) {
    if (event.persisted) reset();
  });

  // A submit the person cancels — Escape, or Stop, while the body is still
  // going out — aborts the navigation without unloading the document, so no
  // pageshow follows it and the veil would stay up with every later drop
  // locked out behind `uploading`. Escape is the way back from either, since
  // it is a keypress on the page rather than an answer to the abort
  document.addEventListener("keydown", function (event) {
    if (uploading && event.key === "Escape") reset();
  });
})();
