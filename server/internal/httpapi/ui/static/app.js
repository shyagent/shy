(function () {
  var csrf = document.querySelector('meta[name="csrf"]');
  if (csrf && csrf.content) {
    document.body.addEventListener("htmx:configRequest", function (ev) {
      if (ev.detail.verb && ev.detail.verb !== "get") {
        ev.detail.headers["X-Shy-CSRF"] = csrf.content;
      }
    });
  }

  document.addEventListener("submit", function (ev) {
    var form = ev.target;
    var msg = form && form.getAttribute && form.getAttribute("data-confirm");
    if (msg && !window.confirm(msg)) ev.preventDefault();
  });

  document.addEventListener("click", function (ev) {
    var btn = ev.target.closest("[data-copy]");
    if (!btn) return;
    var text = btn.getAttribute("data-copy") || "";
    var prev = btn.textContent;
    var done = function () {
      btn.textContent = "Copied";
      setTimeout(function () { btn.textContent = prev; }, 1200);
    };
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).then(done, function () {
        fallback(text);
        done();
      });
      return;
    }
    fallback(text);
    done();
  });

  function fallback(text) {
    var ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "");
    ta.style.position = "fixed";
    ta.style.left = "-9999px";
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand("copy"); } catch (e) {}
    ta.remove();
  }

  var number = document.querySelector('input[name="number"]');
  if (number) {
    number.addEventListener("input", function () {
      var digits = number.value.replace(/\D/g, "").slice(0, 16);
      var parts = digits.match(/.{1,4}/g);
      number.value = parts ? parts.join(" ") : "";
    });
  }
})();
