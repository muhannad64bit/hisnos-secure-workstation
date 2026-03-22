<!-- Vault control panel -->
<script lang="ts">
  import { onMount }      from 'svelte';
  import { refreshState } from '$lib/state.js';
  import * as api         from '$lib/api.js';
  import type { VaultStatus, VaultTelemetry } from '$lib/api.js';
  import ConfirmModal     from '$lib/components/ConfirmModal.svelte';

  let status:    VaultStatus    | null = null;
  let telemetry: VaultTelemetry | null = null;
  let loading = true;
  let error:   string | null = null;

  // Mount form
  let passphrase = '';
  let mounting   = false;
  let mountError: string | null = null;
  let mountSuccess = false;

  // Lock modal
  let showLockModal = false;
  let locking       = false;
  let lockError:    string | null = null;

  // Telemetry loading
  let loadingTelemetry = false;

  async function load() {
    loading = true;
    error   = null;
    try {
      status = await api.vaultStatus();
    } catch (e) {
      error = String(e);
    } finally {
      loading = false;
    }
  }

  async function loadTelemetry() {
    loadingTelemetry = true;
    try {
      telemetry = await api.vaultTelemetry();
    } catch {
      telemetry = null;
    } finally {
      loadingTelemetry = false;
    }
  }

  onMount(() => {
    load();
    if (status?.mounted) loadTelemetry();
  });

  async function handleMount() {
    if (!passphrase) return;
    mounting    = true;
    mountError  = null;
    mountSuccess = false;
    try {
      const r = await api.vaultMount(passphrase);
      if (r.success) {
        mountSuccess = true;
        passphrase   = '';
        await load();
        await loadTelemetry();
        refreshState();
      } else {
        mountError = r.error ?? 'Mount failed';
      }
    } catch (e) {
      mountError = String(e);
    } finally {
      mounting = false;
    }
  }

  async function handleLock() {
    showLockModal = false;
    locking       = true;
    lockError     = null;
    try {
      await api.vaultLock();
      await load();
      telemetry = null;
      refreshState();
    } catch (e) {
      lockError = String(e);
    } finally {
      locking = false;
    }
  }
</script>

<svelte:head><title>HisnOS — Vault</title></svelte:head>

<h1>Vault</h1>

{#if loading}
  <p class="muted mt"><span class="spinner"></span>Loading…</p>
{:else if error}
  <div class="alert alert-error mt">{error}</div>
{:else}
  <!-- ── Status ──────────────────────────────────────────────────────────── -->
  <div class="section mt">
    <div class="section-title">Status</div>
    <div class="card flex-row" style="justify-content:space-between">
      <div>
        <span class="badge {status?.mounted ? 'vault-mounted' : 'normal'}">
          {status?.mounted ? 'Mounted' : 'Locked'}
        </span>
        {#if status?.mounted && status.since}
          <span class="muted" style="font-size:.8rem;margin-left:.5rem">since {status.since}</span>
        {/if}
      </div>
      {#if status?.mounted}
        <button class="danger" on:click={() => showLockModal = true} disabled={locking}>
          {locking ? 'Locking…' : 'Lock vault'}
        </button>
      {/if}
    </div>
    {#if lockError}
      <div class="alert alert-error mt">{lockError}</div>
    {/if}
  </div>

  <!-- ── Mount form (only when locked) ─────────────────────────────────── -->
  {#if !status?.mounted}
    <div class="section">
      <div class="section-title">Mount vault</div>
      <div class="card">
        {#if mountSuccess}
          <div class="alert alert-ok">Vault mounted successfully.</div>
        {:else}
          {#if mountError}
            <div class="alert alert-error">{mountError}</div>
          {/if}
          <form on:submit|preventDefault={handleMount}>
            <div class="form-row">
              <label for="passphrase">Passphrase</label>
              <input
                id="passphrase"
                type="password"
                bind:value={passphrase}
                placeholder="vault passphrase"
                autocomplete="off"
                disabled={mounting}
              />
            </div>
            <div class="form-actions">
              <button type="submit" class="primary" disabled={mounting || !passphrase}>
                {mounting ? 'Mounting…' : 'Mount'}
              </button>
            </div>
          </form>
        {/if}
      </div>
    </div>
  {/if}

  <!-- ── Telemetry (only when mounted) ─────────────────────────────────── -->
  {#if status?.mounted}
    <div class="section">
      <div class="section-title">
        Exposure telemetry
        <button style="float:right;padding:.2rem .5rem;font-size:.75rem"
          on:click={loadTelemetry} disabled={loadingTelemetry}>
          {loadingTelemetry ? '…' : '↻'}
        </button>
      </div>

      {#if telemetry?.exposure_warning}
        <div class="alert alert-warn">
          ⚠ Exposure warning — vault may have been mounted during a suspend cycle without locking.
        </div>
      {/if}

      {#if telemetry}
        <div class="card">
          <table style="border-collapse:collapse;width:100%;font-size:.82rem">
            <tbody>
              {#if telemetry.mounted_duration}
                <tr>
                  <td class="muted" style="padding:.3rem .5rem">Mounted for</td>
                  <td style="padding:.3rem .5rem">{telemetry.mounted_duration}</td>
                </tr>
              {/if}
              {#if telemetry.suspend_events_since_mount}
                <tr>
                  <td class="muted" style="padding:.3rem .5rem">Suspend events since mount</td>
                  <td style="padding:.3rem .5rem">{telemetry.suspend_events_since_mount}</td>
                </tr>
              {/if}
              {#if telemetry.lazy_unmounts_7d}
                <tr>
                  <td class="muted" style="padding:.3rem .5rem">Lazy unmounts (7 days)</td>
                  <td style="padding:.3rem .5rem">{telemetry.lazy_unmounts_7d}</td>
                </tr>
              {/if}
            </tbody>
          </table>
        </div>
      {:else if loadingTelemetry}
        <p class="muted"><span class="spinner"></span>Loading telemetry…</p>
      {:else}
        <p class="muted">No telemetry data.</p>
      {/if}
    </div>
  {/if}
{/if}

<!-- Lock confirm modal -->
<ConfirmModal
  open={showLockModal}
  title="Lock vault"
  message="This will immediately unmount the vault. Any process with open files inside will lose access."
  variant="danger"
  on:confirm={handleLock}
  on:cancel={() => showLockModal = false}
/>
