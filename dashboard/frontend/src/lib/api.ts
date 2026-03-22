/**
 * api.ts — Typed API client for the HisnOS Governance Dashboard
 *
 * All requests target the Go backend at http://127.0.0.1:7374.
 * In production the SvelteKit build is served by the same Go process,
 * so relative URLs (/api/...) resolve correctly.
 * In development Vite proxies /api/* → http://127.0.0.1:7374 (vite.config.ts).
 *
 * Destructive actions require the X-HisnOS-Confirm header.
 * Call getToken() once; the result is cached for the session lifetime.
 */

// ── Types ─────────────────────────────────────────────────────────────────

export interface SystemFlags {
  vault_mounted:         boolean;
  gaming:                boolean;
  lab_active:            boolean;
  update_preparing:      boolean;
  update_pending_reboot: boolean;
  rollback_staged:       boolean;
}

export type DisplayState =
  | 'normal'
  | 'vault-mounted'
  | 'gaming'
  | 'lab-active'
  | 'update-preparing'
  | 'update-pending-reboot'
  | 'rollback-mode';

export interface Guard {
  allowed:       boolean;
  block_reason?: string;
  warnings?:     string[];
}

export interface SystemStateResponse {
  flags:       SystemFlags;
  display:     DisplayState;
  transitions: Record<string, Guard>;
}

export interface VaultStatus {
  mounted:   boolean;
  since?:    string;
  lock_file: string;
}

export interface VaultTelemetry {
  mounted:                     boolean;
  mounted_duration?:           string;
  suspend_events_since_mount?: string;
  lazy_unmounts_7d?:           string;
  exposure_warning:            boolean;
  raw:                         Record<string, string>;
}

export interface FirewallStatus {
  nft_available: boolean;
  table_loaded:  boolean;
  rule_count:    number;
  error?:        string;
}

export interface KernelStatus {
  booted_checksum:    string;
  staged_checksum?:   string;
  kernel_override:    boolean;
  override_packages?: string[];
  error?:             string;
}

export interface UpdateStatus {
  state:        Record<string, string>;
  deployments?: unknown;
}

export interface ActionResult {
  success:          boolean;
  exit_code?:       number;
  output?:          string;
  stderr?:          string;
  reboot_required?: boolean;
  error?:           string;
  error_code?:      string;
}

// ── Confirm token ─────────────────────────────────────────────────────────

let _token: string | null = null;

export async function getToken(): Promise<string> {
  if (_token !== null) return _token;
  const r = await fetch('/api/confirm/token');
  if (!r.ok) throw new Error(`confirm token fetch failed: HTTP ${r.status}`);
  _token = (await r.json() as { token: string }).token;
  return _token;
}

/** Call after a 403 to force a fresh token fetch on the next request. */
export function invalidateToken(): void {
  _token = null;
}

// ── Internal helpers ──────────────────────────────────────────────────────

async function get<T>(path: string): Promise<T> {
  const r = await fetch(path);
  if (!r.ok) {
    const body = await r.text();
    throw new Error(`GET ${path} → ${r.status}: ${body}`);
  }
  return r.json() as Promise<T>;
}

async function post<T>(path: string, opts: { confirm?: boolean; body?: unknown } = {}): Promise<T> {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' };
  if (opts.confirm) {
    headers['X-HisnOS-Confirm'] = await getToken();
  }
  const r = await fetch(path, {
    method:  'POST',
    headers,
    body:    opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
  });
  if (r.status === 403) { invalidateToken(); }
  if (!r.ok) {
    const body = await r.text();
    throw new Error(`POST ${path} → ${r.status}: ${body}`);
  }
  return r.json() as Promise<T>;
}

// ── System state ──────────────────────────────────────────────────────────

export const systemState = () => get<SystemStateResponse>('/api/system/state');

export const systemMode = (mode: string) =>
  post<ActionResult>('/api/system/mode', { body: { mode } });

export const systemReboot = () =>
  post<{ success: boolean; rebooting: boolean }>('/api/system/reboot', { confirm: true });

// ── Vault ─────────────────────────────────────────────────────────────────

export const vaultStatus   = () => get<VaultStatus>('/api/vault/status');
export const vaultTelemetry = () => get<VaultTelemetry>('/api/vault/telemetry');

export const vaultLock = () =>
  post<ActionResult>('/api/vault/lock', { confirm: true });

export const vaultMount = (passphrase: string) =>
  post<ActionResult>('/api/vault/mount', { confirm: true, body: { passphrase } });

// ── Firewall ──────────────────────────────────────────────────────────────

export const firewallStatus = () => get<FirewallStatus>('/api/firewall/status');

export const firewallReload = () =>
  post<ActionResult>('/api/firewall/reload', { confirm: true });

// ── Kernel ────────────────────────────────────────────────────────────────

export const kernelStatus = () => get<KernelStatus>('/api/kernel/status');

// ── Update ────────────────────────────────────────────────────────────────

export const updateStatus   = () => get<UpdateStatus>('/api/update/status');
export const updateCheck    = () => post<ActionResult>('/api/update/check');
export const updateApply    = () => post<ActionResult>('/api/update/apply',    { confirm: true });
export const updateRollback = () => post<ActionResult>('/api/update/rollback', { confirm: true });
export const updateValidate = () => post<ActionResult>('/api/update/validate');

/**
 * updatePrepare — POST /api/update/prepare as a streaming SSE call.
 *
 * EventSource only supports GET, so we use fetch + ReadableStream.
 * Returns an AsyncGenerator that yields each output line from the server.
 * Throws on HTTP error before the stream starts.
 *
 * Usage:
 *   for await (const line of updatePrepare()) { ... }
 */
export async function* updatePrepare(): AsyncGenerator<string> {
  const token = await getToken();
  const r = await fetch('/api/update/prepare', {
    method:  'POST',
    headers: { 'X-HisnOS-Confirm': token },
  });

  if (r.status === 403) { invalidateToken(); }
  if (!r.ok || !r.body) throw new Error(`POST /api/update/prepare → ${r.status}`);

  const reader  = r.body.getReader();
  const decoder = new TextDecoder();
  let   buffer  = '';

  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;

      buffer += decoder.decode(value, { stream: true });

      // SSE messages are separated by \n\n; parse each complete message.
      const messages = buffer.split('\n\n');
      buffer = messages.pop() ?? '';  // last element may be incomplete

      for (const msg of messages) {
        for (const line of msg.split('\n')) {
          if (line.startsWith('data: ')) {
            try {
              // Server sends JSON-encoded strings (sseString helper in Go)
              yield JSON.parse(line.slice(6)) as string;
            } catch {
              yield line.slice(6);  // yield raw if JSON parse fails
            }
          }
        }
      }
    }
  } finally {
    reader.cancel();
  }
}
