// The one-line answer after an action. A button posts with htmx and swaps
// nothing; the server's JSON carries a `message`, or an `error.message`, and
// this shows it in the page's #action-result notice. Delegated on the
// document, so it keeps working after a polled fragment replaces its markup.
(function () {
  function say(text) {
    var node = document.getElementById("action-result");
    if (!node) return;
    node.textContent = text;
    node.hidden = !text;
  }

  document.addEventListener("htmx:afterRequest", function (event) {
    var xhr = event.detail.xhr;
    if (!xhr || event.detail.requestConfig.verb !== "post") return;
    var body = {};
    try { body = JSON.parse(xhr.responseText); } catch (ignored) { /* not JSON */ }
    if (event.detail.successful) {
      say(body.message || "Done.");
    } else {
      say((body.error && body.error.message) || "The request failed (" + xhr.status + ").");
    }
  });
})();
