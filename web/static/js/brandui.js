// The main-colour picker used in first-run setup and in Admin › Server settings.
import { $, html, api, debounce } from './lib.js';
import { applyBrand } from './state.js';

const NAMES = {
  '#4453f2': 'Indigo', '#0b57d0': 'Blue', '#0f766e': 'Teal', '#15803d': 'Green', '#b45309': 'Amber',
  '#c2410c': 'Orange', '#be123c': 'Rose', '#7c3aed': 'Violet', '#334155': 'Slate',
};
const isHex = (c) => /^#[0-9a-f]{6}$/i.test(c || '');

export function colourPicker(current, presets) {
  current = isHex(current) ? current.toLowerCase() : presets[0];
  const custom = !presets.includes(current);
  return html`<div class="swatches" role="radiogroup" aria-label="Main colour">
    ${presets.map((c) => html`<button type="button" class="swatch ${c === current ? 'on' : ''}" role="radio" aria-checked="${c === current ? 'true' : 'false'}"
      style="--sw:${c}" data-c="${c}" aria-label="${NAMES[c] || c}" data-tip="${NAMES[c] || c}"></button>`)}
    <label class="swatch custom ${custom ? 'on' : ''}" data-tip="Any colour" style="--sw:${current}">
      <input type="color" value="${current}" aria-label="Choose any colour"><span aria-hidden="true">+</span></label>
    <input type="hidden" name="accent" value="${current}">
    <span class="swatch-hex" aria-live="polite">${custom ? current : NAMES[current] || current}</span>
  </div>`;
}

// wireColourPicker previews the colour on the whole page as it is picked. onPick(colour, vars)
// runs after each change; the page keeps the preview until it is reloaded or reverted.
export function wireColourPicker(root, onPick) {
  const box = $('.swatches', root);
  if (!box) return;
  const hidden = box.querySelector('input[name=accent]');
  const label = box.querySelector('.swatch-hex');
  const customSw = box.querySelector('.swatch.custom');
  const preview = debounce(async (c) => {
    try {
      const b = await api(`/brand/preview?accent=${encodeURIComponent(c)}`, { background: true });
      if (hidden.value !== c || !box.isConnected) return; // a newer pick is on its way, or the page was left
      applyBrand({ vars: b.vars });
      onPick?.(c, b.vars);
    } catch { /* offline: the colour is still saved */ }
  }, 120);
  const pick = (c, custom) => {
    c = c.toLowerCase();
    hidden.value = c;
    box.querySelectorAll('.swatch').forEach((s) => {
      const on = custom ? s === customSw : s.dataset.c === c;
      s.classList.toggle('on', on);
      if (s.getAttribute('role') === 'radio') s.setAttribute('aria-checked', on ? 'true' : 'false');
    });
    customSw.style.setProperty('--sw', c);
    label.textContent = custom ? c : NAMES[c] || c;
    preview(c);
  };
  box.querySelectorAll('button.swatch').forEach((b) => { b.onclick = () => pick(b.dataset.c, false); });
  const inp = customSw.querySelector('input');
  inp.addEventListener('input', () => isHex(inp.value) && pick(inp.value, true));
}

// markSVG draws the app icon for a preview, in the colours brandVars gives the server.
export function markSVG(vars, size = 40) {
  const a = isHex(vars?.['--b-accent']) ? vars['--b-accent'] : '#4453f2';
  const ink = isHex(vars?.['--b-on-accent']) ? vars['--b-on-accent'] : '#ffffff';
  return html`<svg class="mark" width="${size}" height="${size}" viewBox="0 0 64 64" aria-hidden="true"><rect width="64" height="64" rx="14" fill="${a}"/><rect x="14" y="19.5" width="36" height="25" rx="4.5" fill="${ink}"/><path d="M17 23 32 34.5 47 23" fill="none" stroke="${a}" stroke-width="3.8" stroke-linecap="round" stroke-linejoin="round"/></svg>`;
}
