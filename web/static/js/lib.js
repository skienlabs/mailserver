// Shared helpers: safe HTML templating, API client, dialogs, menus, snackbars, formatting.

// ---------------------------------------------------------------- DOM & templating
export const $ = (sel, root = document) => root.querySelector(sel);
export const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];

const ESC = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
export const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ESC[c]);

class Raw {
  constructor(s) { this.s = s; }
  toString() { return this.s; }
}
/** Marks a string as trusted HTML. Only use for markup produced by html``. */
export const raw = (s) => new Raw(String(s ?? ''));

function render(v) {
  if (v == null || v === false || v === true) return '';
  if (v instanceof Raw) return v.s;
  if (Array.isArray(v)) return v.map(render).join('');
  return esc(v);
}

/** Tagged template that escapes every interpolation unless it is raw()/html``. */
export function html(strings, ...vals) {
  let out = '';
  strings.forEach((s, i) => {
    out += s;
    if (i < vals.length) out += render(vals[i]);
  });
  return new Raw(out);
}

export const icon = (name, cls = '') => raw(`<svg class="ic ${cls}" aria-hidden="true"><use href="#i-${name}"></use></svg>`);

/** Creates a single element from markup. */
export function el(markup) {
  const t = document.createElement('template');
  t.innerHTML = String(markup).trim();
  return t.content.firstElementChild;
}

export function setHTML(node, markup) {
  node.innerHTML = Array.isArray(markup) ? markup.map(String).join('') : String(markup);
  return node;
}

// ---------------------------------------------------------------- events
const listeners = {};
export const bus = {
  on(ev, fn) { (listeners[ev] ||= new Set()).add(fn); return () => listeners[ev].delete(fn); },
  emit(ev, data) { (listeners[ev] || []).forEach((fn) => { try { fn(data); } catch (e) { console.error(e); } }); },
};

// ---------------------------------------------------------------- loading indicator
let pending = 0;
let loadTimer = null;
function loading(delta) {
  pending = Math.max(0, pending + delta);
  let bar = document.getElementById('loading-bar');
  if (!bar) {
    bar = el('<div id="loading-bar" class="loading-bar"></div>');
    document.body.append(bar);
  }
  clearTimeout(loadTimer);
  if (pending > 0) loadTimer = setTimeout(() => bar.classList.add('on'), 250);
  else bar.classList.remove('on');
}

// ---------------------------------------------------------------- API
export class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

export async function api(path, { method = 'GET', body, form, signal, background } = {}) {
  const opts = { method, headers: { 'X-SM': '1' }, credentials: 'same-origin', signal };
  if (form) opts.body = form;
  else if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  if (!background) loading(1);
  let res;
  try {
    res = await fetch('/api' + path, opts);
  } catch (e) {
    if (e.name === 'AbortError') throw e;
    throw new ApiError(0, "Can't reach the server. Check your connection.");
  } finally {
    if (!background) loading(-1);
  }
  let data = null;
  try { data = await res.json(); } catch { /* empty body */ }
  if (!res.ok) {
    const msg = data?.error || `Request failed (${res.status})`;
    if (res.status === 401 && !path.startsWith('/auth/login') && !path.startsWith('/setup')) bus.emit('unauthorized');
    if (res.status === 403 && msg === 'password_change_required') bus.emit('mustchange');
    throw new ApiError(res.status, msg);
  }
  return data;
}

/** Multipart upload with progress. */
export function upload(path, file, onProgress, field = 'file') {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open('POST', '/api' + path);
    xhr.setRequestHeader('X-SM', '1');
    xhr.responseType = 'json';
    xhr.upload.onprogress = (e) => e.lengthComputable && onProgress?.(e.loaded / e.total);
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) resolve(xhr.response);
      else reject(new ApiError(xhr.status, xhr.response?.error || 'Upload failed'));
    };
    xhr.onerror = () => reject(new ApiError(0, 'Upload failed — check your connection'));
    const fd = new FormData();
    fd.append(field, file, file.name);
    xhr.send(fd);
    upload.last = xhr;
  });
}

// ---------------------------------------------------------------- formatting
// Some browsers report tags Intl can't use (e.g. "en-US@posix"); pick the first valid one.
const locale = (() => {
  for (const l of [...(navigator.languages || []), navigator.language, 'en-IN']) {
    try { if (l) { new Intl.DateTimeFormat(l); return l; } } catch { /* try the next */ }
  }
  return 'en';
})();
const fmtTime = new Intl.DateTimeFormat(locale, { hour: 'numeric', minute: '2-digit' });
const fmtDay = new Intl.DateTimeFormat(locale, { day: 'numeric', month: 'short' });
const fmtDate = new Intl.DateTimeFormat(locale, { day: 'numeric', month: 'short', year: 'numeric' });
const fmtFull = new Intl.DateTimeFormat(locale, { weekday: 'short', day: 'numeric', month: 'short', year: 'numeric', hour: 'numeric', minute: '2-digit' });
const fmtLong = new Intl.DateTimeFormat(locale, { weekday: 'long', day: 'numeric', month: 'long', year: 'numeric' });
const rtf = new Intl.RelativeTimeFormat(locale, { numeric: 'auto' });

export function shortDate(ms) {
  const d = new Date(ms), now = new Date();
  if (d.toDateString() === now.toDateString()) return fmtTime.format(d);
  if (d.getFullYear() === now.getFullYear()) return fmtDay.format(d);
  return fmtDate.format(d);
}
export const fullDate = (ms) => fmtFull.format(new Date(ms));
export const longDate = (ms) => fmtLong.format(new Date(ms));
export const dateOnly = (ms) => fmtDate.format(new Date(ms));
export function ago(ms) {
  const s = (ms - Date.now()) / 1000;
  const a = Math.abs(s);
  if (a < 60) return rtf.format(Math.round(s), 'second');
  if (a < 3600) return rtf.format(Math.round(s / 60), 'minute');
  if (a < 86400) return rtf.format(Math.round(s / 3600), 'hour');
  if (a < 86400 * 30) return rtf.format(Math.round(s / 86400), 'day');
  return dateOnly(ms);
}
export function bytes(n) {
  if (!n) return '0 B';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.min(u.length - 1, Math.floor(Math.log(n) / Math.log(1024)));
  const v = n / 1024 ** i;
  return `${v >= 100 || i === 0 ? Math.round(v) : v.toFixed(1)} ${u[i]}`;
}
export function greeting() {
  const h = new Date().getHours();
  return h < 12 ? 'Good morning' : h < 17 ? 'Good afternoon' : 'Good evening';
}
export const plural = (n, one, many) => `${n} ${n === 1 ? one : many || one + 's'}`;

// ---------------------------------------------------------------- avatars
const AV_COLORS = ['#1a73e8', '#d93025', '#188038', '#b45309', '#9334e6', '#c5221f', '#00796b', '#b06000', '#3367d6', '#0b8043', '#8430ce', '#d01884', '#5f6368', '#1967d2'];
function hash(s) { let h = 0; for (const c of s) h = (h * 31 + c.charCodeAt(0)) | 0; return Math.abs(h); }
export const people = new Map(); // email -> {id, name, avatar_ver}

export function avatar(name, email, cls = '') {
  const key = (email || name || '?').toLowerCase();
  const p = people.get(key);
  const label = (name || email || '?').trim();
  const letter = [...label.replace(/^["'(]+/, '')][0] || '?';
  if (p && p.avatar_ver) {
    return html`<span class="av ${cls}" title="${label}"><img src="/api/avatar/${p.id}?v=${p.avatar_ver}" alt="" loading="lazy"></span>`;
  }
  return html`<span class="av ${cls}" style="background:${AV_COLORS[hash(key) % AV_COLORS.length]}" title="${label}">${letter}</span>`;
}

// ---------------------------------------------------------------- snackbar
let snackEl = null;
let snackTimer = null;
export function snack(message, { action, onAction, timeout = 5000 } = {}) {
  dismissSnack();
  snackEl = el(html`<div class="snackbar" role="status"><span>${message}</span>${action ? html`<button class="btn">${action}</button>` : ''}<button class="icon-btn" aria-label="Dismiss">${icon('close', 's')}</button></div>`);
  const me = snackEl;
  me.querySelector('.icon-btn').onclick = () => dismissSnack(me);
  if (action) me.querySelector('.btn').onclick = () => { dismissSnack(me); onAction?.(); };
  // A modal dialog sits in the top layer above everything else; show the message inside it so it isn't hidden.
  const dlgs = document.querySelectorAll('dialog[open]');
  (dlgs[dlgs.length - 1] || document.body).append(me);
  if (timeout) snackTimer = setTimeout(() => dismissSnack(me), timeout);
  return () => dismissSnack(me);
}
export function dismissSnack(which) {
  if (which && which !== snackEl) { which.remove(); return; }
  clearTimeout(snackTimer);
  snackEl?.remove();
  snackEl = null;
}
export const toastError = (e) => snack(e?.message || String(e));

// ---------------------------------------------------------------- dialogs
/**
 * Opens a modal dialog. actions: [{label, value, kind: 'filled'|'tonal'|'danger'|'', left}]
 * onAction(value, dlg) may return false (or a promise of false) to keep the dialog open.
 */
export function dialog({ title, text, body = '', iconName, actions, wide, onOpen, onAction, dismissable = true }) {
  actions ||= [{ label: 'OK', value: true, kind: 'filled' }];
  return new Promise((resolve) => {
    const d = el(html`<dialog class="dlg ${wide ? 'wide' : ''}">
      ${title || iconName ? html`<div class="dlg-head">${iconName ? icon(iconName, 'l') : ''}${title ? html`<h2>${title}</h2>` : ''}${text ? html`<p>${text}</p>` : ''}</div>` : ''}
      <div class="dlg-body">${body}</div>
      <div class="dlg-actions">${actions.map((a, i) => html`<button type="button" class="btn ${a.kind === 'danger' ? 'danger filled' : a.kind || ''} ${a.left ? 'left' : ''}" data-i="${i}">${a.label}</button>`)}</div>
    </dialog>`);
    document.body.append(d);
    let done = false;
    const close = (v) => {
      if (done) return;
      done = true;
      const sb = d.querySelector('.snackbar');
      if (sb) document.body.append(sb); // keep a "Saved" message visible after the dialog goes
      d.close();
      d.remove();
      resolve(v);
    };
    const run = async (a, btn) => {
      if (btn?.classList.contains('busy')) return;
      if (onAction) {
        btn?.classList.add('busy');
        let ok;
        d.querySelector('.dlg-err')?.remove();
        try { ok = await onAction(a.value, d); } catch (e) {
          ok = false;
          // Errors show inside the dialog, next to what the user is looking at.
          const err = el(html`<p class="form-err dlg-err" role="alert">${icon('error', 's')}<span>${e?.message || String(e)}</span></p>`);
          d.querySelector('.dlg-body').append(err);
          err.scrollIntoView({ block: 'nearest' });
        }
        btn?.classList.remove('busy');
        if (ok === false) return;
      }
      close(a.value);
    };
    d.querySelectorAll('.dlg-actions button').forEach((b) => {
      b.onclick = (e) => { e.preventDefault(); run(actions[+b.dataset.i], b); };
    });
    // Enter in a field (or submitting a form in the body) runs the primary action.
    const primary = () => {
      const i = actions.findIndex((a) => a.submit) >= 0 ? actions.findIndex((a) => a.submit) : actions.length - 1;
      run(actions[i], d.querySelector(`.dlg-actions [data-i="${i}"]`));
    };
    d.addEventListener('submit', (e) => { e.preventDefault(); primary(); });
    d.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.shiftKey && e.target.matches('input:not([type=checkbox]):not([type=file])') && !e.target.form) { e.preventDefault(); primary(); }
    });
    d.addEventListener('cancel', (e) => { e.preventDefault(); if (dismissable) close(undefined); });
    d.addEventListener('click', (e) => { if (e.target === d && dismissable) close(undefined); });
    d.showModal();
    onOpen?.(d, close);
    const first = d.querySelector('[autofocus], .dlg-body input:not([type=checkbox]):not([readonly]), .dlg-body select, .dlg-body textarea');
    if (first) setTimeout(() => first.focus(), 30);
  });
}

export function confirmDialog(title, text, okLabel = 'OK', danger = false) {
  return dialog({
    title, text,
    actions: [{ label: 'Cancel', value: false }, { label: okLabel, value: true, kind: danger ? 'danger' : 'filled' }],
  });
}

// ---------------------------------------------------------------- menus
let openMenu = null;
let menuAnchor = null;
export function closeMenu(restoreFocus = false) {
  const hadFocus = openMenu?.contains(document.activeElement);
  openMenu?.remove();
  openMenu = null;
  if (restoreFocus && hadFocus && menuAnchor?.isConnected) menuAnchor.focus({ preventScroll: true });
  menuAnchor = null;
}
/** items: [{icon, label, onClick, danger}] | 'sep' | {html: Raw} */
export function menu(anchor, items, { align = 'right', className = '' } = {}) {
  closeMenu();
  const m = el(html`<div class="menu ${className}" role="menu"></div>`);
  items.forEach((it) => {
    if (it === 'sep') return m.append(el('<hr>'));
    if (it.html) return m.append(el(String(it.html)));
    const b = el(html`<button role="menuitem">${it.icon ? icon(it.icon) : ''}<span>${it.label}</span></button>`);
    if (it.danger) b.style.color = 'var(--danger)';
    b.onclick = (e) => { e.stopPropagation(); closeMenu(); it.onClick?.(); };
    m.append(b);
  });
  document.body.append(m);
  const r = anchor.getBoundingClientRect();
  const mw = m.offsetWidth, mh = m.offsetHeight;
  let left = align === 'right' ? r.right - mw : r.left;
  let top = r.bottom + 4;
  if (top + mh > innerHeight - 8) top = Math.max(8, r.top - mh - 4);
  left = Math.max(8, Math.min(left, innerWidth - mw - 8));
  m.style.left = left + 'px';
  m.style.top = top + 'px';
  openMenu = m;
  menuAnchor = anchor;
  setTimeout(() => m.querySelector('button')?.focus({ preventScroll: true }), 0);
  m.addEventListener('keydown', (e) => {
    const btns = [...m.querySelectorAll('button')];
    const i = btns.indexOf(document.activeElement);
    if (e.key === 'ArrowDown') { e.preventDefault(); btns[(i + 1) % btns.length]?.focus(); }
    if (e.key === 'ArrowUp') { e.preventDefault(); btns[(i - 1 + btns.length) % btns.length]?.focus(); }
  });
  return m;
}
document.addEventListener('mousedown', (e) => { if (openMenu && !openMenu.contains(e.target)) closeMenu(); });
document.addEventListener('keydown', (e) => { if (e.key === 'Escape' && openMenu) { closeMenu(true); e.stopPropagation(); } }, true);
addEventListener('resize', () => closeMenu());

// ---------------------------------------------------------------- tooltips
let tip = null;
let tipTimer = null;
const noHover = matchMedia('(hover: none)');
document.addEventListener('mouseover', (e) => {
  if (noHover.matches) return; // touch screens: a tap shouldn't leave a tooltip behind
  const t = e.target.closest?.('[data-tip]');
  clearTimeout(tipTimer);
  if (!t) { tip?.classList.remove('show'); return; }
  tipTimer = setTimeout(() => {
    if (!tip) { tip = el('<div class="tooltip" role="tooltip"></div>'); document.body.append(tip); }
    tip.textContent = t.dataset.tip;
    const r = t.getBoundingClientRect();
    tip.classList.add('show');
    const w = tip.offsetWidth;
    tip.style.left = Math.max(4, Math.min(r.left + r.width / 2 - w / 2, innerWidth - w - 4)) + 'px';
    tip.style.top = (r.bottom + 6 + 24 > innerHeight ? r.top - 30 : r.bottom + 6) + 'px';
  }, 450);
});
document.addEventListener('mousedown', () => { clearTimeout(tipTimer); tip?.classList.remove('show'); });

// ---------------------------------------------------------------- misc
// debounce(fn, ms, maxWait): trailing debounce; with maxWait, a continuous stream of calls
// still runs fn at least every maxWait ms instead of postponing it forever.
export function debounce(fn, ms, maxWait = 0) {
  let t;
  let first = 0;
  const run = (...a) => { clearTimeout(t); first = 0; return fn(...a); };
  const d = (...a) => {
    clearTimeout(t);
    const now = Date.now();
    if (!first) first = now;
    const wait = maxWait ? Math.max(0, Math.min(ms, first + maxWait - now)) : ms;
    t = setTimeout(() => run(...a), wait);
  };
  d.flush = run;
  d.cancel = () => { clearTimeout(t); first = 0; };
  return d;
}

export async function copyText(text, label = 'Copied to clipboard') {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    const ta = el('<textarea style="position:fixed;opacity:0"></textarea>');
    ta.value = text;
    document.body.append(ta);
    ta.select();
    document.execCommand('copy');
    ta.remove();
  }
  snack(label, { timeout: 2500 });
}

export function fileIcon(type = '', name = '') {
  if (type.startsWith('image/')) return 'image';
  if (type === 'application/pdf' || name.endsWith('.pdf')) return 'picture_as_pdf';
  if (/zip|compressed|tar|rar|7z/.test(type)) return 'folder_zip';
  if (type === 'message/rfc822') return 'mail';
  return 'description';
}

export const isEmail = (s) => /^[^\s@<>(),;:"]+@[^\s@<>(),;:"]+\.[^\s@<>(),;:"]{2,}$/.test(String(s).trim());

export function field({ label, name, type = 'text', value = '', hint = '', attrs = '', suffix = '', addon = '', required = false }) {
  return html`<div class="fld"><div class="field ${suffix ? 'with-suffix' : ''} ${addon ? 'has-addon' : ''}">
    <input id="f-${name}" name="${name}" type="${type}" value="${value}" placeholder=" " ${raw(attrs)} ${required ? raw('required') : ''}>
    <label for="f-${name}">${label}</label>
    ${addon ? html`<span class="addon">${addon}</span>` : ''}
    ${suffix ? html`<span class="suffix">${suffix}</span>` : ''}
  </div>${hint ? html`<div class="hint">${hint}</div>` : ''}</div>`;
}

export function selectField({ label, name, options, value }) {
  return html`<div class="field filled">
    <select id="f-${name}" name="${name}">${options.map((o) => html`<option value="${o.value}" ${String(o.value) === String(value) ? raw('selected') : ''}>${o.label}</option>`)}</select>
    <label for="f-${name}">${label}</label>${icon('expand_more', 'sel-ic')}
  </div>`;
}

export function formData(form) {
  const o = {};
  new FormData(form).forEach((v, k) => { o[k] = typeof v === 'string' ? v : v; });
  return o;
}

export function passwordToggle(root) {
  root.querySelectorAll('[data-pw-toggle]').forEach((b) => {
    b.onclick = () => {
      const inp = root.querySelector(`#${b.dataset.pwToggle}`);
      const show = inp.type === 'password';
      inp.type = show ? 'text' : 'password';
      b.innerHTML = String(icon(show ? 'visibility_off' : 'visibility'));
      b.dataset.tip = show ? 'Hide password' : 'Show password';
    };
  });
}

/** Resizes an image file to a square JPEG (for avatars). */
export function squareImage(file, size = 256) {
  return new Promise((resolve, reject) => {
    const img = new Image();
    const url = URL.createObjectURL(file);
    img.onload = () => {
      const c = document.createElement('canvas');
      c.width = c.height = size;
      const s = Math.min(img.width, img.height);
      c.getContext('2d').drawImage(img, (img.width - s) / 2, (img.height - s) / 2, s, s, 0, 0, size, size);
      URL.revokeObjectURL(url);
      c.toBlob((b) => (b ? resolve(new File([b], 'avatar.jpg', { type: 'image/jpeg' })) : reject(new Error('Could not read image'))), 'image/jpeg', 0.9);
    };
    img.onerror = () => { URL.revokeObjectURL(url); reject(new Error("That file isn't an image we can read")); };
    img.src = url;
  });
}
