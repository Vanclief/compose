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
    const article = set.closest('.op-samples') ? set.closest('[data-op]') : null;
    if (article && responseTab(article, tab.textContent.trim()) === tab) {
      markResponse(article, tab.textContent.trim());
    }
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
    // Backspace or Alt+Left steps the model dialog back along its trail.
    if (modal && modal.stack.length > 1 && !typing && ((e.key === 'Backspace' && !e.metaKey && !e.ctrlKey && !e.altKey) || (e.key === 'ArrowLeft' && e.altKey))) {
      e.preventDefault();
      modelBack(modal.stack.length - 1);
      return;
    }
    if (e.key === '/' && !typing && !e.metaKey && !e.ctrlKey && !e.altKey && !body.classList.contains('map-open')) {
      e.preventDefault();
      if (body.dataset.mode === 'docs') {
        const tab = doc.querySelector('.modes a[data-mode="endpoints"]');
        if (tab) {
          openMode(tab);
        }
      }
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

  // ----------------------------------------------------------------- modes

  // The page is three views of one document, one shown at a time:
  // Documentation, Endpoint Reference and Models, each a pane of the content
  // and of the sidebar. The schema map (below) covers them while it is open.
  // A link shows the view holding its target; a view's tab returns to where
  // that view was left.
  const body = doc.body;
  const modeScroll = {};
  const modeHash = {};
  const searchHints = { endpoints: 'Search endpoints', models: 'Search models' };
  let mapWasOpen = false;

  function modeOf(node) {
    const pane = node ? node.closest('[data-pane]') : null;
    return pane ? pane.dataset.pane : '';
  }

  function markModes() {
    const current = body.classList.contains('map-open') ? 'map' : body.dataset.mode;
    doc.querySelectorAll('.modes a[data-mode]').forEach((a) => {
      if (a.dataset.mode === current) {
        a.setAttribute('aria-current', 'page');
      } else {
        a.removeAttribute('aria-current');
      }
    });
  }

  // setMode shows a view, remembering the scroll position and the active
  // anchor of the one it leaves.
  function setMode(mode) {
    const from = body.dataset.mode;
    if (mode && mode !== from) {
      modeScroll[from] = window.scrollY;
      if (activeId && modeOf(doc.getElementById(activeId)) === from) {
        modeHash[from] = activeId;
      }
      body.dataset.mode = mode;
      if (searchInput) {
        searchInput.placeholder = (searchHints[mode] || 'Search') + '  /';
        if (searchInput.value) {
          runSearch();
        }
      }
    }
    markModes();
  }

  // openMode follows a view's tab: back to the view's last place, or to
  // the tab's target the first time. The tab of the view the map covered
  // just closes the map.
  function openMode(tab) {
    const mode = tab.dataset.mode;
    if (mode === body.dataset.mode && mapWasOpen) {
      markModes();
      if (activeId) {
        setHash(activeId, true);
      }
      return;
    }
    if (mode !== body.dataset.mode && modeScroll[mode] !== undefined) {
      setMode(mode);
      window.scrollTo(0, modeScroll[mode]);
      if (modeHash[mode]) {
        setHash(modeHash[mode], true);
        setActive(modeHash[mode], false);
      }
      setMenu(false);
      return;
    }
    navigate(tab);
  }

  // The map closes itself in its own capture listener, which runs after
  // this one: note whether it was open for openMode.
  doc.addEventListener('click', () => {
    mapWasOpen = body.classList.contains('map-open');
  }, true);

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
    setMode(modeOf(target));
    revealModelOrOp(target);
    // Jump instantly: smooth scrolling across a 180-operation page is slow
    // and lets the scroll spy rewrite the hash on the way.
    target.scrollIntoView({ block: 'start' });
    pin(target);
    setHash(id, true);
    setActive(id, false);
    if (!target.hasAttribute('tabindex')) {
      target.setAttribute('tabindex', '-1');
    }
    target.focus({ preventScroll: true });
    setMenu(false);
  }

  // pin keeps a target jumped to at the top while inspectors around it
  // render and change the page's height: the browser's scroll anchoring
  // holds the element half under the app bar instead. It lets go when the
  // user scrolls, or after a second.
  let unpin = null;

  function pin(target) {
    if (unpin) {
      unpin();
    }
    if (!('ResizeObserver' in window)) {
      return;
    }
    const main = doc.getElementById('main');
    const ro = new ResizeObserver(() => target.scrollIntoView({ block: 'start' }));
    const stop = () => {
      ro.disconnect();
      clearTimeout(timer);
      ['wheel', 'touchstart', 'keydown', 'mousedown'].forEach((type) => window.removeEventListener(type, stop, true));
      unpin = null;
    };
    const timer = setTimeout(stop, 1000);
    ['wheel', 'touchstart', 'keydown', 'mousedown'].forEach((type) => window.addEventListener(type, stop, { capture: true, passive: true }));
    ro.observe(main);
    unpin = stop;
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

  // Group and folder names take part in search, so "cards" also finds the
  // operations filed under a cards folder.
  doc.querySelectorAll('.sidebar a[data-search]').forEach((a) => {
    const names = [];
    for (let li = a.closest('.nav-folder'); li; li = li.parentElement.closest('.nav-folder')) {
      names.push(li.querySelector(':scope > .nav-folder-tg .nav-folder-name').textContent);
    }
    const group = a.closest('.nav-group');
    if (group) {
      names.push(group.querySelector(':scope > .nav-group-tg .nav-group-name').textContent);
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
      // of the viewport, below the app bar and the scroll padding that keeps
      // a jumped-to anchor clear of it.
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
    }, { rootMargin: '-72px 0px -70% 0px' });
    doc.querySelectorAll('[data-anchor]').forEach((node) => spy.observe(node));
    // Do not rewrite the hash until the user actually scrolls.
    window.addEventListener('scroll', () => {
      spyReady = true;
    }, { once: true, passive: true });
  }

  // showHash shows the anchor of the current hash, in its view: on load
  // and when history moves. The map handles its own hashes.
  function showHash() {
    if (!location.hash || location.hash.length < 2 || location.hash.startsWith('#map')) {
      markModes();
      return;
    }
    const id = idFromHref(location.hash);
    const target = doc.getElementById(id);
    if (target) {
      setMode(modeOf(target));
      target.scrollIntoView({ block: 'start' });
      pin(target);
      setActive(id, false);
    }
  }

  window.addEventListener('popstate', showHash);

  // --------------------------------------------------------------- search

  const searchInput = doc.getElementById('nav-search');
  const noResults = doc.querySelector('.no-results');

  function navPane() {
    return doc.querySelector('.nav-pane[data-pane="' + body.dataset.mode + '"]');
  }

  // runSearch filters the sidebar of the current view: links match every
  // term in their own text or their folders' names, and groups and folders
  // open around the matches.
  function runSearch() {
    const pane = navPane();
    if (!pane) {
      return;
    }
    const terms = searchInput.value.trim().toLowerCase().split(/\s+/).filter(Boolean);
    let total = 0;
    pane.querySelectorAll('a[data-search]').forEach((a) => {
      const hay = a.dataset.search + ' ' + (a.dataset.folders || '');
      const match = terms.every((t) => hay.includes(t));
      a.parentElement.hidden = !match;
      if (match) {
        total++;
      }
    });
    pane.querySelectorAll('.nav-folder, .nav-group').forEach((box) => {
      const btn = box.querySelector(':scope > .nav-folder-tg, :scope > .nav-group-tg');
      if (terms.length > 0) {
        const any = Array.from(box.querySelectorAll('a[data-search]')).some((a) => !a.parentElement.hidden);
        box.hidden = !any;
        setExpanded(btn, any);
      } else {
        box.hidden = false;
        setExpanded(btn, btn.dataset.user ? btn.dataset.user === 'open' : box.querySelector('a.active') !== null);
      }
    });
    noResults.hidden = terms.length === 0 || total > 0;
  }

  if (searchInput) {
    searchInput.addEventListener('input', runSearch);
    searchInput.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') {
        searchInput.value = '';
        runSearch();
      } else if (e.key === 'Enter') {
        const pane = navPane();
        const first = pane ? Array.from(pane.querySelectorAll('li:not([hidden]) > a[data-search]')).find((a) => !a.closest('[hidden]')) : null;
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

  // The body port opens the raw JSON editor. With a field tree (the
  // ObjectInspector with inputs), the editor starts from what the tree
  // holds and hands its JSON back on close; without one it starts from the
  // example.
  function toggleBodyEditor(btn) {
    const form = btn.closest('form.try');
    const box = form ? form.querySelector('.mc-body') : null;
    if (!box) {
      return;
    }
    const open = box.hidden;
    const tree = form.querySelector('[data-btree] > .oi-list');
    const body = box.querySelector('[data-body]');
    if (tree && !open) {
      let v;
      try {
        v = body.value.trim() === '' ? undefined : JSON.parse(body.value);
      } catch (err) {
        body.classList.add('invalid');
        body.focus({ preventScroll: true });
        return;
      }
      body.classList.remove('invalid');
      fillTree(tree, v);
      body.value = '';
    }
    box.hidden = !open;
    btn.setAttribute('aria-expanded', String(open));
    if (tree) {
      tree.parentElement.hidden = open;
      btn.textContent = open ? 'Edit as fields' : 'Edit as JSON';
    } else {
      btn.textContent = open ? 'Hide body' : 'Edit body';
    }
    if (!open) {
      litPorts(form);
      return;
    }
    const example = form.closest('[data-op]').querySelector('[data-req-example]');
    const v = tree ? treeValue(tree) : undefined;
    if (v !== undefined) {
      body.value = JSON.stringify(v, null, 2);
    } else if (!body.value && example) {
      body.value = example.textContent;
    }
    litPorts(form);
    body.focus({ preventScroll: true });
  }

  // The body port lists the body's fields as the same tree the samples
  // use, with an input per value. The example fills the placeholders and
  // decides how each value is read back: numbers, booleans, lists.
  function buildBodyTree(box) {
    if (box.dataset.done) {
      return;
    }
    box.dataset.done = '1';
    const article = box.closest('[data-op]');
    const example = article ? article.querySelector('[data-req-example]') : null;
    let v;
    try {
      v = JSON.parse(example ? example.textContent : '');
    } catch (err) {
      v = null;
    }
    const btn = box.parentElement.querySelector('[data-body-tg]');
    if (v === null || typeof v !== 'object' || Array.isArray(v) || Object.keys(v).length === 0) {
      box.remove();
      if (btn) {
        btn.textContent = 'Edit body';
      }
      return;
    }
    const root = btList(v, 0, '');
    root.classList.add('oi-root');
    box.append(root);
    // Body ids keep the operations they come from, on their own rows.
    const fields = box.parentElement.querySelector('.pfields');
    if (fields) {
      fields.querySelectorAll(':scope > li[data-port]').forEach((li) => {
        const row = root.querySelector(':scope > li[data-key="' + CSS.escape(li.dataset.port.slice(8)) + '"]');
        if (!row) {
          return;
        }
        row.dataset.port = li.dataset.port;
        if (li.dataset.m) {
          row.dataset.m = li.dataset.m;
        }
        const src = li.querySelector('.src');
        if (src) {
          row.querySelector('.oi-row').append(src);
        }
      });
      fields.remove();
    }
  }

  function btKind(v) {
    if (typeof v === 'number') {
      return 'number';
    }
    if (typeof v === 'boolean') {
      return 'boolean';
    }
    return 'string';
  }

  function btList(v, depth, path) {
    const ul = el('ul', 'oi-list');
    for (const key of Object.keys(v)) {
      ul.append(btItem(key, v[key], depth, path ? path + '.' + key : key));
    }
    return ul;
  }

  function btItem(key, v, depth, path) {
    const li = el('li');
    li.dataset.key = key;
    const isList = Array.isArray(v);
    const item = isList && v.length > 0 && v[0] !== null && typeof v[0] === 'object' && !Array.isArray(v[0]) ? v[0] : null;
    if (item || (!isList && v !== null && typeof v === 'object' && Object.keys(v).length > 0)) {
      // A list of objects is edited as its first item, sent as a list of one.
      const open = depth < 1;
      const line = el('button', 'oi-row oi-tg');
      line.type = 'button';
      line.setAttribute('aria-expanded', String(open));
      line.append(
        el('span', item ? 'oi-ico oi-arr' : 'oi-ico oi-obj', item ? '[]' : '{}'),
        el('span', 'oi-k', key),
      );
      const kids = btList(item || v, depth + 1, path);
      kids.hidden = !open;
      li.dataset.shape = item ? 'list' : 'object';
      li.append(line, kids);
      return li;
    }
    const line = el('label', 'oi-row');
    line.append(el('span', 'oi-k', key));
    let input;
    if (typeof v === 'boolean') {
      input = el('select', 'bt-v');
      for (const opt of ['', 'true', 'false']) {
        const o = el('option', '', opt === '' ? '—' : opt);
        o.value = opt;
        input.append(o);
      }
      li.dataset.kind = 'boolean';
    } else {
      input = el('input', 'bt-v');
      input.type = 'text';
      input.spellcheck = false;
      input.autocomplete = 'off';
      if (isList) {
        li.dataset.kind = 'list-' + (v.length ? btKind(v[0]) : 'string');
        input.placeholder = v.length ? v.join(', ') : 'a, b';
        input.title = 'Comma-separated list';
      } else if (v !== null && typeof v === 'object') {
        li.dataset.kind = 'json';
        input.placeholder = '{}';
        input.title = 'JSON';
      } else {
        li.dataset.kind = btKind(v);
        input.placeholder = v === null ? 'null' : v === '' ? 'string' : String(v);
      }
    }
    input.setAttribute('aria-label', path);
    line.append(input);
    li.append(line);
    return li;
  }

  function btRead(kind, s) {
    if (kind === 'number') {
      const n = Number(s);
      return s.trim() !== '' && !Number.isNaN(n) ? n : s;
    }
    if (kind === 'boolean') {
      return s === 'true';
    }
    if (kind === 'json') {
      try {
        return JSON.parse(s);
      } catch (err) {
        return s;
      }
    }
    if (kind.startsWith('list-')) {
      return s.split(',').map((part) => part.trim()).filter((part) => part !== '').map((part) => btRead(kind.slice(5), part));
    }
    return s;
  }

  // treeValue reads the filled fields back into an object, leaving out
  // whatever is empty; undefined when nothing is filled.
  function treeValue(ul) {
    const out = {};
    let any = false;
    for (const li of ul.children) {
      let v;
      if (li.dataset.shape) {
        const kids = treeValue(li.querySelector(':scope > .oi-list'));
        v = kids !== undefined && li.dataset.shape === 'list' ? [kids] : kids;
      } else {
        const input = li.querySelector('.bt-v');
        v = input.value === '' ? undefined : btRead(li.dataset.kind, input.value);
      }
      if (v !== undefined) {
        out[li.dataset.key] = v;
        any = true;
      }
    }
    return any ? out : undefined;
  }

  // fillTree puts a JSON value back into the tree's inputs; keys the tree
  // does not have are dropped.
  function fillTree(ul, v) {
    const obj = v !== null && typeof v === 'object' && !Array.isArray(v) ? v : {};
    for (const li of ul.children) {
      let x = obj[li.dataset.key];
      if (li.dataset.shape) {
        if (li.dataset.shape === 'list' && Array.isArray(x)) {
          x = x[0];
        }
        fillTree(li.querySelector(':scope > .oi-list'), x);
        continue;
      }
      const input = li.querySelector('.bt-v');
      if (x === undefined || x === null) {
        input.value = '';
      } else if (Array.isArray(x) && li.dataset.kind.startsWith('list-')) {
        input.value = x.join(', ');
      } else if (typeof x === 'object') {
        input.value = JSON.stringify(x);
      } else {
        input.value = String(x);
      }
    }
  }

  // bodyText is the body to send: the JSON editor when it is open, the
  // field tree otherwise.
  function bodyText(form) {
    const box = form.querySelector('.mc-body');
    const textarea = box ? box.querySelector('[data-body]') : null;
    const tree = form.querySelector('[data-btree] > .oi-list');
    if (tree && box && box.hidden) {
      const v = treeValue(tree);
      return v === undefined ? '' : JSON.stringify(v, null, 2);
    }
    return textarea ? textarea.value : '';
  }

  const btObserver = 'IntersectionObserver' in window
    ? new IntersectionObserver((entries) => {
      for (const entry of entries) {
        if (entry.isIntersecting) {
          btObserver.unobserve(entry.target);
          buildBodyTree(entry.target);
        }
      }
    }, { rootMargin: '800px 0px' })
    : null;

  doc.querySelectorAll('[data-btree]').forEach((box) => {
    if (btObserver) {
      btObserver.observe(box);
    } else {
      buildBodyTree(box);
    }
  });

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

  // putParam adds one parameter value to a request being built. Arrays are
  // typed comma separated. Exploded ones (the default for query parameters)
  // travel as repeated keys, the rest comma-joined.
  function putParam(req, input, value) {
    const name = input.dataset.name;
    const items = input.hasAttribute('data-array') ? value.split(',').map((v) => v.trim()) : [value];
    if (input.dataset.in === 'path') {
      req.path = req.path.split('{' + name + '}').join(items.map(encodeURIComponent).join(','));
    } else if (input.dataset.in === 'query') {
      if (input.hasAttribute('data-explode')) {
        items.forEach((v) => req.query.append(name, v));
      } else {
        req.query.append(name, items.join(input.dataset.sep || ','));
      }
    } else if (input.dataset.in === 'header') {
      req.headers[name] = items.join(',');
    }
  }

  // ------------------------------------------------------------ copying
  // The machine copies its request as curl, or the whole endpoint as
  // Markdown for an agent. Typed values win; required ones fall back to
  // their examples. Credentials stay shell variables such as $API_KEY, so a
  // copy never carries a secret.

  function requestExample(form) {
    const example = form.querySelector('[data-req-example]');
    return example ? example.textContent : '';
  }

  function copiedCurl(form) {
    const method = form.dataset.method;
    const req = { path: form.dataset.path, query: new URLSearchParams(), headers: {} };
    form.querySelectorAll('input[data-in]').forEach((input) => {
      const value = input.value || (input.hasAttribute('data-required') ? input.placeholder : '');
      if (value !== '') {
        putParam(req, input, value);
      }
    });
    let auth = {};
    try {
      auth = JSON.parse(form.dataset.authArgs || '{}') || {};
    } catch (err) {
      auth = {};
    }
    let bodyArgs = [];
    const raw = bodyText(form) || requestExample(form);
    if (raw.trim() !== '' && method !== 'GET' && method !== 'HEAD') {
      try {
        bodyArgs = requestBody(raw, form.dataset.ct || 'application/json', req.headers).args;
      } catch (err) {
        bodyArgs = ['--data-raw ' + shellQuote(raw)];
      }
    }
    const qs = req.query.toString();
    let target = shellQuote(serverBase(form) + req.path + (qs ? '?' + qs : ''));
    (auth.query || []).forEach((q, i) => {
      target += shellQuote((i === 0 && !qs ? '?' : '&') + encodeURIComponent(q.name) + '=') + '"$' + q.var + '"';
    });
    const parts = ['curl' + (method === 'GET' ? '' : ' -X ' + method) + ' ' + target];
    parts.push(...(auth.args || []));
    for (const [k, v] of Object.entries(req.headers)) {
      parts.push('-H ' + shellQuote(k + ': ' + v));
    }
    return parts.concat(bodyArgs).join(' \\\n  ');
  }

  function fence(lang, text) {
    const ticks = text.includes('```') ? '~~~~' : '```';
    return ticks + lang + '\n' + text.trim() + '\n' + ticks;
  }

  function oneLine(node) {
    return node ? node.textContent.replace(/\s+/g, ' ').trim() : '';
  }

  // portType names what an output port carries: the documented schema from
  // its title, which a list port shortens to its items.
  function portType(port) {
    if (!port) {
      return '';
    }
    const pt = port.querySelector('.pt');
    const m = pt && pt.title ? /\(([^()]+)\)$/.exec(pt.title) : null;
    return m ? m[1] : oneLine(port.querySelector('.pout, .pty'));
  }

  function copiedMarkdown(form) {
    const article = form.closest('[data-op]');
    const out = [];
    out.push('# ' + oneLine(article.querySelector('.op-title')), '');
    out.push('`' + form.dataset.method + ' ' + form.dataset.path + '`', '');
    const desc = article.querySelector('.op-main > .md');
    if (desc) {
      out.push(desc.innerText.trim(), '');
    }
    out.push('- Base URL: ' + serverBase(form));
    const meta = article.querySelector('.op-meta');
    const auth = meta ? Array.from(meta.querySelectorAll('.ml')).find((n) => n.textContent.trim().toLowerCase() === 'auth') : null;
    if (auth && auth.nextElementSibling) {
      out.push('- Auth: ' + oneLine(auth.nextElementSibling));
    }
    const opID = meta ? meta.querySelector('code') : null;
    if (opID) {
      out.push('- operationId: `' + opID.textContent + '`');
    }

    const groups = form.querySelectorAll('.mc-detail[data-detail="params"] .io[data-in]');
    if (groups.length > 0) {
      out.push('', '## Parameters', '');
      groups.forEach((io) => {
        io.querySelectorAll('.rows > .row').forEach((row) => {
          const head = row.querySelector(':scope > .row-head');
          let line = '- `' + oneLine(head.querySelector('.pn')) + '` (' + io.dataset.in + ', ' + oneLine(head.querySelector('.ty'));
          line += head.querySelector('.mk.req') ? ', required)' : ')';
          const d = row.querySelector(':scope > .row-desc');
          if (d) {
            line += ': ' + oneLine(d);
          }
          out.push(line);
        });
      });
    }

    const body = bodyText(form) || requestExample(form);
    if (body.trim() !== '') {
      const name = oneLine(form.querySelector('.port[data-port="in:body"] .pin'));
      out.push('', '## Request body' + (name ? ': ' + name : '') + ' (' + (form.dataset.ct || 'application/json') + ')', '');
      out.push(fence('json', body));
    }

    const card = article.querySelector('.op-samples .card:last-child');
    const tabs = card ? card.querySelectorAll('[role="tab"]') : [];
    if (tabs.length > 0) {
      out.push('', '## Responses');
      const panels = card.querySelectorAll(':scope > .tabpanel');
      tabs.forEach((tab, i) => {
        const status = tab.textContent.trim();
        const port = form.querySelector('.mc-outs > .port[data-port="out:' + CSS.escape(status) + '"]');
        const type = portType(port);
        out.push('', '### ' + status + (type ? ': ' + type : ''), '');
        const code = panels[i] ? panels[i].querySelector('pre.raw code') : null;
        if (code && code.textContent.trim() !== '') {
          out.push(fence('json', code.textContent));
        }
      });
      // Statuses documented without an example still name their type.
      form.querySelectorAll('.mc-outs > .port').forEach((port) => {
        const status = port.dataset.port.slice(4);
        if (!Array.from(tabs).some((tab) => tab.textContent.trim() === status)) {
          const type = portType(port);
          out.push('', '### ' + status + (type ? ': ' + type : ''));
        }
      });
    }

    out.push('', '## Example', '', fence('sh', copiedCurl(form)));
    return out.join('\n') + '\n';
  }

  async function send(form) {
    touchTry(form);
    const method = form.dataset.method;
    const query = new URLSearchParams();
    const headers = {};
    const req = { path: form.dataset.path, query, headers };
    let missing = false;

    form.querySelectorAll('input[data-in]').forEach((input) => {
      if (input.value === '') {
        if (input.hasAttribute('data-required')) {
          input.classList.add('invalid');
          missing = true;
        }
        return;
      }
      input.classList.remove('invalid');
      putParam(req, input, input.value);
    });
    const path = req.path;

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
    const raw = bodyText(form);
    if (raw.trim() !== '' && method !== 'GET' && method !== 'HEAD') {
      try {
        const built = requestBody(raw, form.dataset.ct || 'application/json', headers);
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

  // ------------------------------------------------------ ObjectInspector

  // The ObjectInspector shows an object as a tree of fields, each with its
  // name, type and example. Response samples and the model dialog use it,
  // and the body port's editor is the same tree with inputs. A response
  // sample keeps its raw JSON in the page for Copy and the JSON toggle; its
  // inspector is built from it when the card nears the viewport.

  function oiScalar(v) {
    if (v === null) {
      return el('span', 'oi-val oi-null', 'null');
    }
    if (typeof v === 'boolean') {
      return el('span', 'oi-val oi-bool', String(v));
    }
    if (typeof v === 'number') {
      return el('span', 'oi-val oi-num', String(v));
    }
    if (v === '') {
      return el('span', 'oi-val oi-empty', 'empty');
    }
    // "string" is the generator's placeholder for unformatted strings.
    return el('span', v === 'string' ? 'oi-val oi-ph' : 'oi-val oi-str', String(v));
  }

  function oiIsNode(v) {
    return v !== null && typeof v === 'object' && Object.keys(v).length > 0 &&
      !(Array.isArray(v) && v.every((item) => item === null || typeof item !== 'object'));
  }

  function oiSummary(v) {
    if (Array.isArray(v)) {
      return v.length + (v.length === 1 ? ' item' : ' items');
    }
    const n = Object.keys(v).length;
    return n + (n === 1 ? ' field' : ' fields');
  }

  // Type notes describe a sample's values: one line per value, indented by
  // depth, naming its step from the parent ("[]" for list items), then a tab,
  // its kind and label (an enum's values as JSON), "!" when the value is the
  // author's example rather than a generated one, and a tab and the spec's
  // description when it has one. They parse into a tree that walks
  // alongside the sample.
  let typeNotes = null;

  function typeTree(ref) {
    if (ref === undefined || ref === '') {
      return null;
    }
    if (!typeNotes) {
      const node = doc.getElementById('type-notes');
      try {
        typeNotes = JSON.parse(node ? node.textContent : '[]') || [];
      } catch (err) {
        typeNotes = [];
      }
    }
    const text = typeNotes[Number(ref)];
    if (typeof text !== 'string') {
      return null;
    }
    const root = { kids: new Map() };
    const stack = [root];
    for (const line of text.split('\n')) {
      const cut = line.indexOf('\t');
      if (cut < 0) {
        continue;
      }
      const head = line.slice(0, cut);
      const step = head.trimStart();
      const level = head.length - step.length;
      const rest = line.slice(cut + 1);
      const tab = rest.indexOf('\t');
      const note = tab < 0 ? rest : rest.slice(0, tab);
      const own = note.endsWith('!');
      const node = { kind: note.charAt(0), label: note.slice(2, own ? -1 : undefined), own, desc: tab < 0 ? '' : rest.slice(tab + 1), kids: new Map() };
      if (level === 0) {
        Object.assign(root, node, { kids: root.kids });
        stack.length = 1;
        continue;
      }
      const parent = stack[level - 1];
      if (!parent) {
        continue;
      }
      parent.kids.set(step, node);
      stack.length = level;
      stack.push(node);
    }
    return root;
  }

  function typeKid(t, step) {
    return t ? t.kids.get(step) || null : null;
  }

  // Model titles name their anchors, so a type in a sample can open its
  // model.
  let modelAnchors = null;

  function modelAnchor(name) {
    if (!modelAnchors) {
      modelAnchors = new Map();
      doc.querySelectorAll('.model-title > a[href^="#model/"]').forEach((a) => {
        modelAnchors.set(a.textContent, idFromHref(a.getAttribute('href')));
      });
    }
    return modelAnchors.get(name.replace(/\[\]$/, '')) || '';
  }

  // Each model's own example and type notes, by anchor, for the model
  // dialog and for models the inspector opens on demand.
  let modelSamples = null;

  function modelSample(anchor) {
    if (!modelSamples) {
      const node = doc.getElementById('model-samples');
      try {
        modelSamples = JSON.parse(node ? node.textContent : '{}') || {};
      } catch (err) {
        modelSamples = {};
      }
    }
    return modelSamples[anchor] || null;
  }

  // Types read like a typed language: primitives in one color, models in
  // another, enums as the union of their values. Models open in the model
  // dialog: a link on a value row, a marked span inside a folder's button.
  function oiType(t, folder) {
    if (t.kind === 'e') {
      return el('span', 'oi-type t-kw', 'enum');
    }
    if (t.kind !== 'm' && t.kind !== 'l') {
      return el('span', 'oi-type t-prim', t.label);
    }
    const anchor = modelAnchor(t.label);
    if (!anchor) {
      return el('span', 'oi-type t-model', t.label);
    }
    const link = el(folder ? 'span' : 'a', 'oi-type t-model', t.label);
    link.dataset.model = anchor;
    link.title = 'Open ' + t.label.replace(/\[\]$/, '');
    if (!folder) {
      link.href = '#' + anchor;
    }
    return link;
  }

  // oiLit renders a value as a highlighted JSON literal.
  function oiLit(v) {
    if (Array.isArray(v)) {
      const span = el('span', 'oi-val');
      span.append(el('span', 'j-p', '['));
      v.forEach((item, i) => {
        if (i > 0) {
          span.append(el('span', 'j-p', ', '));
        }
        span.append(oiLit(item));
      });
      span.append(el('span', 'j-p', ']'));
      return span;
    }
    if (v === null) {
      return el('span', 'j-n', 'null');
    }
    if (typeof v === 'string') {
      return el('span', 'j-s', JSON.stringify(v));
    }
    if (typeof v === 'number') {
      return el('span', 'j-d', String(v));
    }
    if (typeof v === 'boolean') {
      return el('span', 'j-b', String(v));
    }
    return el('span', 'j-p', JSON.stringify(v));
  }

  // An enum shows its values as a union.
  const oiEnumMax = 4;
  function oiEnum(t) {
    let values = [];
    try {
      values = JSON.parse(t.label);
    } catch (err) {
      values = [];
    }
    const span = el('span', 'oi-val oi-enum');
    const chip = (v) => {
      const c = oiLit(v);
      c.classList.add('oi-ex');
      c.title = JSON.stringify(v);
      return c;
    };
    // Long enums show their first values and expand on click.
    const shown = values.length > oiEnumMax + 1 ? oiEnumMax : values.length;
    values.slice(0, shown).forEach((v) => span.append(chip(v)));
    if (shown < values.length) {
      const more = el('button', 'oi-more', '+' + (values.length - shown));
      more.type = 'button';
      more.title = 'Show all ' + values.length + ' values';
      more.onclick = () => {
        values.slice(shown).forEach((v) => span.insertBefore(chip(v), more));
        more.remove();
      };
      span.append(more);
    }
    return span;
  }

  // oiExample is the example column: enums list their values, empty
  // containers and the generator's bare "string" say nothing the type
  // does not.
  function oiExample(v, t) {
    if (t.kind === 'e') {
      return oiEnum(t);
    }
    if (v !== null && typeof v === 'object' && Object.keys(v).length === 0) {
      return null;
    }
    if (!t.own && (v === 'string' || (Array.isArray(v) && v.length > 0 && v.every((x) => x === 'string')))) {
      return null;
    }
    const span = oiLit(v);
    span.classList.add('oi-val', 'oi-ex');
    span.title = typeof v === 'string' ? v : JSON.stringify(v);
    return span;
  }

  // The inspector is a file tree: objects and lists are folders with a
  // chevron, an icon and their type; values are leaves on elbow connectors
  // with their type and, when the spec gives one, their example.
  function oiList(v, depth, t) {
    const ul = el('ul', 'oi-list');
    if (Array.isArray(v) && v.length === 1 && oiIsNode(v[0]) && !Array.isArray(v[0])) {
      // A list of one object reads better as that object's fields.
      v = v[0];
      t = typeKid(t, '[]');
    }
    if (Array.isArray(v)) {
      v.forEach((item, i) => ul.append(oiItem('#' + (i + 1), item, depth, typeKid(t, '[]'))));
    } else {
      for (const key of Object.keys(v)) {
        ul.append(oiItem(key, v[key], depth, typeKid(t, key)));
      }
    }
    return ul;
  }

  // The whole tree shares one type column and one example column. Each
  // level indents by oiIndent (about 3ch), so a row's key cell is the
  // shared width less its depth; oiCols collects the widest key, counted
  // with its indent, and the widest type while a tree renders. The shared
  // key width is capped by oiKeyCap: a row whose key needs more (--kl)
  // takes it and pushes only its own type over.
  const oiIndent = 3;
  const oiKeyMax = 22;

  // oiKeyCap lets the key column take up to 38% of an inspector width px
  // wide (7.5px is the 12.5px monospace ch), and never less than oiKeyMax.
  function oiKeyCap(width) {
    return Math.max(oiKeyMax, Math.floor(width * 0.38 / 7.5));
  }
  let oiCols = { k: 1, t: 4 };

  function oiColumns(line, key, depth, t, folder) {
    const need = key.length + (folder ? 3 : 0);
    line.classList.add('oi-cols');
    line.style.setProperty('--d', String(depth));
    line.style.setProperty('--kl', need + 'ch');
    oiCols.k = Math.max(oiCols.k, need + depth * oiIndent + (folder ? 2 : 0) + 1);
    oiCols.t = Math.max(oiCols.t, t.kind === 'e' ? 4 : t.label.length);
  }

  // A model the example stopped at (left empty for depth, or because the
  // dialog's samples hold one level) still opens: its own example and type
  // notes graft in on first expand, a level at a time, so models that hold
  // each other never recurse. oiModel is that sample, or null.
  const oiLazy = new WeakMap();

  function oiModel(v, t) {
    if (!t || t.own || (t.kind !== 'm' && t.kind !== 'l') || oiIsNode(v)) {
      return null;
    }
    const anchor = modelAnchor(t.label);
    const sample = anchor ? modelSample(anchor) : null;
    return sample && oiIsNode(sample.v) ? sample : null;
  }

  function oiGraft(kids, sample, depth) {
    const root = kids.closest('.oi-root');
    oiCols = { k: Number(root.dataset.k) || 1, t: Number(root.dataset.t) || 4 };
    kids.append(...oiList(sample.v, depth, typeTree(String(sample.t))).childNodes);
    oiSetCols(root, oiKeyCap(root.clientWidth));
  }

  // oiSetCols sizes the shared columns from oiCols, keeping the counts on
  // the root for rows grafted in later.
  function oiSetCols(root, keyMax) {
    root.dataset.k = String(oiCols.k);
    root.dataset.t = String(oiCols.t);
    root.style.setProperty('--kw', Math.min(keyMax, oiCols.k) + 'ch');
    root.style.setProperty('--tw', Math.min(24, oiCols.t) + 'ch');
  }

  function oiItem(key, v, depth, t) {
    const li = el('li');
    const model = oiModel(v, t);
    if (!oiIsNode(v) && !model) {
      const line = el('div', 'oi-row');
      line.append(el('span', 'oi-k', key));
      if (t) {
        oiColumns(line, key, depth, t, false);
        line.append(oiType(t, false));
        const ex = oiExample(v, t);
        if (ex) {
          line.append(ex);
        }
      } else if (Array.isArray(v)) {
        const list = el('span', 'oi-val oi-chips');
        if (v.length === 0) {
          list.append(el('span', 'oi-empty', 'empty list'));
        }
        v.forEach((item) => list.append(oiScalar(item)));
        line.append(list);
      } else if (v !== null && typeof v === 'object') {
        line.append(el('span', 'oi-val oi-empty', 'empty object'));
      } else {
        line.append(oiScalar(v));
      }
      li.append(line);
      oiDescribe(li, t, false);
      return li;
    }
    const open = !model && depth < 2;
    const isList = model ? t.kind === 'l' : Array.isArray(v);
    const line = el('button', 'oi-row oi-tg');
    line.type = 'button';
    line.setAttribute('aria-expanded', String(open));
    const name = el('span', 'oi-k');
    name.append(el('span', isList ? 'oi-ico oi-arr' : 'oi-ico oi-obj', isList ? '[]' : '{}'), key);
    const type = t && !t.own ? oiType(t, true) : el('span', 'pill', oiSummary(v));
    line.append(name, type);
    if (type.dataset.model) {
      const go = el('span', 'oi-open', '↗');
      go.dataset.model = type.dataset.model;
      go.title = type.title;
      line.append(go);
    }
    if (t && !t.own) {
      oiColumns(line, key, depth, t, true);
    }
    let kids;
    if (model) {
      kids = el('ul', 'oi-list');
      oiLazy.set(kids, () => oiGraft(kids, model, depth + 1));
    } else {
      kids = oiList(v, depth + 1, t);
    }
    kids.hidden = !open;
    li.append(line);
    oiDescribe(li, t, true);
    li.append(kids);
    return li;
  }

  // oiDescribe adds the spec's description of a field under its row.
  function oiDescribe(li, t, folder) {
    if (t && t.desc) {
      li.append(el('div', folder ? 'oi-desc oi-desc-tg' : 'oi-desc', t.desc));
    }
  }

  function renderInspector(box) {
    if (box.dataset.done) {
      return;
    }
    box.dataset.done = '1';
    const code = box.parentElement.querySelector('pre.raw code');
    let v;
    try {
      v = JSON.parse(code.textContent);
    } catch (err) {
      box.parentElement.classList.add('oi-off');
      return;
    }
    box.append(inspectorTree(v, typeTree(box.dataset.types), oiKeyCap(box.clientWidth)));
  }

  // inspectorTree is the inspector of the value v typed by t, its shared key
  // column at most keyMax ch wide.
  function inspectorTree(v, t, keyMax) {
    if (!oiIsNode(v)) {
      v = { value: v };
      t = t ? { kids: new Map([['value', t]]) } : null;
    }
    oiCols = { k: 1, t: 4 };
    const root = oiList(v, 0, t);
    root.classList.add('oi-root');
    oiSetCols(root, keyMax || oiKeyMax);
    return root;
  }


  const oiObserver = 'IntersectionObserver' in window
    ? new IntersectionObserver((entries) => {
      for (const entry of entries) {
        if (entry.isIntersecting) {
          oiObserver.unobserve(entry.target);
          renderInspector(entry.target);
        }
      }
    }, { rootMargin: '800px 0px' })
    : null;

  doc.querySelectorAll('[data-oi]').forEach((box) => {
    if (oiObserver) {
      oiObserver.observe(box);
    } else {
      renderInspector(box);
    }
  });

  // The Models view shows each model in its own inspector, as in the
  // dialog, built when the model nears the viewport. Its Full schema
  // button swaps in the static tree with every constraint.
  function renderModelInspector(box) {
    if (box.dataset.done) {
      return;
    }
    box.dataset.done = '1';
    const sample = modelSample(box.closest('.model').id);
    if (sample) {
      box.append(inspectorTree(sample.v, typeTree(String(sample.t)), oiKeyCap(box.clientWidth)));
    }
  }

  const modelObserver = 'IntersectionObserver' in window
    ? new IntersectionObserver((entries) => {
      for (const entry of entries) {
        if (entry.isIntersecting) {
          modelObserver.unobserve(entry.target);
          renderModelInspector(entry.target);
        }
      }
    }, { rootMargin: '800px 0px' })
    : null;

  // modelViews is the Inspector / Full schema switch of a model, pressed
  // on the one shown.
  function modelViews(tree) {
    const group = el('div', 'model-views');
    group.setAttribute('role', 'group');
    group.setAttribute('aria-label', 'Show the model as');
    [['oi', 'Inspector', ''], ['tree', 'Full schema', 'Every field with its constraints: required, nullable, formats and limits']].forEach(([view, label, title]) => {
      const btn = el('button', '', label);
      btn.type = 'button';
      btn.dataset.modelView = view;
      btn.setAttribute('aria-pressed', String((view === 'tree') === tree));
      if (title) {
        btn.title = title;
      }
      group.append(btn);
    });
    return group;
  }

  doc.querySelectorAll('[data-model-oi]').forEach((box) => {
    const article = box.closest('.model');
    article.querySelector(':scope > .model-bar').append(modelViews(box.parentElement.hidden));
    if (modelObserver) {
      modelObserver.observe(box);
    } else {
      renderModelInspector(box);
    }
  });

  function showModelView(btn) {
    const article = btn.closest('.model');
    const tree = btn.dataset.modelView === 'tree';
    article.querySelectorAll('[data-model-view]').forEach((b) => b.setAttribute('aria-pressed', String(b === btn)));
    article.querySelector(':scope > .mm-sample').hidden = tree;
    article.querySelector(':scope > .model-tree').hidden = !tree;
    if (!tree) {
      renderModelInspector(article.querySelector('[data-model-oi]'));
    }
  }

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

  // Input ports open their details under the machine: the parameters or
  // the body. Clicking the same port again closes them. Output ports show
  // their response in the Response card instead.
  function detailKey(port) {
    return port.split(':')[1] === 'body' ? 'body' : 'params';
  }

  function responseTab(article, status) {
    const card = article.querySelector('.op-samples .card:last-child');
    if (!card || card.querySelector('.card-head > span').textContent !== 'Response') {
      return null;
    }
    const want = status.toLowerCase();
    return Array.from(card.querySelectorAll('.card-head [role="tab"]')).find((t) => t.textContent.trim().toLowerCase() === want) || null;
  }

  // markResponse marks the output port of the response the card shows.
  function markResponse(article, status) {
    article.querySelectorAll('.mc-outs > .port').forEach((p) => {
      p.classList.toggle('sel', p.dataset.port.toLowerCase() === 'out:' + status.toLowerCase());
    });
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
    if (key.startsWith('out:')) {
      const article = machine.closest('[data-op]');
      const tab = responseTab(article, key.slice(4));
      if (tab) {
        selectTab(tab, false);
        const card = tab.closest('.card');
        card.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
        flash(card);
      }
      return;
    }
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
  // and model link that carries the same model.
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
  const markTraceLinks = marker('lit-link');

  function setTrace(ids) {
    if (ids === traced) {
      return;
    }
    traced = ids;
    const ports = [];
    const links = [];
    if (ids) {
      const f = flowData();
      ids.split(' ').forEach((id) => {
        doc.querySelectorAll('.port[data-m~="' + id + '"] > .pbox, .pfields li[data-m~="' + id + '"]').forEach((n) => ports.push(n));
        const m = f.models[id];
        if (m) {
          doc.querySelectorAll('a.ref[href="#' + m.a + '"], a.nav-model[href="#' + m.a + '"]').forEach((n) => links.push(n));
        }
      });
    }
    markTracePorts(ports);
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
    filled['in:body'] = bodyText(machine).trim() !== '';
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
    // The title is the trail of models opened here: each earlier one goes
    // back to it. Under it is the model's name in the spec, when the title
    // shortens it.
    const head = el('div', 'mm-head');
    const heading = el('div', 'mm-heading');
    const trail = el('nav', 'mm-trail');
    trail.setAttribute('aria-label', 'Models opened');
    modal.stack.forEach((id, i) => {
      const title = doc.getElementById(id).querySelector('.model-title a');
      const name = title ? title.textContent : id;
      if (i === modal.stack.length - 1) {
        const t = el('span', 'mm-title', name);
        t.id = 'mm-title';
        trail.append(t);
        return;
      }
      const crumb = el('button', 'mm-crumb', name);
      crumb.type = 'button';
      crumb.title = 'Back to ' + name;
      crumb.onclick = () => modelBack(i + 1);
      trail.append(crumb, el('span', 'mm-sep', '›'));
    });
    heading.append(el('span', 'ico-obj', '{}'), trail);
    const specName = card.querySelector('.model-name code');
    if (specName) {
      const line = el('code', 'mm-name', specName.textContent);
      line.title = 'Schema name in the spec: #/components/schemas/' + specName.textContent;
      heading.append(line);
    }
    const table = card.querySelector('.model-table');
    if (table) {
      const line = el('span', 'mm-table');
      line.title = table.title;
      line.append(...[...table.childNodes].map((n) => n.cloneNode(true)));
      heading.append(line);
    }
    head.append(heading);
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

    // The model's ObjectInspector, as in a response sample: the models it
    // holds expand in place, and their types open here in turn.
    const body = el('div', 'mm-body');
    const sample = modelSample(anchor);
    ['.model-flow', sample ? '' : ':scope > .tree'].forEach((sel) => {
      const node = sel && card.querySelector(sel);
      if (node) {
        body.append(node.cloneNode(true));
      }
    });
    if (sample) {
      const oi = el('div', 'oi');
      oi.append(inspectorTree(sample.v, typeTree(String(sample.t)), oiKeyCap(box.clientWidth - 60)));
      const frame = el('div', 'card mm-sample oi-deep');
      frame.append(oi);
      body.append(frame);
    }
    box.append(body);
    body.querySelectorAll('[data-autofill]').forEach(observeLazy);
    close.focus({ preventScroll: true });
  }

  // modelBack returns the dialog to the first depth models it opened.
  function modelBack(depth) {
    if (!modal || depth < 1 || depth >= modal.stack.length) {
      return;
    }
    modal.stack.length = depth;
    renderModel();
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
      // A click on the backdrop closes the dialog. Testing the target
      // rather than box.contains keeps clicks that re-render the dialog,
      // whose targets are gone by now, from counting as outside.
      back.addEventListener('click', (e) => {
        if (e.target === back) {
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

    // A model type in an inspector opens its model, even inside a folder
    // row. In a model's own inspector (the dialog and the Models view) a
    // folder's type toggles the row like the rest of it, and its arrow
    // opens the model.
    const typeLink = t.closest('.oi-open[data-model], .oi-type[data-model]');
    if (typeLink && !(typeLink.matches('.oi-type') && typeLink.closest('.oi-deep .oi-tg'))) {
      e.preventDefault();
      closePop();
      openModel(typeLink.dataset.model);
      return;
    }

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
      } else if (btn.matches('.oi-tg')) {
        const open = btn.getAttribute('aria-expanded') !== 'true';
        const graft = oiLazy.get(btn.nextElementSibling);
        if (open && graft) {
          oiLazy.delete(btn.nextElementSibling);
          graft();
        }
        btn.setAttribute('aria-expanded', String(open));
        btn.nextElementSibling.hidden = !open;
      } else if (btn.matches('[data-raw]')) {
        toggleRaw(btn);
      } else if (btn.matches('[data-model-view]')) {
        showModelView(btn);
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
      } else if (btn.matches('[data-copy-req]')) {
        const form = btn.closest('form.try');
        copyText(btn.dataset.copyReq === 'ai' ? copiedMarkdown(form) : copiedCurl(form), btn);
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

    // An output port is one target: its padding, stacked edges and "used by"
    // row switch the Response card like its name does.
    const out = t.closest('.mc-outs .pbox');
    if (out && !t.closest('a')) {
      const pt = out.querySelector(':scope > .pt[data-goto]');
      if (pt) {
        gotoPort(pt);
        return;
      }
    }

    const a = t.closest('a[href]');
    const tab = a ? a.closest('.modes a[data-mode]') : null;
    if (tab && tab.dataset.mode !== 'map') {
      e.preventDefault();
      closePop();
      closeModel();
      openMode(tab);
      return;
    }
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
  showHash();
})();

/* ---- map ---- */
// Schema map: the entity graph embedded once as JSON, drawn as an ER diagram
// in the content pane. Cards sit in layered columns, models that hold keys on
// the left and the models they point at to their right, hubs (models nearly
// every other one points at) in the last column. Edges are curves from the
// field holding the key to the target's header, routed through the gaps
// between cards, in one SVG behind them. Edges into hubs stay hidden until a
// card is hovered or selected. Pan and zoom move one CSS transform. The click
// listener runs in the capture phase so map links and the view switch are
// handled before the reference's own links.
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

  const CARD_W = 280;
  const COL_GAP = 140;
  const ROW_GAP = 48;
  const MAX_FIELDS = 12;
  const PAD = 48;
  const LANE_MARGIN = 14;
  const SVG_NS = 'http://www.w3.org/2000/svg';

  const canvas = section.querySelector('[data-map-canvas]');
  const world = section.querySelector('[data-map-world]');
  const svg = section.querySelector('[data-map-svg]');
  const scopeSelect = section.querySelector('[data-map-scope]');
  const findInput = section.querySelector('[data-map-find]');
  const countEl = section.querySelector('[data-map-count]');
  const hubsBox = section.querySelector('[data-map-hubs]');
  const hubsCount = section.querySelector('[data-map-hubs-count]');
  const pctEl = section.querySelector('[data-map-pct]');

  const nodes = new Map(data.nodes.map((n) => [n.name, n]));
  const edges = (data.edges || []).filter((e) => nodes.has(e.from) && nodes.has(e.to));

  let store = null;
  try {
    store = window.localStorage;
    store.getItem('reference.probe');
  } catch (err) {
    store = null;
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

  function decode(s) {
    try {
      return decodeURIComponent(s);
    } catch (err) {
      return s;
    }
  }

  // ------------------------------------------------------------- scopes

  // Scopes are the operation tags, in page order, and all models.
  const scopes = new Map();
  scopes.set('all', { label: 'All models', names: data.nodes.map((n) => n.name) });
  for (const t of data.tags || []) {
    const names = (t.nodes || []).filter((n) => nodes.has(n));
    if (names.length && !scopes.has(t.name)) {
      scopes.set(t.name, { label: t.name, names });
    }
  }
  const defaultScope = scopes.has(data.default) ? data.default : 'all';
  for (const [id, s] of scopes) {
    const o = el('option', '', s.label + ' (' + s.names.length + ')');
    o.value = id;
    scopeSelect.append(o);
  }

  const state = { scope: defaultScope, selected: null, hover: null, expanded: new Set(), showHubs: false, x: 0, y: 0, k: 1 };
  let cards = new Map(); // name -> { el, n, compact, x, y, w, h, headY, rows, moreY }
  let links = [];
  let lineLayer = null;
  let labelLayer = null;
  let near = new Map(); // name -> Set of names it shares an edge with
  let rendered = null;
  let isOpen = false;
  let refScroll = 0;

  function positionsKey() {
    return 'reference.map.' + (data.spec || '') + '.' + state.scope;
  }

  function savedPositions() {
    if (!store) {
      return {};
    }
    try {
      return JSON.parse(store.getItem(positionsKey()) || '{}') || {};
    } catch (err) {
      return {};
    }
  }

  function savePosition(c) {
    if (!store) {
      return;
    }
    const all = savedPositions();
    all[c.n.name] = [Math.round(c.x), Math.round(c.y)];
    try {
      store.setItem(positionsKey(), JSON.stringify(all));
    } catch (err) {
      // Full or refused storage only loses the saved layout.
    }
  }

  // -------------------------------------------------------------- cards

  function keyIcon() {
    const icon = svgEl('svg', { class: 'mf-key', viewBox: '0 0 16 16', 'aria-hidden': 'true' });
    icon.append(svgEl('circle', { cx: '5', cy: '8', r: '3.2' }), svgEl('path', { d: 'M8.2 8H15M12.5 8v3M15 8v2.2' }));
    return icon;
  }

  // fieldRow is one line per field (name, qualifiers, type or the model it
  // points at), with enum values as chips below.
  function fieldRow(f) {
    const li = el('li', 'mf' + (f.deprecated ? ' deprecated' : ''));
    li.dataset.field = f.name;
    const line = el('div', 'mf-line');
    const name = el('span', 'mf-name');
    if (f.name === 'id') {
      name.append(keyIcon());
    }
    name.append(f.name);
    if (f.optional || f.nullable) {
      name.append(el('span', 'mf-opt', '?'));
    }
    line.append(name);
    li.append(line);
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
      line.append(el('span', 'mf-q', quals.join(' ')));
    }
    const target = f.ref ? nodes.get(f.ref) : null;
    if (target) {
      const ref = el('button', 'mf-ref', '→ ' + target.short + (/\[\]$/.test(f.type || '') ? '[]' : ''));
      ref.type = 'button';
      ref.dataset.mapRef = f.ref;
      ref.title = 'Show ' + target.short;
      line.append(ref);
    } else if (f.type) {
      const type = el('span', 'mf-type', f.type);
      type.title = f.type;
      line.append(type);
    }
    if (f.enum && f.enum.length) {
      const chips = el('div', 'mf-enum');
      chips.title = f.enum.map((v) => (v === '' ? '""' : v)).join(', ');
      for (const v of f.enum.slice(0, 3)) {
        chips.append(el('span', 'mchip', v === '' ? '""' : v));
      }
      if (f.enum.length > 3) {
        chips.append(el('span', 'mchip more', '+' + (f.enum.length - 3)));
      }
      li.append(chips);
    }
    return li;
  }

  // makeCard builds a model card. A compact card (a model outside the scope
  // that a card in it points at) shows its header only.
  function makeCard(n, compact) {
    const c = { n, compact, x: 0, y: 0, w: CARD_W, h: 0, headY: 0, rows: new Map(), moreY: 0 };
    const box = el('div', 'mcard' + (compact ? ' compact' : '') + (n.hub ? ' hub' : ''));
    box.dataset.node = n.name;
    box.tabIndex = 0;
    box.title = n.table ? n.name + '\nTable: ' + n.table : n.name;
    box.style.width = CARD_W + 'px';
    const head = el('div', 'mcard-head');
    head.append(el('span', 'mcard-name', n.short));
    if (n.hub) {
      head.append(el('span', 'mcard-badge hub', 'hub'));
    }
    if (n.group) {
      head.append(el('span', 'mcard-badge', n.group));
    }
    box.append(head);
    const fields = n.fields || [];
    if (compact) {
      box.append(el('div', 'mcard-note', fields.length + ' fields, outside this scope'));
    } else if (fields.length) {
      // A collapsed card keeps the id and the fields that point at other
      // models first, so its edges start at their own row, in field order.
      const open = state.expanded.has(n.name);
      const keep = new Set(fields.filter((f) => f.name === 'id' || f.ref).slice(0, MAX_FIELDS));
      for (const f of fields) {
        if (keep.size < MAX_FIELDS) {
          keep.add(f);
        }
      }
      const ul = el('ul', 'mcard-fields');
      for (const f of fields) {
        if (open || keep.has(f)) {
          ul.append(fieldRow(f));
        }
      }
      box.append(ul);
      if (fields.length > MAX_FIELDS) {
        const more = el('button', 'mcard-more', open ? 'Show fewer fields' : '+' + (fields.length - MAX_FIELDS) + ' more');
        more.type = 'button';
        more.dataset.mapMore = n.name;
        box.append(more);
      }
    }
    c.el = box;
    return c;
  }

  // measure reads a card's height and where its edges attach: the middle of
  // the header and of each field name (offsets inside the card).
  function measure(c) {
    c.h = c.el.offsetHeight;
    const head = c.el.firstElementChild;
    c.headY = head.offsetTop + head.offsetHeight / 2;
    c.el.querySelectorAll('[data-field]').forEach((row) => {
      const name = row.querySelector('.mf-name');
      c.rows.set(row.dataset.field, name.offsetTop + name.offsetHeight / 2);
    });
    const more = c.el.querySelector('.mcard-more');
    c.moreY = more ? more.offsetTop + more.offsetHeight / 2 : c.headY;
  }

  function setPos(c) {
    c.el.style.transform = 'translate(' + c.x + 'px,' + c.y + 'px)';
  }

  // ------------------------------------------------------------- layout

  // ranks places every node one column right of the furthest node pointing
  // at it (longest path from the roots). Depth-first search drops the edges
  // that close a cycle; its finish order reversed is topological.
  function ranks(names, list) {
    const adj = new Map(names.map((n) => [n, []]));
    const indeg = new Map(names.map((n) => [n, 0]));
    for (const e of list) {
      adj.get(e.from).push(e.to);
      indeg.set(e.to, indeg.get(e.to) + 1);
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
    for (const n of names.filter((m) => indeg.get(m) === 0).concat(names)) {
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

  // order sorts each layer by the mean position of its neighbours in the
  // layers before it, then after it, a few times over (barycenter method).
  // Hubs do not pull on the layers before them.
  function order(layers, hubLayer) {
    const at = new Map();
    const index = (layer, li) => {
      layer.forEach((n, i) => at.set(n, [li, (i + 0.5) / layer.length]));
    };
    layers.forEach(index);
    const sweep = (li, before) => {
      const layer = layers[li];
      const key = new Map();
      layer.forEach((n, i) => {
        let sum = 0;
        let count = 0;
        for (const m of near.get(n) || []) {
          const p = at.get(m);
          if (!p || (before ? p[0] >= li : p[0] <= li) || (li < hubLayer && p[0] === hubLayer)) {
            continue;
          }
          sum += p[1];
          count++;
        }
        key.set(n, count ? sum / count : (i + 0.5) / layer.length);
      });
      layer.sort((a, b) => key.get(a) - key.get(b));
      index(layer, li);
    };
    for (let pass = 0; pass < 4; pass++) {
      for (let li = 1; li < layers.length; li++) {
        sweep(li, true);
      }
      for (let li = layers.length - 2; li >= 0; li--) {
        sweep(li, false);
      }
    }
  }

  // layout ranks the cards over the edges between non-hub cards, puts hubs
  // in a column after them and the cards only hubs point at after the hubs.
  // It orders the layers and stacks each one in a column, wrapping a tall
  // layer into several columns. A card in a single-column layer moves down
  // to line its header up with the fields pointing at it.
  function layout(list) {
    const names = Array.from(cards.keys());
    const isHub = (n) => nodes.get(n).hub;
    const plainSource = new Set();
    const toPlain = new Set();
    for (const e of list) {
      if (e.from !== e.to && !isHub(e.from)) {
        plainSource.add(e.to);
      }
      if (e.from !== e.to && !isHub(e.to)) {
        toPlain.add(e.from);
      }
    }
    const hubs = names.filter(isHub);
    const tail = names.filter((n) => !isHub(n) && !plainSource.has(n) && !toPlain.has(n) && list.some((e) => e.to === n && e.from !== n));
    const rest = names.filter((n) => !isHub(n) && !tail.includes(n));
    const plain = new Set(rest);
    const rank = ranks(rest, list.filter((e) => e.from !== e.to && plain.has(e.from) && plain.has(e.to)));
    const layers = [];
    for (const n of rest) {
      const r = rank.get(n);
      while (layers.length <= r) {
        layers.push([]);
      }
      layers[r].push(n);
    }
    const hubLayer = hubs.length ? layers.length : -1;
    for (const layer of [hubs, tail]) {
      if (layer.length) {
        layers.push(layer);
      }
    }
    order(layers, hubLayer);

    let area = 0;
    for (const c of cards.values()) {
      area += (c.h + ROW_GAP) * (CARD_W + COL_GAP);
    }
    const maxH = Math.max(1100, Math.sqrt(area * 0.9));
    const incoming = new Map();
    for (const e of list) {
      if (e.from !== e.to) {
        if (!incoming.has(e.to)) {
          incoming.set(e.to, []);
        }
        incoming.get(e.to).push(e);
      }
    }
    const placed = new Set();
    let x = 0;
    for (const layer of layers) {
      const cols = [];
      let col = [];
      let h = 0;
      for (const n of layer) {
        const ch = cards.get(n).h + ROW_GAP;
        if (col.length && h + ch > maxH) {
          cols.push(col);
          col = [];
          h = 0;
        }
        col.push(n);
        h += ch;
      }
      if (col.length) {
        cols.push(col);
      }
      for (const names of cols) {
        let bottom = -ROW_GAP;
        let drift = 0;
        let wanted = 0;
        for (const n of names) {
          const c = cards.get(n);
          let y = bottom + ROW_GAP;
          const ys = [];
          for (const e of cols.length === 1 ? incoming.get(n) || [] : []) {
            const s = cards.get(e.from);
            if (placed.has(e.from)) {
              ys.push(s.y + fieldY(s, e.fromField) - c.headY);
            }
          }
          if (ys.length) {
            const want = ys.reduce((a, b) => a + b, 0) / ys.length;
            y = Math.max(y, want);
            drift += y - want;
            wanted++;
          }
          c.x = x;
          c.y = y;
          bottom = y + c.h;
          placed.add(n);
        }
        // Cards pushed below where they want to be lift the column back, no
        // higher than the top of the first column.
        const lift = wanted ? Math.min(drift / wanted, Math.max(0, cards.get(names[0]).y)) : 0;
        for (const n of names) {
          cards.get(n).y -= lift;
        }
        x += CARD_W + COL_GAP;
      }
    }

    const saved = savedPositions();
    for (const [name, c] of cards) {
      const p = saved[name];
      if (Array.isArray(p) && p.length === 2 && isFinite(p[0]) && isFinite(p[1])) {
        c.x = p[0];
        c.y = p[1];
      }
      setPos(c);
    }
  }

  function fieldY(c, field) {
    if (c.rows.has(field)) {
      return c.rows.get(field);
    }
    return c.compact ? c.headY : c.moreY;
  }

  // -------------------------------------------------------------- edges

  // makeLink builds an edge: its line and end marks in the line layer and
  // its cardinality pill in the label layer above every line.
  function makeLink(e, lineLayer, labelLayer) {
    const hub = nodes.get(e.to).hub && e.from !== e.to;
    const g = svgEl('g', { class: 'medge' + (e.inferred ? ' inferred' : '') });
    const line = svgEl('path', { class: 'mline' });
    const ends = svgEl('path', { class: 'mends' });
    const rings = svgEl('path', { class: 'mring' });
    g.append(line, ends, rings);
    const label = svgEl('g', { class: 'mlabel' });
    const rect = svgEl('rect', { height: '18', y: '-9', rx: '9' });
    const text = svgEl('text', {});
    text.textContent = e.cardinality;
    label.append(rect, text);
    lineLayer.append(g);
    labelLayer.append(label);
    const parts = e.cardinality.split(' : ');
    return { e, g, line, ends, rings, label, rect, text, w: 0, from: parts[0] || '', to: parts[1] || '', hub };
  }

  // glyph draws the cardinality mark where an edge meets a card at (x, y);
  // dir points away from the card. One is a bar, many a crow's foot, 0..1 a
  // bar and a ring.
  function glyph(kind, x, y, dir) {
    const p = (dx, dy) => (x + dx * dir) + ' ' + (y + dy);
    if (kind === 'many') {
      return { d: 'M' + p(0, -6) + 'L' + p(12, 0) + 'M' + p(0, 6) + 'L' + p(12, 0), ring: '' };
    }
    if (kind === '0..1') {
      const cx = x + 17 * dir;
      return { d: 'M' + p(8, -6) + 'L' + p(8, 6), ring: 'M' + (cx - 3.5) + ' ' + y + 'a3.5 3.5 0 1 0 7 0a3.5 3.5 0 1 0 -7 0' };
    }
    return { d: 'M' + p(8, -6) + 'L' + p(8, 6), ring: '' };
  }

  // freeY returns want, or the middle of the nearest gap between the cards
  // of a lane when want falls on one of them.
  function freeY(list, want) {
    const blocks = list.map((c) => [c.y - LANE_MARGIN, c.y + c.h + LANE_MARGIN]).sort((a, b) => a[0] - b[0]);
    const merged = [];
    for (const b of blocks) {
      const last = merged[merged.length - 1];
      if (last && b[0] <= last[1]) {
        last[1] = Math.max(last[1], b[1]);
      } else {
        merged.push(b.slice());
      }
    }
    for (let i = 0; i < merged.length; i++) {
      const [y0, y1] = merged[i];
      if (want <= y0 || want >= y1) {
        continue;
      }
      const up = i > 0 ? (merged[i - 1][1] + y0) / 2 : y0;
      const down = i < merged.length - 1 ? (y1 + merged[i + 1][0]) / 2 : y1;
      return want - up < down - want ? up : down;
    }
    return want;
  }

  // waypoints returns the points where an edge from sx to tx crosses each
  // lane of cards between them (cards whose columns overlap): a straight
  // stretch through the gap nearest to the direct line.
  function waypoints(a, b, sx, sy, tx, ty) {
    const lo = Math.min(sx, tx);
    const hi = Math.max(sx, tx);
    const inside = [];
    for (const c of cards.values()) {
      if (c !== a && c !== b && c.x < hi - 1 && c.x + c.w > lo + 1) {
        inside.push(c);
      }
    }
    if (!inside.length) {
      return [];
    }
    inside.sort((p, q) => p.x - q.x);
    const lanes = [];
    for (const c of inside) {
      const last = lanes[lanes.length - 1];
      if (last && c.x < last.x1) {
        last.x1 = Math.max(last.x1, c.x + c.w);
        last.cards.push(c);
      } else {
        lanes.push({ x0: c.x, x1: c.x + c.w, cards: [c] });
      }
    }
    const dir = tx > sx ? 1 : -1;
    if (dir < 0) {
      lanes.reverse();
    }
    const pts = [];
    for (const lane of lanes) {
      const x0 = Math.max(lane.x0, lo);
      const x1 = Math.min(lane.x1, hi);
      const enter = dir > 0 ? x0 : x1;
      const leave = dir > 0 ? x1 : x0;
      const t = ((enter + leave) / 2 - sx) / (tx - sx);
      const y = freeY(lane.cards, sy + (ty - sy) * t);
      pts.push([enter, y], [leave, y]);
    }
    return pts;
  }

  // route draws one edge: from the side of the source card facing the
  // target, at the field row, to the facing side of the target at its
  // header; a self reference loops on the right.
  function route(l) {
    const a = cards.get(l.e.from);
    const b = cards.get(l.e.to);
    const sy = a.y + fieldY(a, l.e.fromField);
    const ty = b.y + b.headY;
    let d = '';
    let sx;
    let tx;
    let sdir;
    let tdir;
    if (a === b || (b.x < a.x + a.w && b.x + b.w > a.x)) {
      sx = a.x + a.w;
      tx = b.x + b.w;
      sdir = 1;
      tdir = 1;
      const out = Math.max(sx, tx) + (a === b ? 44 : 64);
      d = 'M' + sx + ' ' + sy + 'C' + out + ' ' + sy + ' ' + out + ' ' + ty + ' ' + tx + ' ' + ty;
    } else {
      sdir = b.x >= a.x + a.w ? 1 : -1;
      tdir = -sdir;
      sx = sdir > 0 ? a.x + a.w : a.x;
      tx = sdir > 0 ? b.x : b.x + b.w;
      d = 'M' + sx + ' ' + sy;
      let px = sx;
      let py = sy;
      for (const [x, y] of waypoints(a, b, sx, sy, tx, ty).concat([[tx, ty]])) {
        if (py === y || Math.abs(x - px) < 1) {
          d += 'L' + x + ' ' + y;
        } else {
          const m = (x - px) / 2;
          d += 'C' + (px + m) + ' ' + py + ' ' + (x - m) + ' ' + y + ' ' + x + ' ' + y;
        }
        px = x;
        py = y;
      }
    }
    l.line.setAttribute('d', d);
    const g1 = glyph(l.from, sx, sy, sdir);
    const g2 = glyph(l.to, tx, ty, tdir);
    l.ends.setAttribute('d', g1.d + g2.d);
    l.rings.setAttribute('d', g1.ring + g2.ring);
  }

  function overlaps(a, b) {
    return a[0] < b[2] && b[0] < a[2] && a[1] < b[3] && b[1] < a[3];
  }

  // placeLabels puts the pill of each visible edge (the highlighted ones
  // while a card has focus) at the middle of the edge, or the nearest point
  // along it that covers no card and no other pill. A pill with no free
  // spot is hidden until its edge is highlighted.
  function placeLabels() {
    const f = focused();
    const blocks = [];
    for (const c of cards.values()) {
      blocks.push([c.x - 4, c.y - 4, c.x + c.w + 4, c.y + c.h + 4]);
    }
    const taken = [];
    const steps = [0.5, 0.42, 0.58, 0.34, 0.66, 0.26, 0.74, 0.18, 0.82];
    for (const l of links) {
      const on = f ? l.e.from === f || l.e.to === f : !l.hub || state.showHubs;
      if (!on) {
        continue;
      }
      const len = l.line.getTotalLength();
      let spot = null;
      let free = false;
      for (const t of steps) {
        const p = l.line.getPointAtLength(len * t);
        const r = [p.x - l.w / 2, p.y - 9, p.x + l.w / 2, p.y + 9];
        if (!spot) {
          spot = r;
        }
        if (!blocks.some((b) => overlaps(b, r)) && !taken.some((b) => overlaps(b, r))) {
          spot = r;
          free = true;
          break;
        }
      }
      l.label.setAttribute('transform', 'translate(' + (spot[0] + l.w / 2) + ' ' + (spot[1] + 9) + ')');
      l.label.classList.toggle('crowded', !free);
      if (free) {
        taken.push([spot[0] - 3, spot[1] - 3, spot[2] + 3, spot[3] + 3]);
      }
    }
  }

  function redraw() {
    for (const l of links) {
      route(l);
    }
    placeLabels();
  }

  let frame = 0;
  function redrawSoon() {
    if (!frame) {
      frame = requestAnimationFrame(() => {
        frame = 0;
        redraw();
      });
    }
  }

  // ------------------------------------------------------------- render

  // render draws the current scope: its models as full cards, the models
  // they point at from outside it as compact cards, and their edges.
  function render(keepCamera) {
    world.querySelectorAll('.mcard').forEach((node) => node.remove());
    svg.textContent = '';
    cards = new Map();
    near = new Map();
    const scope = scopes.get(state.scope);
    const full = new Set(scope.names);
    const shown = scope.names.slice();
    const seen = new Set(shown);
    const list = [];
    for (const e of edges) {
      if (!full.has(e.from)) {
        continue;
      }
      if (!seen.has(e.to)) {
        seen.add(e.to);
        shown.push(e.to);
      }
      list.push(e);
      for (const [p, q] of [[e.from, e.to], [e.to, e.from]]) {
        if (!near.has(p)) {
          near.set(p, new Set());
        }
        if (p !== q) {
          near.get(p).add(q);
        }
      }
    }

    const frag = doc.createDocumentFragment();
    for (const name of shown) {
      const c = makeCard(nodes.get(name), !full.has(name));
      cards.set(name, c);
      frag.append(c.el);
    }
    world.append(frag);
    for (const c of cards.values()) {
      measure(c);
    }
    layout(list);

    lineLayer = svgEl('g', {});
    labelLayer = svgEl('g', {});
    svg.append(lineLayer, labelLayer);
    links = list.map((e) => makeLink(e, lineLayer, labelLayer));
    const widths = new Map();
    for (const l of links) {
      const label = l.text.textContent;
      if (!widths.has(label)) {
        widths.set(label, l.text.getComputedTextLength() + 16);
      }
      l.w = widths.get(label);
      l.rect.setAttribute('width', String(l.w));
      l.rect.setAttribute('x', String(-l.w / 2));
      if (l.hub) {
        l.g.classList.add('hub');
        l.label.classList.add('hub');
      }
    }
    redraw();

    const hubLinks = links.filter((l) => l.hub).length;
    const outside = shown.length - full.size;
    countEl.textContent = full.size + (full.size === 1 ? ' model' : ' models') + ' · ' + list.length + (list.length === 1 ? ' relation' : ' relations') + (outside ? ' · ' + outside + ' linked from outside' : '');
    hubsCount.textContent = hubLinks ? '(' + hubLinks + ')' : '';
    rendered = state.scope;
    if (!keepCamera) {
      home();
    }
    if (findInput.value) {
      find(false);
    }
    applyFocus();
  }

  // ---------------------------------------------------------- highlight

  // applyFocus highlights the hovered card, or else the selected one, with
  // its neighbours and all its edges (hub ones included), dimming the rest.
  function focused() {
    return state.hover && cards.has(state.hover) ? state.hover : state.selected;
  }

  function applyFocus() {
    const f = focused();
    const around = f ? near.get(f) || new Set() : null;
    world.classList.toggle('focus', Boolean(f));
    for (const [name, c] of cards) {
      c.el.classList.toggle('hl', Boolean(f) && (name === f || around.has(name)));
      c.el.classList.toggle('sel', name === state.selected);
    }
    for (const l of links) {
      const on = Boolean(f) && (l.e.from === f || l.e.to === f);
      if (on !== l.g.classList.contains('on')) {
        l.g.classList.toggle('on', on);
        l.label.classList.toggle('on', on);
      }
      if (on) {
        lineLayer.append(l.g);
        labelLayer.append(l.label);
      }
    }
    placeLabels();
  }

  function select(name, center) {
    state.selected = name && cards.has(name) ? name : null;
    applyFocus();
    if (state.selected && center) {
      centerOn(state.selected);
    }
    if (isOpen) {
      setHash(hashFor(), false);
    }
  }

  // ------------------------------------------------------------- camera

  function transform() {
    world.style.transform = 'translate(' + state.x + 'px,' + state.y + 'px) scale(' + state.k + ')';
    world.classList.toggle('far', state.k < 0.8);
    pctEl.textContent = Math.round(state.k * 100) + '%';
  }

  function glide() {
    world.classList.add('glide');
    setTimeout(() => world.classList.remove('glide'), 280);
  }

  function bounds() {
    let x0 = Infinity;
    let y0 = Infinity;
    let x1 = -Infinity;
    let y1 = -Infinity;
    for (const c of cards.values()) {
      x0 = Math.min(x0, c.x);
      y0 = Math.min(y0, c.y);
      x1 = Math.max(x1, c.x + c.w);
      y1 = Math.max(y1, c.y + c.h);
    }
    if (x0 === Infinity) {
      return { x0: 0, y0: 0, w: 1, h: 1 };
    }
    return { x0, y0, w: x1 - x0, h: y1 - y0 };
  }

  // home shows the scope at zoom 1, centred when it fits and from its top
  // left corner when it does not.
  function home() {
    const b = bounds();
    const W = canvas.clientWidth;
    const H = canvas.clientHeight;
    state.k = 1;
    state.x = b.w + 2 * PAD <= W ? (W - b.w) / 2 - b.x0 : PAD - b.x0;
    state.y = b.h + 2 * PAD <= H ? (H - b.h) / 2 - b.y0 : PAD - b.y0;
    transform();
  }

  function fit() {
    const b = bounds();
    const W = canvas.clientWidth;
    const H = canvas.clientHeight;
    const k = Math.max(0.15, Math.min(1, (W - 2 * PAD) / b.w, (H - 2 * PAD) / b.h));
    state.k = k;
    state.x = (W - b.w * k) / 2 - b.x0 * k;
    state.y = Math.max(PAD / 2, (H - b.h * k) / 2) - b.y0 * k;
    glide();
    transform();
  }

  function zoomAt(k, cx, cy) {
    const next = Math.min(2, Math.max(0.15, k));
    state.x = cx - (cx - state.x) * next / state.k;
    state.y = cy - (cy - state.y) * next / state.k;
    state.k = next;
    transform();
  }

  // centerOn brings a card to the middle of the view, or its top near the
  // top when it is taller than the view.
  function centerOn(name) {
    const c = cards.get(name);
    if (!c) {
      return;
    }
    const W = canvas.clientWidth;
    const H = canvas.clientHeight;
    const k = state.k;
    state.x = W / 2 - (c.x + c.w / 2) * k;
    state.y = c.h * k < H - 2 * PAD ? H / 2 - (c.y + c.h / 2) * k : PAD - c.y * k;
    glide();
    transform();
  }

  // --------------------------------------------------------------- find

  function find(center) {
    const q = findInput.value.trim().toLowerCase();
    let best = null;
    let bestScore = 0;
    for (const [name, c] of cards) {
      const short = c.n.short.toLowerCase();
      let score = 0;
      if (q && short === q) {
        score = 3;
      } else if (q && short.startsWith(q)) {
        score = 2;
      } else if (q && (short.includes(q) || name.toLowerCase().includes(q))) {
        score = 1;
      }
      c.el.classList.toggle('match', score > 0);
      if (score > bestScore) {
        best = name;
        bestScore = score;
      }
    }
    world.classList.toggle('finding', Boolean(q));
    findInput.classList.toggle('miss', Boolean(q) && !best);
    if (best && center) {
      centerOn(best);
    }
    return best;
  }

  findInput.addEventListener('input', () => find(true));
  findInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') {
      const best = find(true);
      if (best) {
        findInput.value = '';
        findInput.blur();
        find(false);
        select(best, true);
      }
    } else if (e.key === 'Escape') {
      e.stopPropagation();
      findInput.value = '';
      find(false);
    }
  });

  // -------------------------------------------------------- interaction

  let drag = null;
  let suppressClick = false;

  function onMove(e) {
    const dx = e.clientX - drag.px;
    const dy = e.clientY - drag.py;
    if (!drag.moved && Math.hypot(dx, dy) < 4) {
      return;
    }
    if (!drag.moved) {
      drag.moved = true;
      canvas.classList.add(drag.card ? 'dragging' : 'panning');
      if (drag.card) {
        world.append(drag.card.el);
      }
    }
    if (drag.card) {
      drag.card.x = drag.x0 + dx / state.k;
      drag.card.y = drag.y0 + dy / state.k;
      setPos(drag.card);
      redrawSoon();
    } else {
      state.x = drag.x0 + dx;
      state.y = drag.y0 + dy;
      transform();
    }
  }

  function onUp() {
    window.removeEventListener('pointermove', onMove);
    window.removeEventListener('pointerup', onUp);
    window.removeEventListener('pointercancel', onUp);
    if (drag.moved && drag.card) {
      savePosition(drag.card);
    }
    suppressClick = drag.moved;
    canvas.classList.remove('dragging', 'panning');
    drag = null;
  }

  // The camera is the only scroll: focusing a card must not scroll the
  // clipped canvas, and a mouse press on a card does not focus it.
  canvas.addEventListener('scroll', () => {
    canvas.scrollTop = 0;
    canvas.scrollLeft = 0;
  });
  canvas.addEventListener('mousedown', (e) => {
    if (e.target.closest('.mcard') && !e.target.closest('button')) {
      e.preventDefault();
    }
  });

  // Dragging a card moves it; dragging the background pans.
  canvas.addEventListener('pointerdown', (e) => {
    suppressClick = false;
    if (e.button !== 0 || drag || e.target.closest('button, a, input, select')) {
      return;
    }
    const cardEl = e.target.closest('.mcard');
    const card = cardEl ? cards.get(cardEl.dataset.node) : null;
    drag = { card, px: e.clientX, py: e.clientY, x0: card ? card.x : state.x, y0: card ? card.y : state.y, moved: false };
    window.addEventListener('pointermove', onMove);
    window.addEventListener('pointerup', onUp);
    window.addEventListener('pointercancel', onUp);
  });

  // A scroll pans (two-finger scroll on a trackpad); with Ctrl or Cmd, or a
  // pinch (which browsers report as Ctrl + wheel), it zooms at the pointer.
  canvas.addEventListener('wheel', (e) => {
    e.preventDefault();
    const unit = e.deltaMode === 1 ? 16 : e.deltaMode === 2 ? canvas.clientHeight : 1;
    if (e.ctrlKey || e.metaKey) {
      const r = canvas.getBoundingClientRect();
      const delta = Math.max(-60, Math.min(60, e.deltaY * unit));
      zoomAt(state.k * Math.exp(-delta * 0.01), e.clientX - r.left, e.clientY - r.top);
      return;
    }
    state.x -= e.deltaX * unit;
    state.y -= e.deltaY * unit;
    transform();
  }, { passive: false });

  canvas.addEventListener('pointerover', (e) => {
    if (drag) {
      return;
    }
    const cardEl = e.target.closest('.mcard');
    const name = cardEl ? cardEl.dataset.node : null;
    if (name !== state.hover) {
      state.hover = name;
      applyFocus();
    }
  });
  canvas.addEventListener('pointerleave', () => {
    if (state.hover && !drag) {
      state.hover = null;
      applyFocus();
    }
  });

  section.addEventListener('click', (e) => {
    const t = e.target instanceof Element ? e.target : null;
    if (!t) {
      return;
    }
    // The click that ends a drag or a pan is not a click on a card.
    if (suppressClick && t.closest('[data-map-canvas]')) {
      suppressClick = false;
      return;
    }
    suppressClick = false;
    const ref = t.closest('[data-map-ref]');
    if (ref) {
      select(ref.dataset.mapRef, true);
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
      render(true);
      return;
    }
    const zoom = t.closest('[data-map-zoom]');
    if (zoom) {
      const how = zoom.dataset.mapZoom;
      if (how === 'fit') {
        fit();
      } else {
        zoomAt(state.k * (how === 'in' ? 1.25 : 0.8), canvas.clientWidth / 2, canvas.clientHeight / 2);
      }
      return;
    }
    if (t.closest('[data-map-reset]')) {
      if (store) {
        store.removeItem(positionsKey());
      }
      render(false);
      return;
    }
    const cardEl = t.closest('.mcard');
    if (cardEl) {
      select(cardEl.dataset.node, false);
    } else if (t.closest('[data-map-canvas]')) {
      select(null, false);
    }
  });

  section.addEventListener('dblclick', (e) => {
    const cardEl = e.target instanceof Element ? e.target.closest('.mcard') : null;
    if (cardEl && !e.target.closest('button')) {
      openInReference(cardEl.dataset.node);
    }
  });

  section.addEventListener('keydown', (e) => {
    const cardEl = e.target instanceof Element ? e.target.closest('.mcard') : null;
    if (cardEl && e.target === cardEl && e.key === 'Enter') {
      e.preventDefault();
      select(cardEl.dataset.node, true);
    }
  });

  doc.addEventListener('keydown', (e) => {
    if (isOpen && e.key === 'Escape' && state.selected && !doc.querySelector('.mmodal-back')) {
      select(null, false);
    }
  });

  scopeSelect.addEventListener('change', () => {
    state.scope = scopeSelect.value;
    state.selected = null;
    state.hover = null;
    render(false);
    setHash(hashFor(), false);
  });

  hubsBox.addEventListener('change', () => {
    state.showHubs = hubsBox.checked;
    world.classList.toggle('hubs', state.showHubs);
    placeLabels();
  });

  // openInReference closes the map and shows the model card, through its
  // sidebar link when there is one so the reference updates as usual.
  function openInReference(name) {
    const n = nodes.get(name);
    if (!n) {
      return;
    }
    const link = Array.from(doc.querySelectorAll('.sidebar a.nav-model')).find((a) => decode(a.getAttribute('href').slice(1)) === n.anchor);
    if (link) {
      link.click();
      return;
    }
    close();
    const target = doc.getElementById(n.anchor);
    if (target) {
      target.scrollIntoView({ block: 'start' });
      setHash(n.anchor, true);
    }
  }

  // ------------------------------------------------------- open / close

  function isMapHash(h) {
    return h === '#map' || h.startsWith('#map/');
  }

  function hashFor() {
    let h = 'map/' + encodeURIComponent(state.scope);
    if (state.selected) {
      h += '/' + encodeURIComponent(state.selected);
    }
    return h;
  }

  // setSwitch marks the app bar tab of the map, or of the view under it.
  function setSwitch(map) {
    const current = map ? 'map' : doc.body.dataset.mode;
    doc.querySelectorAll('.modes a[data-mode]').forEach((a) => {
      if (a.dataset.mode === current) {
        a.setAttribute('aria-current', 'page');
      } else {
        a.removeAttribute('aria-current');
      }
    });
  }

  // open shows the map on a scope ('' keeps the current one) with a card
  // selected ('' for none); push is true to add a history entry, false to
  // replace the current one and null to leave the hash alone.
  function open(scope, focus, push) {
    if (!isOpen) {
      refScroll = window.scrollY;
      isOpen = true;
      section.hidden = false;
      doc.body.classList.add('map-open');
      setSwitch(true);
      const sidebar = doc.getElementById('sidebar');
      const menu = doc.querySelector('[data-menu]');
      if (sidebar && menu && sidebar.classList.contains('open')) {
        menu.click();
      }
      window.scrollTo(0, 0);
    }
    let next = state.scope;
    if (scopes.has(scope)) {
      next = scope;
    }
    if (next !== rendered) {
      state.scope = next;
      state.selected = null;
      scopeSelect.value = next;
      render(false);
    }
    if (focus && cards.has(focus)) {
      state.selected = focus;
      applyFocus();
      centerOn(focus);
    } else if (!focus && state.selected) {
      state.selected = null;
      applyFocus();
    }
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
    state.hover = null;
    section.hidden = true;
    doc.body.classList.remove('map-open');
    setSwitch(false);
    window.scrollTo(0, refScroll);
  }

  doc.addEventListener('click', (e) => {
    const t = e.target instanceof Element ? e.target : null;
    if (!t) {
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
