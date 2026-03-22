/**
 * state.ts — Reactive system state store
 *
 * Polls GET /api/system/state every POLL_INTERVAL_MS and exposes the result
 * as a Svelte writable store that all pages can subscribe to.
 *
 * Usage in +layout.svelte:
 *   import { systemStateStore, startPolling, stopPolling } from '$lib/state';
 *   onMount(() => { startPolling(); return stopPolling; });
 */

import { writable, derived } from 'svelte/store';
import type { SystemStateResponse, DisplayState } from '$lib/api.js';

const POLL_INTERVAL_MS = 5_000;

// ── Stores ────────────────────────────────────────────────────────────────

/** Full system state response from GET /api/system/state. Null until first fetch. */
export const systemStateStore = writable<SystemStateResponse | null>(null);

/** Convenience derived store: just the display state string. */
export const displayState = derived(
  systemStateStore,
  ($s): DisplayState | null => $s?.display ?? null,
);

/** Convenience derived store: vault mounted flag. */
export const vaultMounted = derived(
  systemStateStore,
  ($s): boolean => $s?.flags.vault_mounted ?? false,
);

// ── Polling ───────────────────────────────────────────────────────────────

let _timer: ReturnType<typeof setInterval> | null = null;

async function fetchState(): Promise<void> {
  try {
    const r = await fetch('/api/system/state');
    if (r.ok) {
      systemStateStore.set(await r.json() as SystemStateResponse);
    }
  } catch {
    // Network errors are silently ignored; store retains last known state.
  }
}

export function startPolling(): void {
  if (_timer !== null) return;
  void fetchState();                               // immediate first fetch
  _timer = setInterval(() => void fetchState(), POLL_INTERVAL_MS);
}

export function stopPolling(): void {
  if (_timer !== null) {
    clearInterval(_timer);
    _timer = null;
  }
}

/** Force an immediate refresh (e.g., after a mutating API call). */
export function refreshState(): void {
  void fetchState();
}
