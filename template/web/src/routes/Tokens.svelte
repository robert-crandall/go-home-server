<script lang="ts">
  import { onMount } from 'svelte';
  import { push } from 'svelte-spa-router';
  import { api } from '../lib/api/client';
  import { auth } from '../lib/auth.svelte';
  import type { components } from '../lib/api/schema';

  type Token = components['schemas']['APIToken'];

  let tokens = $state<Token[]>([]);
  let loading = $state(true);
  let name = $state('');
  let expiresAt = $state(''); // datetime-local value; empty = never expires
  let creating = $state(false);
  let error = $state('');
  // The plaintext of a just-created token, shown exactly once.
  let freshToken = $state<string | null>(null);
  let copied = $state(false);

  onMount(async () => {
    if (!auth.user) {
      push('/login');
      return;
    }
    await load();
  });

  async function load() {
    loading = true;
    error = '';
    try {
      const { data, error: apiError } = await api.GET('/api/tokens');
      if (apiError) {
        error = 'Could not load your tokens.';
        return;
      }
      tokens = data ?? [];
    } catch {
      error = 'Could not load your tokens.';
    } finally {
      loading = false;
    }
  }

  async function create(event: Event) {
    event.preventDefault();
    if (!name.trim() || creating) return;
    creating = true;
    error = '';

    // A datetime-local value has no timezone; convert to an ISO (RFC3339)
    // instant the server can validate. Omit entirely for a non-expiring token.
    let expiresIso: string | undefined;
    if (expiresAt) {
      const when = new Date(expiresAt);
      if (Number.isNaN(when.getTime()) || when.getTime() <= Date.now()) {
        error = 'Expiry must be a valid date in the future.';
        creating = false;
        return;
      }
      expiresIso = when.toISOString();
    }

    const { data, error: apiError } = await api.POST('/api/tokens', {
      body: { name: name.trim(), expiresAt: expiresIso },
    });
    creating = false;
    if (apiError || !data) {
      error = 'Could not create the token.';
      return;
    }

    freshToken = data.token;
    copied = false;
    name = '';
    expiresAt = '';
    await load();
  }

  async function copyToken() {
    if (!freshToken) return;
    try {
      await navigator.clipboard.writeText(freshToken);
      copied = true;
    } catch {
      // Clipboard can be blocked (no HTTPS / permissions); the token is still
      // visible for a manual copy, so just leave the button unchanged.
    }
  }

  async function revoke(id: number) {
    error = '';
    const { error: apiError } = await api.DELETE('/api/tokens/{id}', {
      params: { path: { id } },
    });
    if (apiError) {
      error = 'Could not revoke that token.';
      return; // leave it in place if the server rejected it
    }
    tokens = tokens.filter((t) => t.id !== id);
  }

  function fmt(value: string | null): string {
    return value ? new Date(value).toLocaleString() : '—';
  }
</script>

<h1 class="mb-2 text-2xl font-bold">API tokens</h1>
<p class="mb-4 text-sm opacity-70">
  Bearer tokens for scripts, cron jobs, or an MCP server. Send them as
  <code class="rounded bg-base-300 px-1">Authorization: Bearer &lt;token&gt;</code>.
  A token has full access to your account, so treat it like a password.
</p>

{#if freshToken}
  <div class="alert alert-success mb-4 flex-col items-start gap-2" role="alert">
    <span class="font-semibold">Copy your new token now — it won't be shown again.</span>
    <code class="w-full break-all rounded bg-base-100 p-2 text-sm text-base-content">{freshToken}</code>
    <div class="flex gap-2">
      <button class="btn btn-sm" onclick={copyToken}>{copied ? 'Copied' : 'Copy'}</button>
      <button class="btn btn-ghost btn-sm" onclick={() => (freshToken = null)}>Dismiss</button>
    </div>
  </div>
{/if}

<form onsubmit={create} class="mb-6 flex flex-col gap-2 sm:flex-row sm:items-end">
  <label class="form-control flex-1">
    <span class="label-text mb-1">Name</span>
    <input
      class="input input-bordered w-full"
      placeholder="e.g. laptop-cli"
      aria-label="Token name"
      bind:value={name}
    />
  </label>
  <label class="form-control">
    <span class="label-text mb-1">Expires (optional)</span>
    <input
      type="datetime-local"
      class="input input-bordered"
      aria-label="Token expiry"
      bind:value={expiresAt}
    />
  </label>
  <button class="btn btn-primary" type="submit" disabled={creating}>Create</button>
</form>

{#if error}
  <div class="alert alert-error mb-4" role="alert">{error}</div>
{/if}

{#if loading}
  <span class="loading loading-spinner"></span>
{:else if tokens.length === 0}
  <p class="opacity-60">No tokens yet. Create one above.</p>
{:else}
  <ul class="flex flex-col gap-2">
    {#each tokens as token (token.id)}
      <li class="card bg-base-200">
        <div class="card-body flex-row items-center justify-between gap-4 py-3">
          <div class="min-w-0">
            <div class="truncate font-medium">
              {token.name}
              <span class="ml-1 font-mono text-sm opacity-60">…{token.last4}</span>
            </div>
            <div class="text-xs opacity-60">
              Created {fmt(token.createdAt)} · Last used {fmt(token.lastUsedAt)}
              {#if token.expiresAt}· Expires {fmt(token.expiresAt)}{/if}
            </div>
          </div>
          <button class="btn btn-ghost btn-sm" onclick={() => revoke(token.id)}>Revoke</button>
        </div>
      </li>
    {/each}
  </ul>
{/if}
