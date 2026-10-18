// Notifications when the app is closed (Web Push). The server encrypts each notice for this
// browser; the service worker (sw.js) shows it.
import { api } from './lib.js';
import { pref } from './state.js';

const isIOS = () => /iPhone|iPad|iPod/.test(navigator.userAgent) || (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1);
const standalone = () => matchMedia('(display-mode: standalone)').matches || navigator.standalone === true;

// pushSupport says whether this browser can get notifications while closed, and if not, why.
export function pushSupport() {
  if (!window.isSecureContext || !('serviceWorker' in navigator)) return { ok: false, reason: 'Needs a secure (https) connection.' };
  if (isIOS() && !standalone()) {
    return { ok: false, ios: true, reason: 'On iPhone and iPad, first add Skien Mails to your Home Screen (Share → Add to Home Screen), then open it from there and turn this on.' };
  }
  if (!('PushManager' in window) || !('Notification' in window)) return { ok: false, reason: "This browser doesn't support notifications while closed." };
  return { ok: true };
}

function keyBytes(b64) {
  const s = atob(b64.replace(/-/g, '+').replace(/_/g, '/') + '='.repeat((4 - (b64.length % 4)) % 4));
  return Uint8Array.from(s, (c) => c.charCodeAt(0));
}

function sameKey(buf, b64) {
  if (!buf) return false;
  const a = new Uint8Array(buf);
  const b = keyBytes(b64);
  return a.length === b.length && a.every((x, i) => x === b[i]);
}

async function registration() {
  const reg = await navigator.serviceWorker.getRegistration();
  if (reg) return reg;
  await navigator.serviceWorker.register('/sw.js');
  return navigator.serviceWorker.ready;
}

export async function currentSubscription() {
  if (!pushSupport().ok) return null;
  try { return await (await registration()).pushManager.getSubscription(); } catch { return null; }
}

// pushActiveHere is true when this browser gets push notices (so the open app shouldn't show its
// own notification for the same mail).
export function pushActiveHere() {
  return pref.get('push', false) && 'Notification' in window && Notification.permission === 'granted';
}

// enablePush asks for permission, subscribes and registers this browser. Returns the device id.
export async function enablePush(preview = true) {
  const sup = pushSupport();
  if (!sup.ok) throw new Error(sup.reason);
  const perm = await Notification.requestPermission();
  if (perm !== 'granted') throw new Error('Notifications are blocked. Allow them in your browser’s site settings, then try again.');
  const info = await api('/push');
  if (!info.available) throw new Error('Notifications aren’t available on this server right now.');
  const reg = await registration();
  let sub = await reg.pushManager.getSubscription();
  if (sub && !sameKey(sub.options?.applicationServerKey, info.public_key)) { await sub.unsubscribe().catch(() => {}); sub = null; }
  if (!sub) sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: keyBytes(info.public_key) });
  const j = sub.toJSON();
  const res = await api('/push', { method: 'POST', body: { endpoint: j.endpoint, keys: j.keys, preview } });
  pref.set('push', true);
  return res.id;
}

// disablePush stops notifications for this browser.
export async function disablePush(id) {
  pref.set('push', false);
  const sub = await currentSubscription();
  if (id) await api('/push/' + id, { method: 'DELETE' }).catch(() => {});
  if (sub) await sub.unsubscribe().catch(() => {});
}

// thisDevice finds this browser in the server's device list.
export async function thisDevice(devices) {
  const sub = await currentSubscription();
  return sub ? devices.find((d) => d.endpoint === sub.endpoint) || null : null;
}

// syncPush runs after sign-in: if this browser was turned off elsewhere (another device's
// settings, signing out, a password change), the local state follows; if the server's key
// changed, it subscribes again.
export async function syncPush() {
  if (!pref.get('push', false) || !pushSupport().ok) return;
  try {
    const sub = await currentSubscription();
    const info = await api('/push', { background: true });
    const mine = sub && info.devices.find((d) => d.endpoint === sub.endpoint);
    if (!mine || Notification.permission !== 'granted') {
      pref.set('push', false);
      if (mine) await api('/push/' + mine.id, { method: 'DELETE', background: true }).catch(() => {});
      if (sub) await sub.unsubscribe().catch(() => {});
      return;
    }
    if (!sameKey(sub.options?.applicationServerKey, info.public_key)) await enablePush(mine.preview);
  } catch { /* offline: try next time */ }
}

// beforeSignOut drops this browser's subscription (the server forgets it with the session).
export async function beforeSignOut() {
  if (!pref.get('push', false)) return;
  pref.set('push', false);
  const sub = await currentSubscription();
  if (sub) await sub.unsubscribe().catch(() => {});
}
