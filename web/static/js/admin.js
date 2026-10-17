// Admin console: overview, users, groups, domains & DNS, delivery queue, activity log, server settings.
import { $, $$, html, raw, icon, setHTML, api, snack, dialog, menu, avatar, bytes, ago, fullDate, field, selectField, toastError,
  confirmDialog, copyText, plural, upload, squareImage } from './lib.js';
import { S, navigate, loadDirectory } from './state.js';

const TABS = [
  ['overview', 'dashboard', 'Overview'],
  ['users', 'group', 'Users'],
  ['groups', 'alternate_email', 'Groups'],
  ['domains', 'dns', 'Domains & DNS'],
  ['queue', 'schedule', 'Delivery queue'],
  ['log', 'receipt_long', 'Activity'],
  ['settings', 'tune', 'Server settings'],
];

export function render(viewEl, r) {
  const tab = TABS.some((t) => t[0] === r.tab) ? r.tab : 'overview';
  setHTML(viewEl, html`
    <div class="page-head"><div class="grow"><p class="greet">${S.me.org.name}</p><h1>Admin console</h1><p class="sub">Manage people, addresses and how mail flows.</p></div></div>
    <nav class="tabs">${TABS.map(([id, ic, label]) => html`<a class="tab ${tab === id ? 'on' : ''}" href="#/admin/${id}">${icon(ic, 's')}${label}</a>`)}</nav>
    <div class="page"><div class="page-inner" id="pane" style="max-width:1200px"><div class="center-fill" style="padding:64px"><div class="spinner"></div></div></div></div>`);
  const pane = $('#pane');
  ({ overview, users, groups, domains, queue, log, settings })[tab](pane).catch?.(toastError);
}

const statusPill = (s) => ({
  ok: html`<span class="pill green">${icon('check_circle', 'xs')}OK</span>`,
  warn: html`<span class="pill amber">${icon('warning', 'xs')}Check</span>`,
  missing: html`<span class="pill red">${icon('error', 'xs')}Missing</span>`,
  unknown: html`<span class="pill gray">Not checked</span>`,
}[s] || html`<span class="pill gray">${s}</span>`);

function fmtUptime(s) {
  const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
  return d ? `${d}d ${h}h` : h ? `${h}h ${m}m` : `${m}m`;
}

// ---------------------------------------------------------------- overview
async function overview(pane) {
  const o = await api('/admin/overview');
  const max = Math.max(1, ...o.traffic.map((d) => d.in + d.out));
  const h = o.last24h;
  setHTML(pane, html`
    <div class="section mt0">
      <div class="grid-stats">
        <div class="stat"><div class="lbl">${icon('group', 's')}People</div><div class="num">${o.active_users}</div><div class="sm">${plural(o.admins, 'administrator')} · ${o.users - o.active_users} disabled</div></div>
        <div class="stat"><div class="lbl">${icon('storage', 's')}Storage</div><div class="num">${bytes(o.storage)}</div><div class="sm">${o.messages.toLocaleString()} messages</div></div>
        <div class="stat"><div class="lbl">${icon('south_west', 's')}Received · 24h</div><div class="num">${(h['in:delivered'] || 0) + (h['local:delivered'] || 0)}</div><div class="sm">${h['in:spam'] || 0} spam · ${h['in:rejected'] || 0} rejected</div></div>
        <div class="stat"><div class="lbl">${icon('north_east', 's')}Sent out · 24h</div><div class="num">${h['out:sent'] || 0}</div><div class="sm">${h['out:deferred'] || 0} retrying · ${h['out:failed'] || 0} failed</div></div>
        <div class="stat" style="cursor:pointer" data-go="#/admin/queue"><div class="lbl">${icon('schedule', 's')}Queue</div><div class="num">${o.queue.pending + o.queue.deferred}</div><div class="sm">${o.queue.deferred} waiting to retry</div></div>
      </div>
    </div>
    <div class="section">
      <h2>Mail traffic</h2><p class="lead">Messages delivered and sent over the last 14 days.</p>
      <div class="panel" style="padding:16px 20px 12px">
        <div class="chart">${o.traffic.map((d) => html`<div class="col" data-tip="${d.day}: ${d.in} in, ${d.out} out">
          <i class="out" style="height:${(d.out / max) * 100}%"></i><i class="in" style="height:${(d.in / max) * 100}%"></i>
          <small>${new Date(d.day + 'T00:00').getDate()}</small></div>`)}</div>
        <div class="chart-legend"><span>Received</span><span class="out">Sent</span></div>
      </div>
    </div>
    <div class="section">
      <h2>Server</h2>
      <div class="panel">
        <div class="kv"><span class="k">Mail hostname</span><span class="v">${o.hostname}</span></div>
        <div class="kv"><span class="k">TLS certificate</span><span class="v">${{ letsencrypt: "Let's Encrypt (automatic)", file: 'Certificate file (from your reverse proxy)', 'self-signed': 'Self-signed — fine for server-to-server mail; set SM_TLS_CERT for email apps' }[o.tls_mode] || o.tls_mode}</span></div>
        <div class="kv"><span class="k">Outgoing mail</span><span class="v">${o.relay ? 'Through an SMTP relay' : 'Delivered directly to recipients’ servers (port 25)'}</span><button class="btn sm outlined" id="test25">Test port 25</button></div>
        <div class="kv"><span class="k">Uptime</span><span class="v">${fmtUptime(o.uptime)} · ${bytes(o.memory)} memory</span></div>
      </div>
    </div>`);
  pane.querySelectorAll('[data-go]').forEach((x) => (x.onclick = () => navigate(x.dataset.go)));
  $('#test25').onclick = async (e) => {
    e.target.classList.add('busy');
    try {
      const r = await api('/admin/test-port25', { method: 'POST' });
      dialog({ iconName: r.ok ? 'check_circle' : 'error', title: r.ok ? 'Port 25 is open' : 'Port 25 looks blocked', text: r.message,
        body: r.banner ? html`<div class="cred-box">${r.banner}</div>` : r.error ? html`<div class="cred-box">${r.error}</div>` : '' });
    } catch (er) { toastError(er); } finally { e.target.classList.remove('busy'); }
  };
}

// ---------------------------------------------------------------- users
let domainCache = null;
async function getDomains() {
  domainCache = (await api('/admin/domains')).map((d) => ({ id: d.id, name: d.name }));
  return domainCache;
}

function credentialsText(user, password) {
  return `Hi ${user.name.split(' ')[0]},

Your ${S.me.org.name} email account is ready.

  Sign in at:  https://${S.me.org.hostname}
  Email:       ${user.email}
  Password:    ${password}

You'll be asked to choose your own password the first time you sign in.
Tip: install Skien Mails as an app from the browser for quick access.`;
}

function showCredentials(user, password) {
  const text = credentialsText(user, password);
  return dialog({
    iconName: 'key', title: `${user.name}'s sign-in details`,
    text: 'Share these with them privately. The password is shown only once.',
    body: html`<div class="cred-box">${text}</div>`,
    actions: [{ label: 'Done', value: false }, { label: 'Copy details', value: true, kind: 'filled' }],
    onAction: (v) => { if (v) copyText(text, 'Sign-in details copied'); return true; },
  });
}

async function users(pane) {
  const [list] = await Promise.all([api('/admin/users'), getDomains()]);
  const me = S.me.user;
  setHTML(pane, html`
    <div class="section mt0" style="display:flex;align-items:center;gap:12px;flex-wrap:wrap">
      <div class="grow" style="flex:1"><h2 style="margin:0">${plural(list.length, 'user')}</h2><p class="lead" style="margin:0">Each user gets a mailbox and can sign in at ${S.me.org.hostname}.</p></div>
      <button class="btn filled" id="add-user">${icon('person_add')}Add user</button>
    </div>
    <div class="tbl-wrap"><table class="tbl">
      <thead><tr><th>Name</th><th>Role</th><th>Status</th><th>Storage</th><th>Last sign-in</th><th></th></tr></thead>
      <tbody>${list.map((u) => {
        const pct = u.quota_bytes ? Math.min(100, (u.used_bytes / u.quota_bytes) * 100) : 0;
        return html`<tr data-uid="${u.id}">
          <td><div class="who-cell">${avatar(u.name, u.email)}<div><b>${u.name}${u.id === me.id ? html` <span class="muted small">(you)</span>` : ''}</b><small>${u.email}${u.title ? ` · ${u.title}` : ''}</small></div></div></td>
          <td>${u.role === 'admin' ? html`<span class="pill blue">Admin</span>` : html`<span class="pill gray">User</span>`}</td>
          <td>${u.status === 'active' ? (u.must_change_password ? html`<span class="pill amber">Invited</span>` : html`<span class="pill green">Active</span>`) : html`<span class="pill red">Disabled</span>`}</td>
          <td class="nowrap">${u.quota_bytes ? html`<span class="mini-bar"><i style="width:${pct.toFixed(1)}%"></i></span>` : ''}<span class="small">${bytes(u.used_bytes)}${u.quota_bytes ? ` / ${bytes(u.quota_bytes)}` : ''}</span></td>
          <td class="muted small nowrap">${u.last_login_at ? ago(u.last_login_at) : 'Never'}</td>
          <td class="num"><button class="icon-btn sm" data-umenu="${u.id}" aria-label="Actions" data-tip="Actions">${icon('more_vert')}</button></td>
        </tr>`;
      })}</tbody></table></div>`);
  $('#add-user').onclick = () => userDialog(null).then((ok) => ok && users(pane));
  pane.querySelector('tbody').onclick = (e) => {
    const b = e.target.closest('[data-umenu]');
    const tr = e.target.closest('tr[data-uid]');
    if (!tr) return;
    const u = list.find((x) => x.id === +tr.dataset.uid);
    if (!b) return userDialog(u).then((ok) => ok && users(pane));
    menu(b, [
      { icon: 'edit', label: 'Edit', onClick: () => userDialog(u).then((ok) => ok && users(pane)) },
      { icon: 'key', label: 'Reset password', onClick: () => resetPassword(u).then(() => users(pane)) },
      ...(u.id !== me.id ? [{ icon: u.status === 'active' ? 'lock' : 'check_circle', label: u.status === 'active' ? 'Disable account' : 'Enable account', onClick: () => toggleStatus(u).then(() => users(pane)) }] : []),
      ...(u.id !== me.id ? ['sep', { icon: 'delete', label: 'Delete user', danger: true, onClick: () => deleteUser(u).then((ok) => ok && users(pane)) }] : []),
    ]);
  };
}

async function userDialog(u) {
  const editing = !!u;
  const settings = editing ? null : await api('/admin/settings');
  const doms = domainCache || (await getDomains());
  const quotaMB = editing ? Math.round(u.quota_bytes / 1048576) : +settings.default_quota_mb || 0;
  let created = null;
  let enteredPw = '';
  const ok = await dialog({
    title: editing ? 'Edit user' : 'Add a user',
    text: editing ? u.email : 'Create a mailbox for a colleague. You’ll get sign-in details to share with them.',
    body: html`<form id="ufm" autocomplete="off">
      ${editing ? html`<div style="display:flex;gap:16px;align-items:center;margin:8px 0">${avatar(u.name, u.email, 's64')}<button type="button" class="btn sm tonal" id="u-photo">${icon('upload')}Photo</button><input type="file" id="u-photo-in" accept="image/png,image/jpeg" hidden></div>` : ''}
      ${field({ label: 'Full name', name: 'name', value: u?.name || '', attrs: 'autofocus maxlength="120"' })}
      ${editing ? '' : html`<div class="row2" style="grid-template-columns:1fr ${doms.length > 1 ? '1fr' : 'auto'}">
          ${field({ label: 'Username', name: 'local', attrs: 'autocapitalize="off" spellcheck="false"', addon: doms.length === 1 ? '@' + doms[0].name : '' })}
          ${doms.length > 1 ? selectField({ label: 'Domain', name: 'domain_id', options: doms.map((d) => ({ value: d.id, label: '@' + d.name })), value: doms[0].id }) : html`<input type="hidden" name="domain_id" value="${doms[0].id}">`}
        </div>`}
      <div class="row2">
        ${field({ label: 'Job title (optional)', name: 'title', value: u?.title || '', attrs: 'maxlength="120"' })}
        ${editing ? field({ label: 'Phone (optional)', name: 'phone', value: u?.phone || '' }) : field({ label: 'Mailbox size (MB, 0 = unlimited)', name: 'quota_mb', type: 'number', value: quotaMB, attrs: 'min="0" step="512"' })}
      </div>
      ${editing ? field({ label: 'Mailbox size (MB, 0 = unlimited)', name: 'quota_mb', type: 'number', value: quotaMB, attrs: 'min="0" step="512"' }) : ''}
      ${selectField({ label: 'Role', name: 'role', value: u?.role || 'user', options: [{ value: 'user', label: 'User — reads and sends their own mail' }, { value: 'admin', label: 'Administrator — can manage users and settings' }] })}
      ${editing ? '' : html`<label class="check"><input type="checkbox" class="cb" name="gen" checked> Generate a temporary password</label>
        <div id="pw-wrap" hidden>${field({ label: 'Temporary password', name: 'password', type: 'text', attrs: 'autocomplete="off"', hint: 'They’ll choose their own password at first sign-in.' })}</div>`}
    </form>`,
    actions: [{ label: 'Cancel', value: false }, { label: editing ? 'Save' : 'Create user', value: true, kind: 'filled', submit: true }],
    onOpen: (d) => {
      const gen = d.querySelector('[name=gen]');
      if (gen) gen.onchange = () => { d.querySelector('#pw-wrap').hidden = gen.checked; };
      const nm = d.querySelector('[name=name]');
      const lc = d.querySelector('[name=local]');
      if (nm && lc) {
        let touched = false;
        lc.addEventListener('input', () => { touched = true; });
        nm.addEventListener('input', () => { if (!touched) lc.value = nm.value.trim().split(/\s+/)[0]?.toLowerCase().replace(/[^a-z0-9._-]/g, '') || ''; });
      }
      const pb = d.querySelector('#u-photo');
      if (pb) {
        const inp = d.querySelector('#u-photo-in');
        pb.onclick = () => inp.click();
        inp.onchange = async () => {
          try { await upload(`/admin/users/${u.id}/avatar`, await squareImage(inp.files[0])); await loadDirectory(); snack('Photo updated'); } catch (e) { toastError(e); }
        };
      }
    },
    onAction: async (v, d) => {
      if (!v) return true;
      const f = d.querySelector('#ufm');
      const body = { name: f.name.value, title: f.title.value, role: f.role.value, quota_mb: Math.max(0, parseInt(f.quota_mb.value || '0', 10) || 0) };
      if (editing) {
        await api(`/admin/users/${u.id}`, { method: 'PUT', body: { ...body, phone: f.phone.value, status: u.status } });
        snack('User saved');
      } else {
        enteredPw = f.gen.checked ? '' : f.password.value;
        Object.assign(body, { local: f.local.value.trim(), domain_id: +f.domain_id.value, password: enteredPw });
        created = await api('/admin/users', { method: 'POST', body });
      }
      return true;
    },
  });
  if (created) {
    loadDirectory();
    await showCredentials(created.user, created.password || enteredPw);
  }
  return ok;
}

async function resetPassword(u) {
  const ok = await confirmDialog(`Reset ${u.name}'s password?`, `They'll be signed out everywhere and must choose a new password with the temporary one you give them.`, 'Reset password');
  if (!ok) return;
  try {
    const r = await api(`/admin/users/${u.id}/password`, { method: 'POST', body: {} });
    await showCredentials(u, r.password);
  } catch (e) { toastError(e); }
}

async function toggleStatus(u) {
  const disable = u.status === 'active';
  if (disable && !(await confirmDialog(`Disable ${u.name}?`, 'They will be signed out and can no longer sign in or receive new mail. Their mail is kept.', 'Disable', true))) return;
  try {
    await api(`/admin/users/${u.id}`, { method: 'PUT', body: { name: u.name, role: u.role, status: disable ? 'disabled' : 'active', quota_mb: Math.round(u.quota_bytes / 1048576), title: u.title, phone: u.phone } });
    snack(disable ? 'Account disabled' : 'Account enabled');
  } catch (e) { toastError(e); }
}

async function deleteUser(u) {
  return dialog({
    iconName: 'warning', title: `Delete ${u.name}?`,
    text: `This permanently deletes ${u.email} and all of its mail. This can't be undone. To keep their mail, disable the account instead.`,
    body: html`${field({ label: `Type ${u.email} to confirm`, name: 'confirm', attrs: 'autocomplete="off" spellcheck="false"' })}`,
    actions: [{ label: 'Cancel', value: false }, { label: 'Delete forever', value: true, kind: 'danger' }],
    onAction: async (v, d) => {
      if (!v) return true;
      if (d.querySelector('[name=confirm]').value.trim().toLowerCase() !== u.email) { snack('Type the email address exactly to confirm'); return false; }
      await api(`/admin/users/${u.id}`, { method: 'DELETE' });
      snack(`${u.email} deleted`);
      loadDirectory();
      return true;
    },
  });
}

// ---------------------------------------------------------------- groups
async function groups(pane) {
  const [list, usersList] = await Promise.all([api('/admin/aliases'), api('/admin/users'), getDomains()]);
  setHTML(pane, html`
    <div class="section mt0" style="display:flex;align-items:center;gap:12px;flex-wrap:wrap">
      <div style="flex:1"><h2 style="margin:0">Groups</h2><p class="lead" style="margin:0">Shared addresses like info@ or support@ that deliver to several people. Members you allow can also send as the group.</p></div>
      <button class="btn filled" id="add-group">${icon('add')}New group</button>
    </div>
    ${list.length ? html`<div class="tbl-wrap"><table class="tbl"><thead><tr><th>Address</th><th>Members</th><th></th></tr></thead><tbody>
      ${list.map((g) => html`<tr data-gid="${g.id}" style="cursor:pointer">
        <td><div class="who-cell">${avatar(g.name || g.address, g.address)}<div><b>${g.name || g.address}</b><small>${g.address}${g.description ? ` · ${g.description}` : ''}</small></div></div></td>
        <td><div style="display:flex;gap:4px;flex-wrap:wrap">${g.members.slice(0, 6).map((m) => html`<span data-tip="${m.name}${m.can_send ? ' · can send as group' : ''}">${avatar(m.name, m.email, 's28')}</span>`)}${g.members.length > 6 ? html`<span class="pill gray">+${g.members.length - 6}</span>` : ''}</div></td>
        <td class="num"><button class="icon-btn sm" data-gdel="${g.id}" data-tip="Delete group" aria-label="Delete group">${icon('delete')}</button></td></tr>`)}
    </tbody></table></div>` : html`<div class="panel"><div class="empty">${icon('alternate_email')}<h3>No groups yet</h3><p>Create addresses like info@ or team@ that reach several people at once.</p></div></div>`}`);
  $('#add-group').onclick = () => groupDialog(null, usersList).then((ok) => ok && groups(pane));
  pane.querySelector('tbody')?.addEventListener('click', async (e) => {
    const del = e.target.closest('[data-gdel]');
    const tr = e.target.closest('tr[data-gid]');
    if (!tr) return;
    const g = list.find((x) => x.id === +tr.dataset.gid);
    if (del) {
      e.stopPropagation();
      if (!(await confirmDialog(`Delete ${g.address}?`, 'Mail sent to this address will bounce. Messages already delivered stay in members’ mailboxes.', 'Delete', true))) return;
      try { await api(`/admin/aliases/${g.id}`, { method: 'DELETE' }); snack('Group deleted'); groups(pane); } catch (er) { toastError(er); }
      return;
    }
    groupDialog(g, usersList).then((ok) => ok && groups(pane));
  });
}

async function groupDialog(g, usersList) {
  const doms = domainCache || (await getDomains());
  const members = new Map((g?.members || []).map((m) => [m.user_id, m.can_send]));
  const active = usersList.filter((u) => u.status === 'active');
  return dialog({
    title: g ? `Edit ${g.address}` : 'New group',
    wide: true,
    body: html`<form id="gfm" autocomplete="off">
      ${g ? '' : html`<div class="row2" style="grid-template-columns:1fr ${doms.length > 1 ? '1fr' : 'auto'}">
        ${field({ label: 'Address', name: 'local', attrs: 'autofocus autocapitalize="off" spellcheck="false"', addon: doms.length === 1 ? '@' + doms[0].name : '', hint: 'e.g. info, support, team' })}
        ${doms.length > 1 ? selectField({ label: 'Domain', name: 'domain_id', options: doms.map((d) => ({ value: d.id, label: '@' + d.name })), value: doms[0].id }) : html`<input type="hidden" name="domain_id" value="${doms[0].id}">`}
      </div>`}
      <div class="row2">
        ${field({ label: 'Display name', name: 'name', value: g?.name || '', hint: 'Shown when someone sends as the group, e.g. “Skienlab Support”' })}
        ${field({ label: 'Description (optional)', name: 'description', value: g?.description || '' })}
      </div>
      <h3 style="font-weight:500;font-size:15px;margin:20px 0 0">Members</h3>
      <div class="members">${active.map((u) => html`<label class="member">
        <input type="checkbox" class="cb" data-mem="${u.id}" ${members.has(u.id) ? raw('checked') : ''}>
        ${avatar(u.name, u.email, 's32')}<div class="grow"><b>${u.name}</b><small>${u.email}</small></div>
        <span class="small muted">Can send as</span><input type="checkbox" class="switch" data-send="${u.id}" ${members.get(u.id) !== false ? raw('checked') : ''} aria-label="Can send as group">
      </label>`)}</div>
    </form>`,
    actions: [{ label: 'Cancel', value: false }, { label: g ? 'Save' : 'Create group', value: true, kind: 'filled', submit: true }],
    onAction: async (v, d) => {
      if (!v) return true;
      const f = d.querySelector('#gfm');
      const mem = $$('[data-mem]', d).filter((c) => c.checked).map((c) => ({ user_id: +c.dataset.mem, can_send: d.querySelector(`[data-send="${c.dataset.mem}"]`).checked }));
      const body = { name: f.name.value, description: f.description.value, members: mem };
      if (g) await api(`/admin/aliases/${g.id}`, { method: 'PUT', body });
      else await api('/admin/aliases', { method: 'POST', body: { ...body, local: f.local.value.trim(), domain_id: +f.domain_id.value } });
      snack(g ? 'Group saved' : 'Group created');
      loadDirectory();
      return true;
    },
  });
}

// ---------------------------------------------------------------- domains & DNS
async function domains(pane) {
  const list = await api('/admin/domains');
  const recTable = (d, recs) => html`<div class="tbl-wrap" style="margin-top:12px"><table class="tbl">
    <thead><tr><th>Type</th><th>Name / host</th><th>Value</th><th>Status</th></tr></thead>
    <tbody>${recs.map((r) => html`<tr>
      <td><b>${r.kind}</b><div class="small muted">${r.purpose}</div></td>
      <td><div class="dns-val"><code>${r.host}</code>${r.kind !== 'PTR' ? html`<button class="icon-btn" data-copy="${r.host}" data-tip="Copy" aria-label="Copy">${icon('content_copy')}</button>` : ''}</div></td>
      <td><div class="dns-val"><code>${r.priority ? `${r.priority} ` : ''}${r.value}</code>${r.kind !== 'A' ? html`<button class="icon-btn" data-copy="${r.value}" data-tip="Copy" aria-label="Copy">${icon('content_copy')}</button>` : ''}</div>
        <div class="small muted" style="margin-top:6px;max-width:520px">${r.help}</div>
        ${r.found && r.status !== 'ok' ? html`<div class="small" style="margin-top:4px;color:var(--warning)">Found: ${r.found}</div>` : ''}</td>
      <td>${statusPill(r.status)}</td></tr>`)}</tbody></table></div>`;
  setHTML(pane, html`
    <div class="section mt0" style="display:flex;align-items:center;gap:12px;flex-wrap:wrap">
      <div style="flex:1"><h2 style="margin:0">Domains</h2><p class="lead" style="margin:0">Publish these records at your DNS provider so other servers deliver to you and trust your mail.</p></div>
      <button class="btn outlined" id="add-domain">${icon('add')}Add domain</button>
    </div>
    ${list.map((d) => html`<div class="panel" style="padding:16px 20px;margin-bottom:16px" data-did="${d.id}">
      <div style="display:flex;align-items:center;gap:12px;flex-wrap:wrap">
        ${icon('domain', 'l')}<div style="flex:1"><h2 style="margin:0;font-size:20px;font-weight:500">${d.name}</h2><span class="muted small">${plural(d.users, 'mailbox', 'mailboxes')} · DKIM selector ${d.selector}</span></div>
        <button class="btn tonal" data-check="${d.id}">${icon('verified_user')}Check DNS</button>
        ${list.length > 1 ? html`<button class="icon-btn" data-ddel="${d.id}" data-tip="Remove domain" aria-label="Remove domain">${icon('delete')}</button>` : ''}
      </div>
      <div data-recs="${d.id}">${recTable(d, d.records)}</div>
    </div>`)}
    <div class="banner info">${icon('info', 's')}<span class="grow">DNS changes can take from a few minutes up to a day to spread. Reverse DNS (PTR) is set at your server provider (e.g. Cyfuture, Hetzner, DigitalOcean), not your domain registrar.</span></div>`);
  pane.onclick = async (e) => {
    const c = e.target.closest('[data-copy]');
    if (c) return copyText(c.dataset.copy);
    const chk = e.target.closest('[data-check]');
    if (chk) {
      chk.classList.add('busy');
      try {
        const recs = await api(`/admin/domains/${chk.dataset.check}/check`);
        setHTML(pane.querySelector(`[data-recs="${chk.dataset.check}"]`), recTable(null, recs));
        const bad = recs.filter((r) => r.status !== 'ok').length;
        snack(bad ? `${plural(bad, 'record')} still need attention` : 'All DNS records look good');
      } catch (er) { toastError(er); } finally { chk.classList.remove('busy'); }
      return;
    }
    const del = e.target.closest('[data-ddel]');
    if (del) {
      const d = list.find((x) => x.id === +del.dataset.ddel);
      if (!(await confirmDialog(`Remove ${d.name}?`, 'Only domains without users or groups can be removed.', 'Remove', true))) return;
      try { await api(`/admin/domains/${d.id}`, { method: 'DELETE' }); snack('Domain removed'); domains(pane); } catch (er) { toastError(er); }
    }
  };
  $('#add-domain').onclick = async () => {
    const ok = await dialog({
      title: 'Add a domain', text: 'Receive and send mail for another domain you own, e.g. skienlab.com.',
      body: field({ label: 'Domain name', name: 'dname', attrs: 'autofocus autocapitalize="off" spellcheck="false"' }),
      actions: [{ label: 'Cancel', value: false }, { label: 'Add domain', value: true, kind: 'filled', submit: true }],
      onAction: async (v, d) => {
        if (!v) return true;
        await api('/admin/domains', { method: 'POST', body: { name: d.querySelector('[name=dname]').value.trim() } });
        return true;
      },
    });
    if (ok) { snack('Domain added — publish its DNS records next'); domains(pane); }
  };
}

// ---------------------------------------------------------------- queue
async function queue(pane) {
  const list = await api('/admin/queue');
  const pill = (s) => ({ pending: html`<span class="pill blue">Sending</span>`, deferred: html`<span class="pill amber">Retrying</span>`, sent: html`<span class="pill green">Delivered</span>`, failed: html`<span class="pill red">Failed</span>` }[s]);
  setHTML(pane, html`
    <div class="section mt0" style="display:flex;align-items:center;gap:12px">
      <div style="flex:1"><h2 style="margin:0">Delivery queue</h2><p class="lead" style="margin:0">Mail going to other servers. Temporary failures are retried for up to 4 days; the sender gets a bounce if delivery fails.</p></div>
      <button class="icon-btn" id="q-refresh" data-tip="Refresh" aria-label="Refresh">${icon('refresh')}</button>
    </div>
    ${list.length ? html`<div class="tbl-wrap"><table class="tbl"><thead><tr><th>Recipient</th><th>From</th><th>Status</th><th>Attempts</th><th>Details</th><th></th></tr></thead><tbody>
      ${list.map((q) => html`<tr>
        <td><b>${q.rcpt}</b><div class="small muted ellip">${q.subject || '(no subject)'}</div></td>
        <td class="small">${q.sender || '<> (bounce)'}<div class="muted">${ago(q.created_at)}</div></td>
        <td>${pill(q.status)}</td>
        <td class="num">${q.attempts}</td>
        <td class="small" style="max-width:360px">${q.last_error ? html`<span class="muted">${q.last_error}</span>` : ''}${q.status === 'deferred' ? html`<div class="muted">Next try ${ago(q.next_attempt)}</div>` : ''}</td>
        <td class="num nowrap">${q.status === 'deferred' || q.status === 'pending' ? html`<button class="btn sm" data-retry="${q.id}">Retry now</button><button class="icon-btn sm" data-cancel="${q.id}" data-tip="Cancel" aria-label="Cancel">${icon('close')}</button>` : ''}</td>
      </tr>`)}</tbody></table></div>` : html`<div class="panel"><div class="empty">${icon('check_circle')}<h3>Nothing in the queue</h3><p>All outgoing mail has been delivered.</p></div></div>`}`);
  $('#q-refresh').onclick = () => queue(pane);
  pane.onclick = async (e) => {
    const r = e.target.closest('[data-retry]');
    const c = e.target.closest('[data-cancel]');
    try {
      if (r) { await api(`/admin/queue/${r.dataset.retry}/retry`, { method: 'POST' }); snack('Retrying now'); setTimeout(() => queue(pane), 1500); }
      if (c) {
        if (!(await confirmDialog('Stop delivery?', 'This message will not be delivered to this recipient.', 'Stop delivery', true))) return;
        await api(`/admin/queue/${c.dataset.cancel}/cancel`, { method: 'POST' });
        queue(pane);
      }
    } catch (er) { toastError(er); }
  };
}

// ---------------------------------------------------------------- activity log
async function log(pane, q = '') {
  const list = await api('/admin/log?q=' + encodeURIComponent(q));
  const dir = { in: ['south_west', 'Received'], out: ['north_east', 'Sent'], local: ['sync_alt', 'Internal'], auth: ['lock', 'Sign-in'] };
  const pill = (s) => ({ delivered: 'green', sent: 'green', spam: 'amber', deferred: 'amber', rejected: 'red', failed: 'red' }[s] || 'gray');
  const first = !pane.querySelector('#log-q');
  if (first) {
    setHTML(pane, html`
      <div class="section mt0" style="display:flex;align-items:center;gap:12px;flex-wrap:wrap">
        <div style="flex:1"><h2 style="margin:0">Activity</h2><p class="lead" style="margin:0">Every message received, sent or rejected in the last 30 days, and failed sign-ins.</p></div>
        <div class="search" style="max-width:320px;height:44px">${icon('search')}<input id="log-q" placeholder="Filter by address, subject, status" style="padding-left:6px" value="${q}"></div>
      </div><div id="log-body"></div>`);
    let t;
    $('#log-q').oninput = (e) => { clearTimeout(t); t = setTimeout(() => log(pane, e.target.value), 250); };
  }
  setHTML($('#log-body'), list.length ? html`<div class="tbl-wrap"><table class="tbl"><thead><tr><th>Time</th><th></th><th>From</th><th>To</th><th>Subject</th><th>Status</th></tr></thead><tbody>
    ${list.map((l) => html`<tr title="${l.detail}">
      <td class="small nowrap" title="${fullDate(l.ts)}">${ago(l.ts)}</td>
      <td><span data-tip="${(dir[l.direction] || [])[1] || l.direction}">${icon((dir[l.direction] || ['info'])[0], 's')}</span></td>
      <td class="small ellip">${l.sender || '<>'}</td><td class="small ellip">${l.rcpt}</td>
      <td class="small ellip">${l.subject}${l.detail ? html`<div class="muted ellip">${l.detail}</div>` : ''}</td>
      <td><span class="pill ${pill(l.status)}">${l.status}</span>${l.remote ? html`<div class="small muted">${l.remote}</div>` : ''}</td></tr>`)}
  </tbody></table></div>` : html`<div class="panel"><div class="empty">${icon('receipt_long')}<h3>No activity${q ? ' matches' : ' yet'}</h3></div></div>`);
}

// ---------------------------------------------------------------- server settings
async function settings(pane) {
  const s = await api('/admin/settings');
  setHTML(pane, html`<form id="sfm" autocomplete="off">
    <div class="section mt0"><h2>Organisation</h2>
      <div class="panel">
        ${field({ label: 'Organisation name', name: 'org_name', value: s.org_name })}
        ${field({ label: 'Mail server hostname', name: 'hostname', value: s.hostname, attrs: s.hostname_locked ? 'readonly' : '', hint: s.hostname_locked ? 'Set by the SM_HOSTNAME environment variable.' : 'Must match your MX record, reverse DNS and TLS certificate, e.g. mail.skienlab.com' })}
        <div style="height:12px"></div>
      </div></div>
    <div class="section"><h2>Mailboxes & limits</h2>
      <div class="panel"><div class="row2">
        ${field({ label: 'Default mailbox size for new users (MB)', name: 'default_quota_mb', type: 'number', value: s.default_quota_mb, attrs: 'min="0"', hint: '0 means unlimited' })}
        ${field({ label: 'Largest message (MB)', name: 'max_message_mb', type: 'number', value: s.max_message_mb, attrs: 'min="1" max="100"', hint: 'Including attachments. 25 MB matches Gmail.' })}
        ${field({ label: 'Empty Trash & Spam after (days)', name: 'trash_days', type: 'number', value: s.trash_days, attrs: 'min="0"', hint: '0 keeps them forever' })}
      </div><div style="height:12px"></div></div></div>
    <div class="section"><h2>Spam protection</h2>
      <div class="panel"><div class="row2">
        ${field({ label: 'Spam score threshold', name: 'spam_threshold', type: 'number', value: s.spam_threshold, attrs: 'min="1" max="50" step="0.5"', hint: 'Lower catches more spam. Default 5.' })}
        ${field({ label: 'DNS blocklists', name: 'dnsbl', value: s.dnsbl, hint: 'Space-separated, e.g. zen.spamhaus.org. Leave empty to disable.' })}
      </div><div style="height:12px"></div></div></div>
    <div class="section"><h2>Outgoing relay (optional)</h2>
      <p class="lead">Use this if your provider blocks port 25 (Google Cloud, AWS, Azure…) or you want to send through a service like Amazon SES, Brevo or Mailgun.</p>
      <div class="panel">
        <div class="row2">
          ${field({ label: 'Relay host (empty = deliver directly)', name: 'relay_host', value: s.relay_host, attrs: 'autocapitalize="off" spellcheck="false"' })}
          ${field({ label: 'Port', name: 'relay_port', type: 'number', value: s.relay_port })}
          ${field({ label: 'Username', name: 'relay_user', value: s.relay_user, attrs: 'autocomplete="off"' })}
          ${field({ label: s.relay_pass_set ? 'Password (saved — leave empty to keep)' : 'Password', name: 'relay_pass', type: 'password', attrs: 'autocomplete="new-password"' })}
        </div>
        ${selectField({ label: 'Encryption', name: 'relay_tls', value: s.relay_tls, options: [{ value: 'starttls', label: 'STARTTLS (usually port 587)' }, { value: 'tls', label: 'SSL/TLS (usually port 465)' }] })}
        <div style="height:12px"></div>
      </div></div>
    <div class="form-actions" style="position:sticky;bottom:0;background:var(--surface);padding:12px 0"><button class="btn filled" type="submit">Save settings</button></div>
  </form>`);
  $('#sfm').onsubmit = async (e) => {
    e.preventDefault();
    const f = e.target;
    const body = {};
    ['org_name', 'hostname', 'default_quota_mb', 'max_message_mb', 'trash_days', 'spam_threshold', 'dnsbl', 'relay_host', 'relay_port', 'relay_user', 'relay_tls'].forEach((k) => { body[k] = f[k].value; });
    if (f.relay_pass.value) body.relay_pass = f.relay_pass.value;
    if (s.hostname_locked) delete body.hostname;
    const btn = f.querySelector('[type=submit]');
    btn.classList.add('busy');
    try {
      await api('/admin/settings', { method: 'PUT', body });
      S.me.org.name = body.org_name;
      if (body.hostname) S.me.org.hostname = body.hostname;
      snack('Settings saved');
    } catch (er) { toastError(er); } finally { btn.classList.remove('busy'); }
  };
}

