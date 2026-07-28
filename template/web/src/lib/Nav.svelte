<script lang="ts">
  import { link, push } from 'svelte-spa-router';
  import { House, Images, KeyRound, LogIn, LogOut, NotebookPen } from '@lucide/svelte';
  import { auth } from './auth.svelte';

  async function handleLogout() {
    await auth.logout();
    push('/login');
  }
</script>

<div class="navbar bg-base-200 px-4">
  <div class="flex-1">
    <a class="btn btn-ghost gap-2 text-xl" href="/" use:link>
      <House size={20} />
      Example App
    </a>
  </div>
  <div class="flex-none items-center gap-2">
    {#if auth.user}
      <a class="btn btn-ghost btn-sm gap-1.5" href="/notes" use:link>
        <NotebookPen size={16} />
        <span class="sr-only sm:not-sr-only">Notes</span>
      </a>
      <a class="btn btn-ghost btn-sm gap-1.5" href="/photos" use:link>
        <Images size={16} />
        <span class="sr-only sm:not-sr-only">Photos</span>
      </a>
      <a class="btn btn-ghost btn-sm gap-1.5" href="/tokens" use:link>
        <KeyRound size={16} />
        <span class="sr-only sm:not-sr-only">Tokens</span>
      </a>
      <span class="hidden text-sm opacity-70 sm:inline">{auth.user.email}</span>
      <button class="btn btn-sm gap-1.5" onclick={handleLogout}>
        <LogOut size={16} />
        <span class="sr-only sm:not-sr-only">Log out</span>
      </button>
    {:else}
      <a class="btn btn-primary btn-sm gap-1.5" href="/login" use:link>
        <LogIn size={16} />
        Log in
      </a>
    {/if}
  </div>
</div>
