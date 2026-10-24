// Sign-in, first-run organisation setup, and forced password change screens.
import { $, html, icon, setHTML, api, field, passwordToggle, snack, dialog } from './lib.js';
import { setTheme, appName, iconURL, applyBrand } from './state.js';
import { beforeSignOut } from './push.js';
import { colourPicker, wireColourPicker, markSVG } from './brandui.js';

const COPYRIGHT = '© 2026 Skien Software Labs';

function themeToggle() {
  return html`<button type="button" id="auth-theme">${icon('dark_mode', 's')} Theme</button>`;
}
function wireFoot(root) {
  const b = root.querySelector('#auth-theme');
  if (b) b.onclick = () => setTheme(document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark');
}
function showError(root, msg) {
  const e = root.querySelector('.form-err');
  if (e) e.innerHTML = msg ? String(html`${icon('error', 'xs')}<span>${msg}</span>`) : '';
}
function busy(btn, on) { btn.classList.toggle('busy', on); }

// ---------------------------------------------------------------- sign in
// The welcome scene beside the sign-in form: a small inbox that receives a message, notifies,
// stars one and files another into a folder. Decorative (hidden from screen readers); the
// motion plays once and is skipped entirely for people who prefer reduced motion.
function welcomeScene(orgName) {
  const row = (cls, initial, name, subject, extra = '') => html`<div class="wl-row ${cls}">
    <span class="wl-av">${initial}</span>
    <span class="wl-tx"><b>${name}</b><span>${subject}</span></span>${extra}</div>`;
  return html`<section class="wl-scene" aria-labelledby="wl-title">
    <div class="wl-brand"><img src="${iconURL()}" alt="" width="32" height="32"><span>${appName()}</span></div>
    <div class="wl-stage" aria-hidden="true">
      <div class="wl-inbox">
        <div class="wl-bar"><span>Inbox</span><span class="wl-badge">3 new</span></div>
        ${row('r-new', 'R', 'Ravi Menon', 'Quarterly report is ready', html`<span class="wl-dot"></span>`)}
        ${row('r-star', 'P', 'Priya Nair', 'Notes from the design review', html`<span class="wl-star">${icon('star-f', 's')}</span>`)}
        ${row('r-file', 'A', 'Acme Ltd', 'Invoice 2041 for October', html`<span class="wl-chip">${icon('filter_alt', 'xs')}Clients/Acme</span>`)}
        ${row('r-old', 'C', 'Build bot', 'All checks passed on main')}
        <div class="wl-note">${icon('filter_alt', 'xs')}Filed 1 message to Clients/Acme</div>
      </div>
      <div class="wl-folders">
        <span class="wl-folder f-acme">${icon('folder', 'xs')}Clients/Acme</span>
        <span class="wl-folder">${icon('folder', 'xs')}Invoices</span>
        <span class="wl-folder">${icon('folder', 'xs')}Projects</span>
      </div>
      <div class="wl-toast"><img src="${iconURL()}" alt="" width="28" height="28">
        <span><b>Ravi Menon</b><span>Quarterly report is ready</span></span></div>
    </div>
    <div class="wl-copy">
      <h2 id="wl-title">Mail that finds its own place.</h2>
      <p>${orgName ? html`${orgName} mail` : 'Your mail'} on the web, on your phone and in the mail apps you already use. Filters sort it as it arrives, and new mail reaches you even when the app is closed.</p>
    </div>
  </section>`;
}

export async function renderLogin(root, done, notice = '') {
  let org = '';
  try {
    const st = await api('/setup', { background: true });
    applyBrand(st.brand);
    org = st.org || '';
  } catch { /* ignore */ }
  document.title = `Sign in – ${appName()}`;
  const orgName = org && org !== appName() ? org : '';
  setHTML(root, html`<div class="welcome">
    ${welcomeScene(orgName)}
    <main class="wl-side">
      <div class="wl-form">
      <div class="wl-head">
        <img class="logo" src="${iconURL()}" alt="">
        <h1>Sign in</h1>
        <p class="lead">to continue to <b>${appName()}</b></p>
      </div>
      <form id="login" novalidate>
        ${field({ label: 'Email or username', name: 'email', type: 'text', attrs: 'autocomplete="username" autocapitalize="off" spellcheck="false" autofocus', required: true })}
        ${field({ label: 'Password', name: 'password', type: 'password', attrs: 'autocomplete="current-password"', required: true,
          suffix: html`<button type="button" class="icon-btn" data-pw-toggle="f-password" data-tip="Show password" aria-label="Show password">${icon('visibility')}</button>` })}
        <label class="check"><input type="checkbox" class="cb" name="remember" checked> Stay signed in on this device</label>
        ${notice ? html`<div class="banner info" role="status" style="margin-top:8px">${icon('info', 's')}<span class="grow">${notice}</span></div>` : ''}
        <div class="form-err" role="alert"></div>
        <div class="auth-actions">
          <button type="button" class="btn left" id="forgot">Forgot password?</button>
          <button type="submit" class="btn filled" id="login-btn">Next</button>
        </div>
      </form>
      </div>
      <div class="auth-foot"><span class="copy">${COPYRIGHT}</span>${themeToggle()}</div>
    </main>
  </div>`);
  wireFoot(root);
  passwordToggle(root);
  const form = $('#login', root);
  $('#forgot', root).onclick = () => dialog({
    iconName: 'lock', title: 'Forgot your password?',
    text: 'Ask your administrator to reset it. They can set a temporary password from the Admin console, and you will choose a new one when you sign in.',
  });
  form.onsubmit = async (e) => {
    e.preventDefault();
    const email = form.email.value.trim();
    const password = form.password.value;
    if (!email) return showError(root, 'Enter your email address');
    if (!password) return showError(root, 'Enter your password');
    const btn = $('#login-btn', root);
    busy(btn, true);
    showError(root, '');
    try {
      await api('/auth/login', { method: 'POST', body: { email, password, remember: form.remember.checked } });
      if (location.hash.startsWith('#/settings') || !location.hash) history.replaceState(null, '', '#/inbox');
      done();
    } catch (err) {
      showError(root, err.message);
      form.password.select();
    } finally {
      busy(btn, false);
    }
  };
}

// ---------------------------------------------------------------- first-run setup
const DEFAULT_PRESETS = ['#4453f2', '#0b57d0', '#0f766e', '#15803d', '#b45309', '#c2410c', '#be123c', '#7c3aed', '#334155'];

export function renderSetup(root, done, st = {}) {
  const presets = Array.isArray(st.presets) && st.presets.length ? st.presets : DEFAULT_PRESETS;
  const data = {
    org_name: '', domain: '', hostname: location.hostname === 'localhost' ? '' : location.hostname,
    app_name: '', accent: st.brand?.accent || presets[0],
    name: '', local: '', password: '', confirm: '', token: '',
  };
  let nameEdited = false; // the app name follows the organisation name until it is typed in
  let vars = st.brand?.vars || null;
  // the same rules as the server: "<organisation> Mails", or the organisation's name when that's too long
  const chars = (s) => [...s].length;
  const suggested = () => {
    const org = data.org_name.trim().replace(/\s+/g, ' ');
    if (!org) return '';
    return chars(`${org} Mails`) <= 40 ? `${org} Mails` : [...org].slice(0, 40).join('').trim();
  };
  let step = 0;

  const steps = [
    {
      title: 'Your organisation',
      body: () => html`
        ${field({ label: 'Organisation name', name: 'org_name', value: data.org_name, attrs: 'autofocus autocomplete="organization" maxlength="100"' })}
        ${field({ label: 'Email domain', name: 'domain', value: data.domain, hint: 'The part after @ in everyone’s address — for example example.com', attrs: 'autocapitalize="off" spellcheck="false"' })}
        ${field({ label: 'Web address of this server', name: 'hostname', value: data.hostname, hint: 'Where people open the web app, and the name mail servers connect to — for example mail.example.com', attrs: 'autocapitalize="off" spellcheck="false"' })}`,
      check: () => {
        data.domain = data.domain.toLowerCase().replace(/^@/, '').trim();
        data.hostname = data.hostname.toLowerCase().replace(/^https?:\/\//, '').replace(/\/.*$/, '').trim();
        if (!data.org_name.trim()) return 'Enter your organisation name';
        if (!/^([a-z0-9-]+\.)+[a-z]{2,}$/.test(data.domain)) return 'Enter a domain like example.com';
        if (!/^([a-z0-9-]+\.)+[a-z]{2,}$/.test(data.hostname)) return 'Enter a web address like mail.example.com';
        if (!nameEdited) data.app_name = suggested();
        return '';
      },
    },
    {
      title: 'Name and colour',
      body: () => html`
        <p class="muted mt0">This is what everyone sees: on the sign-in page, in the browser tab, on the app icon and in notifications. You can change both later in Admin console › Server settings.</p>
        ${field({ label: 'App name', name: 'app_name', value: data.app_name, attrs: 'autofocus maxlength="40" autocomplete="off"', hint: 'For example “Softop Mails”' })}
        <div class="fld-label">Main colour</div>
        ${colourPicker(data.accent, presets)}
        <div class="brand-preview" id="brand-preview">${previewCard()}</div>`,
      check: () => {
        data.app_name = data.app_name.replace(/\s+/g, ' ').trim();
        if (!data.app_name) return 'Enter a name for the app';
        if (chars(data.app_name) > 40) return 'Keep the app name to 40 characters or fewer';
        if (/[<>"\\/]|[\p{Cc}\p{Cf}]/u.test(data.app_name)) return 'The app name can’t contain < > " / or \\';
        if (!/^#[0-9a-f]{6}$/i.test(data.accent)) return 'Choose a main colour';
        return '';
      },
      wire: (form) => {
        const nm = form.querySelector('[name=app_name]');
        nm.addEventListener('input', () => { nameEdited = true; data.app_name = nm.value; repaintPreview(); });
        wireColourPicker(form, (c, v) => { data.accent = c; vars = v; repaintPreview(); });
      },
    },
    {
      title: 'Your administrator account',
      body: () => html`
        ${field({ label: 'Your full name', name: 'name', value: data.name, attrs: 'autofocus autocomplete="name"' })}
        ${field({ label: 'Username', name: 'local', value: data.local, addon: '@' + data.domain, attrs: 'autocapitalize="off" spellcheck="false" autocomplete="off"' })}
        ${field({ label: 'Password', name: 'password', type: 'password', value: data.password, attrs: 'autocomplete="new-password"', hint: 'At least 8 characters. Use a long passphrase — this account manages everyone’s mail.',
          suffix: html`<button type="button" class="icon-btn" data-pw-toggle="f-password" aria-label="Show password">${icon('visibility')}</button>` })}
        ${field({ label: 'Confirm password', name: 'confirm', type: 'password', value: data.confirm, attrs: 'autocomplete="new-password"' })}`,
      check: () => {
        data.local = data.local.toLowerCase().trim();
        if (!data.name.trim()) return 'Enter your name';
        if (!/^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$/.test(data.local)) return 'Use letters, numbers, dots, dashes or underscores for the username';
        if (data.password.length < 8) return 'Use at least 8 characters for the password';
        if (data.password !== data.confirm) return "Passwords don't match";
        return '';
      },
    },
    {
      title: 'Confirm it’s you',
      body: () => html`
        <p class="muted mt0">To make sure only the server owner can create the organisation, enter the setup code printed in the server log.</p>
        <div class="cred-box">docker logs skienmail 2>&1 | grep -i "setup code"</div>
        ${field({ label: 'Setup code', name: 'token', value: data.token, attrs: 'autofocus autocapitalize="characters" spellcheck="false" autocomplete="one-time-code"' })}
        <div class="panel summary">
          <span class="summary-mark">${markSVG(vars, 36)}</span>
          <div><div><b>${data.app_name}</b> for ${data.org_name}</div>
          <div class="small muted">${data.local}@${data.domain} · ${data.hostname}</div></div>
        </div>`,
      check: () => (data.token.trim() ? '' : 'Enter the setup code'),
    },
  ];

  function previewCard() {
    return html`${markSVG(vars, 40)}<div class="bp-text"><b>${data.app_name || 'Your app name'}</b><span>Sign in to continue</span></div>
      <span class="bp-btn">Next</span>`;
  }
  function repaintPreview() {
    const p = $('#brand-preview', root);
    if (p) setHTML(p, previewCard());
    const m = $('#setup-mark', root);
    if (m) setHTML(m, markSVG(vars, 40));
  }

  const paint = () => {
    const s = steps[step];
    document.title = `Set up – ${data.app_name || appName()}`;
    setHTML(root, html`<div class="auth"><div>
      <div class="auth-card">
        <div>
          <span id="setup-mark">${markSVG(vars, 40)}</span>
          <h1>Set up your mail server</h1>
          <p class="lead">Email for your organisation, on your own server. It takes about a minute.</p>
          <div class="steps">${steps.map((_, i) => html`<i class="${i <= step ? 'on' : ''}"></i>`)}</div>
          <ul class="checklist">
            <li>${icon('check_circle', 's')}<span>Mailboxes like <b>name@${data.domain || 'yourdomain.com'}</b> for every employee</span></li>
            <li>${icon('check_circle', 's')}<span>Signed (DKIM) and verified (SPF, DMARC) mail out of the box</span></li>
            <li>${icon('check_circle', 's')}<span>Your name and colour, in the browser and as an app on computers and phones</span></li>
          </ul>
        </div>
        <form id="setup" novalidate>
          <p class="step-title">Step ${step + 1} of ${steps.length}</p>
          <h2 style="font-weight:400;font-size:24px;margin:0 0 8px">${s.title}</h2>
          ${s.body()}
          <div class="form-err" role="alert"></div>
          <div class="auth-actions">
            ${step > 0 ? html`<button type="button" class="btn left" id="back">Back</button>` : ''}
            <button type="submit" class="btn filled" id="next">${step === steps.length - 1 ? 'Create organisation' : 'Next'}</button>
          </div>
        </form>
      </div>
      <div class="auth-foot"><span class="copy">${COPYRIGHT}</span>${themeToggle()}</div>
    </div></div>`);
    wireFoot(root);
    passwordToggle(root);
    const form = $('#setup', root);
    form.querySelectorAll('input:not([type=color]):not([type=hidden])').forEach((i) => i.addEventListener('input', () => { data[i.name] = i.value; }));
    s.wire?.(form);
    const first = form.querySelector('[autofocus]');
    first?.focus();
    $('#back', root)?.addEventListener('click', () => { collect(form); step--; paint(); });
    form.onsubmit = async (e) => {
      e.preventDefault();
      collect(form);
      const err = s.check();
      if (err) return showError(root, err);
      if (step < steps.length - 1) { step++; return paint(); }
      const btn = $('#next', root);
      busy(btn, true);
      try {
        const { confirm, ...body } = data;
        await api('/setup', { method: 'POST', body: { ...body, token: data.token.trim() } });
        history.replaceState(null, '', '#/admin/domains');
        snack('Your organisation is ready. Next: publish the DNS records below.', { timeout: 9000 });
        done();
      } catch (er) {
        showError(root, er.message);
      } finally {
        busy(btn, false);
      }
    };
  };
  const collect = (form) => {
    form.querySelectorAll('input[name]:not([type=color])').forEach((i) => { if (i.name in data) data[i.name] = i.value; });
  };
  paint();
}

// ---------------------------------------------------------------- forced password change
export function renderForceChange(root, me, done) {
  document.title = `Choose a password – ${appName()}`;
  setHTML(root, html`<div class="auth"><div>
    <div class="auth-card narrow">
      <div>
        <img class="logo" src="${iconURL()}" alt="">
        <h1 style="font-size:30px">Choose a new password</h1>
        <p class="lead">Hi ${me.user.name.split(' ')[0]} — welcome to ${me.org.name}. Replace the temporary password you were given with one only you know.</p>
      </div>
      <form id="pw" novalidate>
        <input type="email" name="username" value="${me.user.email}" autocomplete="username" hidden>
        ${field({ label: 'Temporary password', name: 'current', type: 'password', attrs: 'autocomplete="current-password" autofocus' })}
        ${field({ label: 'New password', name: 'new', type: 'password', attrs: 'autocomplete="new-password"', hint: 'At least 8 characters',
          suffix: html`<button type="button" class="icon-btn" data-pw-toggle="f-new" aria-label="Show password">${icon('visibility')}</button>` })}
        ${field({ label: 'Confirm new password', name: 'confirm', type: 'password', attrs: 'autocomplete="new-password"' })}
        <div class="form-err" role="alert"></div>
        <div class="auth-actions">
          <button type="button" class="btn left" id="signout">Sign out</button>
          <button type="submit" class="btn filled" id="save">Save password</button>
        </div>
      </form>
    </div>
    <div class="auth-foot"><span>${me.user.email} · ${COPYRIGHT}</span>${themeToggle()}</div>
  </div></div>`);
  wireFoot(root);
  passwordToggle(root);
  const form = $('#pw', root);
  $('#signout', root).onclick = async () => {
    try { await beforeSignOut(); await api('/auth/logout', { method: 'POST' }); } catch { /* ignore */ }
    renderLogin(root, done);
  };
  form.onsubmit = async (e) => {
    e.preventDefault();
    if (form.new.value.length < 8) return showError(root, 'Use at least 8 characters');
    if (form.new.value !== form.confirm.value) return showError(root, "Passwords don't match");
    const btn = $('#save', root);
    busy(btn, true);
    try {
      await api('/profile/password', { method: 'POST', body: { current: form.current.value, new: form.new.value } });
      snack('Password saved. Welcome aboard!');
      done();
    } catch (er) {
      showError(root, er.message);
    } finally {
      busy(btn, false);
    }
  };
}

