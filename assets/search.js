/* tcldoc — the only JavaScript on the site: search, and the theme toggle.
 *
 * Two tiers. Names (every page, subcommand and option) load once and answer
 * prefix queries locally. Full-text queries fetch only the index shards for the
 * terms typed, so the payload scales with the query rather than the corpus.
 *
 * Navigation, indexes and every documentation page work with JS disabled; this
 * file only adds search. */

(function () {
  'use strict';

  var ROOT = document.documentElement.getAttribute('data-root') || '.';

  // A page may carry more than one search box — the masthead has one on every
  // page, and the landing page adds a larger one in the hero. They share the
  // loaded index; only the query and selection state are per-box.
  var inputs = document.querySelectorAll('[data-search]');
  if (!inputs.length) return;

  var names = null, docs = null, loading = null;
  var shards = Object.create(null);

  function get(url) {
    return fetch(url).then(function (r) {
      if (!r.ok) throw new Error(r.status + ' ' + url);
      return r.json();
    });
  }

  // Names and the document table are small and always needed together.
  function ready() {
    if (loading) return loading;
    loading = Promise.all([
      get(ROOT + '/names.json'),
      get(ROOT + '/docs.json')
    ]).then(function (r) {
      names = r[0];
      docs = r[1];
    });
    return loading;
  }

  function shardKey(term) {
    var k = term.slice(0, 2).toLowerCase();
    return k.replace(/[^a-z0-9]/g, '_') || '__';
  }

  function shard(key) {
    if (shards[key]) return shards[key];
    shards[key] = get(ROOT + '/idx/' + key + '.json').catch(function () { return {}; });
    return shards[key];
  }

  // Tier 1: substring match over names, ranked by where the match lands.
  // Records reference their page by index, so expand them here.
  function byName(query) {
    var needle = query.toLowerCase(), hits = [];
    for (var i = 0; i < names.length && hits.length < 400; i++) {
      var rec = names[i], pos = rec.n.toLowerCase().indexOf(needle);
      if (pos < 0) continue;
      var doc = docs[rec.d];
      if (!doc) continue;
      var score = 1000 - pos * 12 - (rec.n.length - needle.length);
      if (pos === 0) score += 400;
      if (rec.n.toLowerCase() === needle) score += 1200;
      if (!rec.k) score += 150; // a page name outranks a member of a page
      hits.push({
        n: rec.n,
        u: doc.u + (rec.a ? '#' + rec.a : ''),
        d: rec.k ? doc.t : doc.s, // members show their page, pages show a summary
        k: rec.k,
        s: score
      });
    }
    return hits;
  }

  // Tier 2: intersect postings for every term, sum the precomputed weights.
  function byText(terms) {
    var keys = {};
    terms.forEach(function (t) { keys[shardKey(t)] = 1; });
    return Promise.all(Object.keys(keys).map(shard)).then(function (loaded) {
      var table = {};
      loaded.forEach(function (s) {
        for (var t in s) table[t] = s[t];
      });

      var acc = null;
      for (var i = 0; i < terms.length; i++) {
        var term = terms[i], postings = table[term];
        if (!postings) {
          // Fall back to prefix expansion so partial words still match.
          postings = [];
          for (var t in table) {
            if (t.indexOf(term) === 0) postings = postings.concat(table[t]);
          }
        }
        var round = {};
        postings.forEach(function (p) { round[p[0]] = Math.max(round[p[0]] || 0, p[1]); });
        if (acc === null) {
          acc = round;
        } else {
          var next = {};
          for (var d in round) if (acc[d] !== undefined) next[d] = acc[d] + round[d];
          acc = next;
        }
        if (!Object.keys(acc).length) break;
      }

      var hits = [];
      for (var id in acc || {}) {
        var doc = docs[id];
        if (doc) hits.push({ n: doc.t, u: doc.u, d: doc.s, k: doc.m, s: acc[id] });
      }
      return hits;
    });
  }

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"]/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c];
    });
  }

  // attach wires one input to its own results panel.
  function attach(q) {
    var out = q.parentNode.querySelector('.results');
    if (!out) return;
    var seq = 0, sel = -1, items = [];

    function render(hits, note) {
      items = hits.slice(0, 40);
      sel = items.length ? 0 : -1;
      if (!items.length) {
        out.innerHTML = '<p class="empty">No matches for that query.</p>';
        out.setAttribute('data-open', '');
        return;
      }
      var html = '<ol>';
      items.forEach(function (h, i) {
        html += '<li' + (i === sel ? ' data-sel=""' : '') + '><a href="' + ROOT + '/' + h.u + '">' +
          '<span class="rn">' + esc(h.n) + '</span>' +
          (h.k ? '<span class="rk">' + esc(h.k) + '</span>' : '') +
          (h.d ? '<span class="rd">' + esc(h.d) + '</span>' : '') +
          '</a></li>';
      });
      html += '</ol>';
      if (note) html += '<p class="status">' + esc(note) + '</p>';
      out.innerHTML = html;
      out.setAttribute('data-open', '');
    }

    function close() { out.removeAttribute('data-open'); sel = -1; items = []; }

    function run() {
      var query = q.value.trim();
      if (query.length < 2) { close(); return; }
      var mine = ++seq;

      ready().then(function () {
        if (mine !== seq) return;
        var nameHits = byName(query);
        nameHits.sort(function (a, b) { return b.s - a.s; });

        // Exact-ish name matches are what people almost always want; only reach
        // for the full-text index when names come up short.
        if (nameHits.length >= 8) { render(nameHits); return; }

        var terms = query.toLowerCase().match(/[a-z0-9_:]{2,}/g) || [];
        if (!terms.length) { render(nameHits); return; }

        render(nameHits, 'Searching full text\u2026');
        byText(terms).then(function (textHits) {
          if (mine !== seq) return;
          var seen = {};
          nameHits.forEach(function (h) { seen[h.u] = 1; });
          var merged = nameHits.concat(textHits.filter(function (h) { return !seen[h.u]; })
            .sort(function (a, b) { return b.s - a.s; }));
          render(merged);
        });
      }).catch(function (e) {
        out.innerHTML = '<p class="empty">Search index unavailable.</p>';
        out.setAttribute('data-open', '');
        if (window.console) console.error(e);
      });
    }

    var timer;
    q.addEventListener('input', function () {
      clearTimeout(timer);
      timer = setTimeout(run, 90);
    });
    q.addEventListener('focus', ready);

    q.addEventListener('keydown', function (e) {
      if (e.key === 'Escape') { q.blur(); close(); return; }
      if (!items.length) return;
      if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
        e.preventDefault();
        var lis = out.querySelectorAll('li');
        if (lis[sel]) lis[sel].removeAttribute('data-sel');
        sel = (sel + (e.key === 'ArrowDown' ? 1 : -1) + items.length) % items.length;
        lis[sel].setAttribute('data-sel', '');
        lis[sel].scrollIntoView({ block: 'nearest' });
      } else if (e.key === 'Enter' && items[sel]) {
        e.preventDefault();
        location.href = ROOT + '/' + items[sel].u;
      }
    });

    document.addEventListener('click', function (e) {
      if (!out.contains(e.target) && e.target !== q) close();
    });
  }

  Array.prototype.forEach.call(inputs, attach);

  // "/" focuses search from anywhere, the way every reference site should.
  // The masthead box comes first in the document and is always on screen.
  document.addEventListener('keydown', function (e) {
    if (e.key === '/' && !/^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement.tagName)) {
      e.preventDefault();
      inputs[0].focus();
      inputs[0].select();
    }
  });
})();

/* Colour theme. The system's preference is the default and needs no JavaScript
 * at all — the stylesheet honours prefers-color-scheme on its own. This only
 * adds the override.
 *
 * All three choices are shown at once rather than cycled through one button:
 * a control reading "Auto" tells you neither what it governs nor what else it
 * could say. The <head> applies a stored choice before the first paint, so all
 * this has to do is mark which one is current. */
(function () {
  'use strict';

  var KEY = 'tcldoc-theme';
  var root = document.documentElement;
  var wrap = document.getElementById('theme');
  if (!wrap) return;
  var buttons = wrap.querySelectorAll('[data-theme-set]');

  function stored() {
    try {
      var v = localStorage.getItem(KEY);
      return v === 'light' || v === 'dark' ? v : 'auto';
    } catch (e) {
      return 'auto';
    }
  }

  function apply(mode) {
    if (mode === 'auto') {
      root.removeAttribute('data-theme');
    } else {
      root.setAttribute('data-theme', mode);
    }
    Array.prototype.forEach.call(buttons, function (b) {
      b.setAttribute('aria-pressed', b.getAttribute('data-theme-set') === mode ? 'true' : 'false');
    });
    try {
      if (mode === 'auto') localStorage.removeItem(KEY);
      else localStorage.setItem(KEY, mode);
    } catch (e) { /* private browsing: the choice just will not persist */ }
  }

  apply(stored());
  wrap.hidden = false;

  Array.prototype.forEach.call(buttons, function (b) {
    b.addEventListener('click', function () {
      apply(b.getAttribute('data-theme-set'));
    });
  });
})();
