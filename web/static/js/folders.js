// Custom folders, "Move to", filters and blocked/allowed senders (shared by the mail views and Settings).
import { html, raw, icon, api, snack, menu, closeMenu, dialog, confirmDialog, toastError, field, selectField, plural, esc } from './lib.js';
import { S, loadFolders, folderPath, isCustom, setCounts } from './state.js';

// ---------------------------------------------------------------- folder dialogs
export async function newFolderDialog(parent = '') {
  let created = null;
  await dialog({
    iconName: 'create_new_folder', title: parent ? `New folder in ${parent}` : 'New folder',
    text: parent ? '' : 'Use "/" to put it inside another folder, like Clients/Acme.',
    body: html`<form id="nf-form">${field({ label: 'Folder name', name: 'fname', attrs: 'maxlength="200" autocomplete="off" spellcheck="false"', required: true })}</form>`,
    actions: [{ label: 'Cancel', value: false }, { label: 'Create', value: true, kind: 'filled', submit: true }],
    onAction: async (v, d) => {
      if (!v) return true;
      const name = d.querySelector('[name=fname]').value.trim();
      if (!name) throw new Error('Enter a name');
      created = await api('/user-folders', { method: 'POST', body: { name: parent ? `${parent}/${name}` : name } });
      return true;
    },
  });
  if (created) { await loadFolders(); snack(`Folder “${created.name}” created`); }
  return created;
}

export async function renameFolderDialog(f) {
  let done = false;
  await dialog({
    iconName: 'edit', title: 'Rename folder', text: 'Use "/" to move it inside another folder. Folders inside it move along.',
    body: html`<form>${field({ label: 'Folder name', name: 'fname', value: f.name, attrs: 'maxlength="200" autocomplete="off" spellcheck="false"', required: true })}</form>`,
    actions: [{ label: 'Cancel', value: false }, { label: 'Rename', value: true, kind: 'filled', submit: true }],
    onAction: async (v, d) => {
      if (!v) return true;
      await api(`/user-folders/${f.id}`, { method: 'PUT', body: { name: d.querySelector('[name=fname]').value } });
      done = true;
      return true;
    },
  });
  if (done) { await loadFolders(); snack('Folder renamed'); }
  return done;
}

export async function deleteFolderDialog(f) {
  await loadFolders(); // current message counts for the warning
  f = (S.folders || []).find((x) => x.id === f.id) || f;
  const subs = (S.folders || []).filter((x) => x.name.toLowerCase().startsWith(f.name.toLowerCase() + '/'));
  const total = [f, ...subs].reduce((n, x) => n + (x.total || 0), 0);
  const what = subs.length ? `“${f.name}” and the ${plural(subs.length, 'folder')} inside it` : `“${f.name}”`;
  if (!(await confirmDialog(`Delete ${f.leaf}?`, `${what} will be deleted.${total ? ` ${plural(total, 'message')} in ${subs.length ? 'them' : 'it'} will move to Archive — nothing is deleted.` : ''}`, 'Delete folder', true))) return false;
  try {
    const res = await api(`/user-folders/${f.id}`, { method: 'DELETE' });
    setCounts(res.counts);
    await loadFolders();
    snack('Folder deleted');
    return true;
  } catch (e) { toastError(e); return false; }
}

export function folderMenu(anchor, f, { onChange } = {}) {
  menu(anchor, [
    { icon: 'create_new_folder', label: 'New folder inside', onClick: () => newFolderDialog(f.name).then((x) => x && onChange?.()) },
    { icon: 'edit', label: 'Rename', onClick: () => renameFolderDialog(f).then((x) => x && onChange?.()) },
    'sep',
    { icon: 'delete', label: 'Delete folder', danger: true, onClick: () => deleteFolderDialog(f).then((x) => x && onChange?.()) },
  ]);
}

// ---------------------------------------------------------------- move to
// Shows Inbox, Archive and the user's folders (minus `exclude`); resolves with the chosen key.
export function moveMenu(anchor, { exclude = '', onPick }) {
  const items = [];
  if (exclude !== 'inbox') items.push({ icon: 'inbox', label: 'Inbox', onClick: () => onPick('inbox') });
  if (exclude !== 'archive') items.push({ icon: 'archive', label: 'Archive', onClick: () => onPick('archive') });
  const custom = (S.folders || []).filter((f) => f.key !== exclude);
  if (custom.length) items.push('sep');
  custom.forEach((f) => items.push({
    html: html`<button role="menuitem" data-key="${f.key}" style="padding-left:${16 + f.depth * 18}px">${icon('folder')}<span>${f.leaf}</span></button>`,
  }));
  items.push('sep', { icon: 'create_new_folder', label: 'New folder…', onClick: async () => { const f = await newFolderDialog(); if (f) onPick(f.key); } });
  const m = menu(anchor, items, { className: 'move-menu' });
  m.querySelectorAll('[data-key]').forEach((b) => (b.onclick = (e) => { e.stopPropagation(); closeMenu(); onPick(b.dataset.key); }));
  return m;
}

export const folderOptions = (withNone = true) => [
  ...(withNone ? [{ value: '', label: 'Leave it where it is' }] : []),
  { value: 'archive', label: 'Archive (skip the inbox)' },
  ...(S.folders || []).map((f) => ({ value: f.key, label: `${'  '.repeat(f.depth)}${f.name}` })),
  { value: 'trash', label: 'Trash' },
];

// ---------------------------------------------------------------- filters
// f: an existing filter to edit, or initial values ({from: 'x@y'}) for a new one.
export async function filterDialog(f = {}) {
  const editing = !!f.id;
  let saved = null;
  const body = html`<form class="filter-form" autocomplete="off">
    <p class="small muted mt0">Messages that match <b>all</b> the boxes you fill in. Separate alternatives with commas, like <i>amazon.in, flipkart.com</i>.</p>
    ${field({ label: 'From', name: 'from', value: f.from || '', attrs: 'spellcheck="false" maxlength="500"' })}
    ${field({ label: 'To (or the group it was sent to)', name: 'to', value: f.to || '', attrs: 'spellcheck="false" maxlength="500"' })}
    ${field({ label: 'Subject contains', name: 'subject', value: f.subject || '', attrs: 'maxlength="500"' })}
    ${field({ label: 'Has the words', name: 'words', value: f.words || '', attrs: 'maxlength="500"' })}
    <h3 class="filter-h">Then</h3>
    ${selectField({ label: 'Move to', name: 'folder', value: f.folder || '', options: folderOptions() })}
    <label class="check"><input type="checkbox" class="cb" name="mark_read" ${f.mark_read ? raw('checked') : ''}><span>Mark as read</span></label>
    <label class="check"><input type="checkbox" class="cb" name="star" ${f.star ? raw('checked') : ''}><span>Star it</span></label>
    <label class="check apply-row"><input type="checkbox" class="cb" name="apply"><span>Also apply to matching messages already in my mailbox</span></label>
  </form>`;
  await dialog({
    iconName: 'filter_alt', title: editing ? 'Edit filter' : 'New filter', body, wide: true,
    actions: [{ label: 'Cancel', value: false }, { label: editing ? 'Save' : 'Create filter', value: true, kind: 'filled', submit: true }],
    onAction: async (v, d) => {
      if (!v) return true;
      const q = (n) => d.querySelector(`[name=${n}]`);
      const payload = {
        enabled: editing ? f.enabled : true,
        from: q('from').value, to: q('to').value, subject: q('subject').value, words: q('words').value,
        folder: q('folder').value, mark_read: q('mark_read').checked, star: q('star').checked, apply: q('apply').checked,
      };
      const res = await api(editing ? `/filters/${f.id}` : '/filters', { method: editing ? 'PUT' : 'POST', body: payload });
      saved = res.filter;
      snack(`${editing ? 'Filter saved' : 'Filter created'}${payload.apply ? ` · ${plural(res.applied, 'message')} updated` : ''}`);
      return true;
    },
  });
  return saved;
}

// ---------------------------------------------------------------- blocked / allowed senders
// Resolves with null (cancelled) or {moved} (how many existing messages went to Spam).
export async function blockSender(email, name = '') {
  const domain = email.split('@')[1] || '';
  let done = null;
  await dialog({
    iconName: 'block', title: `Block ${name || email}?`,
    text: 'Their future messages will go straight to Spam. You can unblock them in Settings → Filters & blocked.',
    body: html`<form>
      <label class="check"><input type="radio" name="what" value="${email}" checked><span>Only <b>${email}</b></span></label>
      ${domain ? html`<label class="check"><input type="radio" name="what" value="@${domain}"><span>Everyone at <b>${domain}</b></span></label>` : ''}
      <label class="check" style="margin-top:8px"><input type="checkbox" class="cb" name="move"><span>Also move their messages already here to Spam</span></label>
    </form>`,
    actions: [{ label: 'Cancel', value: false }, { label: 'Block', value: true, kind: 'danger' }],
    onAction: async (v, d) => {
      if (!v) return true;
      const pattern = d.querySelector('[name=what]:checked').value;
      const res = await api('/senders', { method: 'POST', body: { pattern, kind: 'block', move_existing: d.querySelector('[name=move]').checked } });
      setCounts(res.counts);
      done = { moved: res.moved };
      snack(`Blocked ${pattern.startsWith('@') ? pattern.slice(1) : pattern}${res.moved ? ` · ${plural(res.moved, 'message')} moved to Spam` : ''}`);
      return true;
    },
  });
  return done;
}

export async function allowSender(email) {
  try {
    await api('/senders', { method: 'POST', body: { pattern: email, kind: 'allow' } });
    snack(`Messages from ${email} won't go to Spam`);
    return true;
  } catch (e) { toastError(e); return false; }
}

export async function unlistSender(pattern) {
  try {
    await api(`/senders?pattern=${encodeURIComponent(pattern)}`, { method: 'DELETE' });
    return true;
  } catch (e) { toastError(e); return false; }
}

// senderStatus: { kind: 'block' | 'allow' | '', pattern: the address or '@domain' that decides }
export const senderStatus = (email) => api(`/senders/status?email=${encodeURIComponent(email)}`, { background: true })
  .then((r) => ({ kind: r.kind || '', pattern: r.pattern || '' })).catch(() => ({ kind: '', pattern: '' }));

export { folderPath, isCustom, esc };
