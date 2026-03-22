<!-- System page — global mode selector + reboot control -->
<script lang="ts">
  import { systemStateStore, refreshState } from '$lib/state.js';
  import * as api      from '$lib/api.js';
  import type { ActionResult } from '$lib/api.js';
  import ConfirmModal  from '$lib/components/ConfirmModal.svelte';

  // Operator-selectable modes (lifecycle modes are subsystem-managed, not user-selectable).
  const operatorModes: { value: string; label: string; desc: string }[] = [
    { value: 'normal',     label: 'Normal',     desc: 'Standard desktop use. Vault idle timer active.' },
    { value: 'gaming',     label: 'Gaming',     desc: 'GameMode active. Vault idle timer suspended.' },
    { value: 'lab-active', label: 'Lab Active', desc: 'Lab VM or Distrobox session running.' },
  ];

  let settingMode  = false;
  let modeResult:  ActionResult | null = null;
  let modeError:   string | null = null;

  let showRebootModal = false;
  let rebooting       = false;
  let rebootError:    string | null = null;

  $: currentDisplay = $systemStateStore?.display ?? 'normal';
  $: flags          = $systemStateStore?.flags;
  $: rebootGuard    = $systemStateStore?.transitions['system.reboot'];

  async function setMode(mode: string) {
    if (mode === currentDisplay) return;
    settingMode = true; modeResult = null; modeError = null;
    try {
      modeResult = await api.systemMode(mode);
      refreshState();
    } catch (e) {
      modeError = String(e);
    } finally {
      settingMode = false;
    }
  }

  async function handleReboot() {
    showRebootModal = false;
    rebooting       = true;
    rebootError     = null;
    try {
      await api.systemReboot();
      // After this call succeeds the machine will reboot; show a holding message.
    } catch (e) {
      rebootError = String(e);
      rebooting   = false;
    }
  }
</script>

<svelte:head><title>HisnOS — System</title></svelte:head>

<h1>System</h1>

<!-- ── Current state ──────────────────────────────────────────────────── -->
<div class="section mt">
  <div class="section-title">Current state</div>
  <div class="card flex-row" style="flex-wrap:wrap;gap:.75rem">
    <span class="badge {currentDisplay}">{currentDisplay}</span>
    {#if flags?.vault_mounted}
      <span class="badge vault-mounted">Vault mounted</span>
    {/if}
    {#if flags?.update_pending_reboot}
      <span class="badge update-pending-reboot">Reboot pending</span>
    {/if}
    {#if flags?.rollback_staged}
      <span class="badge rollback-mode">Rollback staged</span>
    {/if}
  </div>
</div>

<!-- ── Mode selector ──────────────────────────────────────────────────── -->
<div class="section">
  <div class="section-title">Operating mode</div>
  <div class="card">
    <p class="muted" style="font-size:.82rem;margin-bottom:.75rem">
      Operator-driven modes. Lifecycle modes (update-preparing, rollback-mode, etc.)
      are set automatically by subsystem operations.
    </p>

    <div style="display:flex;flex-direction:column;gap:.5rem">
      {#each operatorModes as m}
        {@const active = currentDisplay === m.value}
        <button
          on:click={() => setMode(m.value)}
          disabled={settingMode || active}
          style="text-align:left;padding:.6rem .85rem;{active ? 'border-color:var(--c-normal);color:var(--c-normal)' : ''}"
        >
          <span style="font-weight:600">{m.label}</span>
          {#if active}<span style="margin-left:.5rem;font-size:.75rem">(current)</span>{/if}
          <span class="muted" style="display:block;font-size:.78rem;margin-top:.15rem">{m.desc}</span>
        </button>
      {/each}
    </div>

    {#if modeError}
      <div class="alert alert-error mt">{modeError}</div>
    {:else if modeResult?.success === false}
      <div class="alert alert-error mt">{modeResult.error ?? 'Mode transition failed'}</div>
    {:else if modeResult?.success}
      <div class="alert alert-ok mt">Mode updated.</div>
    {/if}
  </div>
</div>

<!-- ── Reboot ──────────────────────────────────────────────────────────── -->
<div class="section">
  <div class="section-title">Reboot</div>
  <div class="card">
    {#if rebooting}
      <div class="alert alert-warn">
        <span class="spinner"></span>Reboot initiated — system will restart shortly…
      </div>
    {:else}
      <p class="muted" style="font-size:.82rem;margin-bottom:.75rem">
        Required to apply a staged update or rollback. The vault will be locked before the system reboots.
      </p>

      {#if rebootGuard && !rebootGuard.allowed}
        <div class="alert alert-error" style="margin-bottom:.75rem">{rebootGuard.block_reason}</div>
      {:else if rebootGuard?.warnings?.length}
        {#each rebootGuard.warnings as w}
          <div class="alert alert-warn" style="margin-bottom:.4rem">⚠ {w}</div>
        {/each}
      {/if}

      {#if flags?.update_pending_reboot || flags?.rollback_staged}
        <div class="alert alert-info" style="margin-bottom:.75rem">
          A staged deployment is ready. Rebooting will apply it.
        </div>
      {/if}

      <button
        class="danger"
        on:click={() => showRebootModal = true}
        disabled={rebooting || !rebootGuard?.allowed}
        title={rebootGuard?.block_reason ?? ''}
      >
        Reboot system
      </button>

      {#if rebootError}
        <div class="alert alert-error mt">{rebootError}</div>
      {/if}
    {/if}
  </div>
</div>

<ConfirmModal
  open={showRebootModal}
  title="Reboot system"
  message="The system will reboot now. All unsaved work will be lost. The vault will be locked automatically."
  warnings={rebootGuard?.warnings ?? []}
  variant="danger"
  on:confirm={handleReboot}
  on:cancel={() => showRebootModal = false}
/>
