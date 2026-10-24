// Personal settings: profile, appearance, notifications, security, out-of-office, email apps.
import { $, html, raw, icon, setHTML, api, upload, snack, avatar, ago, field, passwordToggle, toastError, squareImage, confirmDialog, dateOnly, copyText,
  selectField, bytes, plural, bus } from './lib.js';
import { S, setTheme, themePref, navigate, pref, loadDirectory, loadFolders, folderPath, appName } from './state.js';
import { cleanForEditor } from './compose.js';
import { newFolderDialog, renameFolderDialog, deleteFolderDialog, filterDialog, unlistSender } from './folders.js';
import { pushSupport, enablePush, disablePush, thisDevice } from './push.js';

const TABS = [
  ['general', 'person', 'General'],
  ['security', 'lock', 'Security'],
  ['vacation', 'beach_access', 'Out of office'],
  ['filters', 'filter_alt', 'Filters & blocked'],
  ['apps', 'devices', 'Email apps'],
  ['import', 'download', 'Import & export'],
];

export function render(viewEl, r) {
  const tab = TABS.some((t) => t[0] === r.tab) ? r.tab : 'general';
  setHTML(viewEl, html`
    <div class="page-head"><div class="grow"><h1>Settings</h1><p class="sub">${S.me.user.email}</p></div></div>
    <nav class="tabs">${TABS.map(([id, ic, label]) => html`<a class="tab ${tab === id ? 'on' : ''}" href="#/settings/${id}">${icon(ic, 's')}${label}</a>`)}</nav>
    <div class="page"><div class="page-inner" id="pane"></div></div>`);
  const pane = $('#pane');
  ({ general, security, vacation, filters, apps, import: importMail })[tab](pane);
}

// ---------------------------------------------------------------- general
function general(pane) {
  const u = S.me.user;
  const theme = themePref();
  const notifyOn = pref.get('notify', false) && 'Notification' in window && Notification.permission === 'granted';
  setHTML(pane, html`
    <div class="section">
      <h2>Profile</h2><p class="lead">How colleagues see you in ${S.me.org.name}.</p>
      <div class="panel" style="padding:20px">
        <div style="display:flex;gap:20px;align-items:center;flex-wrap:wrap;margin-bottom:8px">
          <span id="me-avatar">${avatar(u.name, u.email, 's96')}</span>
          <div style="display:flex;gap:8px;flex-wrap:wrap">
            <button class="btn tonal" id="photo-btn">${icon('upload')}Change photo</button>
            ${u.avatar_ver ? html`<button class="btn" id="photo-rm">Remove</button>` : ''}
            <input type="file" accept="image/png,image/jpeg" id="photo-input" hidden>
          </div>
        </div>
        <form id="profile">
          ${field({ label: 'Full name', name: 'name', value: u.name, attrs: 'autocomplete="name" maxlength="120"' })}
          <div class="row2">
            ${field({ label: 'Job title', name: 'title', value: u.title, attrs: 'maxlength="120"' })}
            ${field({ label: 'Phone', name: 'phone', value: u.phone, type: 'tel', attrs: 'maxlength="60"' })}
          </div>
          <div class="hint">Email address: <b>${u.email}</b> · Only an administrator can change it.</div>
          <div class="section" style="margin:20px 0 0">
            <h2 style="font-size:15px">Signature</h2>
            <p class="lead small">Added to new messages, replies and forwards.</p>
            <div class="field" style="margin-top:0"><div id="sig" class="editor" contenteditable="true" role="textbox" aria-multiline="true" aria-label="Signature" dir="auto" data-ph="e.g. Asi K · Acme Ltd · +91 98765 43210"
              style="border:1px solid var(--outline);border-radius:6px;padding:12px 16px;min-height:96px">${raw(cleanForEditor(u.signature))}</div></div>
          </div>
          <div class="form-actions"><button class="btn filled" type="submit">Save profile</button></div>
        </form>
      </div>
    </div>
    <div class="section">
      <h2>Appearance</h2><p class="lead">Choose how ${appName()} looks on this device.</p>
      <div class="panel" style="padding:16px 20px">
        <div class="seg" role="radiogroup" aria-label="Theme">
          ${[['light', 'light_mode', 'Light'], ['dark', 'dark_mode', 'Dark'], ['system', 'desktop_windows', 'System']].map(([v, ic, l]) => html`<button class="${theme === v ? 'on' : ''}" data-theme-pick="${v}" role="radio" aria-checked="${theme === v}">${icon(ic, 's')}${l}</button>`)}
        </div>
      </div>
    </div>
    <div class="section">
      <h2>Notifications</h2>
      <div class="panel" style="padding:4px 20px">
        <label class="kv check" style="width:100%"><span class="v"><b>Desktop notifications</b><br><span class="muted small">Get a notification when new mail arrives while ${appName()} is open in the background.</span></span>
          <input type="checkbox" class="switch" id="notify" ${notifyOn ? raw('checked') : ''}></label>
        <div id="push-box"></div>
      </div>
    </div>
    <div class="section">
      <h2>Install the app</h2>
      <div class="panel" style="padding:16px 20px">
        <p class="mt0">Use ${appName()} like a desktop app — its own window, taskbar icon, unread badge, and it opens <b>mailto:</b> links.</p>
        ${S.installPrompt ? html`<button class="btn filled" id="install">${icon('install_desktop')}Install ${appName()}</button>`
          : html`<p class="muted small" style="margin-bottom:4px">In <b>Microsoft Edge</b> or <b>Google Chrome</b> on Windows, click the install icon (${icon('install_desktop', 'xs')}) at the right of the address bar, or open the browser menu → <b>Apps</b> → <b>Install this site as an app</b>. On a phone, use <b>Add to Home screen</b>.</p>`}
      </div>
    </div>`);

  // photo
  const input = $('#photo-input');
  $('#photo-btn').onclick = () => input.click();
  input.onchange = async () => {
    const f = input.files[0];
    if (!f) return;
    try {
      const sq = await squareImage(f, 256);
      S.me.user = await upload('/profile/avatar', sq);
      await loadDirectory();
      snack('Photo updated');
      navigate('#/settings/general', { replace: true });
      document.querySelector('#account-btn').innerHTML = String(avatar(S.me.user.name, S.me.user.email, 's32'));
    } catch (e) { toastError(e); }
  };
  $('#photo-rm')?.addEventListener('click', async () => {
    try { S.me.user = await api('/profile/avatar', { method: 'DELETE' }); await loadDirectory(); navigate('#/settings/general', { replace: true }); } catch (e) { toastError(e); }
  });

  $('#profile').onsubmit = async (e) => {
    e.preventDefault();
    const f = e.target;
    const btn = f.querySelector('[type=submit]');
    btn.classList.add('busy');
    try {
      S.me.user = await api('/profile', { method: 'PUT', body: { name: f.name.value, title: f.title.value, phone: f.phone.value, signature: $('#sig').innerHTML.trim() === '<br>' ? '' : $('#sig').innerHTML } });
      loadDirectory();
      snack('Profile saved');
    } catch (er) { toastError(er); } finally { btn.classList.remove('busy'); }
  };

  pane.querySelectorAll('[data-theme-pick]').forEach((b) => (b.onclick = () => {
    setTheme(b.dataset.themePick);
    pane.querySelectorAll('[data-theme-pick]').forEach((x) => { x.classList.toggle('on', x === b); x.setAttribute('aria-checked', x === b); });
  }));

  $('#notify').onchange = async (e) => {
    if (!e.target.checked) { pref.set('notify', false); return; }
    if (!('Notification' in window)) { e.target.checked = false; return snack("This browser doesn't support notifications"); }
    const p = await Notification.requestPermission();
    if (p !== 'granted') { e.target.checked = false; pref.set('notify', false); return snack('Notifications are blocked. Allow them in your browser’s site settings.'); }
    pref.set('notify', true);
    snack('You’ll be notified about new mail');
  };
  renderPushBox($('#push-box'));
  $('#install')?.addEventListener('click', async () => {
    const p = S.installPrompt;
    S.installPrompt = null;
    p.prompt();
    await p.userChoice;
    navigate('#/settings/general', { replace: true });
  });
}

// ---------------------------------------------------------------- security
// Notifications while the app is closed (Web Push), for this browser, plus the other devices.
async function renderPushBox(box) {
  if (!box) return;
  const sup = pushSupport();
  const title = html`<b>When ${appName()} is closed</b>`;
  if (!sup.ok) {
    setHTML(box, html`<div class="kv"><span class="v">${title}<br><span class="muted small">${sup.reason}</span></span></div>`);
    return;
  }
  let info;
  try { info = await api('/push'); } catch (e) { setHTML(box, html`<div class="kv"><span class="v">${title}<br><span class="form-err">${e.message}</span></span></div>`); return; }
  if (!box.isConnected) return;
  const mine = Notification.permission === 'granted' ? await thisDevice(info.devices) : null;
  if (!box.isConnected) return;
  const others = info.devices.filter((d) => !mine || d.id !== mine.id);
  setHTML(box, html`
    <label class="kv check" style="width:100%"><span class="v">${title}<br><span class="muted small">Get new-mail notifications on this device even when the app or browser tab is closed.</span></span>
      <input type="checkbox" class="switch" id="push-on" ${mine ? raw('checked') : ''} ${info.available ? '' : raw('disabled')}></label>
    ${mine ? html`
      <label class="kv check" style="width:100%"><span class="v"><b>Show sender and subject</b><br><span class="muted small">Turn off to show only “New message”, for example on a shared computer or a phone’s lock screen.</span></span>
        <input type="checkbox" class="switch" id="push-preview" ${mine.preview ? raw('checked') : ''}></label>
      <div class="kv"><span class="v">${mine.last_err ? html`<span class="muted small">Last attempt failed: ${mine.last_err}</span>` : ''}</span>
        <button class="btn tonal" id="push-test">${icon('notifications_active')}Send a test notification</button></div>` : ''}
    ${others.length ? html`
      <div class="kv push-devices"><span class="v"><b>Other devices with notifications</b>
        ${others.map((d) => { const ua = describeUA(d.label); return html`<div class="push-dev"><span>${icon(ua.mobile ? 'smartphone' : 'desktop_windows', 's')}${ua.label}<span class="muted small"> · ${d.last_ok ? 'last notified ' + ago(d.last_ok) : 'added ' + ago(d.created_at)}</span></span>
          <button class="btn" data-push-rm="${d.id}">Remove</button></div>`; })}</span></div>` : ''}`);

  const on = $('#push-on', box);
  on.onchange = async () => {
    on.disabled = true;
    try {
      if (on.checked) { await enablePush(true); snack('Notifications are on for this device'); }
      else { await disablePush(mine?.id); snack('Notifications are off for this device'); }
    } catch (e) { on.checked = !on.checked; snack(e.message || 'Couldn’t change notifications'); }
    renderPushBox(box);
  };
  const pv = $('#push-preview', box);
  if (pv) pv.onchange = async () => {
    try { await api('/push/' + mine.id, { method: 'PUT', body: { preview: pv.checked } }); } catch (e) { pv.checked = !pv.checked; toastError(e); }
  };
  const test = $('#push-test', box);
  if (test) test.onclick = async () => {
    test.classList.add('busy');
    try { await api('/push/' + mine.id + '/test', { method: 'POST' }); snack('Test notification sent'); } catch (e) { toastError(e); renderPushBox(box); }
    finally { test.classList.remove('busy'); }
  };
  box.querySelectorAll('[data-push-rm]').forEach((b) => (b.onclick = async () => {
    try { await api('/push/' + b.dataset.pushRm, { method: 'DELETE' }); renderPushBox(box); } catch (e) { toastError(e); }
  }));
}

function describeUA(ua) {
  const b = /Edg\//.test(ua) ? 'Edge' : /OPR\//.test(ua) ? 'Opera' : /Chrome\//.test(ua) ? 'Chrome' : /Firefox\//.test(ua) ? 'Firefox' : /Safari\//.test(ua) ? 'Safari' : 'Browser';
  const os = /Windows/.test(ua) ? 'Windows' : /Android/.test(ua) ? 'Android' : /iPhone|iPad/.test(ua) ? 'iOS' : /Mac OS X/.test(ua) ? 'macOS' : /Linux/.test(ua) ? 'Linux' : '';
  return { label: os ? `${b} on ${os}` : b, mobile: /Android|iPhone|iPad/.test(ua) };
}

async function security(pane) {
  setHTML(pane, html`
    <div class="section">
      <h2>Password</h2><p class="lead">Changing your password signs you out everywhere else.</p>
      <div class="panel" style="padding:12px 20px">
        <form id="pw" style="max-width:440px">
          <input type="email" name="username" value="${S.me.user.email}" autocomplete="username" hidden>
          ${field({ label: 'Current password', name: 'current', type: 'password', attrs: 'autocomplete="current-password"' })}
          ${field({ label: 'New password', name: 'new', type: 'password', attrs: 'autocomplete="new-password"', hint: 'At least 8 characters',
            suffix: html`<button type="button" class="icon-btn" data-pw-toggle="f-new" aria-label="Show password">${icon('visibility')}</button>` })}
          ${field({ label: 'Confirm new password', name: 'confirm', type: 'password', attrs: 'autocomplete="new-password"' })}
          <div class="form-actions" style="justify-content:flex-start"><button class="btn filled" type="submit">Change password</button></div>
        </form>
      </div>
    </div>
    <div class="section">
      <h2>Where you're signed in</h2><p class="lead">Sign out of devices you don't recognise.</p>
      <div class="panel" id="sessions" style="padding:4px 20px"><div class="center-fill" style="padding:24px"><div class="spinner"></div></div></div>
    </div>`);
  passwordToggle(pane);
  $('#pw').onsubmit = async (e) => {
    e.preventDefault();
    const f = e.target;
    if (f.new.value.length < 8) return snack('Use at least 8 characters');
    if (f.new.value !== f.confirm.value) return snack("Passwords don't match");
    const btn = f.querySelector('[type=submit]');
    btn.classList.add('busy');
    try {
      await api('/profile/password', { method: 'POST', body: { current: f.current.value, new: f.new.value } });
      f.reset();
      snack('Password changed. Other devices were signed out.');
      loadSessions();
    } catch (er) { toastError(er); } finally { btn.classList.remove('busy'); }
  };
  const loadSessions = async () => {
    let list;
    try { list = await api('/sessions'); } catch (e) { return toastError(e); }
    const box = $('#sessions');
    if (!box) return;
    setHTML(box, html`${list.map((s) => {
      const d = describeUA(s.user_agent);
      return html`<div class="kv">
        <span>${icon(d.mobile ? 'phone_iphone' : 'desktop_windows', 'l')}</span>
        <div class="v"><b>${d.label}</b>${s.current ? html` <span class="pill green">This device</span>` : ''}<br>
          <span class="muted small">${s.ip || 'Unknown location'} · ${s.current ? 'Active now' : `Last active ${ago(s.last_seen)}`} · Signed in ${dateOnly(s.created_at)}</span></div>
        ${s.current ? '' : html`<button class="btn" data-revoke="${s.id}">Sign out</button>`}
      </div>`;
    })}
    ${list.length > 1 ? html`<div class="form-actions"><button class="btn danger" id="revoke-all">Sign out all other devices</button></div>` : ''}`);
    box.querySelectorAll('[data-revoke]').forEach((b) => (b.onclick = async () => {
      try { await api(`/sessions/${b.dataset.revoke}`, { method: 'DELETE' }); loadSessions(); } catch (e) { toastError(e); }
    }));
    $('#revoke-all')?.addEventListener('click', async () => {
      if (!(await confirmDialog('Sign out everywhere else?', 'All other browsers and devices will need to sign in again.', 'Sign out others'))) return;
      try { await api('/sessions/revoke-others', { method: 'POST' }); snack('Signed out of other devices'); loadSessions(); } catch (e) { toastError(e); }
    });
  };
  loadSessions();
}

// ---------------------------------------------------------------- export
async function loadExport() {
  let list;
  try { list = await api('/export/folders'); } catch (e) { return toastError(e); }
  const box = $('#exp-panel');
  if (!box) return;
  setHTML(box, html`<form id="exp-form">
      <div class="exp-folders">${list.map((f) => html`<label class="check exp-folder" style="--depth:${f.name.split('/').length - 1}">
        <input type="checkbox" class="cb" name="f" value="${f.key}" ${f.default ? raw('checked') : ''} data-bytes="${f.bytes}" data-n="${f.messages}">
        <span>${f.name}</span><span class="muted small">${plural(f.messages, 'message')} · ${bytes(f.bytes)}</span></label>`)}</div>
      <p class="small muted" id="exp-sum"></p>
      <div class="form-actions" style="justify-content:flex-start"><button class="btn filled" type="submit" id="exp-go">${icon('download')}Download .zip</button></div>
    </form>`);
  const form = $('#exp-form');
  const sum = () => {
    const on = [...form.querySelectorAll('[name=f]:checked')];
    const n = on.reduce((t, c) => t + +c.dataset.n, 0);
    const b = on.reduce((t, c) => t + +c.dataset.bytes, 0);
    $('#exp-sum').textContent = on.length ? `${plural(n, 'message')}, about ${bytes(b)} before compression.${b > 500 * 1048576 ? ' Large exports take a while — keep this tab open until the download finishes.' : ''}` : 'Choose at least one folder.';
    $('#exp-go').disabled = !on.length;
  };
  form.onchange = sum;
  sum();
  form.onsubmit = async (e) => {
    e.preventDefault();
    const keys = [...form.querySelectorAll('[name=f]:checked')].map((c) => c.value);
    const q = `folders=${encodeURIComponent(keys.join(','))}`;
    try { await api(`/export?check=1&${q}`); } catch (er) { return toastError(er); }
    const a = document.createElement('a');
    a.href = `/api/export?${q}`;
    a.download = '';
    document.body.append(a);
    a.click();
    a.remove();
    snack('Your download is starting');
  };
}

// ---------------------------------------------------------------- folders, filters, blocked/allowed senders
async function filters(pane) {
  setHTML(pane, html`
    <div class="section">
      <div class="sec-head"><div><h2>Folders</h2><p class="lead">Your own folders, shown in the side menu. Deleting a folder moves its mail to Archive.</p></div>
        <button class="btn tonal" id="add-folder">${icon('create_new_folder')}New folder</button></div>
      <div class="panel" id="folder-list" style="padding:4px 20px"></div>
    </div>
    <div class="section">
      <div class="sec-head"><div><h2>Filters</h2><p class="lead">Sort new mail automatically as it arrives. Filters run top to bottom; the first one that moves a message decides its folder.</p></div>
        <button class="btn tonal" id="add-filter">${icon('add')}New filter</button></div>
      <div class="panel" id="filter-list" style="padding:4px 20px"></div>
    </div>
    <div class="section">
      <h2>Blocked and allowed senders</h2>
      <p class="lead">Mail from blocked addresses or domains goes straight to Spam. Allowed senders never go to Spam when their mail is verified as really coming from their domain, so forged copies still do.</p>
      <div class="panel" style="padding:12px 20px">
        <form id="sender-form" class="sender-form" autocomplete="off">
          ${field({ label: 'Email address or domain', name: 'pattern', attrs: 'spellcheck="false" autocapitalize="off" maxlength="254"', hint: 'Like name@example.com, or example.com for everyone there' })}
          ${selectField({ label: 'List', name: 'kind', value: 'block', options: [{ value: 'block', label: 'Block' }, { value: 'allow', label: 'Always allow' }] })}
          <button class="btn filled" type="submit">Add</button>
        </form>
        <div id="sender-list"></div>
      </div>
    </div>`);

  const loadFolderList = async () => {
    const box = $('#folder-list');
    if (!box) return;
    const list = S.folders || [];
    setHTML(box, list.length ? list.map((f) => html`<div class="kv folder-kv" style="--depth:${f.depth}">
        <span>${icon('folder')}</span>
        <div class="v"><b>${f.leaf}</b>${f.depth ? html` <span class="muted small">in ${f.name.slice(0, -f.leaf.length - 1)}</span>` : ''}<br>
          <span class="muted small">${plural(f.total, 'message')}${f.unread ? ` · ${f.unread} unread` : ''}</span></div>
        <button class="icon-btn" data-fedit="${f.id}" data-tip="Rename" aria-label="Rename ${f.name}">${icon('edit')}</button>
        <button class="icon-btn" data-fdel="${f.id}" data-tip="Delete" aria-label="Delete ${f.name}">${icon('delete')}</button>
      </div>`) : html`<p class="muted" style="margin:14px 0">No folders yet. Create one here or with “Move to” → “New folder…” on any conversation.</p>`);
    box.querySelectorAll('[data-fedit]').forEach((b) => (b.onclick = () => renameFolderDialog(list.find((x) => x.id === +b.dataset.fedit))));
    box.querySelectorAll('[data-fdel]').forEach((b) => (b.onclick = () => deleteFolderDialog(list.find((x) => x.id === +b.dataset.fdel))));
  };
  const off = bus.on('folders', () => { if (pane.isConnected) { loadFolderList(); loadFilters(); } else off?.(); });
  $('#add-folder').onclick = () => newFolderDialog();

  const describe = (f) => {
    const cond = [['From', f.from], ['To', f.to], ['Subject', f.subject], ['Words', f.words]].filter((x) => x[1]).map(([k, v]) => html`<span class="muted">${k}:</span> ${v}`);
    const acts = [];
    if (f.folder) acts.push(f.folder === 'archive' ? 'Skip the inbox (Archive)' : f.folder === 'trash' ? 'Delete it' : `Move to ${folderPath(f.folder)}`);
    if (f.mark_read) acts.push('Mark as read');
    if (f.star) acts.push('Star it');
    return html`${cond.map((c, i) => html`${i ? ' · ' : ''}${c}`)}<br><span class="small">→ ${acts.join(', ') || 'Nothing'}</span>`;
  };
  const loadFilters = async () => {
    let list;
    try { list = await api('/filters'); } catch (e) { return toastError(e); }
    const box = $('#filter-list');
    if (!box) return;
    setHTML(box, list.length ? list.map((f, i) => html`<div class="kv filter-kv ${f.enabled ? '' : 'off'}" data-fid="${f.id}">
        <label class="switch-wrap" data-tip="${f.enabled ? 'On' : 'Off'}"><input type="checkbox" class="switch" data-ftoggle="${f.id}" ${f.enabled ? raw('checked') : ''} aria-label="Filter on"></label>
        <div class="v">${describe(f)}<br><span class="muted small">${f.hits ? `Used ${plural(f.hits, 'time')}, last ${ago(f.last_hit)}` : 'Not used yet'}</span></div>
        <div class="row-acts">
          <button class="icon-btn" data-fup="${i}" data-tip="Move up" aria-label="Move up" ${i === 0 ? raw('disabled') : ''}>${icon('arrow_upward')}</button>
          <button class="icon-btn" data-fdown="${i}" data-tip="Move down" aria-label="Move down" ${i === list.length - 1 ? raw('disabled') : ''}>${icon('arrow_downward')}</button>
          <button class="icon-btn" data-fedit2="${f.id}" data-tip="Edit" aria-label="Edit filter">${icon('edit')}</button>
          <button class="icon-btn" data-fdel2="${f.id}" data-tip="Delete" aria-label="Delete filter">${icon('delete')}</button>
        </div>
      </div>`) : html`<p class="muted" style="margin:14px 0">No filters yet. Tip: open a message and choose <b>Filter messages like this</b> from its menu.</p>`);
    const byId = (id) => list.find((x) => x.id === +id);
    box.querySelectorAll('[data-ftoggle]').forEach((c) => (c.onchange = async () => {
      const f = byId(c.dataset.ftoggle);
      try { await api(`/filters/${f.id}`, { method: 'PUT', body: { ...f, enabled: c.checked } }); loadFilters(); } catch (e) { c.checked = !c.checked; toastError(e); }
    }));
    box.querySelectorAll('[data-fedit2]').forEach((b) => (b.onclick = () => filterDialog(byId(b.dataset.fedit2)).then((x) => x && loadFilters())));
    box.querySelectorAll('[data-fdel2]').forEach((b) => (b.onclick = async () => {
      if (!(await confirmDialog('Delete this filter?', 'Mail it already sorted stays where it is.', 'Delete', true))) return;
      try { await api(`/filters/${b.dataset.fdel2}`, { method: 'DELETE' }); loadFilters(); } catch (e) { toastError(e); }
    }));
    const reorder = async (from, to) => {
      const ids = list.map((x) => x.id);
      [ids[from], ids[to]] = [ids[to], ids[from]];
      try { await api('/filters/order', { method: 'POST', body: { ids } }); loadFilters(); } catch (e) { toastError(e); }
    };
    box.querySelectorAll('[data-fup]').forEach((b) => (b.onclick = () => reorder(+b.dataset.fup, +b.dataset.fup - 1)));
    box.querySelectorAll('[data-fdown]').forEach((b) => (b.onclick = () => reorder(+b.dataset.fdown, +b.dataset.fdown + 1)));
  };
  $('#add-filter').onclick = () => filterDialog().then((x) => x && loadFilters());

  const loadSenders = async () => {
    let list;
    try { list = await api('/senders'); } catch (e) { return toastError(e); }
    const box = $('#sender-list');
    if (!box) return;
    const group = (kind, title) => {
      const items = list.filter((x) => x.kind === kind);
      return html`<h3 class="sender-h">${icon(kind === 'block' ? 'block' : 'verified_user', 's')}${title} <span class="muted small">(${items.length})</span></h3>
        ${items.length ? html`<div class="chips sender-chips">${items.map((x) => html`<span class="chip on sender-chip">${x.pattern.startsWith('@') ? html`<span class="muted">everyone at</span> ${x.pattern.slice(1)}` : x.pattern}
          <button class="icon-btn sm" data-unlist="${x.pattern}" aria-label="Remove ${x.pattern}" data-tip="Remove">${icon('close')}</button></span>`)}</div>`
          : html`<p class="muted small" style="margin:4px 0 12px">Nobody yet.</p>`}`;
    };
    setHTML(box, html`${group('block', 'Blocked')}${group('allow', 'Always allowed')}`);
    box.querySelectorAll('[data-unlist]').forEach((b) => (b.onclick = async () => { if (await unlistSender(b.dataset.unlist)) loadSenders(); }));
  };
  $('#sender-form').onsubmit = async (e) => {
    e.preventDefault();
    const f = e.target;
    const btn = f.querySelector('[type=submit]');
    btn.classList.add('busy');
    try {
      await api('/senders', { method: 'POST', body: { pattern: f.pattern.value, kind: f.kind.value } });
      f.pattern.value = '';
      snack(f.kind.value === 'block' ? 'Blocked' : 'Added to always allowed');
      loadSenders();
    } catch (er) { toastError(er); } finally { btn.classList.remove('busy'); }
  };
  loadFolderList();
  loadFolders(); // fresh message counts (the event above repaints)
  loadFilters();
  loadSenders();
}

// ---------------------------------------------------------------- out of office
const toDateInput = (ms) => (ms ? new Date(ms - new Date(ms).getTimezoneOffset() * 60000).toISOString().slice(0, 10) : '');
const fromDateInput = (s, endOfDay) => {
  if (!s) return 0;
  const d = new Date(s + 'T00:00:00');
  if (endOfDay) d.setHours(23, 59, 59, 999);
  return d.getTime();
};

async function vacation(pane) {
  let v;
  try { v = await api('/vacation'); } catch (e) { return toastError(e); }
  setHTML(pane, html`
    <div class="section">
      <h2>Out of office</h2><p class="lead">Automatically reply to people who email you while you're away. Each sender gets at most one reply every 4 days; mailing lists and automated mail are never answered.</p>
      <div class="panel" style="padding:8px 20px 12px">
        <form id="vac">
          <label class="kv check" style="width:100%"><span class="v"><b>Automatic replies</b></span><input type="checkbox" class="switch" name="enabled" ${v.enabled ? raw('checked') : ''}></label>
          <div class="row2">
            <div class="field filled"><input type="date" name="start" value="${toDateInput(v.start)}" id="f-start"><label for="f-start">First day (optional)</label></div>
            <div class="field filled"><input type="date" name="end" value="${toDateInput(v.end)}" id="f-end"><label for="f-end">Last day (optional)</label></div>
          </div>
          ${field({ label: 'Subject (optional)', name: 'subject', value: v.subject, hint: 'Leave empty to use “Re: ” and the original subject' })}
          <div class="field"><textarea name="body" id="f-body" placeholder=" " rows="6">${v.body}</textarea><label for="f-body">Message</label></div>
          <div class="form-actions"><button class="btn filled" type="submit">Save</button></div>
        </form>
      </div>
    </div>`);
  $('#vac').onsubmit = async (e) => {
    e.preventDefault();
    const f = e.target;
    const btn = f.querySelector('[type=submit]');
    btn.classList.add('busy');
    try {
      await api('/vacation', { method: 'PUT', body: { enabled: f.enabled.checked, subject: f.subject.value, body: f.body.value, start: fromDateInput(f.start.value), end: fromDateInput(f.end.value, true) } });
      snack(f.enabled.checked ? 'Automatic replies are on' : 'Automatic replies are off');
    } catch (er) { toastError(er); } finally { btn.classList.remove('busy'); }
  };
}

// ---------------------------------------------------------------- email apps
function apps(pane) {
  setHTML(pane, html`<div class="section"><p class="muted">Loading…</p></div>`);
  api('/mail-apps').then((info) => {
    if (!pane.isConnected) return;
    renderApps(pane, info);
  }).catch((e) => { if (pane.isConnected) setHTML(pane, html`<div class="section"><p class="form-err">${e.message}</p></div>`); });
}

function renderApps(pane, info) {
  const host = info.host;
  const row = (k, v, copy) => html`<div class="kv"><span class="k">${k}</span><span class="v">${v}</span>${copy ? html`<button class="icon-btn sm" data-copy="${copy}" data-tip="Copy" aria-label="Copy ${k}">${icon('content_copy')}</button>` : ''}</div>`;
  const ports = (ssl, starttls) => [ssl && `${ssl} (SSL/TLS)`, starttls && `${starttls} (STARTTLS)`].filter(Boolean).join('  or  ');
  const imapOn = !!(info.imap_ssl || info.imap_starttls);
  const smtpOn = !!(info.smtp_ssl || info.smtp_starttls);
  const selfSigned = info.tls_mode === 'self-signed';
  setHTML(pane, html`
    <div class="section">
      <h2>Use Outlook, Apple Mail, Thunderbird or your phone</h2>
      <p class="lead">Add your ${appName()} account to any mail app that supports IMAP. Mail, folders, read status and stars stay in sync with this web app.</p>
      ${selfSigned ? html`<div class="banner warn" role="note">${icon('warning', 's')}<span class="grow">This server doesn't have a trusted security certificate yet, so mail apps will show a warning or refuse to connect. Ask your administrator to set one up.</span></div>` : ''}
      ${imapOn ? '' : html`
        <div class="banner" role="note">${icon('info', 's')}<span class="grow">Mail-app access (IMAP) isn't turned on for this server. Ask your administrator. You can still send from other apps with the settings below.</span></div>`}
    </div>
    ${imapOn ? html`
    <div class="section">
      <h2>Incoming mail (IMAP)</h2>
        <div class="panel">
          ${row('Server', host, host)}
          ${row('Port', ports(info.imap_ssl, info.imap_starttls))}
          ${row('Username', info.username, info.username)}
          ${row('Password', `Your ${appName()} password`)}
        </div>
    </div>` : ''}
    ${smtpOn ? html`
    <div class="section">
      <h2>Outgoing mail (SMTP)</h2>
      <p class="lead">Mail apps, printers, scanners, websites and scripts send as you with these settings.</p>
      <div class="panel">
        ${row('Server', host, host)}
        ${row('Port', ports(info.smtp_ssl, info.smtp_starttls))}
        ${row('Username', info.username, info.username)}
        ${row('Password', `Your ${appName()} password`)}
        ${row('Authentication', 'Required · normal password')}
      </div>
    </div>` : ''}
    ${imapOn ? html`
    <div class="section">
      <h2>Adding the account</h2>
      <div class="panel" style="padding:16px 20px">
        <ul class="apps-steps">
          <li><b>iPhone and iPad</b> — Settings → Apps → Mail → Mail Accounts → Add Account → Other → Add Mail Account. Choose <b>IMAP</b> and enter the settings above for both incoming and outgoing mail.</li>
          <li><b>Android (Gmail app)</b> — tap your profile picture → Add another account → Other. Choose <b>Personal (IMAP)</b>, then enter the settings above.</li>
          <li><b>Outlook</b> — Add account and type your email address. If Outlook doesn't find the settings itself, choose <b>IMAP</b> and enter the settings above.</li>
          <li><b>Apple Mail on Mac</b> — Mail → Add Account → Other Mail Account. If it can't find the server, enter the settings above.</li>
          <li><b>Thunderbird</b> — Add a mail account, then choose <b>Configure manually</b> and enter the settings above.</li>
        </ul>
        <p class="muted small" style="margin-bottom:0">Good to know: deleting a message in a mail app moves it to Trash here. Drafts saved in a mail app stay in that app's Drafts folder, and drafts started here stay here. If you change your password, mail apps ask for the new one.</p>
      </div>
    </div>` : ''}
    <div class="section">
      <h2>Or install ${appName()} as an app</h2>
      <div class="panel" style="padding:16px 20px">
        <p class="mt0">Open <b>https://${host}</b> and install ${appName()} as an app — it works on Windows, macOS, Android and iPhone, stays signed in, and shows new-mail notifications.</p>
        <a class="btn tonal" href="#/settings/general">${icon('install_desktop')}Install options</a>
      </div>
    </div>`);
  pane.querySelectorAll('[data-copy]').forEach((b) => (b.onclick = () => copyText(b.dataset.copy)));
}

// ---------------------------------------------------------------- import mail
// Bring mail over from Zoho, Gmail or any IMAP account, or from exported files.
const PROVIDERS = [
  { id: 'zoho-org', label: 'Zoho Mail — company domain', hosts: { in: 'imappro.zoho.in', com: 'imappro.zoho.com', eu: 'imappro.zoho.eu' } },
  { id: 'zoho', label: 'Zoho Mail — @zohomail.com address', hosts: { in: 'imap.zoho.in', com: 'imap.zoho.com', eu: 'imap.zoho.eu' } },
  { id: 'gmail', label: 'Gmail or Google Workspace', host: 'imap.gmail.com' },
  { id: 'yahoo', label: 'Yahoo Mail', host: 'imap.mail.yahoo.com' },
  { id: 'other', label: 'Another provider (IMAP)', host: '' },
];
const REGIONS = [{ value: 'in', label: 'India (zoho.in)' }, { value: 'com', label: 'United States (zoho.com)' }, { value: 'eu', label: 'Europe (zoho.eu)' }];
const TARGETS = [
  { value: 'keep', label: 'A folder with the same name' },
  { value: 'inbox', label: 'Inbox' }, { value: 'archive', label: 'Archive' }, { value: 'sent', label: 'Sent' },
  { value: 'spam', label: 'Spam' }, { value: 'trash', label: 'Trash' }, { value: 'skip', label: 'Don\u2019t import' },
];
const PROVIDER_HELP = {
  'zoho-org': html`In Zoho Mail, open <b>Settings → Mail Accounts → IMAP</b> and turn on <b>IMAP Access</b>. If you sign in with two-step verification, create an <b>app-specific password</b> (Zoho Accounts → Security → App Passwords) and use it below. The exact server name is shown in Zoho under the same IMAP settings.`,
  zoho: html`In Zoho Mail, open <b>Settings → Mail Accounts → IMAP</b> and turn on <b>IMAP Access</b>. With two-step verification on, use an <b>app-specific password</b> from Zoho Accounts → Security.`,
  gmail: html`Google needs an <b>app password</b>: turn on 2-Step Verification in your Google Account, then create one under <b>Security → App passwords</b>. Some company Google accounts don't allow app passwords — ask your Google admin, or use a Google Takeout <b>.mbox</b> file below instead.`,
  yahoo: html`Create an <b>app password</b> in Yahoo Account Info → Account security, and use it below.`,
  other: html`Use the IMAP server name from your provider's help pages (it must support SSL/TLS on port 993).`,
};

let importPoll = null;

function importMail(pane) {
  clearTimeout(importPoll);
  const me = S.me.user;
  setHTML(pane, html`
    <div class="section">
      <h2>Import from another account</h2>
      <p class="lead">Copy your old mail into ${appName()} directly from Zoho, Gmail or any provider with IMAP. Your old account isn't changed, and folders, read status, stars and original dates are kept.</p>
      <div class="panel imp-panel">
        <form id="imp-form" autocomplete="off">
          <div class="row2">
            ${selectField({ label: 'Provider', name: 'provider', value: 'zoho-org', options: PROVIDERS.map((p) => ({ value: p.id, label: p.label })) })}
            <div id="imp-region">${selectField({ label: 'Zoho data centre', name: 'region', value: 'in', options: REGIONS })}</div>
          </div>
          <p class="imp-help" id="imp-help"></p>
          ${field({ label: 'IMAP server', name: 'host', value: 'imappro.zoho.in', hint: 'Connects securely (SSL/TLS) on port 993.', attrs: 'autocapitalize="off" spellcheck="false" required' })}
          <div class="row2">
            ${field({ label: 'Email address (old account)', name: 'username', value: me.email, attrs: 'autocapitalize="off" spellcheck="false" required autocomplete="off"' })}
            ${field({ label: 'Password or app password', name: 'password', type: 'password', attrs: 'required autocomplete="new-password"' })}
          </div>
          <p class="form-err" id="imp-err" role="alert"></p>
          <div class="form-actions"><button class="btn filled" type="submit" id="imp-connect">${icon('sync_alt')}Connect and choose folders</button></div>
        </form>
        <div id="imp-folders" hidden></div>
      </div>
    </div>
    <div class="section">
      <h2>Import from a file</h2>
      <p class="lead">Upload an export from your old provider: a <b>.zip</b> of emails (Zoho Mail → Settings → Import/Export → Export), an <b>.mbox</b> file (Google Takeout, Thunderbird) or a single <b>.eml</b> message.</p>
      <div class="panel imp-panel">
        <form id="file-form">
          <label class="imp-drop" id="imp-drop">
            <input type="file" id="imp-file" accept=".zip,.mbox,.eml,application/zip,message/rfc822" hidden>
            ${icon('folder_zip', 'l')}<span id="imp-file-label"><b>Choose a file</b> or drop it here</span>
          </label>
          <div class="row2">
            ${selectField({ label: 'Put messages in', name: 'folder', value: 'auto', options: [
              { value: 'auto', label: 'Sort automatically' }, { value: 'inbox', label: 'Inbox' }, { value: 'archive', label: 'Archive' }, { value: 'sent', label: 'Sent' }] })}
            ${field({ label: 'Your address in the old account', name: 'old_email', value: me.email, hint: 'Messages from this address go to Sent.', attrs: 'autocapitalize="off" spellcheck="false"' })}
          </div>
          <label class="check"><input type="checkbox" class="cb" name="mark_read" checked><span>Mark imported messages as read</span></label>
          <div class="imp-upload" id="imp-upload" hidden><div class="progress"><i id="imp-bar" style="width:0%"></i></div><span id="imp-upload-label"></span></div>
          <p class="form-err" id="file-err" role="alert"></p>
          <div class="form-actions">
            <button class="btn" type="button" id="file-cancel" hidden>Cancel upload</button>
            <button class="btn filled" type="submit" id="file-start" disabled>${icon('upload')}Upload and import</button>
          </div>
        </form>
      </div>
    </div>
    <div class="section">
      <h2>Your imports</h2>
      <p class="lead">Imports keep running if you close this page. Running one again is safe — messages already in your mailbox are skipped.</p>
      <div id="imp-jobs"></div>
    </div>
    <div class="section">
      <h2>Export your mail</h2>
      <p class="lead">Download a copy of your mail as a .zip: one folder per mail folder, one .eml file per message. It opens in Outlook, Apple Mail or Thunderbird, and can be imported into ${appName()} or another service.</p>
      <div class="panel" style="padding:12px 20px" id="exp-panel"><div class="center-fill" style="padding:16px"><div class="spinner"></div></div></div>
    </div>`);
  loadExport();

  // ---- account form
  const form = $('#imp-form');
  const impErr = $('#imp-err');
  const syncProvider = (changed) => {
    const p = PROVIDERS.find((x) => x.id === form.provider.value);
    const zoho = !!p.hosts;
    $('#imp-region').hidden = !zoho;
    setHTML($('#imp-help'), PROVIDER_HELP[p.id] || '');
    if (changed) {
      form.host.value = zoho ? p.hosts[form.region.value] : p.host;
    }
  };
  form.provider.onchange = () => syncProvider(true);
  form.region.onchange = () => syncProvider(true);
  syncProvider(false);
  const source = () => ({ host: form.host.value.trim(), security: 'tls', port: 993, username: form.username.value.trim(), password: form.password.value });
  form.onsubmit = async (e) => {
    e.preventDefault();
    const btn = $('#imp-connect');
    impErr.textContent = '';
    btn.classList.add('busy');
    try {
      const res = await api('/import/imap/folders', { method: 'POST', body: source() });
      showFolders(res.folders);
    } catch (er) {
      impErr.textContent = er.message;
    } finally { btn.classList.remove('busy'); }
  };

  function showFolders(folders) {
    const box = $('#imp-folders');
    if (!folders.length) { impErr.textContent = 'No folders were found in that account.'; return; }
    form.querySelectorAll('input, select, button').forEach((x) => (x.disabled = true));
    setHTML(box, html`
      <div class="imp-ok">${icon('check_circle', 's')}<span>Connected to <b>${form.host.value}</b>. Choose where each folder goes — your own folders are recreated here with the same names.</span></div>
      <div class="imp-folders">${folders.map((f, i) => html`
        <div class="imp-folder">
          <span class="nm" dir="auto">${icon(f.target === 'skip' ? 'remove' : 'folder_zip', 's')}<span>${f.name}</span></span>
          <span class="ct">${f.messages.toLocaleString()}</span>
          <select data-i="${i}" aria-label="Destination for ${f.name}">${TARGETS.map((t) => html`<option value="${t.value}" ${t.value === f.target ? raw('selected') : ''}>${t.label}</option>`)}</select>
        </div>`)}</div>
      <p class="muted small" id="imp-total"></p>
      <p class="form-err" id="imp-err2" role="alert"></p>
      <div class="form-actions">
        <button class="btn" type="button" id="imp-back">Back</button>
        <button class="btn filled" type="button" id="imp-start">${icon('download')}Start import</button>
      </div>`);
    box.hidden = false;
    const err2 = $('#imp-err2');
    const recount = () => {
      let n = 0;
      box.querySelectorAll('select[data-i]').forEach((sel) => { if (sel.value !== 'skip') n += folders[+sel.dataset.i].messages; });
      $('#imp-total').textContent = `${plural(n, 'message')} will be imported.`;
      $('#imp-start').disabled = ![...box.querySelectorAll('select[data-i]')].some((x) => x.value !== 'skip');
    };
    box.querySelectorAll('select[data-i]').forEach((sel) => (sel.onchange = recount));
    recount();
    $('#imp-back').onclick = () => {
      box.hidden = true;
      form.querySelectorAll('input, select, button').forEach((x) => (x.disabled = false));
    };
    $('#imp-start').onclick = async (e) => {
      const btn = e.currentTarget;
      btn.classList.add('busy');
      err2.textContent = '';
      try {
        const chosen = [...box.querySelectorAll('select[data-i]')].map((sel) => ({ name: folders[+sel.dataset.i].name, delim: folders[+sel.dataset.i].delim || '', target: sel.value }));
        await api('/import/imap', { method: 'POST', body: { ...source(), folders: chosen } });
        form.password.value = ''; // not kept anywhere once the import has started
        box.hidden = true;
        form.querySelectorAll('input, select, button').forEach((x) => (x.disabled = false));
        snack('Import started — you can keep working while it runs');
        loadJobs();
      } catch (er) {
        err2.textContent = er.message;
      } finally { btn.classList.remove('busy'); }
    };
  }

  // ---- file form (elements captured once: an upload keeps going if you leave this page)
  const fform = $('#file-form');
  const input = $('#imp-file');
  const fileLabel = $('#imp-file-label');
  const startBtn = $('#file-start');
  const cancelBtn = $('#file-cancel');
  const errEl = $('#file-err');
  const upBox = $('#imp-upload');
  const bar = $('#imp-bar');
  const upLabel = $('#imp-upload-label');
  let file = null;
  let aborter = null;
  const pick = (f) => {
    file = f || null;
    setHTML(fileLabel, file ? html`<b dir="auto">${file.name}</b> · ${bytes(file.size)}` : html`<b>Choose a file</b> or drop it here`);
    startBtn.disabled = !file;
    errEl.textContent = '';
  };
  input.onchange = () => pick(input.files[0]);
  const drop = $('#imp-drop');
  drop.ondragover = (e) => { e.preventDefault(); drop.classList.add('over'); };
  drop.ondragleave = () => drop.classList.remove('over');
  drop.ondrop = (e) => { e.preventDefault(); drop.classList.remove('over'); if (e.dataTransfer.files[0]) pick(e.dataTransfer.files[0]); };
  const warnLeave = (e) => { e.preventDefault(); e.returnValue = ''; };
  fform.onsubmit = async (e) => {
    e.preventDefault();
    if (!file || aborter) return;
    const ext = file.name.toLowerCase().split('.').pop();
    if (!['zip', 'mbox', 'eml', 'mbx'].includes(ext) && !(await confirmDialog('Import this file?', `“${file.name}” doesn't look like an email export (.zip, .mbox or .eml). Try it anyway?`, 'Try anyway'))) return;
    const opts = { folder: fform.folder.value, mark_read: fform.mark_read.checked, old_email: fform.old_email.value.trim() };
    aborter = new AbortController();
    errEl.textContent = '';
    upBox.hidden = false;
    cancelBtn.hidden = false;
    startBtn.classList.add('busy');
    addEventListener('beforeunload', warnLeave);
    const label = upLabel;
    const t0 = Date.now();
    try {
      const id = await uploadInChunks(file, aborter.signal, (sent) => {
        bar.style.width = ((sent / file.size) * 100).toFixed(1) + '%';
        const secs = (Date.now() - t0) / 1000;
        const eta = sent > 0 && secs > 2 ? Math.round(((file.size - sent) / sent) * secs) : 0;
        label.textContent = `Uploading ${bytes(sent)} of ${bytes(file.size)}${eta > 5 ? ` · about ${eta > 90 ? Math.round(eta / 60) + ' min' : eta + ' s'} left` : ''}`;
      });
      label.textContent = 'Starting the import…';
      await api('/import/file', { method: 'POST', body: { upload_id: id, ...opts } });
      snack('Import started — you can keep working while it runs');
      input.value = '';
      pick(null);
      loadJobs();
    } catch (er) {
      if (er.name !== 'AbortError') {
        errEl.textContent = er.message;
        if (!errEl.isConnected) toastError(er); // you've moved on to another page
      }
    } finally {
      removeEventListener('beforeunload', warnLeave);
      aborter = null;
      upBox.hidden = true;
      cancelBtn.hidden = true;
      startBtn.classList.remove('busy');
      bar.style.width = '0%';
    }
  };
  cancelBtn.onclick = () => aborter?.abort();

  // ---- jobs
  const jobsEl = $('#imp-jobs');
  async function loadJobs() {
    clearTimeout(importPoll);
    if (!jobsEl.isConnected) return;
    let jobs;
    try { jobs = (await api('/import/jobs', { background: true })).jobs; } catch { jobs = null; }
    if (!jobsEl.isConnected) return;
    if (jobs) paintJobs(jobs);
    if (!jobs || jobs.some((j) => j.status === 'running' || j.status === 'queued')) importPoll = setTimeout(loadJobs, 2000);
  }
  function paintJobs(jobs) {
    if (!jobs.length) { setHTML(jobsEl, html`<div class="panel imp-empty"><span class="muted">Nothing imported yet.</span></div>`); return; }
    const STATUS = { queued: ['Waiting', 'gray'], running: ['Importing', 'blue'], done: ['Finished', 'green'], failed: ['Stopped', 'red'], cancelled: ['Cancelled', 'gray'], interrupted: ['Interrupted', 'amber'] };
    setHTML(jobsEl, html`<div class="panel imp-jobs">${jobs.map((j) => {
      const [label, tone] = STATUS[j.status] || [j.status, 'gray'];
      const doneN = j.imported + j.skipped + j.failed;
      const pct = j.total ? Math.min(100, (doneN / j.total) * 100) : j.status === 'done' ? 100 : 0;
      const live = j.status === 'running' || j.status === 'queued';
      const counts = [
        j.imported ? `${j.imported.toLocaleString()} imported` : '',
        j.skipped ? `${j.skipped.toLocaleString()} already there` : '',
        j.failed ? `${j.failed.toLocaleString()} couldn't be imported` : '',
      ].filter(Boolean).join(' · ') || (live ? 'Getting ready…' : 'No messages');
      return html`<div class="imp-job">
        <div class="hd">
          ${icon(j.kind === 'imap' ? 'sync_alt' : 'folder_zip', 's')}
          <b class="src" dir="auto">${j.source}</b>
          <span class="pill ${tone}">${label}</span>
          <span class="spacer"></span>
          <span class="muted small">${ago(j.created_at)}</span>
          ${live ? html`<button class="btn sm" data-cancel="${j.id}">Cancel</button>` : ''}
        </div>
        ${live || j.total ? html`<div class="progress ${live ? 'live' : ''}"><i style="width:${pct.toFixed(1)}%"></i></div>` : ''}
        <div class="muted small">${counts}${j.total ? ` of ${j.total.toLocaleString()}` : ''}${live && j.current ? html` · <span dir="auto">${j.current}</span>` : ''}</div>
        ${j.message ? html`<div class="small imp-msg ${j.status === 'failed' ? 'bad' : ''}">${j.message}</div>` : ''}
      </div>`;
    })}</div>`);
    jobsEl.querySelectorAll('[data-cancel]').forEach((b) => (b.onclick = async () => {
      if (!(await confirmDialog('Cancel this import?', 'Messages imported so far stay in your mailbox. You can run it again later to continue.', 'Cancel import', true))) return;
      try { await api(`/import/jobs/${b.dataset.cancel}/cancel`, { method: 'POST' }); loadJobs(); } catch (er) { toastError(er); }
    }));
  }
  loadJobs();
}

/** Uploads a file in chunks; survives brief connection drops by resuming at the server's offset. */
async function uploadInChunks(file, signal, onProgress) {
  const { id, chunk } = await api('/import/uploads', { method: 'POST', body: { name: file.name, size: file.size } });
  let off = 0;
  let tries = 0;
  const wait = (ms) => new Promise((r) => setTimeout(r, ms));
  try {
    while (off < file.size) {
      if (signal.aborted) throw new DOMException('Aborted', 'AbortError');
      let res;
      try {
        res = await fetch(`/api/import/uploads/${id}?offset=${off}`, {
          method: 'PUT', credentials: 'same-origin', signal, body: file.slice(off, Math.min(off + chunk, file.size)),
          headers: { 'X-SM': '1', 'Content-Type': 'application/octet-stream' },
        });
      } catch (e) {
        if (e.name === 'AbortError') throw e;
        if (++tries > 6) throw new Error('The upload keeps getting interrupted. Check your connection and try again.');
        await wait(1000 * tries);
        continue;
      }
      const data = await res.json().catch(() => ({}));
      if (res.status === 409 && typeof data.received === 'number') {
        // "busy": the previous attempt of this chunk is still being finished on the server
        if (data.error === 'busy') {
          if (++tries > 60) throw new Error('The upload got stuck. Try again.');
          await wait(2000);
        }
        off = data.received;
        continue;
      }
      if (res.status === 401) { bus.emit('unauthorized'); throw new Error('You were signed out.'); }
      if (res.status === 413) throw new Error(`The web server in front of ${appName()} refused the upload size. Ask your administrator to raise its upload limit.`);
      if (!res.ok) {
        if (res.status >= 500 && ++tries <= 6) { await wait(1000 * tries); continue; }
        throw new Error(data.error || `Upload failed (${res.status})`);
      }
      off = data.received;
      tries = 0;
      onProgress(off);
    }
  } catch (e) {
    api(`/import/uploads/${id}`, { method: 'DELETE', background: true }).catch(() => {});
    throw e;
  }
  return id;
}
