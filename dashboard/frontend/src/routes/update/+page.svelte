<!-- Update control panel -->
<script lang="ts">
  import { onMount }           from 'svelte';
  import { systemStateStore, refreshState } from '$lib/state.js';
  import * as api              from '$lib/api.js';
  import type { UpdateStatus, ActionResult } from '$lib/api.js';
  import ConfirmModal          from '$lib/components/ConfirmModal.svelte';

  let updateStatus: UpdateStatus | null = null;
  let loading  = true;
  let error:   string | null = null;

  // Per-action state
  let checking  = false; let checkResult:   ActionResult | null = null;
  let preparing = false; let prepareLines:  string[]    = [];  let prepareError: string | null = null;
  let applying  = false; let applyResult:   ActionResult | null = null;
  let rolling   = false; let rollbackResult: ActionResult | null = null;
  let validating = false; let validateResult: ActionResult | null = null;

  // Confirm modals
  let showApply    = false;
  let showRollback = false;

  async function load() {
    loading = true; error = null;
    try { updateStatus = await api.updateStatus(); }
    catch (e) { error = String(e); }
    finally { loading = false; }
  }

  onMount(load);

  $: transitions = $systemStateStore?.transitions ?? {};
  $: prepareGuard  = transitions['update.prepare'];
  $: applyGuard    = transitions['update.apply'];
  $: rollbackGuard = transitions['update.rollback'];
  $: flags         = $systemStateStore?.flags;

  async function handleCheck() {
    checking = true; checkResult = null;
    try { checkResult = await api.updateCheck(); }
    catch (e) { checkResult = { success: false, error: String(e) }; }
    finally { checking = false; }
  }

  async function handlePrepare() {
    preparing    = true;
    prepareLines = [];
    prepareError = null;
    try {
      for await (const line of api.updatePrepare()) {
        prepareLines = [...prepareLines, line];
      }
      await load();
      refreshState();
    } catch (e) {
      prepareError = String(e);
    } finally {
      preparing = false;
    }
  }

  async function handleApply() {
    showApply = false;
    applying  = true; applyResult = null;
    try {
      applyResult = await api.updateApply();
      if (applyResult.success) { await load(); refreshState(); }
    } catch (e) {
      applyResult = { success: false, error: String(e) };
    } finally {
      applying = false;
    }
  }

  async function handleRollback() {
    showRollback  = false;
    rolling       = true; rollbackResult = null;
    try {
      rollbackResult = await api.updateRollback();
      if (rollbackResult.success) { await load(); refreshState(); }
    } catch (e) {
      rollbackResult = { success: false, error: String(e) };
    } finally {
      rolling = false;
    }
  }

  async function handleValidate() {
    validating = true; validateResult = null;
    try {
      validateResult = await api.updateValidate();
      refreshState();
    } catch (e) {
      validateResult = { success: false, error: String(e) };
    } finally {
      validating = false;
    }
  }
</script>

<svelte:head><title>HisnOS — Update</title></svelte:head>

<h1>System Update</h1>

<!-- ── Deployment state badges ─────────────────────────────────────────── -->
<div class="flex-row mt" style="flex-wrap:wrap;gap:.5rem">
  {#if flags?.update_preparing}
    <span class="badge update-preparing">Preparing update…</span>
  {:else if flags?.rollback_staged}
    <span class="badge rollback-mode">Rollback staged — reboot required</span>
  {:else if flags?.update_pending_reboot}
    <span class="badge update-pending-reboot">Update staged — reboot required</span>
  {:else}
    <span class="badge normal">No pending deployment</span>
  {/if}

  {#if updateStatus?.state?.staged_deployment}
    <code style="font-size:.75rem;color:var(--text-muted)">
      {updateStatus.state.staged_deployment.slice(0, 16)}…
    </code>
  {/if}
</div>

{#if loading}
  <p class="muted mt"><span class="spinner"></span>Loading…</p>
{:else if error}
  <div class="alert alert-error mt">{error}</div>
{:else}
  <!-- ── Check ────────────────────────────────────────────────────────────── -->
  <div class="section mt2">
    <div class="section-title">Check for updates</div>
    <div class="card">
      <p class="muted" style="font-size:.82rem;margin-bottom:.75rem">
        Queries the Fedora update server. Does not download or stage anything.
      </p>
      <button on:click={handleCheck} disabled={checking}>
        {checking ? 'Checking…' : 'Check now'}
      </button>
      {#if checkResult}
        <div class="log-box mt" style="max-height:120px">
          {checkResult.output ?? checkResult.error ?? JSON.stringify(checkResult)}
        </div>
      {/if}
    </div>
  </div>

  <!-- ── Prepare ──────────────────────────────────────────────────────────── -->
  <div class="section">
    <div class="section-title">Prepare (download &amp; stage)</div>
    <div class="card">
      {#if prepareGuard && !prepareGuard.allowed}
        <div class="alert alert-error" style="margin-bottom:.75rem">{prepareGuard.block_reason}</div>
      {:else if prepareGuard?.warnings?.length}
        {#each prepareGuard.warnings as w}
          <div class="alert alert-warn" style="margin-bottom:.4rem">⚠ {w}</div>
        {/each}
      {/if}

      <button
        class="primary"
        on:click={handlePrepare}
        disabled={preparing || !prepareGuard?.allowed}
      >
        {preparing ? 'Preparing…' : 'Prepare update'}
      </button>

      {#if prepareError}
        <div class="alert alert-error mt">{prepareError}</div>
      {/if}

      {#if prepareLines.length > 0}
        <div class="log-box mt">
          {#each prepareLines as line}{line + '\n'}{/each}
        </div>
      {/if}
    </div>
  </div>

  <!-- ── Apply / Rollback / Validate ─────────────────────────────────────── -->
  <div class="section">
    <div class="section-title">Deployment actions</div>
    <div class="card">
      <p class="muted" style="font-size:.82rem;margin-bottom:.75rem">
        Apply stages the update for the next reboot (vault is locked first).
        Rollback stages the previous deployment. Both require a manual reboot via the System page.
      </p>
      <div class="flex-row" style="flex-wrap:wrap;gap:.5rem">
        <button
          class="primary"
          on:click={() => showApply = true}
          disabled={applying || !applyGuard?.allowed}
          title={applyGuard?.block_reason ?? ''}
        >
          {applying ? 'Applying…' : 'Apply update'}
        </button>

        <button
          class="warn"
          on:click={() => showRollback = true}
          disabled={rolling || !rollbackGuard?.allowed}
          title={rollbackGuard?.block_reason ?? ''}
        >
          {rolling ? 'Rolling back…' : 'Rollback'}
        </button>

        <button on:click={handleValidate} disabled={validating}>
          {validating ? 'Validating…' : 'Validate'}
        </button>
      </div>

      <!-- Action results -->
      {#if applyResult}
        <div class="alert {applyResult.success ? 'alert-ok' : 'alert-error'} mt">
          {applyResult.success
            ? 'Update staged. Reboot to apply — use the System page.'
            : (applyResult.error ?? 'Apply failed')}
        </div>
      {/if}
      {#if rollbackResult}
        <div class="alert {rollbackResult.success ? 'alert-ok' : 'alert-error'} mt">
          {rollbackResult.success
            ? 'Rollback staged. Reboot to apply — use the System page.'
            : (rollbackResult.error ?? 'Rollback failed')}
        </div>
      {/if}
      {#if validateResult}
        <div class="alert {validateResult.passed ?? validateResult.success ? 'alert-ok' : 'alert-error'} mt">
          {validateResult.passed ?? validateResult.success
            ? '✓ Validation passed — system is healthy.'
            : '✗ Validation failed — rollback recommended.'}
        </div>
        {#if validateResult.output}
          <div class="log-box mt">{validateResult.output}</div>
        {/if}
      {/if}
    </div>
  </div>
{/if}

<ConfirmModal
  open={showApply}
  title="Apply update"
  message="The vault will be locked and the update will be staged for the next reboot. No reboot will happen automatically."
  warnings={applyGuard?.warnings ?? []}
  variant="warn"
  on:confirm={handleApply}
  on:cancel={() => showApply = false}
/>

<ConfirmModal
  open={showRollback}
  title="Stage rollback"
  message="This stages the previous deployment for the next reboot. The vault will be locked first."
  warnings={rollbackGuard?.warnings ?? []}
  variant="danger"
  on:confirm={handleRollback}
  on:cancel={() => showRollback = false}
/>
