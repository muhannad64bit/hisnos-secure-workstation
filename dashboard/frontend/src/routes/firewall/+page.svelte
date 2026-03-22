<!-- Firewall panel -->
<script lang="ts">
  import { onMount }      from 'svelte';
  import { systemStateStore } from '$lib/state.js';
  import * as api         from '$lib/api.js';
  import type { FirewallStatus } from '$lib/api.js';
  import ConfirmModal     from '$lib/components/ConfirmModal.svelte';

  let status:  FirewallStatus | null = null;
  let loading  = true;
  let error:   string | null = null;

  let showReloadModal = false;
  let reloading       = false;
  let reloadResult:   string | null = null;
  let reloadError:    string | null = null;

  async function load() {
    loading = true; error = null;
    try { status = await api.firewallStatus(); }
    catch (e) { error = String(e); }
    finally { loading = false; }
  }

  onMount(load);

  // Derive guard from control-plane transitions
  $: reloadGuard = $systemStateStore?.transitions['firewall.reload'];

  async function handleReload() {
    showReloadModal = false;
    reloading       = true;
    reloadResult    = null;
    reloadError     = null;
    try {
      const r = await api.firewallReload();
      if (r.success) {
        reloadResult = 'Firewall reloaded successfully.';
        await load();
      } else {
        reloadError = r.error ?? r.stderr ?? 'Reload failed';
      }
    } catch (e) {
      reloadError = String(e);
    } finally {
      reloading = false;
    }
  }
</script>

<svelte:head><title>HisnOS — Firewall</title></svelte:head>

<h1>Firewall</h1>

{#if loading}
  <p class="muted mt"><span class="spinner"></span>Loading…</p>
{:else if error}
  <div class="alert alert-error mt">{error}</div>
{:else}
  <!-- ── Status card ─────────────────────────────────────────────────────── -->
  <div class="section mt">
    <div class="section-title">nftables status</div>
    <div class="card">
      <div class="flex-row" style="justify-content:space-between;margin-bottom:.5rem">
        <div class="flex-row">
          <span class="badge {status?.table_loaded ? 'normal' : 'rollback-mode'}">
            {status?.table_loaded ? 'Enforcing' : 'Not loaded'}
          </span>
          {#if status?.nft_available && !status.table_loaded}
            <span class="badge lab-active">nft available</span>
          {/if}
        </div>
        <button
          on:click={() => showReloadModal = true}
          disabled={reloading || !reloadGuard?.allowed}
          class="warn"
          title={reloadGuard?.block_reason ?? ''}
        >
          {reloading ? 'Reloading…' : 'Reload rules'}
        </button>
      </div>

      {#if status?.table_loaded}
        <div class="flex-row">
          <span class="muted">Terminal rules:</span>
          <strong>{status.rule_count}</strong>
          <span class="muted">(accept / drop / reject / queue)</span>
        </div>
      {:else if status?.error}
        <div class="alert alert-error" style="margin-top:.5rem">{status.error}</div>
      {/if}
    </div>
  </div>

  <!-- ── Guard warnings ─────────────────────────────────────────────────── -->
  {#if reloadGuard && !reloadGuard.allowed}
    <div class="alert alert-error">
      Firewall reload blocked: {reloadGuard.block_reason}
    </div>
  {:else if reloadGuard?.warnings?.length}
    {#each reloadGuard.warnings as w}
      <div class="alert alert-warn">⚠ {w}</div>
    {/each}
  {/if}

  <!-- ── Action result ──────────────────────────────────────────────────── -->
  {#if reloadResult}
    <div class="alert alert-ok">{reloadResult}</div>
  {/if}
  {#if reloadError}
    <div class="alert alert-error">{reloadError}</div>
  {/if}
{/if}

<ConfirmModal
  open={showReloadModal}
  title="Reload firewall rules"
  message="This will re-apply the nftables policy from disk. A brief window of reduced enforcement may occur."
  warnings={reloadGuard?.warnings ?? []}
  variant="warn"
  on:confirm={handleReload}
  on:cancel={() => showReloadModal = false}
/>
