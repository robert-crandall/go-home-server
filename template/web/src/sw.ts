/// <reference lib="webworker" />
import { clientsClaim } from 'workbox-core';
import { cleanupOutdatedCaches, precacheAndRoute } from 'workbox-precaching';

// This service worker is built by vite-plugin-pwa (injectManifest strategy).
// It precaches the built assets and handles Web Push notifications.

declare let self: ServiceWorkerGlobalScope & {
  __WB_MANIFEST: Array<{ url: string; revision: string | null }>;
};

// Activate a new build immediately instead of waiting for every tab to close.
// Installed PWAs (especially iOS standalone) rarely close all their tabs, so
// without this a new service worker would sit in "waiting" and the UI would
// never update. With registerType 'autoUpdate', taking control here triggers the
// page reload that loads the new UI.
self.skipWaiting();
clientsClaim();

// Drop precaches from previous deploys so storage doesn't grow unbounded.
cleanupOutdatedCaches();
precacheAndRoute(self.__WB_MANIFEST);

self.addEventListener('push', (event: PushEvent) => {
  const payload = (() => {
    try {
      return event.data?.json() ?? {};
    } catch {
      return {};
    }
  })();

  const title = payload.title ?? 'Notification';
  event.waitUntil(
    self.registration.showNotification(title, {
      body: payload.body ?? '',
      icon: '/icons/icon-192.png',
      badge: '/icons/icon-192.png',
      data: { url: payload.url ?? '/' },
    }),
  );
});

self.addEventListener('notificationclick', (event: NotificationEvent) => {
  event.notification.close();
  const url = event.notification.data?.url ?? '/';
  event.waitUntil(
    self.clients
      .matchAll({ type: 'window', includeUncontrolled: true })
      .then(async (clients) => {
        // Reuse an existing window: navigate it to the target and focus it.
        for (const client of clients) {
          if ('focus' in client) {
            if ('navigate' in client) {
              await client.navigate(url).catch(() => undefined);
            }
            await client.focus();
            return;
          }
        }
        await self.clients.openWindow(url);
      }),
  );
});
