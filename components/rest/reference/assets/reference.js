// compose reference client: navigation, search, toggles, tabs, copy, JSON
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
      s.setItem('reference.probe', '1');
      s.removeItem('reference.probe');
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

  const savedTheme = local.getItem('reference.theme');
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
    local.setItem('reference.theme', next);
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
    if (btn.classList.contains('nav-group-tg') || btn.classList.contains('nav-folder-tg') || btn.classList.contains('opt-tg')) {
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
  // start open, when they come near the viewport. Named oneOf/anyOf variants
  // (.variant-body) are filled the same way when their tab is shown. Nested
  // toggles in the clone start collapsed; visible variant bodies in the clone
  // are observed again so they fill too.
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
    body.querySelectorAll('.tg[aria-expanded="true"]').forEach((b) => {
      b.setAttribute('aria-expanded', 'false');
      const kids = toggleBody(b);
      if (kids) {
        kids.hidden = true;
        kids.removeAttribute('data-autofill');
      }
    });
    body.querySelectorAll('[data-autofill]').forEach(observeLazy);
  }

  // autoFill fills a lazy node that has no toggle of its own. A model that
  // already encloses the node is not cloned again, so variants that refer
  // back to an ancestor cannot fill forever.
  function autoFill(node) {
    if (!node.dataset.model || node.dataset.loaded) {
      return;
    }
    for (let up = node.parentElement && node.parentElement.closest('[data-model]'); up; up = up.parentElement && up.parentElement.closest('[data-model]')) {
      if (up.dataset.model === node.dataset.model) {
        node.dataset.loaded = '1';
        const note = el('div', 'note');
        const a = el('a', 'ref', 'Recursive');
        a.href = '#' + node.dataset.model;
        note.append(a, ', see the model above.');
        node.append(note);
        return;
      }
    }
    fillLazy(node);
  }

  const lazyObserver = 'IntersectionObserver' in window
    ? new IntersectionObserver((entries) => {
      for (const entry of entries) {
        if (entry.isIntersecting) {
          lazyObserver.unobserve(entry.target);
          autoFill(entry.target);
        }
      }
    }, { rootMargin: '1200px 0px' })
    : null;

  function observeLazy(node) {
    if (lazyObserver) {
      lazyObserver.observe(node);
    } else {
      autoFill(node);
    }
  }

  doc.querySelectorAll('[data-autofill]').forEach(observeLazy);

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
    // Remember sidebar toggles the user chose, so automatic expansion and
    // search leave them as they were.
    if (btn.classList.contains('nav-group-tg') || btn.classList.contains('nav-folder-tg')) {
      btn.dataset.user = open ? 'open' : 'closed';
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
    if (panels[index]) {
      panels[index].querySelectorAll(':scope > [data-model]').forEach(autoFill);
    }
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

    if (e.key === 'Escape' && pop) {
      const btn = pop.btn;
      closePop();
      btn.focus();
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

  // navigate handles an in-page link; the caller has already prevented the
  // default, which inside a srcdoc iframe would load a different document.
  function navigate(a) {
    const id = idFromHref(a.getAttribute('href'));
    const target = doc.getElementById(id);
    if (!target) {
      return;
    }
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

  // navToggles lists the sidebar toggles above a link, outermost first: its
  // tag group, then each path folder down to the link.
  function navToggles(link) {
    const out = [];
    for (let li = link.closest('.nav-folder'); li; li = li.parentElement.closest('.nav-folder')) {
      out.unshift(li.querySelector(':scope > .nav-folder-tg'));
    }
    const group = link.closest('.nav-group');
    const btn = group ? group.querySelector(':scope > .nav-group-tg') : null;
    if (btn) {
      out.unshift(btn);
    }
    return out;
  }

  // Folder names take part in search, so "cards" also finds the operations
  // filed under a cards folder.
  doc.querySelectorAll('.sidebar a[data-search]').forEach((a) => {
    const names = [];
    for (let li = a.closest('.nav-folder'); li; li = li.parentElement.closest('.nav-folder')) {
      names.push(li.querySelector(':scope > .nav-folder-tg .nav-folder-name').textContent);
    }
    a.dataset.folders = names.join(' ').toLowerCase();
  });

  let activeId = null;
  let autoOpened = new Set();
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
      // Open the group and folders that lead to the link; close the ones
      // opened automatically for the previous link unless the user touched
      // them.
      const chain = navToggles(link);
      const kept = new Set();
      for (const btn of autoOpened) {
        if (chain.includes(btn)) {
          kept.add(btn);
        } else if (!btn.dataset.user) {
          setExpanded(btn, false);
        }
      }
      for (const btn of chain) {
        if (btn.getAttribute('aria-expanded') !== 'true') {
          setExpanded(btn, true);
          kept.add(btn);
        }
      }
      autoOpened = kept;
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
      const label = btn.querySelector('.nav-group-name');
      const groupName = (label ? label.textContent : '').toLowerCase();
      let shown = 0;
      group.querySelectorAll('a[data-search]').forEach((a) => {
        const hay = a.dataset.search + ' ' + groupName + ' ' + (a.dataset.folders || '');
        const match = terms.every((t) => hay.includes(t));
        a.parentElement.hidden = !match;
        if (match) {
          shown++;
        }
      });
      total += shown;
      group.querySelectorAll('.nav-folder').forEach((folder) => {
        const fbtn = folder.querySelector(':scope > .nav-folder-tg');
        if (terms.length > 0) {
          const any = Array.from(folder.querySelectorAll('a[data-search]')).some((a) => !a.parentElement.hidden);
          folder.hidden = !any;
          setExpanded(fbtn, any);
        } else {
          folder.hidden = false;
          setExpanded(fbtn, fbtn.dataset.user ? fbtn.dataset.user === 'open' : folder.querySelector('a.active') !== null);
        }
      });
      group.hidden = terms.length > 0 && shown === 0;
      if (terms.length > 0) {
        setExpanded(btn, shown > 0);
      } else if (btn.dataset.user) {
        setExpanded(btn, btn.dataset.user === 'open');
      } else {
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
        const first = doc.querySelector('.nav-group:not([hidden]) li:not([hidden]) > a[data-search]');
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
    const saved = session.getItem('reference.auth.' + input.dataset.auth);
    if (saved) {
      input.value = saved;
    }
  });
  if (serverInput) {
    const savedServer = session.getItem('reference.server');
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
      session.setItem('reference.auth.' + t.dataset.auth, t.value);
      doc.querySelectorAll('form.try:not([hidden])').forEach(updateAuthNote);
    } else if (t.hasAttribute('data-server-input')) {
      session.setItem('reference.server', t.value);
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
    // An empty requirement ({}) means "optional"; prefer a complete set of
    // credentials over it, then the optional entry, then the first set.
    return reqs.find((r) => r.length > 0 && r.every(schemeFilled)) ||
      reqs.find((r) => r.length === 0) ||
      reqs[0];
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

  // serverBase is the Server setting, or the operation's own server while
  // the setting still holds the page default.
  function serverBase(form) {
    const fallback = doc.body.dataset.server || '';
    let base = (serverInput && serverInput.value.trim()) || fallback;
    if (form.dataset.server && base === fallback) {
      base = form.dataset.server;
    }
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
    litPorts(form);
    const first = form.querySelector('input, textarea');
    if (first) {
      first.focus({ preventScroll: true });
    }
  }

  function shellQuote(s) {
    return "'" + s.replace(/'/g, "'\\''") + "'";
  }

  function curlFor(method, url, headers, bodyArgs) {
    const parts = ['curl -X ' + method + ' ' + shellQuote(url)];
    for (const [k, v] of Object.entries(headers)) {
      parts.push('-H ' + shellQuote(k + ': ' + v));
    }
    return parts.concat(bodyArgs).join(' \\\n  ');
  }

  // formFields turns the JSON object in the body textarea into name/value
  // pairs for form content types: scalars as text, arrays of scalars as
  // repeated fields, nested objects skipped.
  function formFields(text) {
    const obj = JSON.parse(text);
    if (!obj || typeof obj !== 'object' || Array.isArray(obj)) {
      throw new Error('expected a JSON object');
    }
    const out = [];
    for (const [k, v] of Object.entries(obj)) {
      const list = Array.isArray(v) ? v : [v];
      for (const item of list) {
        if (item !== null && typeof item !== 'object') {
          out.push([k, String(item)]);
        }
      }
    }
    return out;
  }

  // requestBody builds the fetch body and the matching curl flags for the
  // textarea contents. Only JSON content types are sent as raw text with
  // their Content-Type; form types are encoded from the JSON object.
  function requestBody(text, ct, headers) {
    const type = ct.split(';')[0].trim().toLowerCase();
    if (type === 'multipart/form-data') {
      const fd = new FormData();
      const args = [];
      for (const [k, v] of formFields(text)) {
        fd.append(k, v);
        args.push('-F ' + shellQuote(k + '=' + v));
      }
      return { body: fd, args };
    }
    if (type === 'application/x-www-form-urlencoded') {
      const params = new URLSearchParams(formFields(text));
      headers['Content-Type'] = 'application/x-www-form-urlencoded';
      return { body: params, args: ['--data-raw ' + shellQuote(params.toString())] };
    }
    headers['Content-Type'] = ct;
    return { body: text, args: ['--data-raw ' + shellQuote(text)] };
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
      // Arrays are typed comma separated. Exploded ones (the default for
      // query parameters) travel as repeated keys, the rest comma-joined.
      const items = input.hasAttribute('data-array') ? value.split(',').map((v) => v.trim()) : [value];
      if (input.dataset.in === 'path') {
        path = path.split('{' + name + '}').join(items.map(encodeURIComponent).join(','));
      } else if (input.dataset.in === 'query') {
        if (input.hasAttribute('data-explode')) {
          items.forEach((v) => query.append(name, v));
        } else {
          query.append(name, items.join(input.dataset.sep || ','));
        }
      } else if (input.dataset.in === 'header') {
        headers[name] = items.join(',');
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
    let bodyArgs = [];
    const textarea = form.querySelector('[data-body]');
    if (textarea && textarea.value.trim() !== '' && method !== 'GET' && method !== 'HEAD') {
      try {
        const built = requestBody(textarea.value, form.dataset.ct || 'application/json', headers);
        body = built.body;
        bodyArgs = built.args;
      } catch (err) {
        result.append(el('p', 'try-error', 'The body must be a JSON object to send it as ' + form.dataset.ct + ': ' + err.message + '.'));
        return;
      }
    }

    // Header values the browser rejects make fetch throw a TypeError that
    // reads like a network failure; check them first.
    for (const [k, v] of Object.entries(headers)) {
      try {
        new Headers([[k, v]]);
      } catch (err) {
        result.append(el('p', 'try-error', 'Invalid header value for ' + k + ': ' + (err && err.message ? err.message : String(err))));
        return;
      }
    }

    const qs = query.toString();
    const url = serverBase(form) + path + (qs ? '?' + qs : '');
    const curl = curlFor(method, url, headers, bodyArgs);
    sentCurl.set(result, curl);

    const status = el('div', 'try-status');
    status.append(el('span', 'muted', 'Sending ' + method + ' ' + url + ' …'));
    result.append(status);

    const machine = machineOf(form);
    litPorts(form);
    machineState(machine, 'running');
    const started = performance.now();
    // The belt runs for a beat even when the answer is instant.
    const settle = (state, code) => setTimeout(() => machineState(machine, state, code), Math.max(0, 450 - (performance.now() - started)));
    let res;
    try {
      res = await fetch(url, { method, headers, body });
    } catch (err) {
      settle('jam');
      status.textContent = '';
      status.append(el('span', 'try-error', 'Network error: ' + (err && err.message ? err.message : String(err)) +
        '. The server may be unreachable, or it does not allow cross-origin requests from this page (CORS).'));
      status.append(copyCurlButton());
      return;
    }
    const elapsed = Math.round(performance.now() - started);
    settle('done', res.status);
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

  // ------------------------------------------------- pretty response view

  // Response samples render as labeled values instead of JSON. The raw JSON
  // stays in the page for Copy and the JSON toggle; the view is built from it
  // when the card nears the viewport.

  function pvScalar(v) {
    if (v === null) {
      return el('span', 'pv-v pv-null', 'null');
    }
    if (typeof v === 'boolean') {
      return el('span', 'pv-v pv-bool', String(v));
    }
    if (typeof v === 'number') {
      return el('span', 'pv-v pv-num', String(v));
    }
    if (v === '') {
      return el('span', 'pv-v pv-empty', 'empty');
    }
    // "string" is the generator's placeholder for unformatted strings.
    return el('span', v === 'string' ? 'pv-v pv-ph' : 'pv-v pv-str', String(v));
  }

  function pvIsNode(v) {
    return v !== null && typeof v === 'object' && Object.keys(v).length > 0 &&
      !(Array.isArray(v) && v.every((item) => item === null || typeof item !== 'object'));
  }

  function pvSummary(v) {
    if (Array.isArray(v)) {
      return v.length + (v.length === 1 ? ' item' : ' items');
    }
    const n = Object.keys(v).length;
    return n + (n === 1 ? ' field' : ' fields');
  }

  // Samples render as a file tree: objects and lists are folders with a
  // chevron, an icon and a count; values are leaves on elbow connectors.
  function pvList(v, depth) {
    const ul = el('ul', 'jt');
    if (Array.isArray(v) && v.length === 1 && pvIsNode(v[0]) && !Array.isArray(v[0])) {
      // A list of one object reads better as that object's fields.
      v = v[0];
    }
    if (Array.isArray(v)) {
      v.forEach((item, i) => ul.append(pvItem('#' + (i + 1), item, depth)));
    } else {
      for (const key of Object.keys(v)) {
        ul.append(pvItem(key, v[key], depth));
      }
    }
    return ul;
  }

  function pvItem(key, v, depth) {
    const li = el('li');
    if (!pvIsNode(v)) {
      const line = el('div', 'jt-line');
      line.append(el('span', 'jt-k', key));
      if (Array.isArray(v)) {
        const list = el('span', 'pv-v pv-list');
        if (v.length === 0) {
          list.append(el('span', 'pv-empty', 'empty list'));
        }
        v.forEach((item) => list.append(pvScalar(item)));
        line.append(list);
      } else if (v !== null && typeof v === 'object') {
        line.append(el('span', 'pv-v pv-empty', 'empty object'));
      } else {
        line.append(pvScalar(v));
      }
      li.append(line);
      return li;
    }
    const open = depth < 2;
    const isList = Array.isArray(v);
    const line = el('button', 'jt-line jt-tg');
    line.type = 'button';
    line.setAttribute('aria-expanded', String(open));
    line.append(
      el('span', isList ? 'jt-ico jt-arr' : 'jt-ico jt-obj', isList ? '[]' : '{}'),
      el('span', 'jt-k', key),
      el('span', 'pill', pvSummary(v)),
    );
    const kids = pvList(v, depth + 1);
    kids.hidden = !open;
    li.append(line, kids);
    return li;
  }

  function renderPretty(box) {
    if (box.dataset.done) {
      return;
    }
    box.dataset.done = '1';
    const code = box.parentElement.querySelector('pre.raw code');
    let v;
    try {
      v = JSON.parse(code.textContent);
    } catch (err) {
      box.parentElement.classList.add('pv-off');
      return;
    }
    if (!pvIsNode(v)) {
      v = { value: v };
    }
    const root = pvList(v, 0);
    root.classList.add('jt-root');
    box.append(root);
  }

  const pvObserver = 'IntersectionObserver' in window
    ? new IntersectionObserver((entries) => {
      for (const entry of entries) {
        if (entry.isIntersecting) {
          pvObserver.unobserve(entry.target);
          renderPretty(entry.target);
        }
      }
    }, { rootMargin: '800px 0px' })
    : null;

  doc.querySelectorAll('[data-pv]').forEach((box) => {
    if (pvObserver) {
      pvObserver.observe(box);
    } else {
      renderPretty(box);
    }
  });

  function toggleRaw(btn) {
    const panel = btn.closest('.tabpanel');
    const raw = !panel.classList.contains('show-raw');
    panel.classList.toggle('show-raw', raw);
    btn.setAttribute('aria-pressed', String(raw));
    if (raw) {
      highlightWithin(panel);
    }
  }

  // ----------------------------------------------------------------- flow
  // Every operation is a machine: its inputs wired in, its outputs wired out.
  // The flow data links machines into chains through the models they make
  // and take, for the source and user lists, sidebar cards and tracing.

  let flow = null;

  function flowData() {
    if (flow) {
      return flow;
    }
    const node = doc.getElementById('flow-data');
    try {
      flow = JSON.parse(node ? node.textContent : '{}') || {};
    } catch (err) {
      flow = {};
    }
    flow.ops = flow.ops || [];
    flow.models = flow.models || {};
    flow.opIndex = {};
    flow.ops.forEach((op, i) => {
      flow.opIndex[op.a] = i;
    });
    flow.modelIndex = {};
    Object.keys(flow.models).forEach((id) => {
      flow.modelIndex[flow.models[id].a] = id;
    });
    return flow;
  }

  // recipe renders an operation as one line: inputs -> outputs.
  function recipe(op) {
    const line = el('div', 'recipe');
    const ins = op.i || [];
    ins.forEach((name) => line.append(el('span', 'ri', name)));
    if (op.q) {
      line.append(el('span', 'ri opt', '+' + op.q + ' optional'));
    }
    if (ins.length === 0 && !op.q) {
      line.append(el('span', 'ri none', 'nothing'));
    }
    line.append(el('span', 'r-arrow', '→'));
    (op.o && op.o.length ? op.o : ['empty']).forEach((out) => line.append(el('span', 'ri out', out)));
    if (op.e && op.e.length) {
      line.append(el('span', 'r-err', 'or ' + op.e.join(', ')));
    }
    return line;
  }

  function opTitle(op, cls) {
    const t = el('div', cls);
    t.append(el('span', 'm m-' + op.m.toLowerCase(), op.m), el('span', '', op.t));
    return t;
  }

  // Popover: the operations that make (by) or take (use) the models of the
  // port or model card the button sits in.
  let pop = null;

  function closePop() {
    if (!pop) {
      return;
    }
    pop.box.remove();
    pop.btn.setAttribute('aria-expanded', 'false');
    pop = null;
  }

  function openPop(btn) {
    const same = pop && pop.btn === btn;
    closePop();
    if (same) {
      return;
    }
    const f = flowData();
    const holder = btn.closest('[data-m]');
    const ids = holder ? holder.dataset.m.split(' ') : [];
    const kind = btn.dataset.pop;
    const article = btn.closest('[data-op]');
    const self = article ? article.id : '';
    const seen = new Set();
    const ops = [];
    const names = [];
    ids.forEach((id) => {
      const m = f.models[id];
      if (!m) {
        return;
      }
      names.push(m.n);
      (m[kind] || []).forEach((i) => {
        const op = f.ops[i];
        if (op && op.a !== self && !seen.has(i)) {
          seen.add(i);
          ops.push(op);
        }
      });
    });

    const box = el('div', 'fpop');
    box.setAttribute('role', 'dialog');
    const head = el('div', 'fpop-h');
    head.append(kind === 'by' ? 'Where to get ' : 'What takes ');
    head.append(el('b', '', names.join(', ')));
    head.append(el('span', '', '· ' + ops.length));
    box.append(head);
    const list = el('div', 'fpop-list');
    ops.forEach((op) => {
      const a = el('a', 'fpop-op');
      a.href = '#' + op.a;
      a.dataset.search = (op.t + ' ' + op.m + ' ' + op.p).toLowerCase();
      a.append(opTitle(op, 'fpop-t'), recipe(op));
      list.append(a);
    });
    if (ops.length > 8) {
      const filter = el('input', 'fpop-filter');
      filter.type = 'search';
      filter.placeholder = 'Filter ' + ops.length + ' operations';
      filter.setAttribute('aria-label', 'Filter operations');
      filter.addEventListener('input', () => {
        const q = filter.value.trim().toLowerCase();
        list.querySelectorAll('.fpop-op').forEach((a) => {
          a.hidden = q !== '' && !a.dataset.search.includes(q);
        });
      });
      box.append(filter);
    }
    box.append(list);
    doc.body.append(box);

    const r = btn.getBoundingClientRect();
    const width = box.offsetWidth;
    const left = Math.max(12, Math.min(r.left, window.innerWidth - width - 12));
    box.style.left = (left + window.scrollX) + 'px';
    box.style.top = (r.bottom + window.scrollY + 6) + 'px';
    btn.setAttribute('aria-expanded', 'true');
    pop = { box, btn };
    const first = box.querySelector('input, a');
    if (first) {
      first.focus({ preventScroll: true });
    }
  }

  // A port jumps to its detail: the parameter row, the body, the optional
  // parameters or the response, opening it when collapsed.
  function findRow(scope, name) {
    if (!scope) {
      return null;
    }
    for (const pn of scope.querySelectorAll('.row-head > .pn')) {
      if (pn.textContent.trim() === name) {
        return pn.closest('.row');
      }
    }
    return null;
  }

  function flash(node) {
    node.classList.remove('flash');
    void node.offsetWidth;
    node.classList.add('flash');
    setTimeout(() => node.classList.remove('flash'), 1500);
  }

  function gotoPort(btn) {
    const port = btn.closest('[data-port]');
    const article = btn.closest('[data-op]');
    if (!port || !article) {
      return;
    }
    const parts = port.dataset.port.split(':');
    let target = null;
    if (parts[0] === 'out') {
      target = Array.from(article.querySelectorAll('.resp[data-status]')).find((r) => r.dataset.status === parts[1]) || null;
      const tg = target && target.querySelector('.resp-head > .tg');
      if (tg && tg.getAttribute('aria-expanded') !== 'true') {
        toggle(tg);
      }
    } else if (parts[1] === 'optional') {
      const tgs = article.querySelectorAll('.zone-send .opt-tg');
      tgs.forEach((tg) => {
        if (tg.getAttribute('aria-expanded') !== 'true') {
          toggle(tg);
        }
      });
      target = tgs.length ? tgs[0].closest('.io') : null;
    } else {
      const io = Array.from(article.querySelectorAll('.zone-send .io[data-in]')).find((n) => n.dataset.in === parts[1]) || null;
      target = (parts[2] && findRow(io, parts[2])) || io;
    }
    if (target) {
      target.scrollIntoView({ block: 'center', behavior: 'smooth' });
      flash(target);
    }
  }

  // Tracing: hovering a port, a model link or a model card lights every port
  // that carries the same model and the sidebar operations that make or take
  // it, through one generated style rule.
  const traceStyle = el('style');
  doc.head.append(traceStyle);
  let traced = '';
  let traceTimer = 0;

  function traceIds(t) {
    const holder = t.closest('.port[data-m], .pfields li[data-m], .model-flow[data-m]');
    if (holder) {
      return holder.dataset.m;
    }
    const link = t.closest('a.ref[href^="#model/"], a.nav-model, .model-title a');
    if (link) {
      return flowData().modelIndex[link.getAttribute('href').slice(1)] || '';
    }
    return '';
  }

  function setTrace(ids) {
    if (ids === traced) {
      return;
    }
    traced = ids;
    if (!ids) {
      traceStyle.textContent = '';
      return;
    }
    const f = flowData();
    const ports = [];
    const nav = [];
    const folders = [];
    const links = [];
    ids.split(' ').forEach((id) => {
      ports.push('.port[data-m~="' + id + '"] > .pbox', '.pfields li[data-m~="' + id + '"]');
      nav.push('.nav-op[data-m~="' + id + '"]');
      folders.push('.nav-folder:has(.nav-op[data-m~="' + id + '"]) > .nav-folder-tg .count', '.nav-group:has(.nav-op[data-m~="' + id + '"]) > .nav-group-tg .count');
      const m = f.models[id];
      if (m) {
        links.push('a.ref[href="#' + m.a + '"]', 'a.nav-model[href="#' + m.a + '"]');
      }
    });
    let css = ports.join(',') + '{border-color:var(--accent)!important;box-shadow:0 0 0 3px var(--accent-soft)!important;border-radius:7px}';
    css += nav.join(',') + '{color:var(--accent);box-shadow:inset 2px 0 0 var(--accent)}';
    css += folders.join(',') + '{background:var(--accent);border-color:var(--accent);color:var(--bg)}';
    if (links.length) {
      css += links.join(',') + '{background:var(--accent-soft);box-shadow:0 0 0 2px var(--accent-soft);border-radius:3px}';
    }
    traceStyle.textContent = css;
  }

  // Sidebar card: hovering an operation shows its recipe.
  let card = null;
  let cardFor = null;
  let cardTimer = 0;

  function hideCard() {
    clearTimeout(cardTimer);
    if (card) {
      card.remove();
      card = null;
    }
  }

  function showCard(a) {
    const f = flowData();
    const i = f.opIndex[a.getAttribute('href').slice(1)];
    const side = sidebar ? sidebar.getBoundingClientRect() : null;
    if (i === undefined || !side || side.right + 360 > window.innerWidth) {
      return;
    }
    const op = f.ops[i];
    card = el('div', 'navcard');
    card.setAttribute('aria-hidden', 'true');
    card.append(opTitle(op, 'navcard-t'), el('div', 'navcard-path', op.p), recipe(op));
    doc.body.append(card);
    const r = a.getBoundingClientRect();
    card.style.left = (side.right + 8) + 'px';
    card.style.top = Math.max(8, Math.min(r.top - 6, window.innerHeight - card.offsetHeight - 8)) + 'px';
  }

  doc.addEventListener('mouseover', (e) => {
    const t = e.target instanceof Element ? e.target : null;
    if (!t) {
      return;
    }
    const ids = traceIds(t);
    clearTimeout(traceTimer);
    traceTimer = setTimeout(() => setTrace(ids), ids ? 90 : 160);

    const a = t.closest('.nav-op');
    if (a !== cardFor) {
      cardFor = a;
      hideCard();
      if (a) {
        cardTimer = setTimeout(() => showCard(a), 300);
      }
    }
  });
  if (sidebar) {
    sidebar.addEventListener('scroll', hideCard, { passive: true });
  }

  // Try it drives the machine: filled inputs light their port, a request
  // runs it, and the status that came back lights its output.
  function machineOf(form) {
    const article = form.closest('[data-op]');
    return article ? article.querySelector('.machine') : null;
  }

  function litPorts(form) {
    const machine = machineOf(form);
    if (!machine) {
      return;
    }
    const filled = {};
    let optional = false;
    form.querySelectorAll('input[data-in]').forEach((input) => {
      const has = input.value.trim() !== '';
      filled['in:' + input.dataset.in + ':' + input.dataset.name] = has;
      if (has && !input.hasAttribute('data-required')) {
        optional = true;
      }
    });
    const body = form.querySelector('[data-body]');
    filled['in:body'] = !!(body && body.value.trim());
    filled['in:optional'] = optional;
    machine.querySelectorAll('.mc-ins > .port').forEach((port) => {
      port.classList.toggle('lit', !!filled[port.dataset.port]);
    });
  }

  function machineState(machine, state, status) {
    if (!machine) {
      return;
    }
    machine.classList.remove('running', 'done', 'jam');
    machine.querySelectorAll('.fired').forEach((p) => p.classList.remove('fired'));
    if (!state) {
      return;
    }
    machine.classList.add(state);
    if (state !== 'done') {
      return;
    }
    const code = String(status);
    const ports = Array.from(machine.querySelectorAll('.mc-outs > .port'));
    const key = (k) => ports.find((p) => p.dataset.port.toLowerCase() === 'out:' + k.toLowerCase());
    const port = key(code) || key(code.charAt(0) + 'xx') || key('default');
    if (port) {
      port.classList.add('fired');
    }
  }

  doc.addEventListener('input', (e) => {
    const form = e.target instanceof Element ? e.target.closest('form.try') : null;
    if (form) {
      litPorts(form);
    }
  });

  // ------------------------------------------------------ delegated clicks

  doc.addEventListener('click', (e) => {
    const t = e.target;
    if (!(t instanceof Element)) {
      return;
    }

    if (pop && !t.closest('.fpop') && !t.closest('[data-pop]')) {
      closePop();
    }
    hideCard();

    const btn = t.closest('button');
    if (btn) {
      if (btn.matches('[data-pop]')) {
        openPop(btn);
      } else if (btn.matches('[data-goto]')) {
        gotoPort(btn);
      } else if (btn.matches('.tg, .nav-group-tg, .nav-folder-tg')) {
        toggle(btn);
      } else if (btn.matches('.jt-tg')) {
        const open = btn.getAttribute('aria-expanded') !== 'true';
        btn.setAttribute('aria-expanded', String(open));
        btn.nextElementSibling.hidden = !open;
      } else if (btn.matches('[data-raw]')) {
        toggleRaw(btn);
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
    // The whole response header toggles its body, not only the status.
    const head = t.closest('[data-resp-toggle]');
    if (head && !a && !window.getSelection().toString()) {
      toggle(head.querySelector(':scope > .tg'));
      return;
    }
    if (!a) {
      if (sidebar && sidebar.classList.contains('open') && !t.closest('.sidebar')) {
        setMenu(false);
      }
      return;
    }
    const href = a.getAttribute('href');
    if (href === '' || href.startsWith('#')) {
      e.preventDefault();
      closePop();
      if (href.length > 1) {
        navigate(a);
      } else {
        window.scrollTo(0, 0);
      }
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
