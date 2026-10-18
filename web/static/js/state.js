// Global application state shared by all views.
import { bus, api, people } from './lib.js';

export const S = {
  me: null,          // /api/me payload
  folders: [],       // the user's own folders: [{id, key: 'f:12', name: 'Clients/Acme', leaf, depth, unread, total}]
  counts: {},        // unread counts per folder
  route: {},         // parsed location.hash
  installPrompt: null,
  viewKeys: null,    // keyboard handler of the active view
  online: navigator.onLine,
};

export const FOLDERS = [
  { id: 'inbox', name: 'Inbox', icon: 'inbox' },
  { id: 'starred', name: 'Starred', icon: 'star_border', active: 'star' },
  { id: 'sent', name: 'Sent', icon: 'send' },
  { id: 'drafts', name: 'Drafts', icon: 'insert_drive_file' },
  { id: 'archive', name: 'Archive', icon: 'archive' },
  { id: 'spam', name: 'Spam', icon: 'report' },
  { id: 'trash', name: 'Trash', icon: 'delete' },
  { id: 'all', name: 'All mail', icon: 'all_inbox' },
];
export const isCustom = (id) => /^f:[1-9][0-9]{0,17}$/.test(id || '');
const customFolder = (id) => (S.folders || []).find((f) => f.key === id);
export const folderName = (id) => FOLDERS.find((f) => f.id === id)?.name || (isCustom(id) ? customFolder(id)?.leaf || 'Folder' : 'Mail');
export const folderPath = (id) => (isCustom(id) ? customFolder(id)?.name || 'Folder' : folderName(id));

export async function loadFolders() {
  try {
    S.folders = await api('/user-folders', { background: true });
    bus.emit('folders');
  } catch { /* keep the last list */ }
}

export function navigate(hash, { replace = false } = {}) {
  if (location.hash === hash) { bus.emit('route'); return; }
  if (replace) history.replaceState(null, '', hash), bus.emit('route');
  else location.hash = hash;
}

export function parseRoute() {
  const h = decodeURIComponent(location.hash.replace(/^#\/?/, ''));
  const [path, qs = ''] = h.split('?');
  const params = new URLSearchParams(qs);
  const parts = path.split('/').filter(Boolean);
  const page = Math.max(1, parseInt(params.get('p') || '1', 10) || 1);
  if (!parts.length) return { name: 'list', folder: 'inbox', page, params };
  if (parts[0] === 'search') {
    const q = parts[1] || '';
    if (parts[2] === 't') return { name: 'thread', folder: 'search', q, id: +parts[3], params };
    return { name: 'list', folder: 'search', q, page, params };
  }
  if (parts[0] === 'settings') return { name: 'settings', tab: parts[1] || 'general', params };
  if (parts[0] === 'admin') return { name: 'admin', tab: parts[1] || 'overview', params };
  if (parts[0] === 'people') return { name: 'people', params };
  if (parts[0] === 'compose') return { name: 'compose', params };
  if (parts[0] === 'drafts') return { name: 'drafts', folder: 'drafts', page, params };
  if (FOLDERS.some((f) => f.id === parts[0]) || isCustom(parts[0])) {
    if (parts[1] === 't') return { name: 'thread', folder: parts[0], id: +parts[2], params };
    return { name: 'list', folder: parts[0], page, filter: params.get('f') || '', params };
  }
  return { name: 'list', folder: 'inbox', page, params };
}

// ---------------------------------------------------------------- theme
export function themePref() {
  try { return localStorage.getItem('sm.theme') || 'system'; } catch { return 'system'; }
}
export function setTheme(pref) {
  try { localStorage.setItem('sm.theme', pref); } catch { /* private mode */ }
  applyTheme();
  bus.emit('theme');
}
export function applyTheme() {
  const pref = themePref();
  const dark = pref === 'dark' || (pref === 'system' && matchMedia('(prefers-color-scheme: dark)').matches);
  document.documentElement.dataset.theme = dark ? 'dark' : 'light';
}
matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => themePref() === 'system' && (applyTheme(), bus.emit('theme')));

// ---------------------------------------------------------------- counts & directory
export function setCounts(c) {
  if (!c) return;
  S.counts = c;
  bus.emit('counts', c);
}

let countsInflight = null;
export function refreshCounts() {
  countsInflight ||= api('/folders', { background: true })
    .then(setCounts)
    .catch(() => {})
    .finally(() => { countsInflight = null; });
  return countsInflight;
}

export async function loadDirectory() {
  try {
    const d = await api('/directory', { background: true });
    people.clear();
    d.people.forEach((p) => people.set(p.email.toLowerCase(), p));
    S.directory = d;
    bus.emit('directory');
  } catch { /* not fatal */ }
}

export const pref = {
  get(k, def) { try { const v = localStorage.getItem('sm.' + k); return v == null ? def : JSON.parse(v); } catch { return def; } },
  set(k, v) { try { localStorage.setItem('sm.' + k, JSON.stringify(v)); } catch { /* ignore */ } },
};
