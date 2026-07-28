<script lang="ts">
  import { push } from 'svelte-spa-router';
  import { auth } from '../lib/auth.svelte';

  let mode = $state<'login' | 'register'>('login');
  let email = $state('');
  let password = $state('');
  let error = $state('');
  let busy = $state(false);

  async function submit(event: Event) {
    event.preventDefault();
    error = '';
    busy = true;
    try {
      if (mode === 'login') {
        await auth.login(email, password);
      } else {
        await auth.register(email, password);
      }
      push('/');
    } catch (err) {
      error = err instanceof Error ? err.message : 'Something went wrong';
    } finally {
      busy = false;
    }
  }

  function toggleMode() {
    mode = mode === 'login' ? 'register' : 'login';
    error = '';
  }
</script>

<div class="card bg-base-200 mx-auto mt-8 max-w-sm">
  <div class="card-body">
    <h2 class="card-title">{mode === 'login' ? 'Log in' : 'Create account'}</h2>
    <form onsubmit={submit} class="flex flex-col gap-3">
      <input
        class="input input-bordered w-full"
        type="email"
        placeholder="Email"
        aria-label="Email"
        bind:value={email}
        autocomplete="email"
        required
      />
      <input
        class="input input-bordered w-full"
        type="password"
        placeholder="Password"
        aria-label="Password"
        bind:value={password}
        minlength="8"
        autocomplete="current-password"
        required
      />
      {#if error}
        <p class="text-error text-sm">{error}</p>
      {/if}
      <button class="btn btn-primary" type="submit" disabled={busy}>
        {mode === 'login' ? 'Log in' : 'Register'}
      </button>
    </form>
    <button class="btn btn-link btn-sm" onclick={toggleMode}>
      {mode === 'login' ? 'Need an account? Register' : 'Have an account? Log in'}
    </button>
  </div>
</div>
