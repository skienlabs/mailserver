// People directory: everyone in the organisation, plus group addresses.
import { $, html, icon, setHTML, avatar, plural } from './lib.js';
import { S, loadDirectory } from './state.js';
import * as compose from './compose.js';

export async function render(viewEl) {
  if (!S.directory) await loadDirectory();
  const d = S.directory || { people: [], groups: [] };
  setHTML(viewEl, html`
    <div class="page-head">
      <div class="grow"><p class="greet">${S.me.org.name}</p><h1>People</h1><p class="sub">${plural(d.people.length, 'person', 'people')}${d.groups.length ? ` · ${plural(d.groups.length, 'group')}` : ''}</p></div>
      <div class="search" style="max-width:320px;height:44px">${icon('search')}<input id="pfilter" placeholder="Find a colleague" aria-label="Find a colleague" style="padding-left:6px"></div>
    </div>
    <div class="page"><div class="page-inner" style="max-width:none">
      <div class="people" id="people"></div>
      ${d.groups.length ? html`<div class="section"><h2>Groups</h2><p class="lead">Shared addresses that deliver to several people.</p>
        <div class="tbl-wrap"><table class="tbl"><tbody>${d.groups.map((g) => html`<tr>
          <td><div class="who-cell">${avatar(g.name || g.address, g.address)}<div><b>${g.name || g.address}</b><small>${g.address}</small></div></div></td>
          <td class="muted">${g.description}</td><td class="muted nowrap">${plural(g.members, 'member')}</td>
          <td class="num"><button class="btn sm" data-mail="${g.address}" data-name="${g.name}">${icon('mail_outline')}Email</button></td></tr>`)}</tbody></table></div></div>` : ''}
    </div></div>`);
  const paint = (q = '') => {
    q = q.toLowerCase();
    const list = d.people.filter((p) => !q || `${p.name} ${p.email} ${p.title}`.toLowerCase().includes(q));
    setHTML($('#people'), list.length ? list.map((p) => html`<div class="person">
      ${avatar(p.name, p.email, 's96')}<b>${p.name}</b><small>${p.title || ' '}</small><small>${p.email}</small>${p.phone ? html`<small>${p.phone}</small>` : ''}
      <button class="btn tonal sm" data-mail="${p.email}" data-name="${p.name}">${icon('mail_outline')}Send email</button></div>`)
      : html`<div class="empty">${icon('search')}<p>No one matches “${q}”.</p></div>`);
  };
  paint();
  $('#pfilter').oninput = (e) => paint(e.target.value);
  $('.page', viewEl).onclick = (e) => {
    const b = e.target.closest('[data-mail]');
    if (b) compose.open({ to: [{ email: b.dataset.mail, name: b.dataset.name }] });
  };
}
