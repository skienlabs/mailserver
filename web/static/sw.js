// Skien Mails service worker: makes the app installable and loads instantly.
// API calls always go to the network; the app shell is cached per release.
const VERSION = '{{V}}';
const CACHE = 'sm-shell-' + VERSION;
const SHELL = [
  '/',
  '/css/app.css?v=' + VERSION,
  '/js/theme.js?v=' + VERSION,
  '/js/app.js?v=' + VERSION,
  '/manifest.webmanifest',
  '/fonts/google-sans-latin-wght-normal.woff2',
  '/icons/favicon.svg',
  '/icons/icon-192.png',
];

self.addEventListener('install', (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k.startsWith('sm-shell-') && k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', (e) => {
  const req = e.request;
  if (req.method !== 'GET') return;
  const url = new URL(req.url);
  if (url.origin !== location.origin || url.pathname.startsWith('/api/') || url.pathname === '/healthz') return;

  if (req.mode === 'navigate') {
    // Network first so a new release is picked up immediately; cached shell when offline.
    e.respondWith(fetch(req).catch(() => caches.match('/')));
    return;
  }
  e.respondWith(
    caches.match(req).then((hit) => hit || fetch(req).then((res) => {
      if (res.ok && (url.search.includes('v=' + VERSION) || url.pathname.startsWith('/fonts/') || url.pathname.startsWith('/icons/'))) {
        const copy = res.clone();
        caches.open(CACHE).then((c) => c.put(req, copy));
      }
      return res;
    }))
  );
});

self.addEventListener('notificationclick', (e) => {
  e.notification.close();
  let target = e.notification.data && e.notification.data.url ? e.notification.data.url : '/#/inbox';
  if (!target.startsWith('/#/')) target = '/#/inbox'; // only ever open this app's own pages
  e.waitUntil(self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((wins) => {
    for (const w of wins) {
      if ('focus' in w) {
        w.postMessage({ type: 'open', url: target }); // the open app switches to the conversation
        return w.focus();
      }
    }
    return self.clients.openWindow(target);
  }));
});

// New mail while the app is closed (Web Push; the server encrypts it for this browser only).
self.addEventListener('push', (e) => {
  let m = {};
  try { m = e.data ? e.data.json() : {}; } catch { m = {}; }
  e.waitUntil((async () => {
    let quiet = false;
    if (m.type === 'mail') {
      // Someone looking at the app right now sees the mail arrive; no need to interrupt them.
      const wins = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
      if (wins.some((w) => w.focused && w.visibilityState === 'visible')) {
        // Apple's push service (iPhone and iPad in any browser, Safari on Mac) may end the
        // subscription after pushes that show nothing, so there the notice is shown silently and
        // closed at once; other browsers skip it.
        const ua = (self.navigator && self.navigator.userAgent) || '';
        const apple = /iPhone|iPad|iPod/.test(ua) || (/Macintosh/.test(ua) && /AppleWebKit/.test(ua) && !/Chrome\/|Chromium|Edg\/|OPR|Firefox/.test(ua));
        if (!apple) return;
        quiet = true;
      }
    }
    const tag = m.type === 'test' ? 'sm-test' : 'sm-new-mail';
    await self.registration.showNotification(m.title || 'New mail', {
      body: m.body || '',
      icon: '/icons/icon-192.png',
      badge: '/icons/icon-192.png',
      tag,
      renotify: !quiet,
      silent: quiet,
      timestamp: Date.now(),
      data: { url: typeof m.url === 'string' ? m.url : '/#/inbox' },
    });
    if (quiet) {
      const shown = await self.registration.getNotifications({ tag });
      shown.forEach((n) => n.close());
    }
  })());
});

// The browser renewed this device's subscription: tell the server (the sign-in cookie comes along).
self.addEventListener('pushsubscriptionchange', (e) => {
  e.waitUntil((async () => {
    const old = e.oldSubscription;
    let sub = e.newSubscription;
    const key = old && old.options && old.options.applicationServerKey;
    if (!sub && key) sub = await self.registration.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key });
    if (!sub) return;
    const j = sub.toJSON();
    await fetch('/api/push', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'X-SM': '1' },
      body: JSON.stringify({ endpoint: j.endpoint, keys: j.keys, replaces: old ? old.endpoint : '' }),
    });
  })().catch(() => {}));
});
