<!-- Overview page — aggregates status from all subsystems -->
<script lang="ts">
  import { onMount }      from 'svelte';
  import { systemStateStore } from '$lib/state.js';
  import * as api         from '$lib/api.js';
  import type { VaultStatus, FirewallStatus, UpdateStatus } from '$lib/api.js';

  let vaultData:    VaultStatus    | null = null;
  let firewallData: FirewallStatus | null = null;
  let updateData:   UpdateStatus   | null = null;
  let loading = true;
  let error: string | null = null;

  onMount(async () => {
    try {
      [vaultData, firewallData, updateData] = await Promise.all([
        api.vaultStatus(),
        api.firewallStatus(),
        api.updateStatus(),
      ]);
    } catch (e) {
      error = String(e);
    } finally {
      loading = false;
    }
  });

  $: flags = $systemStateStore?.flags;
</script>

<svelte:head><title>HisnOS — Overview</title></svelte:head>

<h1>Overview</h1>

{#if loading}
  <p class="muted mt"><span class="spinner"></span>Loading subsystem status…</p>
{:else if error}
  <div class="alert alert-error mt">{error}</div>
{:else}
  <div class="card-grid mt">
    <!-- Vault card -->
    <a href="/vault" class="card" style="text-decoration:none;color:inherit">
      <div class="card-title">Vault</div>
      <div class="card-value" style="color: {vaultData?.mounted ? 'var(--c-vault-mounted)' : 'var(--c-normal)'}">
        {vaultData?.mounted ? 'Mounted' : 'Locked'}
      </div>
      {#if vaultData?.mounted && vaultData.since}
        <div class="card-sub">since {vaultData.since}</div>
      {/if}
    </a>

    <!-- Firewall card -->
    <a href="/firewall" class="card" style="text-decoration:none;color:inherit">
      <div class="card-title">Firewall</div>
      <div class="card-value" style="color: {firewallData?.table_loaded ? 'var(--c-normal)' : 'var(--c-rollback-mode)'}">
        {firewallData?.table_loaded ? 'Enforcing' : 'Not loaded'}
      </div>
      {#if firewallData?.table_loaded}
        <div class="card-sub">{firewallData.rule_count} terminal rules</div>
      {:else if firewallData?.error}
        <div class="card-sub" style="color:var(--c-rollback-mode)">{firewallData.error}</div>
      {/if}
    </a>

    <!-- Update card -->
    <a href="/update" class="card" style="text-decoration:none;color:inherit">
      <div class="card-title">Update</div>
      <div class="card-value">
        {#if flags?.update_preparing}
          <span style="color:var(--c-update-preparing)">Preparing</span>
        {:else if flags?.update_pending_reboot}
          <span style="color:var(--c-update-pending)">Reboot pending</span>
        {:else if flags?.rollback_staged}
          <span style="color:var(--c-rollback-mode)">Rollback staged</span>
        {:else}
          <span style="color:var(--c-normal)">Idle</span>
        {/if}
      </div>
      {#if updateData?.state?.staged_deployment}
        <div class="card-sub">staged: {updateData.state.staged_deployment.slice(0, 12)}…</div>
      {/if}
    </a>

    <!-- System card -->
    <a href="/system" class="card" style="text-decoration:none;color:inherit">
      <div class="card-title">System mode</div>
      <div class="card-value">{$systemStateStore?.display ?? '—'}</div>
      {#if flags?.gaming}
        <div class="card-sub" style="color:var(--c-gaming)">GameMode active</div>
      {:else if flags?.lab_active}
        <div class="card-sub" style="color:var(--c-lab-active)">Lab VM running</div>
      {/if}
    </a>
  </div>
{/if}
