// Keyboard navigation for aichronicles' web UI.
//
// Behaviour:
//   - "/" anywhere outside an input → focus the nav search box.
//   - "Esc" inside an input → blur it (also clears the popover via
//     :focus-within hide rules in app.css).
//   - "ArrowDown" / "ArrowUp" outside an input → move focus among
//     elements with class="nav-row" (rendered on rows in the
//     sessions list, the events table, and search hits). Wraps
//     to first/last at the edges.
//   - "Enter" on a focused .nav-row → click the first <a href>
//     inside it (the row's primary link).
//
// Tiny on purpose. No framework, no dependencies. Lives outside
// htmx-ext-sse so it keeps working when the SSE connection
// reconnects or the page rerenders. IIFE so its helpers don't
// pollute window.
(function () {
  function listRows() {
    return document.querySelectorAll('.nav-row');
  }

  function focusedRow() {
    var el = document.activeElement;
    return el && el.classList && el.classList.contains('nav-row') ? el : null;
  }

  function moveFocus(delta) {
    var rows = listRows();
    if (rows.length === 0) return;
    var cur = focusedRow();
    var idx = cur ? Array.prototype.indexOf.call(rows, cur) : -1;
    if (idx === -1) {
      // Nothing focused yet — first ArrowDown lands on row 0,
      // first ArrowUp lands on the last row.
      idx = delta > 0 ? 0 : rows.length - 1;
    } else {
      idx += delta;
      if (idx < 0) idx = 0;
      if (idx >= rows.length) idx = rows.length - 1;
    }
    rows[idx].focus();
  }

  function inEditableField(target) {
    if (!target) return false;
    var tag = (target.tagName || '').toLowerCase();
    if (tag === 'input' || tag === 'textarea' || tag === 'select') return true;
    return !!target.isContentEditable;
  }

  document.addEventListener('keydown', function (e) {
    var inInput = inEditableField(e.target);

    // "/" focuses the nav search. Only when the user isn't already
    // typing into something (otherwise pressing slash inside a
    // prompt would yank focus away).
    if (e.key === '/' && !inInput && !e.ctrlKey && !e.metaKey && !e.altKey) {
      var input = document.querySelector('.navsearch input[type=search]');
      if (input) {
        e.preventDefault();
        input.focus();
        input.select();
      }
      return;
    }

    // "Esc" inside an editable field exits it. The popover hide
    // CSS is :focus-within-driven, so blurring closes it.
    if (e.key === 'Escape' && inInput) {
      e.target.blur();
      return;
    }

    // Arrow / Enter are only navigation triggers when the user
    // isn't typing — inside an input, those keys edit text.
    if (inInput) return;

    if (e.key === 'ArrowDown') {
      e.preventDefault();
      moveFocus(+1);
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      moveFocus(-1);
    } else if (e.key === 'Enter') {
      var row = focusedRow();
      if (!row) return;
      // First <a href> wins — every nav-row carries the canonical
      // detail link as its first anchor (sessions list → /sessions/<id>,
      // hits table → /sessions/<id>, events table cells use the
      // session link in the surrounding row).
      var link = row.querySelector('a[href]');
      if (link) {
        e.preventDefault();
        link.click();
      }
    }
  });
})();
