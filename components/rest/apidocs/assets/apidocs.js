// compose apidocs client: navigation, search, toggles, tabs, copy, JSON
// highlighting and the try-it client. Vanilla ES2020, no dependencies, every
// event delegated at the document level.
(() => {
  'use strict';

  const doc = document;
  const root = doc.documentElement;

  // Storage can throw inside sandboxed or srcdoc iframes; degrade to memory.
  function storage(kind) {
    try {
      const s = window[kind];
      s.setItem('apidocs.probe', '1');
      s.removeItem('apidocs.probe');
      return s;
    } catch (err) {
      const mem = new Map();
      return {
        getItem: (k) => (mem.has(k) ? mem.get(k) : null),
        setItem: (k, v) => mem.set(k, String(v)),
        removeItem: (k) => mem.delete(k),
      };
    }
  }
  const local = storage('localStorage');
  const session = storage('sessionStorage');

  // ---------------------------------------------------------------- theme

  const savedTheme = local.getItem('apidocs.theme');
  if (savedTheme === 'light' || savedTheme === 'dark') {
    root.dataset.theme = savedTheme;
  }

  function isDark() {
    if (root.dataset.theme) {
      return root.dataset.theme === 'dark';
    }
    return window.matchMedia('(prefers-color-scheme: dark)').matches;
  }

  function toggleTheme() {
    const next = isDark() ? 'light' : 'dark';
    root.dataset.theme = next;
    local.setItem('apidocs.theme', next);
  }

  // ------------------------------------------------------------- helpers

  function escapeHTML(s) {
    return s.replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  }

  function el(tag, cls, text) {
    const node = doc.createElement(tag);
    if (cls) {
      node.className = cls;
    }
    if (text !== undefined) {
      node.textContent = text;
    }
    return node;
  }

  function idFromHref(href) {
    try {
      return decodeURIComponent(href.slice(1));
    } catch (err) {
      return href.slice(1);
    }
  }

  function setHash(id, push) {
    try {
      const url = '#' + id;
      if (push) {
        history.pushState(null, '', url);
      } else {
        history.replaceState(null, '', url);
      }
    } catch (err) {
      // about:srcdoc and sandboxed frames may refuse history updates.
    }
  }

  // ------------------------------------------------------------- toggles

  function toggleBody(btn) {
    if (btn.classList.contains('nav-group-tg')) {
      return btn.nextElementSibling;
    }
    const resp = btn.closest('.resp');
    if (btn.classList.contains('rs') && resp) {
      return resp.querySelector(':scope > .resp-body');
    }
    const row = btn.closest('.row');
    return row ? row.querySelector(':scope > .row-kids') : null;
  }

  // Rows that reference a named model are rendered empty; their content is
  // cloned from the model card, either on first expand or, for rows that
  // start open, when they come near the viewport. Nested toggles in the clone
  // start collapsed.
  function fillLazy(body) {
    if (!body.dataset.model || body.dataset.loaded) {
      return;
    }
    body.dataset.loaded = '1';
    const card = doc.getElementById(body.dataset.model);
    const tree = card ? card.querySelector(':scope > .tree') : null;
    if (!tree) {
      return;
    }
    for (const child of tree.children) {
      if (!child.classList.contains('tree-desc')) {
        body.append(child.cloneNode(true));
      }
    }
    body.querySelectorAll('[data-autofill]').forEach((n) => n.removeAttribute('data-autofill'));
    body.querySelectorAll('.tg[aria-expanded="true"]').forEach((b) => {
      b.setAttribute('aria-expanded', 'false');
      const kids = toggleBody(b);
      if (kids) {
        kids.hidden = true;
      }
    });
  }

  const lazyObserver = 'IntersectionObserver' in window
    ? new IntersectionObserver((entries) => {
      for (const entry of entries) {
        if (entry.isIntersecting) {
          lazyObserver.unobserve(entry.target);
          fillLazy(entry.target);
        }
      }
    }, { rootMargin: '1200px 0px' })
    : null;

  doc.querySelectorAll('[data-autofill]').forEach((node) => {
    if (lazyObserver) {
      lazyObserver.observe(node);
    } else {
      fillLazy(node);
    }
  });

  function setExpanded(btn, open) {
    const body = toggleBody(btn);
    if (!body) {
      return;
    }
    if (open) {
      fillLazy(body);
    }
    btn.setAttribute('aria-expanded', String(open));
    body.hidden = !open;
  }

  function toggle(btn) {
    const open = btn.getAttribute('aria-expanded') !== 'true';
    setExpanded(btn, open);
    if (btn.classList.contains('nav-group-tg')) {
      btn.dataset.user = '1';
    }
  }

  // ---------------------------------------------------------------- tabs

  function selectTab(tab, focus) {
    const list = tab.closest('[role="tablist"]');
    const set = tab.closest('.tabset');
    if (!list || !set) {
      return;
    }
    const tabs = Array.from(list.querySelectorAll(':scope > [role="tab"]'));
    const panels = Array.from(set.querySelectorAll(':scope > .tabpanel'));
    const index = tabs.indexOf(tab);
    tabs.forEach((t, i) => {
      t.setAttribute('aria-selected', String(i === index));
      t.tabIndex = i === index ? 0 : -1;
    });
    panels.forEach((p, i) => {
      p.hidden = i !== index;
    });
    if (focus) {
      tab.focus();
    }
    highlightWithin(set);
  }

  function initTabs() {
    doc.querySelectorAll('[role="tablist"]').forEach((list) => {
      list.querySelectorAll(':scope > [role="tab"]').forEach((t) => {
        t.tabIndex = t.getAttribute('aria-selected') === 'true' ? 0 : -1;
      });
    });
  }

  doc.addEventListener('keydown', (e) => {
    const tab = e.target instanceof Element ? e.target.closest('[role="tab"]') : null;
    if (tab && (e.key === 'ArrowRight' || e.key === 'ArrowLeft' || e.key === 'Home' || e.key === 'End')) {
      const tabs = Array.from(tab.parentElement.querySelectorAll(':scope > [role="tab"]'));
      let i = tabs.indexOf(tab);
      if (e.key === 'ArrowRight') {
        i = (i + 1) % tabs.length;
      } else if (e.key === 'ArrowLeft') {
        i = (i - 1 + tabs.length) % tabs.length;
      } else if (e.key === 'Home') {
        i = 0;
      } else {
        i = tabs.length - 1;
      }
      e.preventDefault();
      selectTab(tabs[i], true);
      return;
    }

    const typing = e.target instanceof Element && e.target.closest('input, textarea, select, [contenteditable]');
    if (e.key === '/' && !typing && !e.metaKey && !e.ctrlKey && !e.altKey) {
      e.preventDefault();
      searchInput.focus();
    }
  });

  // ---------------------------------------------------------------- copy

  async function copyText(text, btn) {
    let ok = false;
    try {
      await navigator.clipboard.writeText(text);
      ok = true;
    } catch (err) {
      const ta = el('textarea');
      ta.value = text;
      ta.setAttribute('readonly', '');
      ta.style.position = 'fixed';
      ta.style.opacity = '0';
      doc.body.appendChild(ta);
      ta.select();
      try {
        ok = doc.execCommand('copy');
      } catch (err2) {
        ok = false;
      }
      ta.remove();
    }
    if (btn) {
      const label = btn.dataset.label || btn.textContent;
      btn.dataset.label = label;
      btn.textContent = ok ? 'Copied' : 'Copy failed';
      setTimeout(() => {
        btn.textContent = label;
      }, 1200);
    }
  }

  function copyFromCard(btn) {
    const card = btn.closest('.card');
    if (!card) {
      return;
    }
    const code = card.querySelector('.tabpanel:not([hidden]) code') || card.querySelector('code');
    if (code) {
      copyText(code.textContent, btn);
    }
  }

  // ------------------------------------------------------ JSON highlighting

  const tokenRe = /("(?:\\.|[^"\\])*")(\s*:)?|\b(true|false)\b|\b(null)\b|(-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)/g;

  function highlightJSON(src) {
    let out = '';
    let last = 0;
    for (const m of src.matchAll(tokenRe)) {
      out += escapeHTML(src.slice(last, m.index));
      if (m[1] !== undefined) {
        if (m[2] !== undefined) {
          out += '<span class="j-k">' + escapeHTML(m[1]) + '</span>' + escapeHTML(m[2]);
        } else {
          out += '<span class="j-s">' + escapeHTML(m[1]) + '</span>';
        }
      } else if (m[3] !== undefined) {
        out += '<span class="j-b">' + m[3] + '</span>';
      } else if (m[4] !== undefined) {
        out += '<span class="j-n">null</span>';
      } else {
        out += '<span class="j-d">' + m[5] + '</span>';
      }
      last = m.index + m[0].length;
    }
    return out + escapeHTML(src.slice(last));
  }

  function highlight(code) {
    if (code.dataset.hl) {
      return;
    }
    code.dataset.hl = '1';
    const src = code.textContent;
    if (src.length > 400000) {
      return;
    }
    code.innerHTML = highlightJSON(src);
  }

  function highlightWithin(scope) {
    scope.querySelectorAll('code.json:not([data-hl])').forEach((code) => {
      if (code.offsetParent !== null) {
        highlight(code);
      }
    });
  }

  const codeObserver = 'IntersectionObserver' in window
    ? new IntersectionObserver((entries) => {
      for (const entry of entries) {
        if (entry.isIntersecting) {
          highlight(entry.target);
          codeObserver.unobserve(entry.target);
        }
      }
    }, { rootMargin: '600px 0px' })
    : null;

  doc.querySelectorAll('code.json').forEach((code) => {
    if (codeObserver) {
      codeObserver.observe(code);
    } else {
      highlight(code);
    }
  });

  // ------------------------------------------------------------ navigation

  const sidebar = doc.getElementById('sidebar');
  const menuBtn = doc.querySelector('[data-menu]');

  function setMenu(open) {
    if (!sidebar || !menuBtn) {
      return;
    }
    sidebar.classList.toggle('open', open);
    menuBtn.setAttribute('aria-expanded', String(open));
  }

  function navigate(e, a) {
    const id = idFromHref(a.getAttribute('href'));
    const target = doc.getElementById(id);
    if (!target) {
      return;
    }
    e.preventDefault();
    revealModelOrOp(target);
    // Jump instantly: smooth scrolling across a 180-operation page is slow
    // and lets the scroll spy rewrite the hash on the way.
    target.scrollIntoView({ block: 'start' });
    setHash(id, true);
    setActive(id, false);
    if (!target.hasAttribute('tabindex')) {
      target.setAttribute('tabindex', '-1');
    }
    target.focus({ preventScroll: true });
    setMenu(false);
  }

  // Targets are always visible today, but a hidden response or tab panel
  // could contain an anchor in the future; open the ancestors on the way.
  function revealModelOrOp(target) {
    let node = target.parentElement;
    while (node && node !== doc.body) {
      if (node.hidden) {
        node.hidden = false;
      }
      node = node.parentElement;
    }
  }

  const navLinks = new Map();
  doc.querySelectorAll('.sidebar a[href^="#"]').forEach((a) => {
    const id = idFromHref(a.getAttribute('href'));
    if (!navLinks.has(id)) {
      navLinks.set(id, []);
    }
    navLinks.get(id).push(a);
  });

  let activeId = null;
  let autoOpened = null;
  let hashTimer = 0;

  function setActive(id, updateHash) {
    if (id === activeId) {
      return;
    }
    if (activeId && navLinks.has(activeId)) {
      navLinks.get(activeId).forEach((a) => a.classList.remove('active'));
    }
    activeId = id;
    const links = navLinks.get(id) || [];
    links.forEach((a) => a.classList.add('active'));

    const link = links[0];
    if (link && !searchInput.value) {
      const group = link.closest('.nav-group');
      const btn = group ? group.querySelector('.nav-group-tg') : null;
      if (autoOpened && autoOpened !== btn && !autoOpened.dataset.user) {
        setExpanded(autoOpened, false);
      }
      if (btn && btn.getAttribute('aria-expanded') !== 'true') {
        setExpanded(btn, true);
        autoOpened = btn;
      }
      const box = sidebar.getBoundingClientRect();
      const r = link.getBoundingClientRect();
      if (r.top < box.top + 60 || r.bottom > box.bottom - 20) {
        link.scrollIntoView({ block: 'nearest' });
      }
    }

    if (updateHash) {
      clearTimeout(hashTimer);
      hashTimer = setTimeout(() => setHash(id, false), 200);
    }
  }

  const visible = new Set();
  let spyReady = false;
  if ('IntersectionObserver' in window) {
    const spy = new IntersectionObserver((entries) => {
      for (const entry of entries) {
        if (entry.isIntersecting) {
          visible.add(entry.target);
        } else {
          visible.delete(entry.target);
        }
      }
      // The active entry is the topmost anchor crossing a band near the top
      // of the viewport (below the sticky mobile bar).
      let best = null;
      let bestTop = Infinity;
      for (const node of visible) {
        const top = node.getBoundingClientRect().top;
        if (top < bestTop && navLinks.has(node.id)) {
          best = node;
          bestTop = top;
        }
      }
      if (best) {
        setActive(best.id, spyReady);
      }
    }, { rootMargin: '-60px 0px -70% 0px' });
    doc.querySelectorAll('[data-anchor]').forEach((node) => spy.observe(node));
    // Do not rewrite the hash until the user actually scrolls.
    window.addEventListener('scroll', () => {
      spyReady = true;
    }, { once: true, passive: true });
  }

  function openInitialHash() {
    if (!location.hash || location.hash.length < 2) {
      return;
    }
    const id = idFromHref(location.hash);
    const target = doc.getElementById(id);
    if (target) {
      target.scrollIntoView({ block: 'start' });
      setActive(id, false);
    }
  }

  // --------------------------------------------------------------- search

  const searchInput = doc.getElementById('nav-search');
  const noResults = doc.querySelector('.no-results');
  const staticNav = doc.querySelector('.nav-static');
  const groups = Array.from(doc.querySelectorAll('.nav-group'));

  function runSearch() {
    const terms = searchInput.value.trim().toLowerCase().split(/\s+/).filter(Boolean);
    let total = 0;
    for (const group of groups) {
      const btn = group.querySelector('.nav-group-tg');
      const groupName = btn.textContent.toLowerCase();
      let shown = 0;
      group.querySelectorAll('a[data-search]').forEach((a) => {
        const hay = a.dataset.search + ' ' + groupName;
        const match = terms.every((t) => hay.includes(t));
        a.parentElement.hidden = !match;
        if (match) {
          shown++;
        }
      });
      total += shown;
      group.hidden = terms.length > 0 && shown === 0;
      if (terms.length > 0) {
        setExpanded(btn, shown > 0);
      } else if (!btn.dataset.user) {
        const hasActive = group.querySelector('a.active') !== null;
        setExpanded(btn, hasActive);
      }
    }
    if (staticNav) {
      staticNav.hidden = terms.length > 0;
    }
    noResults.hidden = terms.length === 0 || total > 0;
  }

  if (searchInput) {
    searchInput.addEventListener('input', runSearch);
    searchInput.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') {
        searchInput.value = '';
        runSearch();
      } else if (e.key === 'Enter') {
        const first = doc.querySelector('.nav-group:not([hidden]) li:not([hidden]) a');
        if (first) {
          first.click();
        }
      }
    });
  }

  // ------------------------------------------------------- auth settings

  const authInputs = Array.from(doc.querySelectorAll('[data-auth]'));
  const serverInput = doc.querySelector('[data-server-input]');

  authInputs.forEach((input) => {
    const saved = session.getItem('apidocs.auth.' + input.dataset.auth);
    if (saved) {
      input.value = saved;
    }
  });
  if (serverInput) {
    const savedServer = session.getItem('apidocs.server');
    if (savedServer) {
      serverInput.value = savedServer;
    }
  }

  doc.addEventListener('input', (e) => {
    const t = e.target;
    if (!(t instanceof HTMLInputElement)) {
      return;
    }
    if (t.dataset.auth) {
      session.setItem('apidocs.auth.' + t.dataset.auth, t.value);
      doc.querySelectorAll('form.try:not([hidden])').forEach(updateAuthNote);
    } else if (t.hasAttribute('data-server-input')) {
      session.setItem('apidocs.server', t.value);
    } else if (t.classList.contains('invalid') && t.value) {
      t.classList.remove('invalid');
    }
  });

  function schemeInputs(name) {
    return authInputs.filter((i) => i.dataset.scheme === name);
  }

  function schemeFilled(name) {
    const inputs = schemeInputs(name);
    return inputs.length > 0 && inputs.every((i) => i.value !== '');
  }

  function pickRequirement(form) {
    let reqs = [];
    try {
      reqs = JSON.parse(form.dataset.requires || '[]');
    } catch (err) {
      reqs = [];
    }
    if (reqs.length === 0) {
      return null;
    }
    return reqs.find((r) => r.every(schemeFilled)) || reqs[0];
  }

  function updateAuthNote(form) {
    const note = form.querySelector('.try-auth');
    if (!note) {
      return;
    }
    note.textContent = '';
    const req = pickRequirement(form);
    if (!req) {
      note.textContent = 'No authentication.';
      return;
    }
    if (req.length === 0) {
      note.textContent = 'Authentication optional.';
      return;
    }
    note.append('Sends ');
    req.forEach((name, i) => {
      if (i > 0) {
        note.append(' + ');
      }
      const span = el('span', schemeFilled(name) ? '' : 'missing', name + (schemeFilled(name) ? '' : ' (not set)'));
      note.append(span);
    });
    note.append(' from ');
    const a = el('a', '', 'Try it settings');
    a.href = '#try-settings';
    note.append(a);
  }

  function utf8Base64(s) {
    const bytes = new TextEncoder().encode(s);
    let bin = '';
    bytes.forEach((b) => {
      bin += String.fromCharCode(b);
    });
    return btoa(bin);
  }

  function applyAuth(form, headers, query) {
    const req = pickRequirement(form);
    if (!req) {
      return;
    }
    for (const name of req) {
      const inputs = schemeInputs(name);
      const user = inputs.find((i) => i.dataset.kind === 'basic-user');
      const pass = inputs.find((i) => i.dataset.kind === 'basic-pass');
      if (user || pass) {
        if ((user && user.value) || (pass && pass.value)) {
          headers.Authorization = 'Basic ' + utf8Base64((user ? user.value : '') + ':' + (pass ? pass.value : ''));
        }
        continue;
      }
      for (const input of inputs) {
        if (!input.value) {
          continue;
        }
        if (input.dataset.kind === 'bearer') {
          headers.Authorization = 'Bearer ' + input.value;
        } else if (input.dataset.kind === 'apiKey') {
          if (input.dataset.in === 'query') {
            query.set(input.dataset.param, input.value);
          } else if (input.dataset.in === 'header') {
            headers[input.dataset.param] = input.value;
          }
        }
      }
    }
  }

  // --------------------------------------------------------------- try it

  function serverBase() {
    let base = (serverInput && serverInput.value.trim()) || doc.body.dataset.server || '';
    const origin = location.origin && location.origin !== 'null' ? location.origin : '';
    if (!base) {
      base = origin;
    } else if (base.startsWith('/') && origin) {
      base = origin + base;
    }
    return base.replace(/\/+$/, '');
  }

  function openTry(btn) {
    const op = btn.closest('[data-op]');
    const form = op ? op.querySelector('form.try') : null;
    if (!form) {
      return;
    }
    const open = form.hidden;
    form.hidden = !open;
    btn.setAttribute('aria-expanded', String(open));
    btn.textContent = open ? 'Close' : 'Try it';
    if (!open) {
      return;
    }
    if (!form.dataset.ready) {
      form.dataset.ready = '1';
      const body = form.querySelector('[data-body]');
      const example = op.querySelector('[data-req-example]');
      if (body && !body.value && example) {
        body.value = example.textContent;
      }
    }
    updateAuthNote(form);
    const first = form.querySelector('input, textarea');
    if (first) {
      first.focus({ preventScroll: true });
    }
  }

  function shellQuote(s) {
    return "'" + s.replace(/'/g, "'\\''") + "'";
  }

  function curlFor(method, url, headers, body) {
    const parts = ['curl -X ' + method + ' ' + shellQuote(url)];
    for (const [k, v] of Object.entries(headers)) {
      parts.push('-H ' + shellQuote(k + ': ' + v));
    }
    if (body !== undefined) {
      parts.push('--data-raw ' + shellQuote(body));
    }
    return parts.join(' \\\n  ');
  }

  const sentCurl = new WeakMap();

  async function send(form) {
    const method = form.dataset.method;
    let path = form.dataset.path;
    const query = new URLSearchParams();
    const headers = {};
    let missing = false;

    form.querySelectorAll('input[data-in]').forEach((input) => {
      const name = input.dataset.name;
      const value = input.value;
      if (value === '') {
        if (input.hasAttribute('data-required')) {
          input.classList.add('invalid');
          missing = true;
        }
        return;
      }
      input.classList.remove('invalid');
      if (input.dataset.in === 'path') {
        path = path.split('{' + name + '}').join(encodeURIComponent(value));
      } else if (input.dataset.in === 'query') {
        query.append(name, value);
      } else if (input.dataset.in === 'header') {
        headers[name] = value;
      }
    });

    const result = form.querySelector('.try-result');
    result.hidden = false;
    result.textContent = '';

    if (missing) {
      result.append(el('p', 'try-error', 'Fill in the required parameters.'));
      return;
    }

    applyAuth(form, headers, query);

    let body;
    const textarea = form.querySelector('[data-body]');
    if (textarea && textarea.value.trim() !== '' && method !== 'GET' && method !== 'HEAD') {
      body = textarea.value;
      const ct = form.dataset.ct || 'application/json';
      if (!ct.startsWith('multipart/')) {
        headers['Content-Type'] = ct;
      }
    }

    const qs = query.toString();
    const url = serverBase() + path + (qs ? '?' + qs : '');
    const curl = curlFor(method, url, headers, body);
    sentCurl.set(result, curl);

    const status = el('div', 'try-status');
    status.append(el('span', 'muted', 'Sending ' + method + ' ' + url + ' …'));
    result.append(status);

    const started = performance.now();
    let res;
    try {
      res = await fetch(url, { method, headers, body });
    } catch (err) {
      status.textContent = '';
      status.append(el('span', 'try-error', 'Network error: ' + (err && err.message ? err.message : String(err)) +
        '. The server may be unreachable, or it does not allow cross-origin requests from this page (CORS).'));
      status.append(copyCurlButton());
      return;
    }
    const elapsed = Math.round(performance.now() - started);
    const text = await res.text().catch(() => '');

    status.textContent = '';
    const cls = 's' + String(res.status).charAt(0) + 'xx';
    status.append(el('span', 'status ' + cls, res.status + (res.statusText ? ' ' + res.statusText : '')));
    status.append(el('span', 'muted', elapsed + ' ms'));
    status.append(el('span', 'muted', text.length + ' bytes'));
    status.append(copyCurlButton());

    const headerLines = [];
    res.headers.forEach((v, k) => headerLines.push(k + ': ' + v));
    if (headerLines.length > 0) {
      const details = el('details');
      details.append(el('summary', '', 'Response headers (' + headerLines.length + ')'));
      const pre = el('pre');
      pre.append(el('code', '', headerLines.join('\n')));
      details.append(pre);
      result.append(details);
    }

    const card = el('div', 'card');
    const head = el('div', 'card-head');
    head.append(el('span', '', 'Response body'));
    head.append(el('span', 'ct', res.headers.get('content-type') || ''));
    const copy = el('button', 'copy', 'Copy');
    copy.type = 'button';
    copy.setAttribute('data-copy', '');
    head.append(copy);
    card.append(head);
    const pre = el('pre');
    const code = el('code');
    let pretty = text;
    let isJSON = false;
    try {
      if (text.trim() !== '') {
        pretty = JSON.stringify(JSON.parse(text), null, 2);
        isJSON = true;
      }
    } catch (err) {
      isJSON = false;
    }
    if (isJSON) {
      code.className = 'json';
      code.textContent = pretty;
      highlight(code);
    } else {
      code.textContent = text === '' ? '(empty body)' : text;
    }
    pre.append(code);
    card.append(pre);
    result.append(card);
  }

  function copyCurlButton() {
    const btn = el('button', 'btn', 'Copy as curl');
    btn.type = 'button';
    btn.setAttribute('data-copy-curl', '');
    return btn;
  }

  doc.addEventListener('submit', (e) => {
    const form = e.target instanceof Element ? e.target.closest('form.try') : null;
    if (!form) {
      return;
    }
    e.preventDefault();
    send(form);
  });

  // ------------------------------------------------------ delegated clicks

  doc.addEventListener('click', (e) => {
    const t = e.target;
    if (!(t instanceof Element)) {
      return;
    }

    const btn = t.closest('button');
    if (btn) {
      if (btn.matches('.tg, .nav-group-tg')) {
        toggle(btn);
      } else if (btn.matches('[role="tab"]')) {
        selectTab(btn, false);
      } else if (btn.matches('[data-copy]')) {
        copyFromCard(btn);
      } else if (btn.matches('[data-copy-curl]')) {
        const result = btn.closest('.try-result');
        const curl = result ? sentCurl.get(result) : '';
        if (curl) {
          copyText(curl, btn);
        }
      } else if (btn.matches('[data-try]')) {
        openTry(btn);
      } else if (btn.matches('[data-theme-toggle]')) {
        toggleTheme();
      } else if (btn.matches('[data-menu]')) {
        setMenu(!sidebar.classList.contains('open'));
      }
      return;
    }

    const a = t.closest('a[href]');
    if (!a) {
      if (sidebar && sidebar.classList.contains('open') && !t.closest('.sidebar')) {
        setMenu(false);
      }
      return;
    }
    const href = a.getAttribute('href');
    if (href.startsWith('#') && href.length > 1) {
      navigate(e, a);
    }
  });

  // External links in descriptions open outside the docs (and outside an
  // embedding iframe).
  doc.querySelectorAll('.md a[href^="http"]').forEach((a) => {
    a.target = '_blank';
    a.rel = 'noopener noreferrer';
  });

  initTabs();
  openInitialHash();
})();
