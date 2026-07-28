<script lang="ts">
  import { onMount } from 'svelte';
  import { push } from 'svelte-spa-router';
  import { Plus, Trash2 } from '@lucide/svelte';
  import { api } from '../lib/api/client';
  import { auth } from '../lib/auth.svelte';
  import type { components } from '../lib/api/schema';

  type Note = components['schemas']['Note'];

  let notes = $state<Note[]>([]);
  let body = $state('');
  let loading = $state(true);

  onMount(async () => {
    if (!auth.user) {
      push('/login');
      return;
    }
    await load();
  });

  async function load() {
    loading = true;
    const { data } = await api.GET('/api/notes');
    notes = data ?? [];
    loading = false;
  }

  async function add(event: Event) {
    event.preventDefault();
    if (!body.trim()) return;
    const { data } = await api.POST('/api/notes', { body: { body } });
    if (data) {
      notes = [data, ...notes];
      body = '';
    }
  }

  async function remove(id: number) {
    const { error } = await api.DELETE('/api/notes/{id}', { params: { path: { id } } });
    if (error) return; // leave the note in place if the server rejected it
    notes = notes.filter((note) => note.id !== id);
  }
</script>

<h1 class="mb-4 text-2xl font-bold">Your notes</h1>

<form onsubmit={add} class="mb-4 flex gap-2">
  <input
    class="input input-bordered flex-1"
    placeholder="New note…"
    aria-label="New note"
    bind:value={body}
  />
  <button class="btn btn-primary gap-1.5" type="submit">
    <Plus size={16} />
    Add
  </button>
</form>

{#if loading}
  <span class="loading loading-spinner"></span>
{:else if notes.length === 0}
  <p class="opacity-60">No notes yet. Add your first above.</p>
{:else}
  <ul class="flex flex-col gap-2">
    {#each notes as note (note.id)}
      <li class="card bg-base-200">
        <div class="card-body flex-row items-center justify-between py-3">
          <span>{note.body}</span>
          <button
            class="btn btn-ghost btn-square btn-sm"
            aria-label="Delete note"
            onclick={() => remove(note.id)}
          >
            <Trash2 size={16} />
          </button>
        </div>
      </li>
    {/each}
  </ul>
{/if}
