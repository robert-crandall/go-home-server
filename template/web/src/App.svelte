<script lang="ts">
  import Router from 'svelte-spa-router';
  import { onMount } from 'svelte';
  import { auth } from './lib/auth.svelte';
  import Nav from './lib/Nav.svelte';
  import Home from './routes/Home.svelte';
  import Login from './routes/Login.svelte';
  import Notes from './routes/Notes.svelte';
  import Photos from './routes/Photos.svelte';
  import Tokens from './routes/Tokens.svelte';

  const routes = {
    '/': Home,
    '/login': Login,
    '/notes': Notes,
    '/photos': Photos,
    '/tokens': Tokens,
  };

  onMount(() => {
    void auth.refresh();
  });
</script>

<Nav />

<main class="container mx-auto max-w-2xl p-4">
  {#if auth.loading}
    <div class="flex justify-center p-8">
      <span class="loading loading-spinner loading-lg"></span>
    </div>
  {:else}
    <Router {routes} />
  {/if}
</main>
