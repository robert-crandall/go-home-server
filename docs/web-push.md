# Browser web push

The `notify` package owns the server half of Web Push. Each app still has to
register a service worker and ask the browser for a subscription. This is the
smallest browser half I copy between apps.

Register the service worker through the app's existing build setup before this
code waits on `navigator.serviceWorker.ready`. On iOS, push is only available to
an installed PWA. `Notification.requestPermission()` also has to run directly
from a user gesture, so keep it as the first awaited call in the click path.

```html
<button id="enable-notifications" type="button">Enable notifications</button>
```

```js
function urlBase64ToUint8Array(value) {
  const padding = "=".repeat((4 - (value.length % 4)) % 4);
  const base64 = (value + padding).replace(/-/g, "+").replace(/_/g, "/");
  return Uint8Array.from(atob(base64), (character) => character.charCodeAt(0));
}

async function subscribeToPush() {
  const permission = await Notification.requestPermission();
  if (permission !== "granted") {
    throw new Error("Notification permission was not granted");
  }

  const registration = await navigator.serviceWorker.ready;
  const keyResponse = await fetch("/api/push/vapid-public-key");
  if (!keyResponse.ok) {
    throw new Error(`Could not load the VAPID public key: ${keyResponse.status}`);
  }

  const { enabled, publicKey } = await keyResponse.json();
  if (!enabled) {
    throw new Error("Push is not configured on the server");
  }

  const subscription = await registration.pushManager.subscribe({
    userVisibleOnly: true,
    applicationServerKey: urlBase64ToUint8Array(publicKey),
  });
  const { endpoint, keys } = subscription.toJSON();

  const subscribeResponse = await fetch("/api/push/subscribe", {
    method: "POST",
    credentials: "same-origin",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ endpoint, keys }),
  });
  if (!subscribeResponse.ok) {
    throw new Error(`Could not save the push subscription: ${subscribeResponse.status}`);
  }
}

document.querySelector("#enable-notifications").addEventListener("click", () => {
  subscribeToPush().catch(console.error);
});
```

The public-key endpoint returns `{ publicKey, enabled }`. The
`applicationServerKey` option needs the decoded `Uint8Array`, not the base64url
string. Padding and translating `-` / `_` before `atob` avoids the browser's
opaque `InvalidCharacterError`.

`subscription.toJSON()` converts the browser's key buffers into the
`keys.p256dh` and `keys.auth` strings the server stores. It also returns
`expirationTime`, which is not part of the server request schema, so the example
sends only `{ endpoint, keys }`.

The subscribe, unsubscribe, and test endpoints require an authenticated session
or bearer token. The same-origin subscribe request above sends the existing
session cookie.

## Service worker

The server sends `notify.Payload` as JSON. The worker only has to show it and
open its URL when the notification is clicked:

```js
self.addEventListener("push", (event) => {
  const { title, body, url = "/", tag } = event.data.json();
  event.waitUntil(
    self.registration.showNotification(title, { body, tag, data: { url } }),
  );
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  event.waitUntil(self.clients.openWindow(event.notification.data.url));
});
```
