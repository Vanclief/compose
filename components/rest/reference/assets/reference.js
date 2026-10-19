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
    if (btn.classList.contains('nav-group-tg') || btn.classList.contains('nav-folder-tg')) {
      return btn.nextElementSibling;
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
    if (e.key === 'Escape' && modal) {
      closeModel();
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
      doc.querySelectorAll('form.try[data-touched]').forEach(updateAuthNote);
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

  // The machine is the try-it form. It notes the credentials it will send
  // once it is first used.
  function touchTry(form) {
    if (form.dataset.touched) {
      return;
    }
    form.dataset.touched = '1';
    updateAuthNote(form);
  }

  doc.addEventListener('focusin', (e) => {
    const form = e.target instanceof Element ? e.target.closest('form.try') : null;
    if (form) {
      touchTry(form);
    }
  });

  // The body port opens the body editor, filled with the example on first
  // open.
  function toggleBodyEditor(btn) {
    const form = btn.closest('form.try');
    const box = form ? form.querySelector('.mc-body') : null;
    if (!box) {
      return;
    }
    const open = box.hidden;
    box.hidden = !open;
    btn.setAttribute('aria-expanded', String(open));
    btn.textContent = open ? 'Hide body' : 'Edit body';
    if (!open) {
      return;
    }
    const body = box.querySelector('[data-body]');
    const example = form.closest('[data-op]').querySelector('[data-req-example]');
    if (!body.value && example) {
      body.value = example.textContent;
      litPorts(form);
    }
    body.focus({ preventScroll: true });
  }

  // The optional port opens its inputs in place.
  function toggleOptional(btn) {
    const box = btn.nextElementSibling;
    const open = box.hidden;
    box.hidden = !open;
    btn.setAttribute('aria-expanded', String(open));
    const first = open ? box.querySelector('input') : null;
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
    touchTry(form);
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

    const machine = form;
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
    markFinder([]);
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
    showPop(box, btn);
  }

  function showPop(box, btn) {
    doc.body.append(box);
    const r = btn.getBoundingClientRect();
    const width = box.offsetWidth;
    const left = Math.max(12, Math.min(r.left, window.innerWidth - width - 12));
    // Inside the model dialog the popover follows the fixed dialog.
    if (btn.closest('.mmodal')) {
      box.style.position = 'fixed';
      box.style.left = left + 'px';
      box.style.top = (r.bottom + 6) + 'px';
    } else {
      box.style.left = (left + window.scrollX) + 'px';
      box.style.top = (r.bottom + window.scrollY + 6) + 'px';
    }
    btn.setAttribute('aria-expanded', 'true');
    pop = { box, btn };
    const first = box.querySelector('input, a, button');
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

  // Ports open their details under the machine: the parameters, the body
  // or the response they stand for. Clicking the same port again closes
  // them.
  function detailKey(port) {
    const parts = port.split(':');
    if (parts[0] === 'out') {
      return port;
    }
    return parts[1] === 'body' ? 'body' : 'params';
  }

  function showDetail(machine, port) {
    const key = port ? detailKey(port) : '';
    let open = null;
    machine.querySelectorAll(':scope > .mc-detail').forEach((d) => {
      d.hidden = d.dataset.detail !== key;
      if (!d.hidden) {
        open = d;
      }
    });
    machine.dataset.shown = open ? port : '';
    machine.querySelectorAll('[data-port]').forEach((p) => {
      p.classList.toggle('shown', !!open && p.dataset.port === port);
    });
    return open;
  }

  function gotoPort(btn) {
    const port = btn.closest('[data-port]');
    const machine = btn.closest('.machine');
    if (!port || !machine) {
      return;
    }
    const key = port.dataset.port;
    const detail = showDetail(machine, machine.dataset.shown === key ? '' : key);
    if (!detail) {
      return;
    }
    const parts = key.split(':');
    let target = null;
    if (parts[0] === 'in' && parts[2]) {
      const scope = parts[1] === 'body' ? detail : Array.from(detail.querySelectorAll('.io[data-in]')).find((n) => n.dataset.in === parts[1]);
      target = findRow(scope, parts.slice(2).join(':'));
    }
    detail.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
    if (target) {
      flash(target);
    }
  }

  // Tracing: hovering a port, a model link or a model card lights every port
  // that carries the same model and the sidebar operations that make or take
  // it, through one generated style rule.
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

  // Marks put a class on a computed set of elements and clear it in one
  // batch. Generated selectors (and :has() ones above all) made every click
  // restyle the whole sidebar.
  function marker(cls) {
    let marked = [];
    return (nodes) => {
      marked.forEach((n) => n.classList.remove(cls));
      marked = nodes;
      marked.forEach((n) => n.classList.add(cls));
    };
  }

  // navCounts returns the count pills of the sidebar folders and groups
  // that hold the entries.
  function navCounts(entries) {
    const seen = new Set();
    const counts = [];
    entries.forEach((a) => {
      for (let up = a.parentElement; up && !seen.has(up); up = up.parentElement) {
        if (up.classList.contains('sidebar')) {
          break;
        }
        if (up.classList.contains('nav-folder') || up.classList.contains('nav-group')) {
          seen.add(up);
          const c = up.querySelector(':scope > button .count');
          if (c) {
            counts.push(c);
          }
        }
      }
    });
    return counts;
  }

  const markTracePorts = marker('lit-port');
  const markTraceNav = marker('lit-nav');
  const markTraceCounts = marker('lit-count');
  const markTraceLinks = marker('lit-link');

  function setTrace(ids) {
    if (ids === traced) {
      return;
    }
    traced = ids;
    const ports = [];
    const nav = [];
    const links = [];
    if (ids) {
      const f = flowData();
      ids.split(' ').forEach((id) => {
        doc.querySelectorAll('.port[data-m~="' + id + '"] > .pbox, .pfields li[data-m~="' + id + '"]').forEach((n) => ports.push(n));
        doc.querySelectorAll('.nav-op[data-m~="' + id + '"]').forEach((n) => nav.push(n));
        const m = f.models[id];
        if (m) {
          doc.querySelectorAll('a.ref[href="#' + m.a + '"], a.nav-model[href="#' + m.a + '"]').forEach((n) => links.push(n));
        }
      });
    }
    markTracePorts(ports);
    markTraceNav(nav);
    markTraceCounts(navCounts(nav));
    markTraceLinks(links);
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

  // The machine runs its own request: filled inputs light their port, a
  // request runs it, and the status that came back lights its output.
  function litPorts(machine) {
    const filled = {};
    let optional = false;
    machine.querySelectorAll('input[data-in]').forEach((input) => {
      const has = input.value.trim() !== '';
      filled['in:' + input.dataset.in + ':' + input.dataset.name] = has;
      if (has && !input.hasAttribute('data-required')) {
        optional = true;
      }
    });
    const body = machine.querySelector('[data-body]');
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

  // Path bar: every segment opens a Finder-like browser of the operations
  // beneath it. Folders drill in, the bar on top climbs back out, and the
  // sidebar lights the operations in the folder being browsed.
  const markFinderNav = marker('fd-nav');
  const markFinderCounts = marker('fd-count');
  let navIndex = null;

  function navOp(anchor) {
    if (!navIndex) {
      navIndex = new Map();
      doc.querySelectorAll('.nav-op').forEach((a) => navIndex.set(a.getAttribute('href').slice(1), a));
    }
    return navIndex.get(anchor);
  }

  function markFinder(ops) {
    const nav = ops.map((op) => navOp(op.a)).filter(Boolean);
    markFinderNav(nav);
    markFinderCounts(navCounts(nav));
  }

  function pathSegs(path) {
    return path.split('/').filter(Boolean);
  }

  function isParamSeg(seg) {
    return seg.charAt(0) === '{';
  }

  // under returns the segments of path when it lies beneath prefix, where
  // any two parameters match.
  function under(path, prefix) {
    const segs = pathSegs(path);
    if (segs.length < prefix.length) {
      return null;
    }
    for (let i = 0; i < prefix.length; i++) {
      if (segs[i] !== prefix[i] && !(isParamSeg(segs[i]) && isParamSeg(prefix[i]))) {
        return null;
      }
    }
    return segs;
  }

  function folderIcon() {
    const svg = doc.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('class', 'ico-folder');
    svg.setAttribute('viewBox', '0 0 16 16');
    svg.setAttribute('aria-hidden', 'true');
    const path = doc.createElementNS('http://www.w3.org/2000/svg', 'path');
    path.setAttribute('d', 'M1.75 3.5h4.1l1.5 1.6h6.9v7.4H1.75z');
    svg.append(path);
    return svg;
  }

  function renderFinder(box, prefix, here) {
    box.textContent = '';
    const f = flowData();
    const head = el('div', 'fd-crumbs');
    const root = el('button', 'crumb root', '/');
    root.type = 'button';
    root.dataset.fd = '0';
    head.append(root);
    prefix.forEach((seg, i) => {
      head.append(el('span', 'crumb-sep', '›'));
      const b = el('button', 'crumb' + (isParamSeg(seg) ? ' param' : ''), seg);
      b.type = 'button';
      b.dataset.fd = String(i + 1);
      head.append(b);
    });
    box.append(head);

    const herePath = pathSegs(here.p);
    const ops = [];
    const folders = new Map();
    const inside = [];
    f.ops.forEach((op) => {
      const segs = under(op.p, prefix);
      if (!segs) {
        return;
      }
      inside.push(op);
      if (segs.length === prefix.length) {
        ops.push(op);
        return;
      }
      let next = segs[prefix.length];
      const key = isParamSeg(next) ? '{}' : next;
      if (key === '{}' && under(here.p, prefix) && herePath.length > prefix.length && isParamSeg(herePath[prefix.length])) {
        next = herePath[prefix.length];
      }
      if (!folders.has(key)) {
        folders.set(key, { name: next, count: 0 });
      }
      folders.get(key).count++;
    });

    const list = el('div', 'fpop-list');
    if (ops.length > 0) {
      list.append(el('div', 'fd-sec', 'Operations'));
      ops.forEach((op) => {
        const a = el('a', 'fd-row' + (op.a === here.a ? ' here' : ''));
        a.href = '#' + op.a;
        a.append(el('span', 'm m-' + op.m.toLowerCase(), op.m), el('span', 'fd-name', op.t));
        list.append(a);
      });
    }
    if (folders.size > 0) {
      list.append(el('div', 'fd-sec', 'Folders'));
      Array.from(folders.values()).sort((x, y) => x.name.localeCompare(y.name)).forEach((fd) => {
        const b = el('button', 'fd-row');
        b.type = 'button';
        b.dataset.open = fd.name;
        b.append(folderIcon(), el('span', 'fd-name' + (isParamSeg(fd.name) ? ' param' : ''), fd.name), el('span', 'count', String(fd.count)), el('span', 'fd-go', '›'));
        list.append(b);
      });
    }
    box.append(list);

    box.onclick = (e) => {
      const b = e.target instanceof Element ? e.target.closest('[data-fd], [data-open]') : null;
      if (!b) {
        return;
      }
      e.stopPropagation();
      const next = b.dataset.open !== undefined ? prefix.concat(b.dataset.open) : prefix.slice(0, Number(b.dataset.fd));
      renderFinder(box, next, here);
      const first = box.querySelector('.fpop-list .fd-row');
      if (first) {
        first.focus({ preventScroll: true });
      }
    };

    markFinder(inside);
  }

  function openFinder(btn) {
    const same = pop && pop.btn === btn;
    closePop();
    if (same) {
      return;
    }
    const article = btn.closest('[data-op]');
    const bar = btn.closest('.crumbs');
    const f = flowData();
    const here = f.ops[f.opIndex[article ? article.id : '']] || { a: '', p: bar.dataset.path };
    const box = el('div', 'fpop finder');
    box.setAttribute('role', 'dialog');
    box.setAttribute('aria-label', 'Browse paths');
    renderFinder(box, pathSegs(bar.dataset.path).slice(0, Number(btn.dataset.crumb)), here);
    showPop(box, btn);
  }

  // Model links open the model in a dialog instead of leaving the
  // operation. Links inside it replace its content, with a way back.
  let modal = null;

  function closeModel() {
    if (!modal) {
      return;
    }
    modal.back.remove();
    doc.documentElement.classList.remove('modal-open');
    const opener = modal.opener;
    modal = null;
    if (opener && opener.isConnected) {
      opener.focus({ preventScroll: true });
    }
  }

  function renderModel() {
    const anchor = modal.stack[modal.stack.length - 1];
    const card = doc.getElementById(anchor);
    const box = modal.box;
    box.textContent = '';
    const head = el('div', 'mm-head');
    if (modal.stack.length > 1) {
      const back = el('button', 'btn ghost icon mm-back', '‹');
      back.type = 'button';
      back.setAttribute('aria-label', 'Back');
      back.onclick = () => {
        modal.stack.pop();
        renderModel();
      };
      head.append(back);
    }
    const title = card.querySelector('.model-title a');
    const t = el('span', 'mm-title');
    t.append(el('span', 'ico-obj', '{}'), title ? title.textContent : anchor);
    t.id = 'mm-title';
    head.append(t);
    const open = el('a', 'btn ghost mm-open', 'Open in Models');
    open.href = '#' + anchor;
    const close = el('button', 'mm-close');
    close.type = 'button';
    close.setAttribute('aria-label', 'Close');
    close.title = 'Close (Esc)';
    close.append(el('kbd', '', 'Esc'), el('span', 'mm-x', '×'));
    close.onclick = closeModel;
    head.append(open, close);
    box.append(head);

    const body = el('div', 'mm-body');
    ['.model-name', '.model-flow', ':scope > .tree'].forEach((sel) => {
      const node = card.querySelector(sel);
      if (node) {
        body.append(node.cloneNode(true));
      }
    });
    box.append(body);
    body.querySelectorAll('[data-autofill]').forEach(observeLazy);
    close.focus({ preventScroll: true });
  }

  function openModel(anchor) {
    if (!doc.getElementById(anchor)) {
      return false;
    }
    if (!modal) {
      const back = el('div', 'mmodal-back');
      const box = el('div', 'mmodal');
      box.setAttribute('role', 'dialog');
      box.setAttribute('aria-modal', 'true');
      box.setAttribute('aria-labelledby', 'mm-title');
      back.append(box);
      // Any click outside the dialog closes it.
      back.addEventListener('click', (e) => {
        if (!box.contains(e.target)) {
          closeModel();
        }
      });
      modal = { back, box, stack: [], opener: doc.activeElement };
      doc.body.append(back);
      doc.documentElement.classList.add('modal-open');
    }
    modal.stack.push(anchor);
    renderModel();
    return true;
  }

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
      } else if (btn.matches('[data-crumb]')) {
        openFinder(btn);
      } else if (btn.matches('[data-goto]')) {
        gotoPort(btn);
      } else if (btn.matches('[data-detail-close]')) {
        showDetail(btn.closest('.machine'), '');
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
      } else if (btn.matches('[data-body-tg]')) {
        toggleBodyEditor(btn);
      } else if (btn.matches('[data-opt]')) {
        toggleOptional(btn);
      } else if (btn.matches('[data-theme-toggle]')) {
        toggleTheme();
      } else if (btn.matches('[data-menu]')) {
        setMenu(!sidebar.classList.contains('open'));
      }
      return;
    }

    const a = t.closest('a[href]');
    if (a && a.matches('a[href^="#model/"]') && !a.matches('.mm-open, .nav-model') && !a.closest('.sidebar, .model-title, .map')) {
      e.preventDefault();
      closePop();
      openModel(idFromHref(a.getAttribute('href')));
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
      closeModel();
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

/* ---- map ---- */
// Schema map: the entity graph embedded once as JSON, drawn as cards in rank
// columns with orthogonal edges in one SVG behind them. Pan and zoom move a
// single CSS transform. The click listener runs in the capture phase so map
// links and the view switcher are handled before the reference's own links.
(() => {
  'use strict';

  const doc = document;
  const dataEl = doc.getElementById('schema-map-data');
  const section = doc.getElementById('section/map');
  if (!dataEl || !section) {
    return;
  }
  let data;
  try {
    data = JSON.parse(dataEl.textContent);
  } catch (err) {
    return;
  }

  const CARD_W = 264;
  const COL_GAP = 140;
  const SUB_GAP = 56;
  const ROW_GAP = 28;
  const MAX_COL_H = 1500;
  const MAX_FIELDS = 12;
  const PAD = 48;
  const SVG_NS = 'http://www.w3.org/2000/svg';

  const canvas = section.querySelector('[data-map-canvas]');
  const world = section.querySelector('[data-map-world]');
  const svg = section.querySelector('[data-map-svg]');
  const scopeSelect = section.querySelector('[data-map-scope]');
  const allBtn = section.querySelector('[data-map-all]');
  const countEl = section.querySelector('[data-map-count]');

  const nodes = new Map(data.nodes.map((n) => [n.name, n]));
  const edges = (data.edges || []).filter((e) => nodes.has(e.from) && nodes.has(e.to));
  // Fields an edge starts from, per node: a collapsed card keeps them.
  const edgeFields = new Map();
  for (const e of edges) {
    if (!edgeFields.has(e.from)) {
      edgeFields.set(e.from, new Set());
    }
    edgeFields.get(e.from).add(e.fromField);
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

  function svgEl(tag, attrs) {
    const node = doc.createElementNS(SVG_NS, tag);
    for (const key of Object.keys(attrs)) {
      node.setAttribute(key, attrs[key]);
    }
    return node;
  }

  // ------------------------------------------------------------- scopes

  const scopes = new Map();
  scopes.set('all', { label: 'All models', names: data.nodes.map((n) => n.name) });
  const groups = Array.from(new Set(data.nodes.map((n) => n.group).filter(Boolean))).sort();
  for (const g of groups) {
    scopes.set(g, { label: g, kind: 'Groups', names: data.nodes.filter((n) => n.group === g).map((n) => n.name) });
  }
  for (const t of data.tags || []) {
    scopes.set('tag:' + t.name, { label: t.name, kind: 'Tags', names: t.nodes.filter((n) => nodes.has(n)) });
  }
  const defaultScope = data.nodes.length <= 25 || !groups.length ? 'all' : groups[0];

  function scopeOption(id) {
    const s = scopes.get(id);
    const o = el('option', '', s.label + ' (' + s.names.length + ')');
    o.value = id;
    return o;
  }
  scopeSelect.appendChild(scopeOption('all'));
  for (const kind of ['Groups', 'Tags']) {
    const og = doc.createElement('optgroup');
    og.label = kind;
    for (const [id, s] of scopes) {
      if (s.kind === kind) {
        og.appendChild(scopeOption(id));
      }
    }
    if (og.children.length) {
      scopeSelect.appendChild(og);
    }
  }

  const state = { scope: defaultScope, focus: null, expanded: new Set(), x: 0, y: 0, k: 1 };
  let boxes = new Map();
  let drawn = [];
  let size = { w: 0, h: 0 };
  let hovered = null;
  let isOpen = false;
  let refHash = 'section/introduction';

  // view returns the nodes to draw with how to draw each ('full', 'ghost'
  // for a neighbour outside the scope: header only, 'link' for a neighbour
  // of the focused node: header and the fields pointing at it) and the
  // edges between them. A focused node shows only itself, its one-hop
  // neighbours and the edges it is on.
  function view() {
    const shown = new Map();
    if (state.focus) {
      shown.set(state.focus, 'full');
      const list = edges.filter((e) => e.from === state.focus || e.to === state.focus);
      for (const e of list) {
        for (const end of [e.from, e.to]) {
          if (!shown.has(end)) {
            shown.set(end, 'link');
          }
        }
      }
      return { shown, list };
    }
    const scope = scopes.get(state.scope) || scopes.get('all');
    for (const name of scope.names) {
      shown.set(name, 'full');
    }
    const list = [];
    for (const e of edges) {
      if (shown.get(e.from) !== 'full' && shown.get(e.to) !== 'full') {
        continue;
      }
      list.push(e);
      for (const end of [e.from, e.to]) {
        if (!shown.has(end)) {
          shown.set(end, 'ghost');
        }
      }
    }
    return { shown, list };
  }

  // ------------------------------------------------------------- layout

  // ranks places every node one column right of the furthest node pointing
  // at it. Depth-first search from the roots (no incoming edge) drops the
  // edges that close a cycle; its finish order reversed is topological.
  function ranks(names, list) {
    const adj = new Map(names.map((n) => [n, []]));
    const indeg = new Map(names.map((n) => [n, 0]));
    for (const e of list) {
      if (e.from !== e.to) {
        adj.get(e.from).push(e.to);
        indeg.set(e.to, indeg.get(e.to) + 1);
      }
    }
    const mark = new Map();
    const order = [];
    const dag = new Map(names.map((n) => [n, []]));
    const visit = (n) => {
      mark.set(n, 1);
      for (const m of adj.get(n)) {
        if (!mark.has(m)) {
          dag.get(n).push(m);
          visit(m);
        } else if (mark.get(m) === 2) {
          dag.get(n).push(m);
        }
      }
      mark.set(n, 2);
      order.push(n);
    };
    const byOut = names.slice().sort((a, b) => adj.get(b).length - adj.get(a).length);
    for (const n of names.filter((m) => indeg.get(m) === 0).concat(byOut)) {
      if (!mark.has(n)) {
        visit(n);
      }
    }
    const rank = new Map(names.map((n) => [n, 0]));
    for (let i = order.length - 1; i >= 0; i--) {
      for (const m of dag.get(order[i])) {
        rank.set(m, Math.max(rank.get(m), rank.get(order[i]) + 1));
      }
    }
    return rank;
  }

  function fieldRow(f) {
    const li = el('li', f.deprecated ? 'mf deprecated' : 'mf');
    li.dataset.field = f.name;
    const name = el('span', 'mf-name', f.name);
    if (f.optional || f.nullable) {
      name.appendChild(el('span', 'mf-opt', '?'));
    }
    li.appendChild(name);
    const quals = [];
    if (f.readOnly) {
      quals.push('read-only');
    }
    if (f.writeOnly) {
      quals.push('write-only');
    }
    if (f.deprecated) {
      quals.push('deprecated');
    }
    if (quals.length) {
      li.appendChild(el('span', 'mf-q', quals.join(' ')));
    }
    const target = f.ref ? nodes.get(f.ref) : null;
    if (target) {
      const ref = el('button', 'mf-ref', '→ ' + target.short + (/\[\]$/.test(f.type || '') ? '[]' : ''));
      ref.type = 'button';
      ref.dataset.mapRef = f.ref;
      ref.title = 'Focus ' + target.short;
      li.appendChild(ref);
    } else if (f.type) {
      li.appendChild(el('span', 'mf-type', f.type));
    }
    if (f.enum && f.enum.length) {
      const chips = el('span', 'mf-enum');
      chips.title = f.enum.join(', ');
      for (const v of f.enum.slice(0, 3)) {
        chips.appendChild(el('span', 'mchip', v));
      }
      if (f.enum.length > 3) {
        chips.appendChild(el('span', 'mchip more', '+' + (f.enum.length - 3) + ' values'));
      }
      li.appendChild(chips);
    }
    return li;
  }

  function card(n, mode) {
    const c = el('div', 'mcard' + (mode === 'ghost' ? ' ghost' : '') + (n.name === state.focus ? ' focus' : ''));
    c.dataset.node = n.name;
    c.tabIndex = 0;
    c.title = n.name;
    c.style.width = CARD_W + 'px';
    const head = el('div', 'mcard-head');
    head.appendChild(el('span', 'mcard-name', n.short));
    if (n.group) {
      head.appendChild(el('span', 'mcard-group', n.group));
    }
    c.appendChild(head);
    if (mode === 'ghost') {
      return c;
    }
    const fields = n.fields || [];
    if (mode === 'link') {
      const ul = el('ul', 'mcard-fields');
      for (const f of fields) {
        if (f.ref === state.focus) {
          ul.appendChild(fieldRow(f));
        }
      }
      if (ul.children.length) {
        c.appendChild(ul);
      }
      return c;
    }
    const all = state.expanded.has(n.name);
    let rows = fields;
    if (!all && fields.length > MAX_FIELDS) {
      // Keep id, then the fields edges start from, then other references,
      // then the rest; show the kept ones in document order.
      const linked = edgeFields.get(n.name) || new Set();
      const weight = (f) => (f.name === 'id' ? 0 : linked.has(f.name) ? 1 : f.ref ? 2 : 3);
      const keep = new Set(fields
        .map((f, i) => ({ f, i, w: weight(f) }))
        .sort((a, b) => a.w - b.w || a.i - b.i)
        .slice(0, MAX_FIELDS)
        .map((x) => x.f));
      rows = fields.filter((f) => keep.has(f));
    }
    const ul = el('ul', 'mcard-fields');
    for (const f of rows) {
      ul.appendChild(fieldRow(f));
    }
    c.appendChild(ul);
    if (fields.length > MAX_FIELDS) {
      const more = el('button', 'mcard-more', all ? 'Show fewer' : '+' + (fields.length - MAX_FIELDS) + ' more');
      more.type = 'button';
      more.dataset.mapMore = n.name;
      c.appendChild(more);
    }
    return c;
  }

  function layout(shown, list) {
    world.querySelectorAll('.mcard').forEach((c) => c.remove());
    boxes = new Map();
    const names = Array.from(shown.keys());
    for (const name of names) {
      const c = card(nodes.get(name), shown.get(name));
      world.appendChild(c);
      boxes.set(name, { el: c, x: 0, y: 0, h: 0 });
    }
    for (const b of boxes.values()) {
      b.h = b.el.offsetHeight;
    }

    const rank = ranks(names, list);
    const cols = [];
    for (const n of names) {
      const r = rank.get(n);
      (cols[r] = cols[r] || []).push(n);
    }
    // Order each column by the mean position of its neighbours, sweeping
    // right then left, to keep edges short and crossings few.
    const pred = new Map(names.map((n) => [n, []]));
    const succ = new Map(names.map((n) => [n, []]));
    for (const e of list) {
      if (e.from !== e.to) {
        succ.get(e.from).push(e.to);
        pred.get(e.to).push(e.from);
      }
    }
    const pos = new Map();
    const settle = (col) => col.forEach((n, i) => pos.set(n, (i + 0.5) / col.length));
    const sortBy = (col, nb) => {
      const score = new Map(col.map((n) => {
        const ps = nb.get(n);
        return [n, ps.length ? ps.reduce((t, m) => t + pos.get(m), 0) / ps.length : pos.get(n)];
      }));
      col.sort((a, b) => score.get(a) - score.get(b));
      settle(col);
    };
    cols.forEach(settle);
    for (let pass = 0; pass < 2; pass++) {
      for (let r = 1; r < cols.length; r++) {
        sortBy(cols[r], pred);
      }
      for (let r = cols.length - 2; r >= 0; r--) {
        sortBy(cols[r], succ);
      }
    }

    // Stack each column; a column taller than MAX_COL_H wraps into
    // sub-columns side by side.
    const placed = [];
    let x = PAD;
    let maxH = 0;
    for (const col of cols) {
      const subs = [[]];
      let h = 0;
      for (const n of col) {
        const bh = boxes.get(n).h;
        if (h + bh > MAX_COL_H && subs[subs.length - 1].length) {
          subs.push([]);
          h = 0;
        }
        subs[subs.length - 1].push(n);
        h += bh + ROW_GAP;
      }
      subs.forEach((sub, i) => {
        const colH = sub.reduce((t, n) => t + boxes.get(n).h + ROW_GAP, -ROW_GAP);
        placed.push({ x, sub, h: colH });
        maxH = Math.max(maxH, colH);
        x += CARD_W + (i < subs.length - 1 ? SUB_GAP : COL_GAP);
      });
    }
    for (const c of placed) {
      let y = PAD + (maxH - c.h) / 2;
      for (const n of c.sub) {
        const b = boxes.get(n);
        b.x = c.x;
        b.y = y;
        b.el.style.left = b.x + 'px';
        b.el.style.top = b.y + 'px';
        y += b.h + ROW_GAP;
      }
    }
    size = { w: x - COL_GAP + PAD, h: maxH + 2 * PAD };
  }

  // anchorY is where an edge meets a card: the row of its field, the
  // "+N more" row when that field is collapsed, otherwise the header.
  function anchorY(name, field) {
    const b = boxes.get(name);
    let row = field ? b.el.querySelector('[data-field="' + CSS.escape(field) + '"]') : null;
    if (field && !row) {
      row = b.el.querySelector('.mcard-more');
    }
    const target = row || b.el.querySelector('.mcard-head');
    return b.y + target.offsetTop + target.offsetHeight / 2;
  }

  // laneRoom is the narrowest gap a group of edges crosses.
  function laneRoom(lane) {
    let room = COL_GAP;
    for (const it of lane) {
      if (it.side !== 's') {
        room = Math.min(room, Math.abs(it.x2 - it.x1));
      }
    }
    return room;
  }

  function draw(list) {
    svg.textContent = '';
    svg.setAttribute('width', size.w);
    svg.setAttribute('height', size.h);
    drawn = [];
    const items = list.map((e) => {
      const s = boxes.get(e.from);
      const t = boxes.get(e.to);
      const it = { e, y1: anchorY(e.from, e.fromField), y2: anchorY(e.to, '') };
      if (e.from !== e.to && t.x > s.x + CARD_W) {
        Object.assign(it, { side: 'r', x1: s.x + CARD_W, x2: t.x });
      } else if (e.from !== e.to && t.x + CARD_W < s.x) {
        Object.assign(it, { side: 'l', x1: s.x, x2: t.x + CARD_W });
      } else {
        Object.assign(it, { side: 's', x1: s.x + CARD_W, x2: t.x + CARD_W });
      }
      return it;
    });
    // Edges entering the same gap get their own vertical lane in it.
    const lanes = new Map();
    for (const it of items) {
      const key = it.side + Math.round(it.x2);
      if (!lanes.has(key)) {
        lanes.set(key, []);
      }
      lanes.get(key).push(it);
    }
    for (const lane of lanes.values()) {
      lane.sort((a, b) => a.y1 - b.y1);
      const step = lane.length > 1 ? Math.max(4, Math.min(12, (laneRoom(lane) - 32) / (lane.length - 1))) : 0;
      lane.forEach((it, i) => {
        const off = 16 + i * step;
        if (it.side === 'r') {
          it.mid = it.x2 - off;
        } else if (it.side === 'l') {
          it.mid = it.x2 + off;
        } else {
          it.mid = Math.max(it.x1, it.x2) + off;
        }
      });
    }
    // Labels avoid cards and each other; one with no free spot along its
    // segment only shows while its edge is highlighted.
    const taken = Array.from(boxes.values(), (b) => ({ x: b.x, y: b.y, w: CARD_W, h: b.h }));
    const free = (r) => !taken.some((o) => r.x < o.x + o.w && o.x < r.x + r.w && r.y < o.y + o.h && o.y < r.y + r.h);
    for (const it of items) {
      const g = svgEl('g', { class: 'medge' + (it.e.inferred ? ' inferred' : '') });
      g.appendChild(svgEl('path', { d: 'M' + it.x1 + ' ' + it.y1 + 'H' + it.mid + 'V' + it.y2 + 'H' + it.x2 }));
      g.appendChild(svgEl('circle', { cx: it.x2, cy: it.y2, r: 3 }));
      const text = it.e.cardinality;
      const w = text.length * 6.4 + 8;
      const spots = [];
      if (Math.abs(it.y2 - it.y1) < 24) {
        spots.push([(it.x1 + it.mid) / 2, it.y1 - 8]);
      } else {
        const lo = Math.min(it.y1, it.y2) + 10;
        const hi = Math.max(it.y1, it.y2) - 10;
        for (const d of [0, 16, -16, 32, -32, 56, -56, 88, -88]) {
          const y = (it.y1 + it.y2) / 2 + d;
          if (y >= lo && y <= hi) {
            spots.push([it.mid, y]);
          }
        }
      }
      let spot = spots.find(([x, y]) => free({ x: x - w / 2, y: y - 7, w, h: 14 }));
      const crowded = !spot;
      spot = spot || spots[0] || [it.mid, (it.y1 + it.y2) / 2];
      if (!crowded) {
        taken.push({ x: spot[0] - w / 2, y: spot[1] - 7, w, h: 14 });
      }
      const label = svgEl('text', { x: spot[0], y: spot[1], class: crowded ? 'crowded' : '' });
      label.textContent = text;
      g.appendChild(label);
      const title = svgEl('title', {});
      title.textContent = nodes.get(it.e.from).short + '.' + (it.e.fromField || '') + ' → ' + nodes.get(it.e.to).short + (it.e.inferred ? ' (inferred)' : '');
      g.appendChild(title);
      svg.appendChild(g);
      drawn.push({ e: it.e, g });
    }
  }

  function render(refit) {
    const { shown, list } = view();
    layout(shown, list);
    draw(list);
    hovered = undefined;
    highlight(null);
    let ghosts = 0;
    shown.forEach((mode) => {
      ghosts += mode === 'ghost' ? 1 : 0;
    });
    countEl.textContent = (shown.size - ghosts) + ' models · ' + list.length + ' relations' + (ghosts ? ' · ' + ghosts + ' linked from elsewhere' : '');
    if (refit) {
      fit(0.3);
    }
  }

  // ------------------------------------------------------- pan and zoom

  function apply() {
    world.style.transform = 'translate(' + state.x + 'px, ' + state.y + 'px) scale(' + state.k + ')';
  }

  function zoomAt(k, cx, cy) {
    const next = Math.min(2, Math.max(0.1, k));
    state.x = cx - (cx - state.x) * next / state.k;
    state.y = cy - (cy - state.y) * next / state.k;
    state.k = next;
    apply();
  }

  // fit scales the drawing into the canvas above the legend; below min it
  // keeps min and starts at the top left instead.
  function fit(min) {
    const legend = section.querySelector('.map-legend');
    const w = canvas.clientWidth;
    const h = canvas.clientHeight - (legend ? legend.offsetHeight + 14 : 0);
    const k = Math.min(1, w / size.w, h / size.h);
    if (k < min) {
      state.k = min;
      state.x = 0;
      state.y = 0;
    } else {
      state.k = k;
      state.x = (w - size.w * k) / 2;
      state.y = Math.max(0, (h - size.h * k) / 2);
    }
    apply();
  }

  canvas.addEventListener('wheel', (e) => {
    e.preventDefault();
    const r = canvas.getBoundingClientRect();
    const speed = e.deltaMode === 1 ? 0.05 : 0.0015;
    zoomAt(state.k * Math.exp(-e.deltaY * speed), e.clientX - r.left, e.clientY - r.top);
  }, { passive: false });

  let drag = null;
  let suppressClick = false;
  canvas.addEventListener('pointerdown', (e) => {
    if (e.button !== 0 || e.target.closest('button, select, a')) {
      return;
    }
    drag = { id: e.pointerId, sx: e.clientX, sy: e.clientY, x: state.x, y: state.y, moved: false };
  });
  canvas.addEventListener('pointermove', (e) => {
    if (!drag || e.pointerId !== drag.id) {
      return;
    }
    const dx = e.clientX - drag.sx;
    const dy = e.clientY - drag.sy;
    if (!drag.moved && Math.abs(dx) + Math.abs(dy) < 4) {
      return;
    }
    if (!drag.moved) {
      drag.moved = true;
      canvas.setPointerCapture(e.pointerId);
      canvas.classList.add('panning');
      highlight(null);
    }
    state.x = drag.x + dx;
    state.y = drag.y + dy;
    apply();
  });
  const endDrag = (e) => {
    if (!drag || e.pointerId !== drag.id) {
      return;
    }
    if (drag.moved) {
      suppressClick = true;
      setTimeout(() => {
        suppressClick = false;
      }, 0);
      canvas.classList.remove('panning');
    }
    drag = null;
  };
  canvas.addEventListener('pointerup', endDrag);
  canvas.addEventListener('pointercancel', endDrag);

  // ------------------------------------------------- hover and focus

  function highlight(name) {
    if (name === hovered) {
      return;
    }
    hovered = name;
    world.classList.toggle('dim', Boolean(name));
    const near = new Set(name ? [name] : []);
    for (const d of drawn) {
      const on = Boolean(name) && (d.e.from === name || d.e.to === name);
      d.g.classList.toggle('hl', on);
      if (on) {
        near.add(d.e.from);
        near.add(d.e.to);
      }
    }
    for (const [n, b] of boxes) {
      b.el.classList.toggle('hl', near.has(n));
    }
  }
  canvas.addEventListener('pointerover', (e) => {
    if (drag && drag.moved) {
      return;
    }
    const c = e.target.closest('.mcard');
    highlight(c ? c.dataset.node : null);
  });
  canvas.addEventListener('pointerleave', () => highlight(null));

  function setFocus(name) {
    state.focus = name && nodes.has(name) ? name : null;
    allBtn.hidden = !state.focus;
    render(true);
    setHash(hashFor(), false);
  }

  function openModel(name) {
    const n = nodes.get(name);
    if (!n) {
      return;
    }
    close();
    // The reference's own link handler scrolls, marks the sidebar and
    // updates the hash.
    const a = el('a');
    a.href = '#' + n.anchor;
    a.hidden = true;
    doc.body.appendChild(a);
    a.click();
    a.remove();
  }

  let clickTimer = 0;
  section.addEventListener('click', (e) => {
    const t = e.target;
    if (suppressClick || !(t instanceof Element)) {
      return;
    }
    const zoom = t.closest('[data-map-zoom]');
    if (zoom) {
      const dir = Number(zoom.dataset.mapZoom);
      if (dir === 0) {
        fit(0.1);
      } else {
        zoomAt(state.k * (dir > 0 ? 1.25 : 0.8), canvas.clientWidth / 2, canvas.clientHeight / 2);
      }
      return;
    }
    if (t.closest('[data-map-all]')) {
      setFocus(null);
      return;
    }
    const more = t.closest('[data-map-more]');
    if (more) {
      const name = more.dataset.mapMore;
      if (state.expanded.has(name)) {
        state.expanded.delete(name);
      } else {
        state.expanded.add(name);
      }
      render(false);
      return;
    }
    const ref = t.closest('[data-map-ref]');
    if (ref) {
      setFocus(ref.dataset.mapRef);
      return;
    }
    const c = t.closest('.mcard');
    if (c && c.dataset.node !== state.focus) {
      clearTimeout(clickTimer);
      clickTimer = setTimeout(() => setFocus(c.dataset.node), 240);
    }
  });
  section.addEventListener('dblclick', (e) => {
    const c = e.target instanceof Element ? e.target.closest('.mcard') : null;
    if (c) {
      clearTimeout(clickTimer);
      openModel(c.dataset.node);
    }
  });
  section.addEventListener('keydown', (e) => {
    const c = e.target instanceof Element ? e.target.closest('.mcard') : null;
    if (e.key === 'Enter' && c && e.target === c) {
      setFocus(c.dataset.node);
    } else if (e.key === 'Escape' && state.focus) {
      setFocus(null);
    }
  });
  scopeSelect.addEventListener('change', () => {
    state.scope = scopeSelect.value;
    setFocus(null);
  });

  // ------------------------------------------------- open, close, hash

  function setHash(id, push) {
    try {
      if (push) {
        history.pushState(null, '', '#' + id);
      } else {
        history.replaceState(null, '', '#' + id);
      }
    } catch (err) {
      // about:srcdoc and sandboxed frames may refuse history updates.
    }
  }

  function hashFor() {
    let id = 'map/' + encodeURIComponent(state.scope);
    if (state.focus) {
      id += '/' + encodeURIComponent(state.focus);
    }
    return id;
  }

  function isMapHash(h) {
    return h === '#map' || h.startsWith('#map/');
  }

  function decode(s) {
    try {
      return decodeURIComponent(s);
    } catch (err) {
      return s;
    }
  }

  // place lays the map over the content pane, beside the sidebar and below
  // the mobile top bar, whatever their size.
  function place() {
    const sidebar = doc.getElementById('sidebar');
    const bar = doc.querySelector('.topbar');
    const left = sidebar && getComputedStyle(sidebar).position !== 'fixed' ? sidebar.getBoundingClientRect().right : 0;
    const top = bar && bar.offsetParent !== null ? bar.getBoundingClientRect().bottom : 0;
    section.style.left = Math.max(0, left) + 'px';
    section.style.top = Math.max(0, top) + 'px';
  }

  function setSwitch(map) {
    doc.querySelectorAll('[data-view]').forEach((a) => {
      if ((a.dataset.view === 'map') === map) {
        a.setAttribute('aria-current', 'page');
      } else {
        a.removeAttribute('aria-current');
      }
    });
  }

  // open shows the map; push is true to add a history entry, false to
  // replace the current one and null to leave the hash alone.
  function open(scope, focus, push) {
    if (!isOpen) {
      if (location.hash.length > 1 && !isMapHash(location.hash)) {
        refHash = location.hash.slice(1);
      }
      isOpen = true;
      section.hidden = false;
      doc.body.classList.add('map-open');
      setSwitch(true);
      const sidebar = doc.getElementById('sidebar');
      if (sidebar) {
        sidebar.classList.remove('open');
      }
      place();
    }
    state.scope = scopes.has(scope) ? scope : state.scope;
    state.focus = focus && nodes.has(focus) ? focus : null;
    scopeSelect.value = state.scope;
    allBtn.hidden = !state.focus;
    render(true);
    if (push !== null) {
      setHash(hashFor(), push);
    }
  }

  function openHash(h, push) {
    const parts = h.slice(1).split('/').map(decode);
    open(parts[1] || '', parts[2] || '', push);
  }

  function close() {
    if (!isOpen) {
      return;
    }
    isOpen = false;
    section.hidden = true;
    doc.body.classList.remove('map-open');
    setSwitch(false);
    highlight(null);
  }

  doc.addEventListener('click', (e) => {
    const t = e.target instanceof Element ? e.target : null;
    if (!t) {
      return;
    }
    const sw = t.closest('[data-view]');
    if (sw) {
      e.preventDefault();
      e.stopPropagation();
      if (sw.dataset.view === 'map') {
        open('', '', true);
      } else if (isOpen) {
        close();
        setHash(refHash, true);
      }
      return;
    }
    const a = t.closest('a[href^="#"]');
    if (!a) {
      return;
    }
    if (isMapHash(a.getAttribute('href'))) {
      e.preventDefault();
      e.stopPropagation();
      openHash(a.getAttribute('href'), true);
    } else if (isOpen && !section.contains(a)) {
      close();
    }
  }, true);

  window.addEventListener('popstate', () => {
    if (isMapHash(location.hash)) {
      openHash(location.hash, null);
    } else {
      close();
    }
  });
  window.addEventListener('resize', () => {
    if (isOpen) {
      place();
    }
  });

  if (isMapHash(location.hash)) {
    openHash(location.hash, false);
  }
})();

// ---- diff ----
// Diff mode (rendered with a base spec): the Show changes toggle, the
// Changes only sidebar filter and opening the collapsed responses and rows
// that hold marked changes. It talks to the client above only through the
// DOM, by clicking the toggles that client already handles.
(() => {
  'use strict';

  const doc = document;
  const body = doc.body;
  const showBtn = doc.querySelector('[data-diff-toggle]');
  if (!showBtn) {
    return;
  }

  function setShow(on) {
    body.classList.toggle('diff', on);
    showBtn.setAttribute('aria-pressed', String(on));
  }

  // Changes only hides unchanged sidebar entries (in CSS) and opens the
  // groups and folders that hold changes.
  function setOnly(on) {
    body.classList.toggle('changes-only', on);
    if (!on) {
      return;
    }
    doc.querySelectorAll('.sidebar .nav-group-tg, .sidebar .nav-folder-tg').forEach((btn) => {
      if (btn.getAttribute('aria-expanded') !== 'true' && btn.parentElement.querySelector('.diff-op')) {
        btn.click();
      }
    });
  }

  doc.addEventListener('click', (e) => {
    const btn = e.target instanceof Element ? e.target.closest('[data-diff-toggle]') : null;
    if (btn) {
      setShow(!body.classList.contains('diff'));
    }
  });

  doc.addEventListener('change', (e) => {
    const t = e.target;
    if (t instanceof HTMLInputElement && t.hasAttribute('data-diff-only')) {
      setOnly(t.checked);
    }
  });

  const only = doc.querySelector('[data-diff-only]');
  if (only && only.checked) {
    setOnly(true);
  }

  // toggleFor returns the button that shows a collapsed container.
  function toggleFor(box) {
    if (box.classList.contains('row-kids')) {
      return box.parentElement.querySelector(':scope > .row-head > .tg');
    }
    return null;
  }

  // Marked rows inside an operation start visible: open every collapsed
  // response, optional-parameter list and row above them.
  doc.querySelectorAll('.op:not(.diff-removed) .row.diff-add, .op:not(.diff-removed) .row.diff-del, .op:not(.diff-removed) .row.diff-chg').forEach((row) => {
    for (let up = row.parentElement; up && !up.classList.contains('op'); up = up.parentElement) {
      if (!up.hidden) {
        continue;
      }
      const btn = toggleFor(up);
      if (btn && btn.getAttribute('aria-expanded') !== 'true') {
        btn.click();
      }
    }
  });
})();
