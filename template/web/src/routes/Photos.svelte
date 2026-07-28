<script lang="ts">
  import { onMount } from 'svelte';
  import { push } from 'svelte-spa-router';
  import { Trash2, Upload } from '@lucide/svelte';
  import { api } from '../lib/api/client';
  import { auth } from '../lib/auth.svelte';
  import type { components } from '../lib/api/schema';

  type FileMeta = components['schemas']['File'];

  let files = $state<FileMeta[]>([]);
  let loading = $state(true);
  let uploading = $state(false);
  let error = $state('');
  let input: HTMLInputElement;

  onMount(async () => {
    if (!auth.user) {
      push('/login');
      return;
    }
    await load();
  });

  async function load() {
    loading = true;
    try {
      const { data } = await api.GET('/api/files');
      files = data ?? [];
    } catch {
      error = 'Could not load your photos.';
    } finally {
      loading = false;
    }
  }

  // openapi-fetch has no multipart story, so uploads use plain fetch with a
  // FormData body. Everything else goes through the typed client.
  async function upload(event: Event) {
    const picked = (event.target as HTMLInputElement).files;
    if (!picked?.length) return;

    uploading = true;
    error = '';
    const uploaded: FileMeta[] = [];

    try {
      for (const file of Array.from(picked)) {
        const form = new FormData();
        form.append('file', file);
        const res = await fetch('/api/files', { method: 'POST', body: form });
        if (!res.ok) {
          error = `${file.name}: ${res.status === 413 ? 'too large' : 'upload failed'}`;
          break;
        }
        uploaded.push((await res.json()) as FileMeta);
      }
    } catch {
      // A dropped connection mid-upload is normal on a phone. Keep whatever
      // succeeded and let the user retry the rest.
      error = 'Upload interrupted. Check your connection and try again.';
    } finally {
      files = [...uploaded.reverse(), ...files];
      uploading = false;
      input.value = ''; // let the same file be picked again
    }
  }

  async function remove(id: number) {
    const { error: err } = await api.DELETE('/api/files/{id}', { params: { path: { id } } });
    if (err) return; // leave it in place if the server rejected the delete
    files = files.filter((f) => f.id !== id);
  }

  function isImage(contentType: string) {
    return contentType.startsWith('image/');
  }

  function humanSize(bytes: number) {
    if (bytes < 1024) return `${bytes} B`;
    if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`;
    return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
  }
</script>

<h1 class="mb-4 text-2xl font-bold">Your photos</h1>

<div class="mb-4 flex items-center gap-3">
  <input
    bind:this={input}
    class="file-input file-input-bordered flex-1"
    type="file"
    multiple
    accept="image/*"
    aria-label="Choose photos to upload"
    disabled={uploading}
    onchange={upload}
  />
  {#if uploading}
    <span class="loading loading-spinner"></span>
  {:else}
    <Upload size={20} class="opacity-60" />
  {/if}
</div>

{#if error}
  <div class="alert alert-error mb-4">{error}</div>
{/if}

{#if loading}
  <span class="loading loading-spinner"></span>
{:else if files.length === 0}
  <p class="opacity-60">No photos yet. Upload your first above.</p>
{:else}
  <ul class="grid grid-cols-2 gap-3 sm:grid-cols-3">
    {#each files as file (file.id)}
      <li class="card bg-base-200 overflow-hidden">
        <a href="/api/files/{file.id}" target="_blank" rel="noopener">
          {#if isImage(file.contentType)}
            <img
              class="aspect-square w-full object-cover"
              src="/api/files/{file.id}"
              alt={file.filename}
              loading="lazy"
            />
          {:else}
            <div class="bg-base-300 flex aspect-square items-center justify-center p-2">
              <span class="text-center text-xs break-all opacity-70">{file.filename}</span>
            </div>
          {/if}
        </a>
        <div class="flex items-center justify-between gap-1 p-2">
          <span class="truncate text-xs opacity-70" title={file.filename}>
            {humanSize(file.size)}
          </span>
          <button
            class="btn btn-ghost btn-square btn-xs"
            aria-label="Delete {file.filename}"
            onclick={() => remove(file.id)}
          >
            <Trash2 size={14} />
          </button>
        </div>
      </li>
    {/each}
  </ul>
{/if}
