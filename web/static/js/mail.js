// Mail views: conversation list, conversation reader and drafts.
import { $, $$, html, raw, icon, el, setHTML, api, bus, snack, menu, avatar, shortDate, fullDate, ago, bytes, longDate, greeting,
  plural, toastError, confirmDialog, fileIcon, people, esc } from './lib.js';
import { S, folderName, navigate, setCounts, refreshCounts } from './state.js';
import * as compose from './compose.js';

const PAGE = 50;
const cache = new Map();      // list key -> {items,total}
let current = null;           // state of the rendered list or thread
let lastItems = [];           // rows of the last list shown (for older/newer in the reader)

const listKey = (r) => `${r.folder}|${r.q || ''}|${r.page}|${r.filter || ''}`;
const listHash = (r, page = r.page, filter = r.filter) =>
  r.folder === 'search' ? `#/search/${encodeURIComponent(r.q)}${page > 1 ? `?p=${page}` : ''}`
    : `#/${r.folder}${page > 1 || filter ? '?' : ''}${page > 1 ? `p=${page}` : ''}${page > 1 && filter ? '&' : ''}${filter ? `f=${filter}` : ''}`;
const threadHash = (r, tid) => r.folder === 'search' ? `#/search/${encodeURIComponent(r.q)}/t/${tid}` : `#/${r.folder}/t/${tid}`;

const FILTERS = [
  { id: '', label: 'All' },
  { id: 'unread', label: 'Unread', q: 'is:unread', icon: 'mark_email_unread' },
  { id: 'starred', label: 'Starred', q: 'is:starred', icon: 'star_border' },
  { id: 'attach', label: 'Attachments', q: 'has:attachment', icon: 'attach_file' },
];
const REAL = ['inbox', 'sent', 'archive', 'spam', 'trash'];

function apiQuery(r) {
  if (r.folder === 'search') return `q=${encodeURIComponent(r.q)}`;
  const f = FILTERS.find((x) => x.id === r.filter && x.id);
  if (f && REAL.includes(r.folder)) return `q=${encodeURIComponent(`in:${r.folder} ${f.q}`)}`;
  if (f && r.folder === 'all') return `q=${encodeURIComponent(f.q)}`;
  if (f && r.folder === 'starred') return `q=${encodeURIComponent(`is:starred ${f.q}`)}`;
  return `folder=${r.folder}`;
}

// Which bulk actions apply in each view: [action, icon, label]
function actionsFor(folder) {
  const read = [['read', 'mark_email_read', 'Mark as read'], ['unread', 'mark_email_unread', 'Mark as unread']];
  switch (folder) {
    case 'inbox': return [['archive', 'archive', 'Archive'], ['spam', 'report', 'Report spam'], ['trash', 'delete', 'Delete'], ...read];
    case 'archive': return [['inbox', 'move_to_inbox', 'Move to Inbox'], ['spam', 'report', 'Report spam'], ['trash', 'delete', 'Delete'], ...read];
    case 'sent': return [['trash', 'delete', 'Delete'], ...read];
    case 'spam': return [['inbox', 'report_off', 'Not spam'], ['delete', 'delete_forever', 'Delete forever'], ...read];
    case 'trash': return [['inbox', 'restore_from_trash', 'Restore'], ['delete', 'delete_forever', 'Delete forever'], ...read];
    default: return [['archive', 'archive', 'Archive'], ['trash', 'delete', 'Delete'], ...read];
  }
}
const viewOf = (r) => (r.folder === 'search' ? 'search' : r.folder);
const MOVE_MSG = { archive: 'Archived', trash: 'Moved to Trash', spam: 'Reported as spam', inbox: 'Moved to Inbox', delete: 'Deleted forever' };
const UNDO = { archive: ['archive', 'inbox'], trash: ['trash', 'inbox'], spam: ['spam', 'inbox'] };

async function act(threads, view, action, { undo = true, quiet = false, ids } = {}) {
  const res = await api('/threads/action', { method: 'POST', body: { threads, view, action, ...(ids ? { ids } : {}) } });
  setCounts(res.counts);
  cache.clear();
  if (quiet || !MOVE_MSG[action]) return;
  const n = threads.length;
  const label = `${n > 1 ? plural(n, 'conversation') : 'Conversation'} ${MOVE_MSG[action].toLowerCase()}`;
  const u = undo && UNDO[action];
  snack(label, u ? {
    action: 'Undo',
    onAction: async () => {
      // Move back only the messages this action moved, not older ones of the same conversation.
      try { await act(threads, u[0], u[1], { quiet: true, ids: res.ids }); snack('Action undone'); onRemoteChange(true); } catch (e) { toastError(e); }
    },
  } : {});
}

// ---------------------------------------------------------------- list
export async function renderList(viewEl, r, prev) {
  const key = listKey(r);
  const sameList = current?.kind === 'list' && current.key === key;
  const state = sameList ? current : { kind: 'list', key, r, selected: new Set(), focus: -1, items: [], total: 0 };
  state.r = r;
  current = state;
  S.lastList = listHash(r);
  const cached = cache.get(key);
  paintListFrame(viewEl, state);
  if (cached) { state.items = cached.items; state.total = cached.total; state.loaded = true; lastItems = cached.items; paintRows(state); }
  else paintSkeleton();
  await loadList(state);
  // Coming back from a conversation: focus the row we came from
  if (prev?.name === 'thread' && prev.id) {
    const i = state.items.findIndex((t) => t.thread_id === prev.id);
    if (i >= 0) setFocus(state, i, true);
  }
}

async function loadList(state) {
  // one request per list at a time: changes during a load trigger exactly one more load
  if (state.loading) { state.again = true; return state.loading; }
  state.loading = loadListOnce(state).finally(() => {
    state.loading = null;
    if (state.again && current === state) { state.again = false; loadList(state); }
  });
  return state.loading;
}

async function loadListOnce(state) {
  const r = state.r;
  const q = `${apiQuery(r)}&offset=${(r.page - 1) * PAGE}&limit=${PAGE}`;
  const userRefresh = state.userRefresh;
  state.userRefresh = false;
  try {
    const res = await api(`/threads?${q}`, { background: state.items.length > 0 });
    if (current !== state) return;
    state.items = res.items;
    state.total = res.total;
    state.loaded = true;
    lastItems = res.items;
    cache.set(state.key, { items: res.items, total: res.total });
    setCounts(res.counts);
    // keep selection only for rows still visible
    const ids = new Set(res.items.map((t) => t.thread_id));
    state.selected.forEach((id) => { if (!ids.has(id)) state.selected.delete(id); });
    paintRows(state);
  } catch (e) {
    if (current === state) {
      const list = $('#list');
      if (list && !state.items.length) {
        setHTML(list, html`<div class="empty">${icon('cloud_off')}<h3>Couldn't load your mail</h3><p>${e.message}</p><button class="btn tonal" id="retry">Try again</button></div>`);
        $('#retry')?.addEventListener('click', () => loadList(state));
      } else if (userRefresh) toastError(e);
    }
  }
}

function paintListFrame(viewEl, state) {
  const r = state.r;
  const me = S.me.user;
  const isSearch = r.folder === 'search';
  const title = isSearch ? 'Search results' : folderName(r.folder);
  const unread = S.counts?.[r.folder] || 0;
  const showFilters = !isSearch;
  setHTML(viewEl, html`
    <div class="page-head">
      <div class="grow">
        ${r.folder === 'inbox' ? html`<p class="greet">${greeting()}, ${me.name.split(' ')[0]}</p>` : ''}
        <h1>${title}</h1>
        ${isSearch ? html`<p class="sub" dir="auto">for “${r.q}”</p>` : r.folder === 'inbox' ? html`<p class="sub">${longDate(Date.now())}</p>` : ''}
        <div class="chips" id="head-chips" style="margin-top:10px">
          ${!isSearch && (r.folder === 'inbox' || r.folder === 'spam') ? html`<span class="pill blue" id="chip-unread">${unread} unread</span>` : ''}
          <span class="pill gray" id="chip-total"></span>
        </div>
      </div>
      ${showFilters ? html`<div class="chips" role="tablist" aria-label="Filters">${FILTERS.map((f) => html`
        <button class="chip ${r.filter === f.id ? 'on' : ''}" data-filter="${f.id}" role="tab" aria-selected="${r.filter === f.id}">${f.icon && r.filter !== f.id ? icon(f.icon) : r.filter === f.id ? icon('check') : ''}${f.label}</button>`)}</div>` : ''}
    </div>
    ${r.folder === 'trash' || r.folder === 'spam' ? html`<div style="padding:0 16px"><div class="banner" style="margin:4px 0 8px">${icon('info', 's')}<span class="grow">Messages in ${folderName(r.folder)} are deleted forever after 30 days.</span><button class="btn" id="empty-folder">Empty ${folderName(r.folder)} now</button></div></div>` : ''}
    <div class="toolbar" id="toolbar"></div>
    <div class="list" id="list" role="list"></div>`);
  viewEl.querySelectorAll('[data-filter]').forEach((b) => (b.onclick = () => navigate(listHash(r, 1, b.dataset.filter))));
  $('#empty-folder')?.addEventListener('click', () => emptyFolder(r.folder));
  wireList(state);
  paintToolbar(state);
  S.viewKeys = (e) => listKeys(state, e);
}

function paintSkeleton() {
  const list = $('#list');
  if (!list) return;
  const rows = Array.from({ length: 12 }, (_, i) => html`<div class="row" style="cursor:default"><span class="cb-wrap"></span><span class="cb-wrap"></span>
    <span class="who"><span class="skel" style="width:${90 + (i * 37) % 60}px"></span></span>
    <span class="mid"><span class="skel" style="width:${180 + (i * 53) % 200}px"></span></span><span class="date"><span class="skel" style="width:40px"></span></span></div>`);
  setHTML(list, html`<div class="skeleton">${rows}</div>`);
}

function paintToolbar(state) {
  const tb = $('#toolbar');
  if (!tb) return;
  const r = state.r;
  const n = state.selected.size;
  const all = state.items.length > 0 && n === state.items.length;
  const from = state.total ? (r.page - 1) * PAGE + 1 : 0;
  const to = Math.min(r.page * PAGE, state.total);
  const acts = actionsFor(viewOf(r));
  setHTML(tb, html`
    <span class="cb-wrap" data-tip="Select"><input type="checkbox" class="cb" id="sel-all" aria-label="Select all" ${all ? raw('checked') : ''}></span>
    ${n ? acts.map(([a, ic, label]) => html`<button class="icon-btn" data-bulk="${a}" data-tip="${label}" aria-label="${label}">${icon(ic)}</button>`)
      : html`<button class="icon-btn" id="refresh" data-tip="Refresh" aria-label="Refresh">${icon('refresh')}</button>
             <button class="icon-btn" id="list-more" data-tip="More" aria-label="More">${icon('more_vert')}</button>`}
    ${n ? html`<span class="range">${n} selected</span>` : ''}
    <span class="spacer"></span>
    <span class="range">${state.total ? `${from}–${to} of ${state.total.toLocaleString()}` : ''}</span>
    <button class="icon-btn" id="pg-prev" data-tip="Newer" aria-label="Newer" ${r.page <= 1 ? raw('disabled') : ''}>${icon('chevron_left')}</button>
    <button class="icon-btn" id="pg-next" data-tip="Older" aria-label="Older" ${to >= state.total ? raw('disabled') : ''}>${icon('chevron_right')}</button>`);
  const cb = $('#sel-all');
  cb.indeterminate = n > 0 && !all;
  cb.onchange = () => {
    if (cb.checked) state.items.forEach((t) => state.selected.add(t.thread_id));
    else state.selected.clear();
    paintSelection(state);
  };
  $('#refresh')?.addEventListener('click', () => { cache.delete(state.key); state.userRefresh = true; loadList(state); refreshCounts(); });
  $('#list-more')?.addEventListener('click', (e) => menu(e.currentTarget, [
    { icon: 'mark_email_read', label: 'Mark all on this page as read', onClick: () => bulk(state, 'read', state.items.map((t) => t.thread_id)) },
  ], { align: 'left' }));
  $('#pg-prev').onclick = () => navigate(listHash(r, r.page - 1));
  $('#pg-next').onclick = () => navigate(listHash(r, r.page + 1));
  tb.querySelectorAll('[data-bulk]').forEach((b) => (b.onclick = () => bulk(state, b.dataset.bulk)));
  const chip = $('#chip-total');
  if (chip) {
    chip.textContent = !state.loaded ? '' : state.total ? plural(state.total, 'conversation') : 'No conversations';
    chip.hidden = !state.loaded;
  }
  const cu = $('#chip-unread');
  if (cu) cu.textContent = `${S.counts?.[r.folder] || 0} unread`;
}

function rowHTML(t, state, i) {
  const r = state.r;
  const sel = state.selected.has(t.thread_id);
  const sent = r.folder === 'sent';
  let who;
  if (sent) who = 'To: ' + (t.to || []).map((a) => a.name || people.get(a.email)?.name || a.email.split('@')[0]).join(', ');
  else who = (t.participants || []).slice(0, 3).join(', ') + ((t.participants || []).length > 3 ? ' …' : '');
  const showFolder = (r.folder === 'search' || r.folder === 'all' || r.folder === 'starred') && t.folder !== 'inbox';
  const acts = actionsFor(viewOf(r)).slice(0, 3);
  const readAct = t.unread ? ['read', 'mark_email_read', 'Mark as read'] : ['unread', 'mark_email_unread', 'Mark as unread'];
  const firstPerson = sent ? (t.to?.[0] || {}) : { name: t.participants?.[0], email: t.from_email };
  const tabStop = i === Math.max(0, state.focus);
  const label = `${t.unread ? 'Unread, ' : ''}${who}, ${t.subject || 'no subject'}, ${shortDate(t.date)}`;
  return html`<div class="row ${t.unread ? 'unread' : ''} ${sel ? 'selected' : ''} ${state.focus === i ? 'focus' : ''}" role="listitem" tabindex="${tabStop ? 0 : -1}" aria-label="${label}" data-i="${i}" data-tid="${t.thread_id}">
    <span class="cb-wrap" data-tip="Select"><input type="checkbox" class="cb" tabindex="-1" aria-label="Select" ${sel ? raw('checked') : ''}></span>
    <button class="icon-btn sm star-btn ${t.flagged ? 'on' : ''}" tabindex="-1" data-tip="${t.flagged ? 'Starred' : 'Not starred'}" aria-label="Star">${icon(t.flagged ? 'star-f' : 'star_border')}</button>
    <span class="av-cell" data-tip="Select">${avatar(firstPerson.name, firstPerson.email)}<span class="av-check">${icon('check')}</span></span>
    <span class="who" dir="auto">${who || '(no recipients)'}${t.count > 1 ? html`<span class="n">${t.count}</span>` : ''}</span>
    <span class="mid">
      ${showFolder ? html`<span class="pill gray folder-tag">${folderName(t.folder)}</span>` : ''}
      <span class="subj" dir="auto">${t.subject || '(no subject)'}</span>
      <span class="snip" dir="auto">${t.snippet ? html`<span class="dash"> &nbsp;–&nbsp; </span>${t.snippet}` : ''}</span>
    </span>
    ${t.has_attach ? html`<span class="att">${icon('attach_file', 's')}</span>` : ''}
    <span class="date" title="${fullDate(t.date)}">${shortDate(t.date)}</span>
    <span class="hover-acts">
      ${acts.filter(([a]) => a !== 'read' && a !== 'unread').map(([a, ic, label]) => html`<button class="icon-btn sm" tabindex="-1" data-row-act="${a}" data-tip="${label}" aria-label="${label}">${icon(ic)}</button>`)}
      <button class="icon-btn sm" tabindex="-1" data-row-act="${readAct[0]}" data-tip="${readAct[2]}" aria-label="${readAct[2]}">${icon(readAct[1])}</button>
    </span>
  </div>`;
}

function paintRows(state) {
  const list = $('#list');
  if (!list || current !== state) return;
  if (!state.items.length) {
    const r = state.r;
    const empty = {
      inbox: ['inbox', 'Your inbox is empty', 'New mail will show up here as soon as it arrives.'],
      starred: ['star_border', 'No starred messages', 'Star important conversations to find them here quickly.'],
      sent: ['send', 'Nothing sent yet', 'Messages you send will appear here.'],
      archive: ['archive', 'Archive is empty', 'Archive conversations to clear your inbox without deleting them.'],
      spam: ['report', 'Hooray, no spam here!', ''],
      trash: ['delete', 'Trash is empty', 'Deleted conversations stay here for 30 days.'],
      all: ['all_inbox', 'No mail yet', ''],
      search: ['search', 'No messages matched your search', 'Try different words, or use search options like from: or has:attachment.'],
    }[r.folder] || ['inbox', 'Nothing here', ''];
    setHTML(list, html`<div class="empty">${icon(empty[0])}<h3>${r.filter ? 'Nothing matches this filter' : empty[1]}</h3><p>${r.filter ? '' : empty[2]}</p></div>`);
  } else {
    setHTML(list, state.items.map((t, i) => rowHTML(t, state, i)));
  }
  paintToolbar(state);
}

function paintSelection(state) {
  $$('#list .row').forEach((row) => {
    const sel = state.selected.has(+row.dataset.tid);
    row.classList.toggle('selected', sel);
    const cb = row.querySelector('.cb');
    if (cb) cb.checked = sel;
  });
  paintToolbar(state);
}

function wireList(state) {
  const list = $('#list');
  list.addEventListener('click', async (e) => {
    const row = e.target.closest('.row[data-tid]');
    if (!row) return;
    const i = +row.dataset.i;
    const t = state.items[i];
    if (!t) return;
    if (e.target.closest('.cb-wrap') || e.target.closest('.av-cell')) { // avatar = select on phones
      e.preventDefault();
      if (e.shiftKey && state.lastClick != null) {
        const [a, b] = [Math.min(state.lastClick, i), Math.max(state.lastClick, i)];
        for (let k = a; k <= b; k++) state.selected.add(state.items[k].thread_id);
      } else if (state.selected.has(t.thread_id)) state.selected.delete(t.thread_id);
      else state.selected.add(t.thread_id);
      state.lastClick = i;
      return paintSelection(state);
    }
    if (e.target.closest('.star-btn')) {
      e.stopPropagation();
      return toggleStar(state, t);
    }
    const ra = e.target.closest('[data-row-act]');
    if (ra) {
      e.stopPropagation();
      return bulk(state, ra.dataset.rowAct, [t.thread_id]);
    }
    if (e.ctrlKey || e.metaKey) { state.selected.add(t.thread_id); return paintSelection(state); }
    navigate(threadHash(state.r, t.thread_id));
  });
  // Keyboard focus on a row makes it the current one (roving tabindex: one Tab stop for the list).
  list.addEventListener('focusin', (e) => {
    const row = e.target.closest('.row[data-tid]');
    if (row && +row.dataset.i !== state.focus) setFocus(state, +row.dataset.i, false, false);
  });
  list.addEventListener('keydown', (e) => {
    if (!e.target.matches('.row[data-tid]')) return;
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') { e.preventDefault(); setFocus(state, state.focus + (e.key === 'ArrowDown' ? 1 : -1), true, true); }
    else if (e.key === ' ') { e.preventDefault(); listKeys(state, { key: 'x' }); }
  });
}

async function toggleStar(state, t) {
  const on = !t.flagged;
  t.flagged = on;
  paintRows(state);
  try {
    await act([t.thread_id], viewOf(state.r), on ? 'star' : 'unstar', { undo: false });
    if (state.r.folder === 'starred' && !on) { state.items = state.items.filter((x) => x !== t); state.total--; paintRows(state); }
  } catch (e) {
    t.flagged = !on;
    paintRows(state);
    toastError(e);
  }
}

async function bulk(state, action, ids = [...state.selected]) {
  if (!ids.length) return;
  if (action === 'delete' && !(await confirmDialog('Delete forever?', `${plural(ids.length, 'conversation')} will be permanently deleted. This can't be undone.`, 'Delete forever', true))) return;
  const removes = ['archive', 'trash', 'spam', 'inbox', 'delete'].includes(action);
  const before = state.items;
  if (removes) {
    state.items = state.items.filter((t) => !ids.includes(t.thread_id));
    state.total = Math.max(0, state.total - (before.length - state.items.length));
  } else {
    state.items.forEach((t) => { if (ids.includes(t.thread_id)) t.unread = action === 'unread'; });
  }
  ids.forEach((id) => state.selected.delete(id));
  paintRows(state);
  try {
    await act(ids, viewOf(state.r), action);
    if (removes && state.items.length < 10 && state.total > state.items.length) loadList(state);
  } catch (e) {
    state.items = before;
    paintRows(state);
    toastError(e);
  }
}

async function emptyFolder(folder) {
  if (!(await confirmDialog(`Empty ${folderName(folder)}?`, `Everything in ${folderName(folder)} will be permanently deleted. This can't be undone.`, 'Empty now', true))) return;
  try {
    for (;;) {
      const res = await api(`/threads?folder=${folder}&limit=200`);
      if (!res.items.length) break;
      await act(res.items.map((t) => t.thread_id), folder, 'delete', { quiet: true });
      if (res.items.length < 200) break;
    }
    snack(`${folderName(folder)} emptied`);
    onRemoteChange(true);
  } catch (e) {
    toastError(e);
  }
}

function setFocus(state, i, scroll, moveDomFocus) {
  state.focus = Math.max(0, Math.min(i, state.items.length - 1));
  let target = null;
  $$('#list .row[data-tid]').forEach((r) => {
    const on = +r.dataset.i === state.focus;
    r.classList.toggle('focus', on);
    r.tabIndex = on ? 0 : -1;
    if (on) target = r;
  });
  if (!target) return;
  if (scroll) target.scrollIntoView({ block: 'nearest' });
  // Follow with keyboard focus when the user is already navigating the list by keyboard.
  const inList = document.activeElement?.closest?.('#list') || document.activeElement === document.body;
  if (moveDomFocus ?? inList) target.focus({ preventScroll: true });
}

function listKeys(state, e) {
  const t = state.items[state.focus];
  const k = e.key;
  if (k === 'j') { setFocus(state, state.focus + 1, true); return true; }
  if (k === 'k') { setFocus(state, state.focus - 1, true); return true; }
  if ((k === 'o' || (k === 'Enter' && (!e.target || e.target === document.body || e.target.matches?.('.row')))) && t) { navigate(threadHash(state.r, t.thread_id)); return true; }
  if (k === 'x' && t) { state.selected.has(t.thread_id) ? state.selected.delete(t.thread_id) : state.selected.add(t.thread_id); paintSelection(state); return true; }
  if (k === 's' && t) { toggleStar(state, t); return true; }
  const ids = state.selected.size ? [...state.selected] : t ? [t.thread_id] : [];
  const avail = actionsFor(viewOf(state.r)).map((a) => a[0]);
  const map = { e: 'archive', '#': avail.includes('trash') ? 'trash' : 'delete', '!': 'spam', I: 'read', U: 'unread' };
  const a = map[k];
  if (a && avail.includes(a) && ids.length) { bulk(state, a, ids); return true; }
  return false;
}

// Re-render an open conversation when the theme changes (message colours depend on it).
bus.on('theme', () => { if (current?.kind === 'thread') paintThread(current); });

// ---------------------------------------------------------------- remote changes
let refreshTimer = null;
export function onRemoteChange(force) {
  cache.clear();
  clearTimeout(refreshTimer);
  refreshTimer = setTimeout(() => {
    if (!current) return;
    if (current.kind === 'list') loadList(current);
    else if (current.kind === 'thread') reloadThread(current, force);
    else if (current.kind === 'drafts') loadDrafts(current);
  }, force ? 0 : 150);
  return current?.kind === 'list' || current?.kind === 'thread'; // these reloads return counts
}

// ---------------------------------------------------------------- thread
export async function renderThread(viewEl, r) {
  const state = { kind: 'thread', r, id: r.id, messages: [], open: new Set(), showImages: new Set(), details: new Set() };
  current = state;
  setHTML(viewEl, html`<div class="toolbar" id="toolbar"></div><div class="thread" id="thread"><div class="center-fill" style="padding:80px"><div class="spinner"></div></div></div>`);
  paintThreadToolbar(state);
  S.viewKeys = (e) => threadKeys(state, e);
  await loadThread(state);
}

function contextFolder(r) { return r.folder === 'search' || r.folder === 'starred' || r.folder === 'all' ? 'all' : r.folder; }

async function loadThread(state, quiet) {
  try {
    const res = await api(`/threads/${state.id}?context=${contextFolder(state.r)}`, { background: quiet });
    if (current !== state) return;
    setCounts(res.counts);
    const fresh = state.messages.length === 0;
    state.messages = res.messages;
    if (fresh) {
      res.messages.forEach((m, i) => { if (!m.seen || i === res.messages.length - 1) state.open.add(m.id); });
    }
    // reflect "read" in the cached list
    cache.forEach((v) => v.items.forEach((t) => { if (t.thread_id === state.id) t.unread = false; }));
    lastItems.forEach((t) => { if (t.thread_id === state.id) t.unread = false; });
    paintThread(state);
  } catch (e) {
    if (current !== state) return;
    if (e.status === 404) { snack('That conversation no longer exists'); navigate(S.lastList || '#/inbox', { replace: true }); return; }
    setHTML($('#thread'), html`<div class="empty">${icon('error')}<h3>Couldn't open this conversation</h3><p>${e.message}</p></div>`);
  }
}

function reloadThread(state, force) {
  const before = state.messages.length;
  api(`/threads/${state.id}?context=${contextFolder(state.r)}&peek=1`, { background: true }).then((res) => {
    if (current !== state) return;
    setCounts(res.counts);
    if (force || res.messages.length !== before) {
      const known = new Set(state.messages.map((m) => m.id));
      res.messages.forEach((m) => { if (!known.has(m.id)) state.open.add(m.id); });
      state.messages = res.messages;
      const scroller = $('#thread');
      const top = scroller?.scrollTop;
      paintThread(state);
      if (scroller) scroller.scrollTop = top;
      if (!force && res.messages.length > before) snack('New message in this conversation');
    }
  }).catch((e) => { if (e.status === 404 && current === state) navigate(S.lastList || '#/inbox', { replace: true }); });
}

function threadFolder(state) {
  // the "real" folder of this conversation as seen from the current view
  const r = state.r;
  if (REAL.includes(r.folder)) return r.folder;
  const fs = new Set(state.messages.map((m) => m.folder));
  if (fs.has('inbox')) return 'inbox';
  if (fs.has('archive')) return 'archive';
  return [...fs][0] || 'all';
}

function neighbours(state) {
  const items = lastItems;
  const i = items.findIndex((t) => t.thread_id === state.id);
  return { i, items, newer: i > 0 ? items[i - 1] : null, older: i >= 0 && i < items.length - 1 ? items[i + 1] : null };
}

function paintThreadToolbar(state) {
  const tb = $('#toolbar');
  if (!tb) return;
  const folder = threadFolder(state);
  const acts = actionsFor(folder).filter(([a]) => a !== 'read');
  const nb = neighbours(state);
  setHTML(tb, html`
    <button class="icon-btn" id="back" data-tip="Back to ${state.r.folder === 'search' ? 'search results' : folderName(state.r.folder)}" aria-label="Back">${icon('arrow_back')}</button>
    ${acts.map(([a, ic, label]) => html`<button class="icon-btn" data-tact="${a}" data-tip="${label}" aria-label="${label}">${icon(ic)}</button>`)}
    <button class="icon-btn" id="t-more" data-tip="More" aria-label="More">${icon('more_vert')}</button>
    <span class="spacer"></span>
    ${nb.i >= 0 ? html`<span class="range">${nb.i + 1} of ${nb.items.length}</span>` : ''}
    <button class="icon-btn" id="t-newer" data-tip="Newer" aria-label="Newer" ${nb.newer ? '' : raw('disabled')}>${icon('chevron_left')}</button>
    <button class="icon-btn" id="t-older" data-tip="Older" aria-label="Older" ${nb.older ? '' : raw('disabled')}>${icon('chevron_right')}</button>`);
  $('#back').onclick = () => navigate(S.lastList || '#/inbox');
  tb.querySelectorAll('[data-tact]').forEach((b) => (b.onclick = () => threadAction(state, b.dataset.tact)));
  $('#t-newer').onclick = () => nb.newer && navigate(threadHash(state.r, nb.newer.thread_id));
  $('#t-older').onclick = () => nb.older && navigate(threadHash(state.r, nb.older.thread_id));
  $('#t-more').onclick = (e) => {
    const last = state.messages[state.messages.length - 1];
    menu(e.currentTarget, [
      { icon: 'reply', label: 'Reply', onClick: () => replyTo(state, last, 'reply') },
      { icon: 'forward', label: 'Forward', onClick: () => replyTo(state, last, 'forward') },
      'sep',
      { icon: 'download', label: 'Download message (.eml)', onClick: () => last && window.open(`/api/messages/${last.id}/raw`, '_blank') },
    ]);
  };
}

async function threadAction(state, action) {
  if (action === 'delete' && !(await confirmDialog('Delete forever?', "This conversation will be permanently deleted. This can't be undone.", 'Delete forever', true))) return;
  if (action === 'unread') {
    try { await act([state.id], 'all', 'unread', { undo: false }); } catch (e) { return toastError(e); }
    return navigate(S.lastList || '#/inbox');
  }
  const nb = neighbours(state);
  try {
    await act([state.id], threadFolder(state), action);
  } catch (e) {
    return toastError(e);
  }
  // Like Gmail: go to the next older conversation, or back to the list
  if (nb.older) navigate(threadHash(state.r, nb.older.thread_id), { replace: true });
  else navigate(S.lastList || '#/inbox');
}

function namesLine(m) {
  const me = S.me.user.email;
  const nm = (a) => (a.email === me ? 'me' : a.name || a.email);
  const to = (m.to || []).map(nm);
  const cc = (m.cc || []).map(nm);
  const bcc = (m.bcc || []).map(nm);
  let s = 'to ' + (to.join(', ') || 'undisclosed recipients');
  if (cc.length) s += `, cc: ${cc.join(', ')}`;
  if (bcc.length) s += `, bcc: ${bcc.join(', ')}`;
  return s;
}

function attachmentCards(m) {
  const files = m.attachments.filter((a) => !a.inline);
  if (!files.length) return '';
  return html`<div class="atts">${files.map((a) => {
    const url = `/api/messages/${m.id}/attachments/${a.index}`;
    const isImg = /^image\/(png|jpe?g|gif|webp|bmp|avif)$/.test(a.type);
    return html`<a class="att-card" href="${url}" target="_blank" rel="noopener" title="${a.filename}">
      <div class="thumb">${isImg ? html`<img src="${url}" alt="" loading="lazy">` : icon(fileIcon(a.type, a.filename))}</div>
      <div class="meta"><div><div class="name">${a.filename}</div><div class="size">${bytes(a.size)}</div></div>
        <button class="icon-btn sm" data-download="${url}?download=1" data-tip="Download" aria-label="Download">${icon('download')}</button></div>
    </a>`;
  })}</div>`;
}

function spamBanner(m) {
  if (m.folder !== 'spam') return '';
  const reasons = (m.auth_summary.split(';')[1] || '').trim();
  return html`<div class="banner danger">${icon('report', 's')}<span class="grow"><b>Why is this message in spam?</b> ${reasons ? html`It was flagged because: ${reasons}.` : 'It was marked as spam.'} Be careful with links and attachments.</span></div>`;
}

function authBanner(m) {
  // Warn when an external message claims to be signed but failed verification
  const s = m.auth_summary || '';
  if (m.folder === 'spam' || !s || s === 'internal') return '';
  if (/dmarc=fail/.test(s)) return html`<div class="banner warn">${icon('warning', 's')}<span class="grow">This message failed sender verification. It may not be from ${m.from.email}.</span></div>`;
  return '';
}

function textBody(text) {
  // escape, linkify, dim quoted lines, fold trailing quote
  const lines = text.replace(/\r\n/g, '\n').split('\n');
  let cut = lines.length;
  for (let i = 0; i < lines.length; i++) {
    const l = lines[i].trim();
    if ((/^On .+wrote:$/.test(l) || l === '-----Original Message-----') && i > 0) { cut = i; break; }
  }
  const fmt = (ls) => ls.map((l) => {
    const e = esc(l).replace(/\bhttps?:\/\/[^\s<>"']+[^\s<>"'.,;:!?)\]]/g, (u) => `<a href="${u}" target="_blank" rel="noopener noreferrer">${u}</a>`);
    return /^\s*&gt;/.test(e) ? `<span class="q">${e}</span>` : e;
  }).join('\n');
  const main = fmt(lines.slice(0, cut)).replace(/\n+$/, '');
  const quoted = cut < lines.length ? fmt(lines.slice(cut)) : '';
  return html`<pre class="mail-text" dir="auto">${raw(main)}</pre>${quoted ? html`<button class="icon-btn sm" data-showquote data-tip="Show trimmed content" aria-label="Show trimmed content">${icon('more_vert')}</button><pre class="mail-text" dir="auto" hidden>${raw(quoted)}</pre>` : ''}`;
}

function messageHTML(state, m, i) {
  const open = state.open.has(m.id);
  const me = S.me.user.email;
  const fromMe = m.from.email === me;
  const remote = m.has_remote_images && !state.showImages.has(m.id) && !people.has(m.from.email);
  return html`<article class="msg ${open ? 'open' : ''}" data-mid="${m.id}" data-i="${i}">
    <div class="msg-head" data-toggle>
      ${avatar(m.from.name, m.from.email)}
      <div class="meta">
        <div class="from"><b>${fromMe ? 'me' : m.from.name || m.from.email}</b>${open && m.from.name ? html`<span class="addr">&lt;${m.from.email}&gt;</span>` : ''}</div>
        ${open ? html`<div class="to">${namesLine(m)}<button class="icon-btn" data-details data-tip="Show details" aria-label="Show details">${icon(state.details.has(m.id) ? 'expand_less' : 'expand_more')}</button></div>` : html`<div class="snippet">${m.snippet}</div>`}
      </div>
      <span class="when" title="${fullDate(m.date)}">${open ? html`<span class="desktop-only">${fullDate(m.date)} (${ago(m.date)})</span><span class="mobile-only">${shortDate(m.date)}</span>` : shortDate(m.date)}</span>
      <span class="acts">
        <button class="icon-btn sm star-btn ${m.flagged ? 'on' : ''}" data-star data-tip="${m.flagged ? 'Starred' : 'Not starred'}" aria-label="Star" style="${m.flagged ? 'color:var(--star)' : ''}">${icon(m.flagged ? 'star-f' : 'star_border')}</button>
        <button class="icon-btn sm hide-closed" data-reply data-tip="Reply" aria-label="Reply">${icon('reply')}</button>
        <button class="icon-btn sm hide-closed" data-mmore data-tip="More" aria-label="More">${icon('more_vert')}</button>
      </span>
    </div>
    ${open && state.details.has(m.id) ? html`<dl class="msg-details">
      <dt>from:</dt><dd>${m.from.name} &lt;${m.from.email}&gt;</dd>
      ${m.reply_to ? html`<dt>reply-to:</dt><dd>${m.reply_to}</dd>` : ''}
      <dt>to:</dt><dd>${(m.to || []).map((a) => a.name ? `${a.name} <${a.email}>` : a.email).join(', ') || '—'}</dd>
      ${m.cc?.length ? html`<dt>cc:</dt><dd>${m.cc.map((a) => a.name ? `${a.name} <${a.email}>` : a.email).join(', ')}</dd>` : ''}
      ${m.bcc?.length ? html`<dt>bcc:</dt><dd>${m.bcc.map((a) => a.email).join(', ')}</dd>` : ''}
      <dt>date:</dt><dd>${fullDate(m.date)}</dd>
      <dt>subject:</dt><dd>${m.subject || '(no subject)'}</dd>
      <dt>security:</dt><dd>${m.auth_summary === 'internal' || !m.auth_summary ? 'Sent within your organisation' : m.auth_summary.split(';')[0]}</dd>
      <dt>size:</dt><dd>${bytes(m.size)}</dd>
    </dl>` : ''}
    ${open ? html`<div class="msg-body">
      ${spamBanner(m)}${authBanner(m)}
      ${remote ? html`<div class="banner">${icon('image', 's')}<span class="grow">Images are hidden to protect your privacy.</span><button class="btn" data-show-images>Show images</button></div>` : ''}
      ${m.html ? html`<div class="mail-frame-wrap ${isRich(m.html) ? 'html' : 'plain'}"><iframe class="mail-frame" title="Message" sandbox="allow-same-origin allow-popups allow-popups-to-escape-sandbox" data-frame="${m.id}"></iframe></div>` : textBody(m.text || '')}
      ${attachmentCards(m)}
    </div>` : ''}
  </article>`;
}

function paintThread(state) {
  const box = $('#thread');
  if (!box) return;
  const ms = state.messages;
  const subject = ms[ms.length - 1]?.subject || ms[0]?.subject || '(no subject)';
  const folders = [...new Set(ms.map((m) => m.folder))].filter((f) => f !== 'sent' || ms.every((m) => m.folder === 'sent'));
  const others = new Set();
  ms.forEach((m) => [m.from, ...(m.to || []), ...(m.cc || [])].forEach((a) => a?.email && a.email !== S.me.user.email && others.add(a.email)));
  setHTML(box, html`
    <div class="thread-head"><h1 dir="auto">${subject}${folders.map((f) => html`<span class="pill gray">${folderName(f)}</span>`)}</h1></div>
    ${ms.map((m, i) => messageHTML(state, m, i))}
    <div class="reply-bar">
      <button class="btn outlined" data-bottom="reply">${icon('reply')}Reply</button>
      ${others.size > 1 ? html`<button class="btn outlined" data-bottom="replyall">${icon('reply_all')}Reply all</button>` : ''}
      <button class="btn outlined" data-bottom="forward">${icon('forward')}Forward</button>
    </div>`);
  paintThreadToolbar(state);
  ms.forEach((m) => { if (m.html && state.open.has(m.id)) mountFrame(state, m); });
  wireThread(state, box);
  if (!state.scrolled) {
    state.scrolled = true;
    const firstOpen = box.querySelector('.msg.open');
    if (ms.length > 2 && firstOpen) firstOpen.scrollIntoView({ block: 'start' });
  }
}

function wireThread(state, box) {
  box.onclick = (e) => {
    const art = e.target.closest('.msg');
    const m = art && state.messages[+art.dataset.i];
    const bottom = e.target.closest('[data-bottom]');
    if (bottom) return replyTo(state, state.messages[state.messages.length - 1], bottom.dataset.bottom);
    if (!m) return;
    const dl = e.target.closest('[data-download]');
    if (dl) { e.preventDefault(); e.stopPropagation(); const a = el(`<a download href="${esc(dl.dataset.download)}"></a>`); a.click(); return; }
    if (e.target.closest('[data-star]')) {
      e.stopPropagation();
      m.flagged = !m.flagged;
      api(`/messages/${m.id}/flag`, { method: 'POST', body: { flagged: m.flagged } }).then(() => cache.clear()).catch(toastError);
      return paintThread(state);
    }
    if (e.target.closest('[data-reply]')) { e.stopPropagation(); return replyTo(state, m, 'reply'); }
    if (e.target.closest('[data-mmore]')) {
      e.stopPropagation();
      return menu(e.target.closest('[data-mmore]'), [
        { icon: 'reply', label: 'Reply', onClick: () => replyTo(state, m, 'reply') },
        { icon: 'reply_all', label: 'Reply all', onClick: () => replyTo(state, m, 'replyall') },
        { icon: 'forward', label: 'Forward', onClick: () => replyTo(state, m, 'forward') },
        'sep',
        { icon: 'download', label: 'Download message (.eml)', onClick: () => window.open(`/api/messages/${m.id}/raw`, '_blank') },
      ]);
    }
    if (e.target.closest('[data-details]')) {
      e.stopPropagation();
      state.details.has(m.id) ? state.details.delete(m.id) : state.details.add(m.id);
      return paintThread(state);
    }
    if (e.target.closest('[data-show-images]')) { state.showImages.add(m.id); return paintThread(state); }
    const sq = e.target.closest('[data-showquote]');
    if (sq) { sq.nextElementSibling.hidden = false; sq.remove(); return; }
    if (e.target.closest('[data-toggle]') && !e.target.closest('a,button')) {
      if (state.open.has(m.id) && state.messages.length > 1) state.open.delete(m.id);
      else state.open.add(m.id);
      paintThread(state);
    }
  };
}

// Newsletters set their own backgrounds and stay on white "paper" in dark mode;
// simple messages are colour-inverted so they blend into the dark theme.
const isRich = (h) => /bgcolor|background|<table/i.test(h);

const FRAME_CSS = `html{overflow-x:auto}body{margin:0;padding:8px 6px;font:14px/1.5 'Google Sans',Arial,Helvetica,sans-serif;color:#1f1f1f;background:#fff;overflow-wrap:anywhere;word-break:normal}
img{max-width:100%;height:auto}pre{white-space:pre-wrap}a{color:#0b57d0}blockquote{margin:0 0 0 .8ex;border-left:1px solid #ccc;padding-left:1ex}
table{border-collapse:collapse}`;

function mountFrame(state, m) {
  const frame = document.querySelector(`iframe[data-frame="${m.id}"]`);
  if (!frame) return;
  const allowImages = state.showImages.has(m.id) || people.has(m.from.email);
  const csp = `default-src 'none'; img-src ${allowImages ? 'https: http: data:' : 'data:'} ${location.origin}; style-src 'unsafe-inline'; font-src data:; media-src 'none'`;
  const invert = document.documentElement.dataset.theme === 'dark' && !isRich(m.html);
  frame.classList.toggle('inverted', invert);
  const extra = invert ? 'body{background:#f0f0ef}img,video,picture{filter:invert(1) hue-rotate(180deg)}' : '';
  frame.srcdoc = `<!doctype html><html><head><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="${csp}"><base target="_blank"><style>${FRAME_CSS}${extra}</style></head><body${isRich(m.html) ? '' : ' dir="auto"'}>${m.html}</body></html>`;
  frame.onload = () => {
    const doc = frame.contentDocument;
    if (!doc) return;
    let scale = 1;
    const fit = () => {
      const body = doc.body;
      if (!body) return;
      const avail = frame.clientWidth;
      if (scale === 1 && avail > 0 && doc.documentElement.scrollWidth > avail + 4) {
        // Fixed-width newsletters (e.g. 700px tables) on narrow screens: shrink to fit.
        body.style.width = doc.documentElement.scrollWidth + 'px';
        scale = avail / body.offsetWidth;
        body.style.transformOrigin = '0 0';
        body.style.transform = `scale(${scale})`;
        // transform doesn't change layout size: hide the unscaled overflow so the frame has
        // nothing to scroll (the frame itself is sized to the scaled height below).
        doc.documentElement.style.overflow = 'hidden';
      }
      if (scale !== 1) {
        // Pull the layout height in to the scaled height too, so there's no empty space below.
        const mb = Math.floor((scale - 1) * body.offsetHeight);
        if (body.style.marginBottom !== mb + 'px') body.style.marginBottom = mb + 'px';
      }
      const h = scale === 1 ? doc.documentElement.scrollHeight : Math.ceil(body.getBoundingClientRect().height) + 8;
      frame.style.height = Math.max(40, h) + 'px';
    };
    fit();
    try { new ResizeObserver(fit).observe(doc.body); } catch { /* old browsers */ }
    doc.querySelectorAll('img').forEach((img) => img.addEventListener('load', fit));
    doc.addEventListener('click', (e) => {
      const a = e.target.closest?.('a[href^="mailto:"]');
      if (a) { e.preventDefault(); compose.open(compose.parseMailto(a.getAttribute('href'))); }
    });
    doc.addEventListener('keydown', (e) => document.dispatchEvent(new KeyboardEvent('keydown', { key: e.key, shiftKey: e.shiftKey, bubbles: true })));
  };
}

function replyTo(state, m, mode) {
  if (!m) return;
  compose.open({ mode, source: m, thread: state.messages });
}

function threadKeys(state, e) {
  const last = state.messages[state.messages.length - 1];
  const nb = neighbours(state);
  const folderActs = actionsFor(threadFolder(state)).map((a) => a[0]);
  switch (e.key) {
    case 'u': case 'Escape': navigate(S.lastList || '#/inbox'); return true;
    case 'r': replyTo(state, last, 'reply'); return true;
    case 'a': replyTo(state, last, 'replyall'); return true;
    case 'f': replyTo(state, last, 'forward'); return true;
    case 'e': if (folderActs.includes('archive')) threadAction(state, 'archive'); return true;
    case '#': threadAction(state, folderActs.includes('trash') ? 'trash' : 'delete'); return true;
    case '!': if (folderActs.includes('spam')) threadAction(state, 'spam'); return true;
    case 'U': threadAction(state, 'unread'); return true;
    case 'j': if (nb.older) navigate(threadHash(state.r, nb.older.thread_id)); return true;
    case 'k': if (nb.newer) navigate(threadHash(state.r, nb.newer.thread_id)); return true;
    case 's': {
      last.flagged = !last.flagged;
      api(`/messages/${last.id}/flag`, { method: 'POST', body: { flagged: last.flagged } }).then(() => cache.clear()).catch(toastError);
      paintThread(state);
      return true;
    }
  }
  return false;
}

// ---------------------------------------------------------------- drafts
export async function renderDrafts(viewEl, r) {
  const state = { kind: 'drafts', r, items: [] };
  current = state;
  S.lastList = '#/drafts';
  setHTML(viewEl, html`
    <div class="page-head"><div class="grow"><h1>Drafts</h1><p class="sub">Messages you started but haven't sent</p></div></div>
    <div class="toolbar"><button class="icon-btn" id="refresh" data-tip="Refresh" aria-label="Refresh">${icon('refresh')}</button><span class="spacer"></span></div>
    <div class="list" id="list"></div>`);
  $('#refresh').onclick = () => loadDrafts(state);
  paintSkeleton();
  await loadDrafts(state);
}

async function loadDrafts(state) {
  try {
    state.items = await api('/drafts', { background: state.items.length > 0 });
  } catch (e) {
    return toastError(e);
  }
  if (current !== state) return;
  const list = $('#list');
  if (!state.items.length) {
    setHTML(list, html`<div class="empty">${icon('insert_drive_file')}<h3>No drafts</h3><p>Messages you're writing are saved here automatically.</p></div>`);
    return;
  }
  setHTML(list, state.items.map((d, i) => html`<div class="row" data-did="${d.id}" role="listitem" tabindex="${i === 0 ? 0 : -1}" aria-label="Draft${d.to ? ` to ${d.to}` : ''}, ${d.subject || 'no subject'}">
    <span class="cb-wrap"></span>
    <span class="av-cell">${avatar(d.to || 'Draft', '')}</span>
    <span class="who"><span style="color:var(--danger)">Draft</span>${d.to ? html` · ${d.to}` : ''}</span>
    <span class="mid"><span class="subj">${d.subject || '(no subject)'}</span><span class="snip">${d.snippet ? html`<span class="dash"> &nbsp;–&nbsp; </span>${d.snippet}` : ''}</span></span>
    <span class="date">${shortDate(d.updated_at)}</span>
    <span class="hover-acts"><button class="icon-btn sm" data-del data-tip="Discard draft" aria-label="Discard draft">${icon('delete')}</button></span>
  </div>`));
  list.onclick = async (e) => {
    const row = e.target.closest('[data-did]');
    if (!row) return;
    const id = +row.dataset.did;
    if (e.target.closest('[data-del]')) {
      try { await api(`/drafts/${id}`, { method: 'DELETE' }); snack('Draft discarded'); refreshCounts(); loadDrafts(state); } catch (er) { toastError(er); }
      return;
    }
    compose.open({ draftId: id });
  };
}
