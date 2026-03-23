<script>
  import { createEventDispatcher, onMount } from 'svelte';
  import { getVerify, postComplete } from '$lib/api.js';

  const dispatch = createEventDispatcher();

  let checks       = [];
  let allOK        = false;
  let checkedAt    = '';
  let loadingCheck = true;
  let loadingDone  = false;
  let error        = '';
  let showDetails  = false;

  // How many checks passed.
  $: passCount = checks.filter(c => c.ok).length;
  $: failCount = checks.filter(c => !c.ok).length;

  onMount(runVerify);

  async function runVerify() {
    loadingCheck = true;
    error = '';
    try {
      const res = await getVerify();
      checks    = res.checks ?? [];
      allOK     = res.ok;
      checkedAt = res.checked_at;
    } catch (e) {
      error = e.message;
    } finally {
      loadingCheck = false;
    }
  }

  async function complete() {
    loadingDone = true;
    error = '';
    try {
      await postComplete();
      dispatch('finish');
    } catch (e) {
      error = e.message;
      loadingDone = false;
    }
  }

  function fixHint(check) {
    // Map check name to a remediation hint shown in the UI.
    const hints = {
      'Vault':          'Run: hisnos-vault init',
      'Firewall':       'Run: sudo systemctl start nftables',
      'hisnosd':        'Run: systemctl --user start hisnosd.service',
      'Audit Pipeline': 'Run: sudo systemctl start auditd',
      'Threat Engine':  'Run: systemctl --user start hisnos-threatd',
    };
    return hints[check.name] ?? null;
  }
</script>

<div class="step">
  <h1>System Verification</h1>
  <p class="desc">
    Confirming all HisnOS subsystems are active before completing setup.
  </p>

  {#if loadingCheck}
    <div class="loading-checks">
      <div class="spinner"></div>
      <span>Running checks…</span>
    </div>

  {:else if error}
    <p class="error-msg">Check failed: {error}</p>

  {:else}
    <!-- Summary bar -->
    <div class="summary-bar" class:all-ok={allOK} class:has-fail={!allOK}>
      <span class="summary-icon">{allOK ? '✓' : '⚠'}</span>
      <span class="summary-text">
        {#if allOK}
          All {checks.length} checks passed
        {:else}
          {passCount} of {checks.length} checks passed — {failCount} need attention
        {/if}
      </span>
    </div>

    <!-- Check rows -->
    <div class="checks">
      {#each checks as c}
        <div class="check-row" class:ok={c.ok} class:fail={!c.ok}>
          <span class="check-icon">{c.ok ? '✓' : '✗'}</span>
          <div class="check-body">
            <span class="check-name">{c.name}</span>
            <span class="check-msg">{c.message}</span>
            {#if !c.ok && fixHint(c)}
              <span class="fix-hint">{fixHint(c)}</span>
            {/if}
          </div>
        </div>
      {/each}
    </div>

    {#if checkedAt}
      <p class="checked-at">
        Checked at {new Date(checkedAt).toLocaleTimeString()}
      </p>
    {/if}

    {#if !allOK}
      <div class="skip-note">
        <p>
          You can complete setup now — failed checks are recorded in the state
          file and can be fixed post-install via the Governance Dashboard.
        </p>
      </div>
    {/if}
  {/if}

  {#if error}
    <p class="error-msg">{error}</p>
  {/if}

  <div class="actions">
    <button class="btn-primary"
            on:click={complete}
            disabled={loadingDone || loadingCheck}>
      {loadingDone ? 'Finishing…' : allOK ? 'Complete Setup ✓' : 'Complete Anyway →'}
    </button>
    <button class="btn-secondary"
            on:click={runVerify}
            disabled={loadingCheck || loadingDone}>
      Re-check
    </button>
  </div>
</div>

<style>
  .step h1 { font-size: 1.4rem; color: #ccd6f6; margin-bottom: 0.5rem; }
  .desc    { color: #8892b0; font-size: 0.9rem; line-height: 1.6; margin-bottom: 1.25rem; }

  .loading-checks {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    color: #556677;
    margin-bottom: 1.5rem;
  }

  .spinner {
    width: 20px;
    height: 20px;
    border: 2px solid #1a2030;
    border-top-color: #00c8ff;
    border-radius: 50%;
    animation: spin 0.7s linear infinite;
    flex-shrink: 0;
  }

  @keyframes spin { to { transform: rotate(360deg); } }

  /* Summary bar */
  .summary-bar {
    display: flex;
    align-items: center;
    gap: 0.6rem;
    padding: 0.6rem 0.9rem;
    border-radius: 6px;
    margin-bottom: 0.9rem;
    font-size: 0.9rem;
    font-weight: 500;
  }

  .summary-bar.all-ok  { background: #00c8ff0d; color: #00c8ff; border: 1px solid #00c8ff22; }
  .summary-bar.has-fail { background: #ffa94d0d; color: #ffa94d; border: 1px solid #ffa94d22; }

  .summary-icon { font-size: 1rem; }

  /* Check rows */
  .checks { display: flex; flex-direction: column; gap: 0.45rem; margin-bottom: 0.75rem; }

  .check-row {
    display: flex;
    align-items: flex-start;
    gap: 0.75rem;
    padding: 0.6rem 0.9rem;
    border-radius: 6px;
    border: 1px solid #1a2030;
  }

  .check-row.ok   { border-color: #00c8ff1a; background: #00c8ff05; }
  .check-row.fail { border-color: #ff6b6b1a; background: #ff6b6b05; }

  .check-icon {
    font-size: 0.88rem;
    font-weight: 700;
    margin-top: 2px;
    flex-shrink: 0;
  }

  .check-row.ok   .check-icon { color: #00c8ff; }
  .check-row.fail .check-icon { color: #ff6b6b; }

  .check-body  { display: flex; flex-direction: column; gap: 0.1rem; }
  .check-name  { font-size: 0.88rem; font-weight: 600; color: #ccd6f6; }
  .check-msg   { font-size: 0.79rem; color: #556677; font-family: monospace; }
  .fix-hint    { font-size: 0.79rem; color: #ffa94d; font-family: monospace; margin-top: 0.1rem; }

  .checked-at { font-size: 0.78rem; color: #2a3a50; margin-bottom: 0.5rem; }

  .skip-note {
    font-size: 0.83rem;
    color: #8892b0;
    background: #1a2030;
    border-radius: 6px;
    padding: 0.6rem 0.85rem;
    margin-bottom: 0.75rem;
    line-height: 1.5;
  }

  .actions { display: flex; gap: 0.75rem; margin-top: 1rem; }
</style>
