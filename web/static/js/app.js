// Skien Mails — application shell, routing, live updates and keyboard shortcuts.
import { SPRITE } from './icons.js';
import { $, html, raw, icon, el, setHTML, api, bus, snack, dialog, menu, avatar, bytes, greeting, toastError, debounce } from './lib.js';
import { S, FOLDERS, folderName, navigate, parseRoute, setTheme, themePref, setCounts, refreshCounts, loadDirectory, applyTheme, pref } from './state.js';
import * as auth from './auth.js';
import * as mail from './mail.js';
import * as compose from './compose.js';

document.body.insertAdjacentHTML('afterbegin', SPRITE);
const root = document.getElementById('root');
let shellReady = false;
// Settings, admin and people are loaded on first use to keep startup light.
const loaders = {
  settings: () => import('./settings.js'),
  admin: () => import('./admin.js'),
  people: () => import('./people.js'),
};
const view = (name) => loaders[name]();

// ---------------------------------------------------------------- boot
async function boot() {
  shellReady = false;
  try {
    const st = await api('/setup');
    if (st.needs_setup) return auth.renderSetup(root, boot);
  } catch (e) {
    return renderOffline(e);
  }
  try {
    S.me = await api('/me');
  } catch (e) {
    if (e.status === 401) return auth.renderLogin(root, boot);
    return renderOffline(e);
  }
  if (S.me.user.must_change_password) return auth.renderForceChange(root, S.me, boot);
  setCounts(S.me.counts);
  renderShell();
  loadDirectory();
  connectEvents();
  route();
}

function renderOffline(e) {
  setHTML(root, html`<div class="auth"><div class="auth-card narrow" style="text-align:center">
    <div>${icon('cloud_off', 'l')}</div><h1 style="font-size:28px">Can't connect</h1><p class="lead">${e.message}</p>
    <div class="auth-actions" style="justify-content:center"><button class="btn filled" id="retry">Try again</button></div></div></div>`);
  $('#retry').onclick = boot;
}

bus.on('unauthorized', () => {
  if (!shellReady) return;
  shellReady = false;
  closeEvents();
  compose.closeAll(true); // keeps an open message in this browser; offered back after sign-in
  auth.renderLogin(root, boot, "You've been signed out. Sign in again to continue.");
});
bus.on('mustchange', () => boot());

// ---------------------------------------------------------------- shell
function renderShell() {
  const u = S.me.user;
  const collapsed = pref.get('drawer.collapsed', false);
  setHTML(root, html`
  <div class="app ${collapsed ? 'collapsed' : ''}" id="app">
    <header class="topbar">
      <button class="icon-btn" id="menu-btn" data-tip="Main menu" aria-label="Main menu">${icon('menu')}</button>
      <a class="brand" href="#/inbox" aria-label="Skien Mails home"><img src="/icons/favicon.svg" alt=""><span><b>Skien</b> Mails</span></a>
      <form class="search" id="search" role="search" autocomplete="off">
        <button class="icon-btn" type="submit" aria-label="Search" data-tip="Search">${icon('search')}</button>
        <input name="q" id="search-input" placeholder="Search mail" aria-label="Search mail" spellcheck="false">
        <button class="icon-btn" type="button" id="search-clear" aria-label="Clear search" hidden>${icon('close')}</button>
        <button class="icon-btn desktop-only" type="button" id="search-opts" aria-label="Search options" data-tip="Search options">${icon('tune')}</button>
        <div class="search-help" id="search-help">
          Search across subjects, people and message text. Narrow it down with:
          <div class="ops">
            <span><code>from:ravi</code></span><span><code>to:sajin</code></span><span><code>subject:invoice</code></span>
            <span><code>has:attachment</code></span><span><code>is:unread</code></span><span><code>is:starred</code></span>
            <span><code>in:sent</code></span><span><code>in:anywhere</code></span><span><code>after:2026-01-31</code></span><span><code>before:2026-03-01</code></span>
          </div>
        </div>
      </form>
      <div class="top-actions">
        <button class="icon-btn" id="theme-btn" aria-label="Switch theme" data-tip="Switch theme"></button>
        <button class="icon-btn desktop-only" id="help-btn" aria-label="Keyboard shortcuts" data-tip="Keyboard shortcuts">${icon('keyboard')}</button>
        <a class="icon-btn desktop-only" href="#/settings" aria-label="Settings" data-tip="Settings">${icon('settings')}</a>
        <button class="avatar-btn" id="account-btn" aria-label="Account" data-tip="${u.name} · ${u.email}">${avatar(u.name, u.email, 's32')}</button>
      </div>
    </header>
    <nav class="drawer" id="drawer" aria-label="Folders">
      <button class="compose-btn" id="compose-btn">${icon('edit')}<span>Compose</span></button>
      <div class="nav" id="nav"></div>
      <div class="drawer-foot" id="drawer-foot"></div>
    </nav>
    <div class="drawer-scrim" id="drawer-scrim"></div>
    <button class="fab" id="fab" aria-label="Compose">${icon('edit')}<span>Compose</span></button>
    <main class="main">
      <section class="surface" id="view" aria-live="polite"></section>
      <aside class="rail" id="rail"></aside>
    </main>
  </div>`);
  shellReady = true;

  const app = $('#app');
  $('#menu-btn').onclick = () => {
    if (matchMedia('(max-width: 900px)').matches) {
      app.classList.toggle('drawer-open');
      if (app.classList.contains('drawer-open')) setTimeout(() => $('#drawer .nav-item.active, #compose-btn')?.focus({ preventScroll: true }), 50);
    }
    else { app.classList.toggle('collapsed'); pref.set('drawer.collapsed', app.classList.contains('collapsed')); }
  };
  $('#drawer-scrim').onclick = () => app.classList.remove('drawer-open');
  $('#compose-btn').onclick = () => { app.classList.remove('drawer-open'); compose.open({}); };
  $('#fab').onclick = () => compose.open({});
  $('#help-btn').onclick = showShortcuts;
  $('#account-btn').onclick = (e) => accountMenu(e.currentTarget);
  $('#theme-btn').onclick = () => setTheme(document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark');
  paintThemeButton();

  const form = $('#search');
  const input = $('#search-input');
  const clear = $('#search-clear');
  form.onsubmit = (e) => {
    e.preventDefault();
    const q = input.value.trim();
    input.blur();
    if (q) navigate('#/search/' + encodeURIComponent(q));
  };
  input.oninput = () => { clear.hidden = !input.value; };
  clear.onclick = () => { input.value = ''; clear.hidden = true; if (S.route.folder === 'search') navigate('#/inbox'); else input.focus(); };
  $('#search-opts').onclick = () => { $('#search-help').classList.toggle('show'); input.focus(); };

  renderNav();
}

function paintThemeButton() {
  const b = $('#theme-btn');
  if (!b) return;
  const dark = document.documentElement.dataset.theme === 'dark';
  setHTML(b, icon(dark ? 'light_mode' : 'dark_mode'));
  b.dataset.tip = dark ? 'Switch to light theme' : 'Switch to dark theme';
}
bus.on('theme', paintThemeButton);

function renderNav() {
  const nav = $('#nav');
  if (!nav) return;
  const r = S.route;
  const cur = r.name === 'thread' || r.name === 'list' || r.name === 'drafts' ? r.folder : r.name;
  const c = S.counts || {};
  const item = (href, ic, label, active, count, unread) => html`
    <a class="nav-item ${active ? 'active' : ''} ${unread ? 'unread' : ''}" href="${href}" ${active ? raw('aria-current="page"') : ''} data-tip="${label}">
      ${icon(ic)}<span>${label}</span>${count ? html`<b class="count">${count > 999 ? '999+' : count}</b>` : ''}
    </a>`;
  const folders = FOLDERS.map((f) => {
    const active = cur === f.id;
    const n = f.id === 'drafts' ? c.drafts : f.id === 'inbox' || f.id === 'spam' ? c[f.id] : 0;
    const ic = active ? (f.active || f.icon + '-f') : f.icon;
    return item('#/' + f.id, ic, f.name, active, n, f.id === 'inbox' && n > 0);
  });
  setHTML(nav, html`${folders}
    <div class="nav-section">Workspace</div>
    ${item('#/people', cur === 'people' ? 'contacts-f' : 'contacts', 'People', cur === 'people')}
    ${item('#/settings', cur === 'settings' ? 'settings-f' : 'settings', 'Settings', cur === 'settings')}
    ${S.me.user.role === 'admin' ? item('#/admin', cur === 'admin' ? 'dashboard-f' : 'admin_panel_settings', 'Admin console', cur === 'admin') : ''}`);
  nav.querySelectorAll('a').forEach((a) => a.addEventListener('click', () => $('#app')?.classList.remove('drawer-open')));
  renderFoot();
  updateTitle();
}

function renderFoot() {
  const u = S.me.user;
  const foot = $('#drawer-foot');
  if (!foot) return;
  const quota = u.quota_bytes;
  const pct = quota ? Math.min(100, (u.used_bytes / quota) * 100) : 0;
  setHTML(foot, html`<div class="storage-mini">
      ${quota ? html`<div class="bar"><i style="width:${pct.toFixed(1)}%"></i></div>${bytes(u.used_bytes)} of ${bytes(quota)} used` : html`${bytes(u.used_bytes)} used`}
    </div>
    <div class="desktop-only" style="margin-top:10px">Shortcuts: <b>c</b> compose · <b>/</b> search · <b>?</b> help</div>`);
}

bus.on('counts', () => { if (shellReady) { renderNav(); updateBadge(); } });

function updateTitle() {
  const r = S.route;
  const n = S.counts?.inbox || 0;
  let label = 'Mail';
  if (r.name === 'list' || r.name === 'thread' || r.name === 'drafts') label = r.folder === 'search' ? `Search results` : folderName(r.folder);
  else if (r.name === 'settings') label = 'Settings';
  else if (r.name === 'admin') label = 'Admin console';
  else if (r.name === 'people') label = 'People';
  document.title = `${label}${r.folder === 'inbox' && n ? ` (${n})` : ''} – ${S.me?.user.email || ''} – Skien Mails`;
}

function updateBadge() {
  const n = S.counts?.inbox || 0;
  if ('setAppBadge' in navigator) (n ? navigator.setAppBadge(n) : navigator.clearAppBadge()).catch?.(() => {});
}

// ---------------------------------------------------------------- account menu & help
function accountMenu(anchor) {
  const u = S.me.user;
  const m = menu(anchor, [
    { html: html`<div class="account-card">
        <div class="email">${u.email}</div>
        ${avatar(u.name, u.email, 's64')}
        <div class="hi">Hi, ${u.name.split(' ')[0]}!</div>
        <a class="btn outlined" href="#/settings/general" style="width:100%">Manage your account</a>
      </div>` },
    'sep',
    { icon: 'contacts', label: 'People directory', onClick: () => navigate('#/people') },
    ...(u.role === 'admin' ? [{ icon: 'admin_panel_settings', label: 'Admin console', onClick: () => navigate('#/admin') }] : []),
    ...(S.installPrompt ? [{ icon: 'install_desktop', label: 'Install Skien Mails app', onClick: installApp }] : []),
    { icon: 'keyboard', label: 'Keyboard shortcuts', onClick: showShortcuts },
    'sep',
    { icon: 'logout', label: 'Sign out', onClick: signOut },
  ], { className: 'account' });
  m.style.padding = '8px 0';
  m.querySelector('.account-card a').onclick = () => { m.remove(); };
}

async function signOut() {
  try { await compose.closeAll(); await api('/auth/logout', { method: 'POST' }); } catch { /* ignore */ }
  closeEvents();
  shellReady = false;
  S.me = null;
  if ('clearAppBadge' in navigator) navigator.clearAppBadge().catch(() => {});
  history.replaceState(null, '', '#/inbox');
  auth.renderLogin(root, boot);
}

async function installApp() {
  const p = S.installPrompt;
  if (!p) return;
  S.installPrompt = null;
  p.prompt();
  const { outcome } = await p.userChoice;
  if (outcome === 'accepted') snack('Skien Mails is now installed as an app');
}
addEventListener('beforeinstallprompt', (e) => { e.preventDefault(); S.installPrompt = e; });
addEventListener('appinstalled', () => { S.installPrompt = null; });

function showShortcuts() {
  const k = (keys, label) => html`<div><span>${label}</span><span>${keys.split(' ').map((x) => html`<span class="kbd">${x}</span> `)}</span></div>`;
  dialog({
    title: 'Keyboard shortcuts', wide: true,
    body: html`<div class="shortcuts">
      ${k('c', 'Compose')}${k('/', 'Search')}
      ${k('j', 'Next conversation')}${k('k', 'Previous conversation')}
      ${k('o', 'Open conversation')}${k('u', 'Back to list')}
      ${k('e', 'Archive')}${k('#', 'Delete')}
      ${k('!', 'Report spam')}${k('s', 'Star / unstar')}
      ${k('r', 'Reply')}${k('a', 'Reply all')}
      ${k('f', 'Forward')}${k('x', 'Select conversation')}
      ${k('Shift+i', 'Mark as read')}${k('Shift+u', 'Mark as unread')}
      ${k('g i', 'Go to Inbox')}${k('g s', 'Go to Starred')}
      ${k('g t', 'Go to Sent')}${k('g d', 'Go to Drafts')}
      ${k('Ctrl+Enter', 'Send (in compose)')}${k('?', 'This help')}
    </div>`,
  });
}

// ---------------------------------------------------------------- routing
let renderSeq = 0;
async function route() {
  if (!shellReady) return;
  const r = parseRoute();
  const prev = S.route;
  S.route = r;
  S.viewKeys = null;
  const seq = ++renderSeq;
  const viewEl = $('#view');
  const rail = $('#rail');
  $('#search-help')?.classList.remove('show');

  if (r.name === 'compose') {
    const p = r.params;
    const opts = {};
    const mailto = p.get('mailto');
    if (mailto) Object.assign(opts, compose.parseMailto(mailto));
    if (p.get('to')) opts.to = p.get('to').split(',').map((e) => ({ email: e.trim() }));
    navigate('#/inbox', { replace: true });
    compose.open(opts);
    return;
  }
  const input = $('#search-input');
  if (input) {
    input.value = r.folder === 'search' ? r.q || '' : '';
    $('#search-clear').hidden = !input.value;
  }
  renderNav();
  $('#app').dataset.view = r.name;
  const mailRoute = r.name === 'list' || r.name === 'thread' || r.name === 'drafts';
  rail.hidden = !mailRoute;
  try {
    if (r.name === 'list') await mail.renderList(viewEl, r, prev);
    else if (r.name === 'thread') await mail.renderThread(viewEl, r);
    else if (r.name === 'drafts') await mail.renderDrafts(viewEl, r);
    else if (r.name === 'settings') (await view('settings')).render(viewEl, r);
    else if (r.name === 'people') (await view('people')).render(viewEl, r);
    else if (r.name === 'admin') {
      if (S.me.user.role !== 'admin') return navigate('#/inbox', { replace: true });
      (await view('admin')).render(viewEl, r);
    }
  } catch (e) {
    if (seq === renderSeq) toastError(e);
  }
  if (mailRoute) renderRail();
  updateTitle();
}
addEventListener('hashchange', route);
bus.on('route', route);

// ---------------------------------------------------------------- right rail
let stats = null;
async function loadStats() {
  const start = new Date();
  start.setHours(0, 0, 0, 0);
  try { stats = await api('/stats?since=' + start.getTime(), { background: true }); } catch { /* ignore */ }
}
const renderRailSoon = debounce(() => renderRail(), 400);

async function renderRail() {
  const rail = $('#rail');
  if (!rail || rail.hidden || getComputedStyle(rail).display === 'none') return;
  await loadStats();
  if (!stats) return;
  const s = stats;
  if (S.me.user.used_bytes !== s.used) { S.me.user.used_bytes = s.used; renderFoot(); }
  const pct = s.today_received ? Math.round((s.today_read / s.today_received) * 100) : 0;
  const quota = s.quota;
  const qpct = quota ? Math.min(100, Math.round((s.used / quota) * 100)) : 0;
  const ppl = (S.directory?.people || []).filter((p) => p.email !== S.me.user.email).slice(0, 6);
  setHTML(rail, html`
    <div class="card">
      <h3>Today <span class="pct" data-tip="Share of today's mail you've read">${pct}% read</span></h3>
      <div class="progress"><i style="width:${pct}%"></i></div>
      <div class="stats3">
        <div><b>${s.today_received}</b><small>Received</small></div>
        <div class="g"><b>${s.today_sent}</b><small>Sent</small></div>
        <div class="r"><b>${s.today_unread}</b><small>Unread</small></div>
      </div>
    </div>
    <div class="card">
      <h3>Storage <span class="pct">${quota ? qpct + '%' : '∞'}</span></h3>
      <div class="progress ${qpct > 90 ? 'full' : qpct > 75 ? 'warn' : ''}"><i style="width:${quota ? qpct : 3}%"></i></div>
      <div class="stats3">
        <div><b>${s.messages}</b><small>Messages</small></div>
        <div class="g"><b>${bytes(s.used).split(' ')[0]}</b><small>${bytes(s.used).split(' ')[1]} used</small></div>
        <div class="${quota ? 'r' : ''}"><b>${quota ? bytes(Math.max(0, quota - s.used)).split(' ')[0] : '∞'}</b><small>${quota ? bytes(Math.max(0, quota - s.used)).split(' ')[1] + ' free' : 'No limit'}</small></div>
      </div>
    </div>
    ${ppl.length ? html`<div class="card">
      <h3>People <span class="muted small">${S.me.org.name}</span></h3>
      <div style="display:flex;gap:6px;flex-wrap:wrap;margin-top:14px">${ppl.map((p) => html`<a href="#" data-mailto="${p.email}" data-tip="Email ${p.name}">${avatar(p.name, p.email)}</a>`)}</div>
      <a class="link" href="#/people">${icon('contacts', 's')} Open directory</a>
    </div>` : ''}`);
  rail.querySelectorAll('[data-mailto]').forEach((a) => a.onclick = (e) => {
    e.preventDefault();
    const p = S.directory.people.find((x) => x.email === a.dataset.mailto);
    compose.open({ to: [{ email: p.email, name: p.name }] });
  });
}
bus.on('directory', () => renderRailSoon());

// ---------------------------------------------------------------- live updates
let es = null;
let esRetry = null;
function connectEvents() {
  closeEvents();
  es = new EventSource('/api/events');
  es.onmessage = (m) => {
    let ev;
    try { ev = JSON.parse(m.data); } catch { return; }
    if (ev.type === 'signout') { closeEvents(); boot(); return; }
    if (ev.type === 'mail') notifyNewMail(ev);
    onSync(ev);
  };
  es.onerror = () => {
    // EventSource reconnects by itself; if the session ended, the next API call shows the sign-in page.
    clearTimeout(esRetry);
    esRetry = setTimeout(() => { if (shellReady && es?.readyState === EventSource.CLOSED) connectEvents(); }, 10000);
  };
}
function closeEvents() { es?.close(); es = null; }

const onSync = debounce(() => {
  // list and conversation responses carry the folder counts; only fetch them separately otherwise
  if (!mail.onRemoteChange()) refreshCounts();
  renderRailSoon();
}, 300, 1500);

async function notifyNewMail(ev) {
  if (!pref.get('notify', false) || !('Notification' in window) || Notification.permission !== 'granted') return;
  if (document.visibilityState === 'visible' && document.hasFocus()) return;
  const title = ev.from || 'New message';
  const opts = { body: ev.subject || '(no subject)', icon: '/icons/icon-192.png', badge: '/icons/icon-192.png', tag: 'sm-new-mail', renotify: true, data: { url: '/#/inbox' } };
  try {
    const reg = await navigator.serviceWorker?.getRegistration();
    if (reg) reg.showNotification(title, opts);
    else new Notification(title, opts);
  } catch { /* ignore */ }
}

addEventListener('online', () => { S.online = true; document.querySelector('.offline-pill')?.remove(); if (shellReady) { connectEvents(); onSync(); } });
addEventListener('offline', () => { S.online = false; if (!document.querySelector('.offline-pill')) document.body.append(el(html`<div class="pill amber offline-pill">${icon('cloud_off', 'xs')} You're offline</div>`)); });
document.addEventListener('visibilitychange', () => { if (document.visibilityState === 'visible' && shellReady) onSync(); });

// ---------------------------------------------------------------- keyboard
let gPrefix = 0;
document.addEventListener('keydown', (e) => {
  if (!shellReady || e.defaultPrevented) return;
  const t = e.target;
  const typing = t.closest?.('input, textarea, select, [contenteditable="true"], [contenteditable=""]');
  if (document.querySelector('dialog[open]')) return;
  if (typing) {
    if (e.key === 'Escape' && t.id === 'search-input') t.blur();
    return;
  }
  if (e.ctrlKey || e.metaKey || e.altKey) return;
  const app = $('#app');
  if (e.key === 'Escape' && app?.classList.contains('drawer-open')) { app.classList.remove('drawer-open'); $('#menu-btn')?.focus(); return; }
  if (Date.now() - gPrefix < 1200) {
    gPrefix = 0;
    const go = { i: '#/inbox', s: '#/starred', t: '#/sent', d: '#/drafts', a: '#/all', p: '#/people' }[e.key];
    if (go) { e.preventDefault(); navigate(go); }
    return;
  }
  if (S.viewKeys && S.viewKeys(e) === true) { e.preventDefault(); return; }
  switch (e.key) {
    case 'c': e.preventDefault(); compose.open({}); break;
    case '/': e.preventDefault(); $('#search-input')?.focus(); break;
    case '?': e.preventDefault(); showShortcuts(); break;
    case 'g': gPrefix = Date.now(); break;
  }
});

// ---------------------------------------------------------------- service worker & updates
if ('serviceWorker' in navigator && (location.protocol === 'https:' || location.hostname === 'localhost')) {
  const hadController = !!navigator.serviceWorker.controller;
  navigator.serviceWorker.register('/sw.js').catch(() => {});
  navigator.serviceWorker.addEventListener('controllerchange', () => {
    if (hadController) snack('A new version of Skien Mails is ready', { action: 'Reload', onAction: () => location.reload(), timeout: 0 });
  });
}

applyTheme();
boot();
