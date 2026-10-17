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
  const target = e.notification.data && e.notification.data.url ? e.notification.data.url : '/#/inbox';
  e.waitUntil(self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((wins) => {
    for (const w of wins) {
      if ('focus' in w) { w.navigate(target).catch(() => {}); return w.focus(); }
    }
    return self.clients.openWindow(target);
  }));
});
