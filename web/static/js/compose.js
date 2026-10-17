// Compose window: recipients with autocomplete, rich text, attachments, autosaved drafts and undo-send.
import { html, raw, icon, el, setHTML, api, upload, snack, dialog, avatar, bytes, fullDate, debounce, toastError, isEmail,
  confirmDialog, esc, fileIcon, people } from './lib.js';
import { S, navigate, refreshCounts } from './state.js';

let win = null; // the open compose window (one at a time)

const reAngle = /^\s*"?([^"<]*?)"?\s*<\s*([^>\s]+)\s*>\s*$/;
export function parseAddress(s) {
  s = String(s).trim().replace(/^mailto:/i, '');
  if (!s) return null;
  const m = s.match(reAngle);
  if (m) return { name: m[1].trim(), email: m[2].toLowerCase() };
  return { name: '', email: s.toLowerCase() };
}
function splitAddresses(text) {
  // split on commas/semicolons/newlines that are not inside quotes or angle brackets
  const out = [];
  let cur = '', q = false, a = false;
  for (const c of text) {
    if (c === '"') q = !q;
    if (c === '<') a = true;
    if (c === '>') a = false;
    if (!q && !a && /[,;\n\t]/.test(c)) { if (cur.trim()) out.push(cur); cur = ''; continue; }
    cur += c;
  }
  if (cur.trim()) out.push(cur);
  // also allow space-separated plain addresses
  return out.flatMap((x) => (!x.includes('<') && !x.includes('"') && x.trim().includes(' ') ? x.trim().split(/\s+/) : [x])).map(parseAddress).filter(Boolean);
}

export function parseMailto(href) {
  const s = String(href || '').replace(/^web\+mailto:/i, '').replace(/^mailto:/i, '');
  const [addr, qs = ''] = s.split('?');
  const p = new URLSearchParams(qs);
  const get = (k) => { for (const [kk, v] of p) if (kk.toLowerCase() === k) return v; return ''; };
  const list = (v) => (v ? splitAddresses(decodeURIComponent(v)) : []);
  return {
    to: list(addr).concat(list(get('to'))),
    cc: list(get('cc')),
    bcc: list(get('bcc')),
    subject: get('subject'),
    html: get('body') ? esc(get('body')).replace(/\n/g, '<br>') : '',
  };
}

const hasPrefix = (subject, p) => new RegExp(`^\\s*(${p})\\s*:`, 'i').test(subject || '');
const nameOf = (a) => (a.name ? `${a.name} <${a.email}>` : a.email);

// Quoted mail is inserted into the editor, which lives in the app page rather than the
// sandboxed reader, so drop anything that could restyle the app (style sheets, classes).
const KEEP_CLASSES = new Set(['sig', 'quote', 'fwd']);
//
// With allowRemote=false, remote images are parked in data-sm-* attributes so quoting a
// message doesn't load its trackers in the editor; restoreRemote() puts them back before
// the draft is saved or sent, so recipients still get the full quote.
const REMOTE = /^\s*(https?:)?\/\//i;
const URL_IN_STYLE = /url\(\s*['"]?\s*(https?:)?\/\/[^)]*\)/gi;
export function cleanForEditor(markup, { allowRemote = true } = {}) {
  const doc = new DOMParser().parseFromString(`<body>${markup || ''}</body>`, 'text/html'); // inert: nothing runs or loads
  doc.querySelectorAll('style, link, meta, base, title, script, iframe, object, embed, form, input, button, textarea, select').forEach((n) => n.remove());
  doc.querySelectorAll('[class]').forEach((n) => {
    const keep = [...n.classList].filter((c) => KEEP_CLASSES.has(c));
    if (keep.length) n.className = keep.join(' '); else n.removeAttribute('class');
  });
  doc.querySelectorAll('[id]').forEach((n) => n.removeAttribute('id'));
  if (!allowRemote) {
    doc.querySelectorAll('img').forEach((img) => {
      const src = img.getAttribute('src') || '';
      if (REMOTE.test(src)) { img.setAttribute('data-sm-src', src); img.removeAttribute('src'); }
      if (img.hasAttribute('srcset')) { img.setAttribute('data-sm-srcset', img.getAttribute('srcset')); img.removeAttribute('srcset'); }
    });
    doc.querySelectorAll('[background]').forEach((n) => {
      if (REMOTE.test(n.getAttribute('background'))) { n.setAttribute('data-sm-bg', n.getAttribute('background')); n.removeAttribute('background'); }
    });
    doc.querySelectorAll('[style]').forEach((n) => {
      const st = n.getAttribute('style');
      if (URL_IN_STYLE.test(st)) { n.setAttribute('data-sm-style', st); n.setAttribute('style', st.replace(URL_IN_STYLE, 'none')); }
      URL_IN_STYLE.lastIndex = 0;
    });
  }
  return doc.body.innerHTML;
}

export function restoreRemote(markup) {
  if (!/data-sm-/.test(markup)) return markup;
  const doc = new DOMParser().parseFromString(`<body>${markup}</body>`, 'text/html');
  const back = (attr, to) => doc.querySelectorAll(`[${attr}]`).forEach((n) => { n.setAttribute(to, n.getAttribute(attr)); n.removeAttribute(attr); });
  back('data-sm-src', 'src');
  back('data-sm-srcset', 'srcset');
  back('data-sm-bg', 'background');
  back('data-sm-style', 'style');
  return doc.body.innerHTML;
}

function signatureHTML() {
  const sig = S.me.user.signature;
  return sig ? `<br><div class="sig">-- <br>${sig}</div>` : '';
}

function quoteHTML(m) {
  const body = m.html || esc(m.text || '').replace(/\n/g, '<br>');
  return `<br><div class="quote">On ${esc(fullDate(m.date))}, ${esc(m.from.name || m.from.email)} &lt;${esc(m.from.email)}&gt; wrote:<br>` +
    `<blockquote style="margin:0 0 0 .8ex;border-left:1px solid #ccc;padding-left:1ex">${body}</blockquote></div>`;
}

function forwardHTML(m) {
  const body = m.html || esc(m.text || '').replace(/\n/g, '<br>');
  const list = (l) => (l || []).map((a) => esc(nameOf(a))).join(', ');
  return `<br><div class="fwd">---------- Forwarded message ---------<br>From: <b>${esc(m.from.name || '')}</b> &lt;${esc(m.from.email)}&gt;<br>` +
    `Date: ${esc(fullDate(m.date))}<br>Subject: ${esc(m.subject)}<br>To: ${list(m.to)}<br>${m.cc?.length ? `Cc: ${list(m.cc)}<br>` : ''}<br>${body}</div>`;
}

/** Builds the initial state for a new message, reply or forward. */
function initialState(o) {
  const me = S.me.user.email.toLowerCase();
  const myAddrs = new Set(S.me.send_as.map((s) => s.email.toLowerCase()));
  const st = { to: o.to || [], cc: o.cc || [], bcc: o.bcc || [], subject: o.subject || '', html: o.html || '', files: [], from: S.me.user.email,
    replyToId: 0, draftId: o.draftId || 0, title: 'New message' };
  const m = o.source;
  // Quoted mail only loads remote images when the reader would show them automatically.
  st.allowRemote = !m || people.has(m.from.email) || myAddrs.has(m.from.email);
  if (m && (o.mode === 'reply' || o.mode === 'replyall')) {
    const fromMe = myAddrs.has(m.from.email);
    if (fromMe) {
      st.to = [...(m.to || [])];
      if (o.mode === 'replyall') st.cc = [...(m.cc || [])];
    } else {
      st.to = [m.reply_to ? { email: m.reply_to, name: '' } : m.from];
      if (o.mode === 'replyall') {
        const seen = new Set([...st.to.map((a) => a.email), ...myAddrs, me]);
        st.cc = [...(m.to || []), ...(m.cc || [])].filter((a) => !seen.has(a.email) && (seen.add(a.email), true));
      }
    }
    // Reply from the address the message was sent to (e.g. a group like info@)
    const hit = [...(m.to || []), ...(m.cc || [])].find((a) => myAddrs.has(a.email));
    if (hit) st.from = hit.email;
    st.subject = hasPrefix(m.subject, 're') ? m.subject : `Re: ${m.subject || ''}`;
    st.html = `<br>${signatureHTML()}${quoteHTML(m)}`;
    st.replyToId = m.id;
    st.title = st.subject;
  } else if (m && o.mode === 'forward') {
    st.subject = hasPrefix(m.subject, 'fwd?') ? m.subject : `Fwd: ${m.subject || ''}`;
    st.html = `<br>${signatureHTML()}${forwardHTML(m)}`;
    st.forwardOf = m;
    st.replyToId = m.id;
    st.title = st.subject;
  } else if (!o.draftId && !st.html) {
    st.html = `<br>${signatureHTML()}`;
  }
  return st;
}

// ---------------------------------------------------------------- public API
export async function open(o = {}) {
  if (win) {
    const empty = !o.mode && !o.draftId && !o.to?.length && !o.subject;
    if (empty) { win.restore(); return; }
    await win.close({ quiet: false });
  }
  let st;
  if (o.draftId) {
    try {
      const d = await api(`/drafts/${o.draftId}`);
      st = { to: d.to || [], cc: d.cc || [], bcc: d.bcc || [], subject: d.subject || '', html: d.html || '', from: d.from || S.me.user.email,
        files: (d.attachments || []).map((f) => ({ ...f, done: true })), replyToId: d.reply_to_id || 0, draftId: d.id, title: d.subject || 'Draft',
        allowRemote: false };
    } catch (e) { return toastError(e); }
  } else {
    st = initialState(o);
  }
  win = createWindow(st);
  if (st.forwardOf) win.attachForwarded(st.forwardOf);
}

export async function closeAll(discardSilently) {
  if (!win) return;
  if (discardSilently) { win.destroy(); return; }
  await win.close({ quiet: true });
}

// ---------------------------------------------------------------- window
function createWindow(st) {
  const sendAs = S.me.send_as;
  const node = el(html`<section class="compose" role="dialog" aria-label="Compose">
    <div class="c-head" data-head><b>${st.title}</b>
      <button class="icon-btn" data-min data-tip="Minimise" aria-label="Minimise">${icon('remove')}</button>
      <button class="icon-btn desktop-only" data-full data-tip="Full screen" aria-label="Full screen">${icon('open_in_full')}</button>
      <button class="icon-btn" data-close data-tip="Save & close" aria-label="Save and close">${icon('close')}</button>
    </div>
    <div class="c-body">
      ${sendAs.length > 1 ? html`<div class="c-line"><label for="c-from">From</label><select id="c-from">${sendAs.map((s) => html`<option value="${s.email}" ${s.email === st.from ? raw('selected') : ''}>${s.name} &lt;${s.email}&gt;</option>`)}</select></div>` : ''}
      <div class="c-line" data-line="to"><label>To</label><div class="rcpts" data-rc="to"></div>
        <div class="toggles"><button type="button" data-show="cc">Cc</button><button type="button" data-show="bcc">Bcc</button></div></div>
      <div class="c-line" data-line="cc" hidden><label>Cc</label><div class="rcpts" data-rc="cc"></div></div>
      <div class="c-line" data-line="bcc" hidden><label>Bcc</label><div class="rcpts" data-rc="bcc"></div></div>
      <div class="c-line"><input class="subject" placeholder="Subject" aria-label="Subject" maxlength="400"></div>
      <div class="editor-wrap" data-editor-wrap><div class="editor" contenteditable="true" role="textbox" aria-multiline="true" aria-label="Message body" data-ph="Write your message…" spellcheck="true"></div></div>
      <div class="c-files" data-files></div>
      <div class="fmt-bar" data-fmt hidden>
        ${[['bold', 'format_bold', 'Bold (Ctrl+B)'], ['italic', 'format_italic', 'Italic (Ctrl+I)'], ['underline', 'format_underlined', 'Underline (Ctrl+U)'], ['strikeThrough', 'format_strikethrough', 'Strikethrough']].map(([c, i, t]) => html`<button class="icon-btn" data-cmd="${c}" data-tip="${t}" aria-label="${t}">${icon(i)}</button>`)}
        <span class="sep"></span>
        <button class="icon-btn" data-cmd="insertUnorderedList" data-tip="Bulleted list" aria-label="Bulleted list">${icon('format_list_bulleted')}</button>
        <button class="icon-btn" data-cmd="insertOrderedList" data-tip="Numbered list" aria-label="Numbered list">${icon('format_list_numbered')}</button>
        <button class="icon-btn" data-cmd="formatBlock:blockquote" data-tip="Quote" aria-label="Quote">${icon('format_quote')}</button>
        <span class="sep"></span>
        <button class="icon-btn" data-cmd="link" data-tip="Insert link" aria-label="Insert link">${icon('link')}</button>
        <button class="icon-btn" data-cmd="removeFormat" data-tip="Remove formatting" aria-label="Remove formatting">${icon('format_clear')}</button>
      </div>
      <div class="c-foot">
        <button class="btn filled send-btn" data-send>Send</button>
        <button class="icon-btn" data-fmt-toggle data-tip="Formatting options" aria-label="Formatting options">${icon('format_bold')}</button>
        <button class="icon-btn" data-attach data-tip="Attach files" aria-label="Attach files">${icon('attach_file')}</button>
        <input type="file" multiple hidden data-file-input>
        <span class="grow"></span>
        <span class="saved" data-saved></span>
        <button class="icon-btn" data-discard data-tip="Discard draft" aria-label="Discard draft">${icon('delete')}</button>
      </div>
      <div class="drop-overlay">${icon('upload', 'l')}&nbsp; Drop files to attach</div>
    </div>
  </section>`);
  document.body.append(node);

  const editor = node.querySelector('.editor');
  const subject = node.querySelector('.subject');
  const savedEl = node.querySelector('[data-saved]');
  editor.innerHTML = cleanForEditor(st.html, { allowRemote: st.allowRemote !== false });
  subject.value = st.subject;
  let dirty = false;
  let saving = null;
  let destroyed = false;
  let scrim = null;

  // ---- recipients
  const fields = {};
  for (const kind of ['to', 'cc', 'bcc']) {
    fields[kind] = recipientField(node.querySelector(`[data-rc="${kind}"]`), st[kind], () => { dirty = true; autosave(); });
    if (kind !== 'to' && st[kind].length) node.querySelector(`[data-line="${kind}"]`).hidden = false;
  }
  node.querySelectorAll('[data-show]').forEach((b) => {
    const kind = b.dataset.show;
    if (st[kind].length) b.hidden = true;
    b.onclick = () => { node.querySelector(`[data-line="${kind}"]`).hidden = false; b.hidden = true; fields[kind].focus(); };
  });

  // ---- attachments
  const filesEl = node.querySelector('[data-files]');
  const paintFiles = () => setHTML(filesEl, st.files.map((f, i) => html`<span class="c-file" title="${f.name}">${icon(fileIcon(f.type, f.name), 's')}
    <span>${f.name}</span>${f.done ? html`<small>${bytes(f.size)}</small>` : html`<span class="bar"><i style="width:${Math.round((f.progress || 0) * 100)}%"></i></span>`}
    <button class="icon-btn sm" data-rmfile="${i}" aria-label="Remove attachment" data-tip="Remove">${icon('close')}</button></span>`));
  paintFiles();
  filesEl.onclick = (e) => {
    const b = e.target.closest('[data-rmfile]');
    if (!b) return;
    const f = st.files[+b.dataset.rmfile];
    st.files.splice(+b.dataset.rmfile, 1);
    if (f?.id) api(`/uploads/${f.id}`, { method: 'DELETE', background: true }).catch(() => {});
    paintFiles();
    dirty = true;
    autosave();
  };
  const addFiles = async (list) => {
    const max = S.me.max_size || 25 << 20;
    for (const file of list) {
      const total = st.files.reduce((n, f) => n + (f.size || 0), 0);
      if (total + file.size > max * 0.74) { snack(`Attachments can be up to ${Math.floor((max * 0.74) / (1 << 20))} MB in total`); break; }
      const entry = { name: file.name, size: file.size, type: file.type, progress: 0, done: false };
      st.files.push(entry);
      paintFiles();
      try {
        const res = await upload('/uploads', file, (p) => { entry.progress = p; paintFiles(); });
        Object.assign(entry, res, { done: true });
        dirty = true;
        autosave();
      } catch (e) {
        st.files.splice(st.files.indexOf(entry), 1);
        toastError(e);
      }
      paintFiles();
    }
  };
  const fileInput = node.querySelector('[data-file-input]');
  node.querySelector('[data-attach]').onclick = () => fileInput.click();
  fileInput.onchange = () => { addFiles([...fileInput.files]); fileInput.value = ''; };
  let dragDepth = 0;
  node.addEventListener('dragenter', (e) => { if (e.dataTransfer?.types?.includes('Files')) { dragDepth++; node.classList.add('drag'); } });
  node.addEventListener('dragleave', () => { if (--dragDepth <= 0) { dragDepth = 0; node.classList.remove('drag'); } });
  node.addEventListener('dragover', (e) => { if (e.dataTransfer?.types?.includes('Files')) e.preventDefault(); });
  node.addEventListener('drop', (e) => {
    if (!e.dataTransfer?.files?.length) return;
    e.preventDefault();
    dragDepth = 0;
    node.classList.remove('drag');
    addFiles([...e.dataTransfer.files]);
  });
  editor.addEventListener('paste', (e) => {
    const files = [...(e.clipboardData?.files || [])];
    if (files.length) { e.preventDefault(); addFiles(files); }
  });

  // ---- formatting
  const fmt = node.querySelector('[data-fmt]');
  node.querySelector('[data-fmt-toggle]').onclick = () => { fmt.hidden = !fmt.hidden; };
  fmt.addEventListener('mousedown', (e) => e.preventDefault()); // keep editor selection
  fmt.onclick = async (e) => {
    const b = e.target.closest('[data-cmd]');
    if (!b) return;
    const [cmd, arg] = b.dataset.cmd.split(':');
    editor.focus();
    if (cmd === 'link') {
      const sel = window.getSelection();
      const range = sel.rangeCount ? sel.getRangeAt(0).cloneRange() : null;
      const url = await promptLink(sel.toString());
      if (!url) return;
      editor.focus();
      if (range) { sel.removeAllRanges(); sel.addRange(range); }
      if (range && !range.collapsed) document.execCommand('createLink', false, url);
      else document.execCommand('insertHTML', false, `<a href="${esc(url)}">${esc(url)}</a>&nbsp;`);
    } else {
      document.execCommand(cmd, false, arg ? `<${arg}>` : null);
    }
    dirty = true;
    autosave();
  };

  // ---- drafts
  const payload = () => ({
    id: st.draftId || 0,
    from: node.querySelector('#c-from')?.value || st.from,
    to: fields.to.value(), cc: fields.cc.value(), bcc: fields.bcc.value(),
    subject: subject.value, html: restoreRemote(editor.innerHTML),
    attachments: st.files.filter((f) => f.done).map(({ id, name, size, type }) => ({ id, name, size, type })),
    reply_to_id: st.replyToId || 0,
  });
  const isEmpty = () => {
    const p = payload();
    const text = editor.innerText.replace(/\s+/g, '').replace(/^--/, '');
    const sigOnly = editor.innerText.trim() === (editor.querySelector('.sig')?.innerText.trim() || '');
    return !p.to.length && !p.cc.length && !p.bcc.length && !p.subject.trim() && (!text || sigOnly) && !p.attachments.length && !st.replyToId;
  };
  const saveDraft = async () => {
    if (destroyed || !dirty || isEmpty()) return;
    dirty = false;
    savedEl.textContent = 'Saving…';
    try {
      const res = await api('/drafts', { method: 'PUT', body: payload(), background: true });
      st.draftId = res.id;
      savedEl.textContent = 'Draft saved';
    } catch {
      dirty = true;
      savedEl.textContent = 'Not saved';
    }
  };
  const autosave = debounce(() => { saving = saveDraft(); }, 1500);
  editor.addEventListener('input', () => { dirty = true; autosave(); });
  subject.addEventListener('input', () => {
    dirty = true;
    autosave();
    node.querySelector('[data-head] b').textContent = subject.value.trim() || 'New message';
  });
  node.querySelector('#c-from')?.addEventListener('change', () => { dirty = true; autosave(); });

  // ---- window controls
  const setFull = (on) => {
    node.classList.toggle('full', on);
    if (on && !scrim) { scrim = el('<div class="compose-scrim"></div>'); scrim.onclick = () => setFull(false); document.body.append(scrim); }
    if (!on) { scrim?.remove(); scrim = null; }
    node.querySelector('[data-full]').innerHTML = String(icon(on ? 'close_fullscreen' : 'open_in_full'));
  };
  node.querySelector('[data-head]').onclick = (e) => { if (!e.target.closest('button')) node.classList.toggle('min'); };
  node.querySelector('[data-min]').onclick = () => node.classList.toggle('min');
  node.querySelector('[data-full]').onclick = () => setFull(!node.classList.contains('full'));
  node.querySelector('[data-close]').onclick = () => api_.close({ quiet: false });
  node.querySelector('[data-discard]').onclick = () => discard();
  node.querySelector('[data-send]').onclick = () => send();
  node.querySelector('[data-editor-wrap]').onclick = (e) => { if (e.target.dataset.editorWrap !== undefined) editor.focus(); };
  node.addEventListener('keydown', (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') { e.preventDefault(); send(); }
    else if (e.key === 'Escape' && !node.querySelector('.suggest')) { e.preventDefault(); api_.close({ quiet: false }); }
    e.stopPropagation(); // don't trigger global shortcuts while typing here
  });

  function destroy() {
    destroyed = true;
    autosave.cancel();
    scrim?.remove();
    node.remove();
    if (win === api_) win = null;
  }

  async function discard() {
    autosave.cancel();
    const id = st.draftId;
    destroy();
    if (id) {
      try { await api(`/drafts/${id}`, { method: 'DELETE', background: true }); refreshCounts(); } catch { /* ignore */ }
    } else {
      st.files.forEach((f) => f.id && api(`/uploads/${f.id}`, { method: 'DELETE', background: true }).catch(() => {}));
    }
    snack('Draft discarded');
  }

  async function send() {
    fields.to.commit(); fields.cc.commit(); fields.bcc.commit();
    const p = payload();
    const all = [...p.to, ...p.cc, ...p.bcc];
    if (!all.length) { snack('Add at least one recipient'); fields.to.focus(); return; }
    const bad = all.find((a) => !isEmail(a.email));
    if (bad) { snack(`“${bad.email}” is not a valid email address`); return; }
    if (st.files.some((f) => !f.done)) { snack('Wait for attachments to finish uploading'); return; }
    if (!p.subject.trim() && !(await confirmDialog('Send without a subject?', 'This message has no subject line.', 'Send anyway'))) return;
    const text = editor.innerText.split(/\n-- \n|On .+wrote:/)[0];
    if (/\b(attach(ed|ment|ing)?|enclosed)\b/i.test(text) && !p.attachments.length &&
      !(await confirmDialog('Did you mean to attach files?', 'You wrote about an attachment, but there are no files attached.', 'Send anyway'))) return;

    autosave.cancel();
    if (dirty) await saveDraft().catch(() => {});
    await saving?.catch(() => {});
    node.hidden = true;
    scrim?.remove();
    let cancelled = false;
    const body = { ...p, draft_id: st.draftId || 0, upload_ids: p.attachments.map((f) => f.id) };
    const timer = setTimeout(async () => {
      if (cancelled) return;
      const dismiss = snack('Sending…', { timeout: 0 });
      try {
        await api('/send', { method: 'POST', body });
        dismiss();
        destroy();
        snack('Message sent', { action: 'View message', onAction: () => navigate('#/sent') });
        refreshCounts();
      } catch (e) {
        dismiss();
        node.hidden = false;
        snack(e.message || "Couldn't send. Your message is saved as a draft.", { timeout: 8000 });
      }
    }, 5000);
    snack('Sending in a moment…', {
      action: 'Undo', timeout: 5000,
      onAction: () => { cancelled = true; clearTimeout(timer); node.hidden = false; node.classList.remove('min'); editor.focus(); snack('Sending cancelled'); },
    });
  }

  const api_ = {
    restore() { node.classList.remove('min'); node.hidden = false; editor.focus(); },
    destroy,
    async close({ quiet }) {
      autosave.cancel();
      if (isEmpty()) {
        const id = st.draftId;
        destroy();
        if (id) api(`/drafts/${id}`, { method: 'DELETE', background: true }).then(refreshCounts).catch(() => {});
        return;
      }
      dirty = true;
      await saveDraft();
      destroy();
      refreshCounts();
      if (!quiet) snack('Draft saved', { action: 'Open', onAction: () => st.draftId && open({ draftId: st.draftId }) });
    },
    async attachForwarded(m) {
      if (!m.attachments?.some((a) => !a.inline)) return;
      try {
        const files = await api(`/messages/${m.id}/forward-files`, { method: 'POST' });
        st.files.push(...files.map((f) => ({ ...f, done: true })));
        paintFiles();
        dirty = true;
        autosave();
      } catch (e) { toastError(e); }
    },
  };

  // focus the first useful field
  if (!fields.to.value().length) fields.to.focus();
  else if (!subject.value) subject.focus();
  else { editor.focus(); placeCaretAtStart(editor); }
  return api_;
}

function placeCaretAtStart(node) {
  const r = document.createRange();
  r.setStart(node, 0);
  r.collapse(true);
  const sel = window.getSelection();
  sel.removeAllRanges();
  sel.addRange(r);
}

async function promptLink(text) {
  let url = '';
  const ok = await dialog({
    title: 'Insert link',
    body: html`<div class="field"><input id="f-url" placeholder=" " value="${/^https?:\/\//.test(text) ? text : ''}" autocomplete="off"><label for="f-url">Web address (https://…)</label></div>`,
    actions: [{ label: 'Cancel', value: false }, { label: 'Insert', value: true, kind: 'filled', submit: true }],
    onAction: (v, d) => {
      if (!v) return true;
      url = d.querySelector('#f-url').value.trim();
      if (!url) return false;
      if (!/^(https?:|mailto:)/i.test(url)) url = 'https://' + url;
      return true;
    },
  });
  return ok ? url : '';
}

// ---------------------------------------------------------------- recipient chips
function recipientField(box, initial, onChange) {
  const list = [...initial];
  const input = el('<input type="text" autocomplete="off" spellcheck="false" aria-label="Recipients">');
  let sugg = null;
  let items = [];
  let hl = 0;
  let seq = 0;

  const chip = (a, i) => {
    const bad = !isEmail(a.email);
    return el(html`<span class="rchip ${bad ? 'bad' : ''}" data-i="${i}" title="${a.email}">${avatar(a.name, a.email)}<span>${a.name || a.email}</span>
      <button type="button" aria-label="Remove ${a.email}">${icon('close')}</button></span>`);
  };
  const paint = () => {
    box.querySelectorAll('.rchip').forEach((c) => c.remove());
    list.forEach((a, i) => box.insertBefore(chip(a, i), input));
  };
  box.append(input);
  paint();

  const hide = () => { sugg?.remove(); sugg = null; items = []; };
  const add = (arr) => {
    let changed = false;
    arr.forEach((a) => {
      if (!a?.email) return;
      if (list.some((x) => x.email === a.email)) return;
      list.push({ name: a.name || '', email: a.email });
      changed = true;
    });
    if (changed) { paint(); onChange(); }
  };
  const commit = () => {
    const v = input.value.trim();
    if (!v) return;
    add(splitAddresses(v));
    input.value = '';
    hide();
  };
  const showSuggestions = async () => {
    const q = input.value.trim();
    const my = ++seq;
    if (!q) return hide();
    let res = [];
    try { res = await api('/contacts?q=' + encodeURIComponent(q), { background: true }); } catch { return; }
    if (my !== seq || document.activeElement !== input) return;
    items = res.filter((c) => !list.some((x) => x.email === c.email));
    if (!items.length) return hide();
    hl = 0;
    const line = box.closest('.c-line');
    if (!sugg) { sugg = el('<div class="suggest" role="listbox"></div>'); line.append(sugg); }
    setHTML(sugg, items.map((c, i) => html`<button type="button" class="${i === hl ? 'on' : ''}" data-si="${i}" role="option">${avatar(c.name, c.email, 's32')}
      <span><b>${c.name || c.email}</b><small>${c.email}${c.kind === 'group' ? ' · group' : ''}</small></span></button>`));
    sugg.onmousedown = (e) => e.preventDefault();
    sugg.onclick = (e) => {
      const b = e.target.closest('[data-si]');
      if (!b) return;
      add([items[+b.dataset.si]]);
      input.value = '';
      hide();
      input.focus();
    };
  };
  const debounced = debounce(showSuggestions, 120);
  input.addEventListener('input', () => {
    if (/[,;]\s*$/.test(input.value)) { commit(); return; }
    debounced();
  });
  input.addEventListener('keydown', (e) => {
    if (sugg && items.length && (e.key === 'ArrowDown' || e.key === 'ArrowUp')) {
      e.preventDefault();
      hl = (hl + (e.key === 'ArrowDown' ? 1 : items.length - 1)) % items.length;
      sugg.querySelectorAll('button').forEach((b, i) => b.classList.toggle('on', i === hl));
      return;
    }
    if (e.key === 'Enter' || e.key === 'Tab' || e.key === ',' || e.key === ';') {
      if (sugg && items.length && input.value.trim()) {
        e.preventDefault();
        add([items[hl]]);
        input.value = '';
        hide();
        return;
      }
      if (input.value.trim()) { e.preventDefault(); commit(); }
      return;
    }
    if (e.key === 'Escape' && sugg) { e.preventDefault(); e.stopPropagation(); hide(); return; }
    if (e.key === 'Backspace' && !input.value && list.length) {
      const a = list.pop();
      paint();
      onChange();
      input.value = a.name ? `${a.name} <${a.email}>` : a.email;
      e.preventDefault();
    }
  });
  input.addEventListener('blur', () => setTimeout(() => { commit(); hide(); }, 120));
  input.addEventListener('paste', (e) => {
    const t = e.clipboardData?.getData('text') || '';
    if (/[,;\n]/.test(t) || isEmail(t.trim())) { e.preventDefault(); add(splitAddresses(t)); }
  });
  box.addEventListener('click', (e) => {
    const rm = e.target.closest('.rchip button');
    if (rm) {
      list.splice(+rm.closest('.rchip').dataset.i, 1);
      paint();
      onChange();
      input.focus();
      return;
    }
    const c = e.target.closest('.rchip');
    if (c) {
      const a = list.splice(+c.dataset.i, 1)[0];
      paint();
      onChange();
      commit();
      input.value = a.name ? `${a.name} <${a.email}>` : a.email;
      input.focus();
      input.select();
      return;
    }
    input.focus();
  });
  return {
    value: () => list.map((a) => ({ name: a.name, email: a.email })),
    focus: () => input.focus(),
    commit,
  };
}

