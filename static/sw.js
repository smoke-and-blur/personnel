/* Оболонка застосунку кешується, дані — ніколи: вони живуть за ключем і
   мають свій кеш у localStorage, а тут би застаріли непомітно. */
const SHELL = 'shell-v1';
const FILES = ['/', '/index.html', '/manifest.webmanifest', '/icon.svg', '/icon-mask.svg'];

self.addEventListener('install', e => {
  e.waitUntil(caches.open(SHELL).then(c => c.addAll(FILES)).then(() => self.skipWaiting()));
});

self.addEventListener('activate', e => {
  e.waitUntil(caches.keys()
    .then(ks => Promise.all(ks.filter(k => k !== SHELL).map(k => caches.delete(k))))
    .then(() => self.clients.claim()));
});

self.addEventListener('fetch', e => {
  const url = new URL(e.request.url);
  if(e.request.method !== 'GET' || url.origin !== location.origin) return;
  if(url.pathname.startsWith('/api/')) return;          // дані — тільки з мережі

  // Свіже з мережі, а якщо її немає — те, що лежить у кеші.
  e.respondWith(
    fetch(e.request)
      .then(res => {
        const copy = res.clone();
        caches.open(SHELL).then(c => c.put(e.request, copy)).catch(() => {});
        return res;
      })
      .catch(() => caches.match(e.request).then(r => r || caches.match('/')))
  );
});
