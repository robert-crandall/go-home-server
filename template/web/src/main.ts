import './app.css';
import { mount } from 'svelte';
import { registerSW } from 'virtual:pwa-register';
import App from './App.svelte';

const app = mount(App, { target: document.getElementById('app')! });

// Keep the UI current. registerType 'autoUpdate' reloads the page once a new
// service worker takes control (the SW calls skipWaiting + clientsClaim). The
// browser only checks for a new SW on navigation and roughly once a day; this
// app uses hash-based routing (so it does no real navigations) and an installed
// iOS PWA can stay open for days. Nudge the check when the app regains focus and
// hourly while it's open so a fresh deploy lands without a manual refresh.
registerSW({
  immediate: true,
  onRegisteredSW(_swScriptUrl, registration) {
    if (!registration) return;
    // update() rejects on routine network failures (e.g. launching the
    // installed PWA offline); swallow it so it doesn't surface as an
    // unhandledrejection. A real update lands on the next successful check.
    const check = () => void registration.update().catch(() => {});
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState === 'visible') check();
    });
    setInterval(check, 60 * 60 * 1000);
  },
});

export default app;
