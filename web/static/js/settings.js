// Personal settings: profile, appearance, notifications, security, out-of-office, email apps.
import { $, html, raw, icon, setHTML, api, upload, snack, avatar, ago, field, passwordToggle, toastError, squareImage, confirmDialog, dateOnly, copyText } from './lib.js';
import { S, setTheme, themePref, navigate, pref, loadDirectory } from './state.js';
import { cleanForEditor } from './compose.js';

const TABS = [
  ['general', 'person', 'General'],
  ['security', 'lock', 'Security'],
  ['vacation', 'beach_access', 'Out of office'],
  ['apps', 'devices', 'Email apps'],
];

export function render(viewEl, r) {
  const tab = TABS.some((t) => t[0] === r.tab) ? r.tab : 'general';
  setHTML(viewEl, html`
    <div class="page-head"><div class="grow"><h1>Settings</h1><p class="sub">${S.me.user.email}</p></div></div>
    <nav class="tabs">${TABS.map(([id, ic, label]) => html`<a class="tab ${tab === id ? 'on' : ''}" href="#/settings/${id}">${icon(ic, 's')}${label}</a>`)}</nav>
    <div class="page"><div class="page-inner" id="pane"></div></div>`);
  const pane = $('#pane');
  ({ general, security, vacation, apps })[tab](pane);
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
            <div class="field" style="margin-top:0"><div id="sig" class="editor" contenteditable="true" role="textbox" aria-multiline="true" aria-label="Signature" dir="auto" data-ph="e.g. Asi K · Skienlab · +91 98765 43210"
              style="border:1px solid var(--outline);border-radius:6px;padding:12px 16px;min-height:96px">${raw(cleanForEditor(u.signature))}</div></div>
          </div>
          <div class="form-actions"><button class="btn filled" type="submit">Save profile</button></div>
        </form>
      </div>
    </div>
    <div class="section">
      <h2>Appearance</h2><p class="lead">Choose how Skien Mails looks on this device.</p>
      <div class="panel" style="padding:16px 20px">
        <div class="seg" role="radiogroup" aria-label="Theme">
          ${[['light', 'light_mode', 'Light'], ['dark', 'dark_mode', 'Dark'], ['system', 'desktop_windows', 'System']].map(([v, ic, l]) => html`<button class="${theme === v ? 'on' : ''}" data-theme-pick="${v}" role="radio" aria-checked="${theme === v}">${icon(ic, 's')}${l}</button>`)}
        </div>
      </div>
    </div>
    <div class="section">
      <h2>Notifications</h2>
      <div class="panel" style="padding:4px 20px">
        <label class="kv check" style="width:100%"><span class="v"><b>Desktop notifications</b><br><span class="muted small">Get a notification when new mail arrives while Skien Mails is in the background.</span></span>
          <input type="checkbox" class="switch" id="notify" ${notifyOn ? raw('checked') : ''}></label>
      </div>
    </div>
    <div class="section">
      <h2>Install the app</h2>
      <div class="panel" style="padding:16px 20px">
        <p class="mt0">Use Skien Mails like a desktop app — its own window, taskbar icon, unread badge, and it opens <b>mailto:</b> links.</p>
        ${S.installPrompt ? html`<button class="btn filled" id="install">${icon('install_desktop')}Install Skien Mails</button>`
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
  $('#install')?.addEventListener('click', async () => {
    const p = S.installPrompt;
    S.installPrompt = null;
    p.prompt();
    await p.userChoice;
    navigate('#/settings/general', { replace: true });
  });
}

// ---------------------------------------------------------------- security
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
  const host = S.me.org.hostname;
  const row = (k, v, copy) => html`<div class="kv"><span class="k">${k}</span><span class="v">${v}</span>${copy ? html`<button class="icon-btn sm" data-copy="${v}" data-tip="Copy" aria-label="Copy">${icon('content_copy')}</button>` : ''}</div>`;
  setHTML(pane, html`
    <div class="section">
      <h2>Send from other apps</h2>
      <p class="lead">Printers, scanners, websites and scripts can send mail as you through these settings. Use your full email address and password.</p>
      <div class="panel">
        ${row('Server', host, true)}
        ${row('Port', '587 (STARTTLS)  or  465 (SSL/TLS)')}
        ${row('Username', S.me.user.email, true)}
        ${row('Password', 'Your Skien Mails password')}
        ${row('Authentication', 'Normal password (PLAIN / LOGIN)')}
      </div>
    </div>
    <div class="section">
      <h2>Read mail on your phone and computer</h2>
      <div class="panel" style="padding:16px 20px">
        <p class="mt0">Open <b>https://${host}</b> and install Skien Mails as an app — it works on Windows, macOS, Android and iPhone, stays signed in, and shows new-mail notifications.</p>
        <a class="btn tonal" href="#/settings/general">${icon('install_desktop')}Install options</a>
      </div>
    </div>`);
  pane.querySelectorAll('[data-copy]').forEach((b) => (b.onclick = () => copyText(b.dataset.copy)));
}
