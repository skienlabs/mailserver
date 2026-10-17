// Sign-in, first-run organisation setup, and forced password change screens.
import { $, html, icon, setHTML, api, field, passwordToggle, snack, dialog } from './lib.js';
import { setTheme } from './state.js';

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
export async function renderLogin(root, done) {
  let org = '';
  try { org = (await api('/setup', { background: true })).org; } catch { /* ignore */ }
  document.title = 'Sign in – Skien Mails';
  setHTML(root, html`<div class="auth"><div>
    <div class="auth-card">
      <div>
        <img class="logo" src="/icons/favicon.svg" alt="">
        <h1>Sign in</h1>
        <p class="lead">to continue to ${org && org !== 'Skien Mails' ? html`<b>${org}</b> mail` : 'Skien Mails'}</p>
      </div>
      <form id="login" novalidate>
        ${field({ label: 'Email or username', name: 'email', type: 'text', attrs: 'autocomplete="username" autocapitalize="off" spellcheck="false" autofocus', required: true })}
        ${field({ label: 'Password', name: 'password', type: 'password', attrs: 'autocomplete="current-password"', required: true,
          suffix: html`<button type="button" class="icon-btn" data-pw-toggle="f-password" data-tip="Show password" aria-label="Show password">${icon('visibility')}</button>` })}
        <label class="check"><input type="checkbox" class="cb" name="remember" checked> Stay signed in on this device</label>
        <div class="form-err" role="alert"></div>
        <div class="auth-actions">
          <button type="button" class="btn left" id="forgot">Forgot password?</button>
          <button type="submit" class="btn filled" id="login-btn">Next</button>
        </div>
      </form>
    </div>
    <div class="auth-foot"><span>Skien Mails</span>${themeToggle()}</div>
  </div></div>`);
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
export function renderSetup(root, done) {
  document.title = 'Set up – Skien Mails';
  const data = { org_name: '', domain: '', hostname: location.hostname === 'localhost' ? '' : location.hostname, name: '', local: '', password: '', confirm: '', token: '' };
  let step = 0;

  const steps = [
    {
      title: 'Your organisation',
      body: () => html`
        ${field({ label: 'Organisation name', name: 'org_name', value: data.org_name, attrs: 'autofocus autocomplete="organization"' })}
        ${field({ label: 'Email domain', name: 'domain', value: data.domain, hint: 'The part after @ in everyone’s address — for example skien.com', attrs: 'autocapitalize="off" spellcheck="false"' })}
        ${field({ label: 'Web address of this server', name: 'hostname', value: data.hostname, hint: 'Where people open Skien Mails, and the name mail servers connect to — for example mail.skienlab.com', attrs: 'autocapitalize="off" spellcheck="false"' })}`,
      check: () => {
        data.domain = data.domain.toLowerCase().replace(/^@/, '').trim();
        data.hostname = data.hostname.toLowerCase().replace(/^https?:\/\//, '').replace(/\/.*$/, '').trim();
        if (!data.org_name.trim()) return 'Enter your organisation name';
        if (!/^([a-z0-9-]+\.)+[a-z]{2,}$/.test(data.domain)) return 'Enter a domain like skien.com';
        if (!/^([a-z0-9-]+\.)+[a-z]{2,}$/.test(data.hostname)) return 'Enter a web address like mail.skienlab.com';
        return '';
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
        <div class="cred-box">docker logs skienmail 2>&1 | grep "setup code"</div>
        ${field({ label: 'Setup code', name: 'token', value: data.token, attrs: 'autofocus autocapitalize="characters" spellcheck="false" autocomplete="one-time-code"' })}
        <div class="panel" style="margin-top:16px;padding:12px 16px">
          <div class="small muted">You're creating</div>
          <div><b>${data.org_name}</b> · ${data.local}@${data.domain}</div>
          <div class="small muted">Web address: ${data.hostname}</div>
        </div>`,
      check: () => (data.token.trim() ? '' : 'Enter the setup code'),
    },
  ];

  const paint = () => {
    const s = steps[step];
    setHTML(root, html`<div class="auth"><div>
      <div class="auth-card">
        <div>
          <img class="logo" src="/icons/favicon.svg" alt="">
          <h1>Welcome to Skien Mails</h1>
          <p class="lead">Let's set up email for your organisation. It takes about a minute.</p>
          <div class="steps">${steps.map((_, i) => html`<i class="${i <= step ? 'on' : ''}"></i>`)}</div>
          <ul class="checklist">
            <li>${icon('check_circle', 's')} Mailboxes like <b>name@${data.domain || 'yourdomain.com'}</b> for every employee</li>
            <li>${icon('check_circle', 's')} Signed (DKIM) and verified (SPF, DMARC) mail out of the box</li>
            <li>${icon('check_circle', 's')} Works in the browser and installs as an app on Windows, Mac and phones</li>
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
      <div class="auth-foot"><span>Skien Mails</span>${themeToggle()}</div>
    </div></div>`);
    wireFoot(root);
    passwordToggle(root);
    const form = $('#setup', root);
    form.querySelectorAll('input').forEach((i) => i.addEventListener('input', () => { data[i.name] = i.value; }));
    const first = form.querySelector('[autofocus]');
    first?.focus();
    $('#back', root)?.addEventListener('click', () => { step--; paint(); });
    form.onsubmit = async (e) => {
      e.preventDefault();
      form.querySelectorAll('input').forEach((i) => { data[i.name] = i.value; });
      const err = s.check();
      if (err) return showError(root, err);
      if (step < steps.length - 1) { step++; return paint(); }
      const btn = $('#next', root);
      busy(btn, true);
      try {
        await api('/setup', { method: 'POST', body: { ...data, token: data.token.trim() } });
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
  paint();
}

// ---------------------------------------------------------------- forced password change
export function renderForceChange(root, me, done) {
  document.title = 'Choose a password – Skien Mails';
  setHTML(root, html`<div class="auth"><div>
    <div class="auth-card narrow">
      <div>
        <img class="logo" src="/icons/favicon.svg" alt="">
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
    <div class="auth-foot"><span>${me.user.email}</span>${themeToggle()}</div>
  </div></div>`);
  wireFoot(root);
  passwordToggle(root);
  const form = $('#pw', root);
  $('#signout', root).onclick = async () => {
    try { await api('/auth/logout', { method: 'POST' }); } catch { /* ignore */ }
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

