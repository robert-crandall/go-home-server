<script lang="ts">
  import { link } from 'svelte-spa-router';
  import { auth } from '../lib/auth.svelte';
  import { subscribeToPush, type PushResult } from '../lib/push';

  let pushStatus = $state('');

  const messages: Record<PushResult, string> = {
    subscribed: 'Push notifications enabled.',
    denied: 'Permission was denied.',
    unsupported: 'Push is not supported in this browser.',
    disabled: 'Push is not configured on the server (no VAPID keys).',
    error: 'Could not save the subscription. Please try again.',
  };

  async function enablePush() {
    pushStatus = 'Requesting…';
    pushStatus = messages[await subscribeToPush()];
  }
</script>

<div class="prose">
  <h1>Welcome</h1>
  <p>This is the reference app for the <code>go-home-server</code> foundation.</p>
</div>

{#if auth.user}
  <div class="mt-6 flex flex-col items-start gap-3">
    <a class="btn btn-primary" href="/notes" use:link>Go to your notes</a>
    <button class="btn btn-outline" onclick={enablePush}>Enable push notifications</button>
    {#if pushStatus}
      <p class="text-sm opacity-80">{pushStatus}</p>
    {/if}
  </div>
{:else}
  <p class="mt-6">
    <a class="link link-primary" href="/login" use:link>Log in</a> to get started.
  </p>
{/if}
