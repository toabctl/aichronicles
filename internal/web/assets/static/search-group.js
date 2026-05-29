// Client-side rendering for the full /search page.
//
// The server returns a flat, data-only fragment (.search-results with
// .search-hit children, see _hits.html) into the hidden #hits-raw
// sink. This script reads those rows and renders the user-facing view
// into #hits: results grouped by session, the first few matches per
// session shown inline with the rest behind a <details> expander, and
// a "Load more" button when the API reported another page.
//
// Why client-side: it keeps the search API a clean, paginated, flat
// endpoint while grouping/capping/paging stay presentation concerns.
// Resume one-liners and the summary topic ride on each row as data
// attributes (the server resolves them; the browser can't), so the
// grouper builds each session header without another round-trip.
//
// Degrades gracefully: with JS off, #hits keeps its placeholder and
// #hits-raw stays hidden — no broken layout, just no grouped view.
//
// Tiny, no framework. The copy-to-clipboard behaviour for the resume
// buttons is provided by keynav.js's delegated .resume-btn handler, so
// the buttons built here work without extra wiring.
(function () {
  // hitsPerSessionPreview caps inline matches per session; the rest go
  // behind the expander so a match-heavy session can't crowd out
  // others. Mirrors the server-side constant the API plan retired.
  var hitsPerSessionPreview = 3;

  function el(tag, className) {
    var node = document.createElement(tag);
    if (className) {
      node.className = className;
    }
    return node;
  }

  function emptyLine(text) {
    var p = el('p', 'empty');
    p.textContent = text;
    return p;
  }

  // resumeButton builds one click-to-copy icon button. data-resume-cmd
  // + class .resume-btn are what keynav.js's delegated handler keys on.
  function resumeButton(cmd, dangerous) {
    var b = el('button', 'resume-btn resume-btn-icon' + (dangerous ? ' resume-btn-dangerous' : ''));
    b.type = 'button';
    b.setAttribute('data-resume-cmd', cmd);
    b.title = dangerous
      ? 'Copy `' + cmd + '` to clipboard — resume with --dangerously-skip-permissions'
      : 'Copy `' + cmd + '` to clipboard so you can paste it in a terminal to resume the agent';
    b.textContent = dangerous ? '⚡' : '↻';
    return b;
  }

  // hitRow renders one match as a table row: relative time, kind badge,
  // snippet. Snippet is set via textContent so it is never interpreted
  // as HTML.
  function hitRow(hit) {
    var tr = el('tr');
    var ts = el('td', 'ts');
    ts.textContent = hit.dataset.when || '';
    tr.appendChild(ts);

    var kindCell = el('td');
    var badge = el('span', 'badge');
    if (hit.dataset.kind) {
      badge.classList.add('badge-' + hit.dataset.kind);
    }
    badge.textContent = hit.dataset.kind || '';
    kindCell.appendChild(badge);
    tr.appendChild(kindCell);

    var snippet = el('td');
    snippet.textContent = hit.dataset.snippet || '';
    tr.appendChild(snippet);
    return tr;
  }

  function tableOf(hits) {
    var table = el('table');
    var tbody = el('tbody');
    hits.forEach(function (h) {
      tbody.appendChild(hitRow(h));
    });
    table.appendChild(tbody);
    return table;
  }

  // buildGroup renders one session: a header (short-id link, topic,
  // resume buttons) plus its matches — first N inline, the rest behind
  // an expander. The header carries class nav-row so keynav.js's
  // ↑/↓/Enter navigation treats the session as the unit (Enter opens
  // it via the first <a href> inside, the session link).
  function buildGroup(sessionID, hits) {
    var first = hits[0];
    var section = el('section', 'search-group');

    var header = el('header', 'search-group-head nav-row');
    header.tabIndex = 0;

    var sid = el('span', 'sid');
    var link = el('a');
    link.href = '/sessions/' + sessionID;
    link.textContent = first.dataset.shortId || sessionID;
    sid.appendChild(link);
    header.appendChild(sid);

    if (first.dataset.topic) {
      var topic = el('small', 'topic');
      topic.textContent = first.dataset.topic;
      header.appendChild(topic);
    }

    var actions = el('span', 'row-actions');
    if (first.dataset.resumeCmd) {
      actions.appendChild(resumeButton(first.dataset.resumeCmd, false));
    }
    if (first.dataset.resumeCmdDangerous) {
      actions.appendChild(resumeButton(first.dataset.resumeCmdDangerous, true));
    }
    header.appendChild(actions);
    section.appendChild(header);

    section.appendChild(tableOf(hits.slice(0, hitsPerSessionPreview)));

    if (hits.length > hitsPerSessionPreview) {
      var rest = hits.slice(hitsPerSessionPreview);
      var details = el('details', 'search-more');
      var summary = el('summary');
      summary.textContent = rest.length + ' more in this session';
      details.appendChild(summary);
      details.appendChild(tableOf(rest));
      section.appendChild(details);
    }
    return section;
  }

  function buildLoadMore(cursor) {
    var b = el('button', 'resume-btn search-loadmore');
    b.type = 'button';
    b.textContent = 'Load more';
    b.addEventListener('click', function () {
      b.disabled = true;
      b.textContent = 'Loading…';
      loadMore(cursor, b);
    });
    return b;
  }

  // loadMore fetches the next page and appends it to #hits-raw, then
  // re-renders. The query + filters come from the search form (the
  // same inputs htmx includes on the initial search); we only add the
  // cursor. On failure the button is restored so the user can retry.
  function loadMore(cursor, button) {
    var form = document.querySelector('main form');
    var params = form ? new URLSearchParams(new FormData(form)) : new URLSearchParams();
    params.set('cursor', cursor);
    fetch('/search/hits?' + params.toString(), { headers: { 'HX-Request': 'true' } })
      .then(function (resp) {
        if (!resp.ok) {
          throw new Error('load more: ' + resp.status);
        }
        return resp.text();
      })
      .then(function (html) {
        var raw = document.getElementById('hits-raw');
        if (raw) {
          raw.insertAdjacentHTML('beforeend', html);
        }
        render();
      })
      .catch(function () {
        button.disabled = false;
        button.textContent = 'Load more (retry)';
      });
  }

  // render rebuilds #hits from every .search-hit currently in
  // #hits-raw. Idempotent: a new search (innerHTML swap) and a "load
  // more" (append) both end here and produce the full grouped view
  // from whatever rows are present.
  function render() {
    var raw = document.getElementById('hits-raw');
    var out = document.getElementById('hits');
    if (!raw || !out) {
      return;
    }
    var fragments = raw.querySelectorAll('.search-results');
    if (fragments.length === 0) {
      return; // nothing fetched yet — keep the initial placeholder
    }
    var last = fragments[fragments.length - 1]; // newest page = current state
    var hits = raw.querySelectorAll('.search-hit');

    out.innerHTML = '';

    if (last.dataset.error) {
      out.appendChild(emptyLine(last.dataset.error));
      return;
    }
    if (hits.length === 0) {
      var q = (last.dataset.query || '').trim();
      out.appendChild(emptyLine(q === ''
        ? '(start typing — results appear here as you go)'
        : '(no hits for that query)'));
      return;
    }

    // Group by session in first-seen (rank) order.
    var order = [];
    var bySession = Object.create(null);
    hits.forEach(function (h) {
      var sid = h.dataset.sessionId;
      if (!bySession[sid]) {
        bySession[sid] = [];
        order.push(sid);
      }
      bySession[sid].push(h);
    });
    order.forEach(function (sid) {
      out.appendChild(buildGroup(sid, bySession[sid]));
    });

    if (last.dataset.nextCursor) {
      out.appendChild(buildLoadMore(last.dataset.nextCursor));
    }
  }

  // Re-render whenever htmx swaps a page into the raw sink (initial
  // search and the autofocus `load` trigger both land here).
  document.addEventListener('htmx:afterSwap', function (e) {
    if (e.target && e.target.id === 'hits-raw') {
      render();
    }
  });
})();
