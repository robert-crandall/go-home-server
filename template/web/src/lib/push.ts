import { api } from './api/client';

export type PushResult = 'subscribed' | 'denied' | 'unsupported' | 'disabled' | 'error';

// Convert a URL-safe base64 VAPID key into the Uint8Array the Push API wants.
// Exported so it can be unit-tested without a browser.
export function urlBase64ToUint8Array(base64: string): Uint8Array<ArrayBuffer> {
  const padding = '='.repeat((4 - (base64.length % 4)) % 4);
  const normalized = (base64 + padding).replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(normalized);
  const output = new Uint8Array(new ArrayBuffer(raw.length));
  for (let i = 0; i < raw.length; i++) {
    output[i] = raw.charCodeAt(i);
  }
  return output;
}

// Ask the browser for notification permission, subscribe via the service
// worker, and register the subscription with the server.
export async function subscribeToPush(): Promise<PushResult> {
  if (!('serviceWorker' in navigator) || !('PushManager' in window)) {
    return 'unsupported';
  }

  const { data } = await api.GET('/api/push/vapid-public-key');
  if (!data?.enabled || !data.publicKey) {
    return 'disabled';
  }

  const permission = await Notification.requestPermission();
  if (permission !== 'granted') {
    return 'denied';
  }

  const registration = await navigator.serviceWorker.ready;
  const subscription = await registration.pushManager.subscribe({
    userVisibleOnly: true,
    applicationServerKey: urlBase64ToUint8Array(data.publicKey),
  });

  const json = subscription.toJSON();
  const { error } = await api.POST('/api/push/subscribe', {
    body: {
      endpoint: json.endpoint ?? '',
      keys: {
        p256dh: json.keys?.p256dh ?? '',
        auth: json.keys?.auth ?? '',
      },
    },
  });
  // openapi-fetch reports non-2xx responses via `error`, not by throwing.
  if (error) {
    return 'error';
  }

  return 'subscribed';
}
