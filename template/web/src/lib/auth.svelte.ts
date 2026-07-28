import { api } from './api/client';
import type { components } from './api/schema';

type User = components['schemas']['User'];

// A small typed data layer over the API client, using Svelte 5 runes so any
// component reading `auth.user` reacts to login/logout automatically.
class AuthStore {
  user = $state<User | null>(null);
  loading = $state(true);

  async refresh(): Promise<void> {
    try {
      const { data } = await api.GET('/api/auth/me');
      this.user = data ?? null;
    } catch {
      // Network failure (openapi-fetch throws on a rejected fetch): treat as
      // logged-out rather than leaving the app stuck on the loading spinner.
      this.user = null;
    } finally {
      this.loading = false;
    }
  }

  async login(email: string, password: string): Promise<void> {
    const { data, error } = await api.POST('/api/auth/login', {
      body: { email, password },
    });
    if (error) throw new Error('Invalid email or password');
    this.user = data ?? null;
  }

  async register(email: string, password: string): Promise<void> {
    const { data, error } = await api.POST('/api/auth/register', {
      body: { email, password },
    });
    if (error) throw new Error('Could not create account');
    this.user = data ?? null;
  }

  async logout(): Promise<void> {
    await api.POST('/api/auth/logout');
    this.user = null;
  }
}

export const auth = new AuthStore();
