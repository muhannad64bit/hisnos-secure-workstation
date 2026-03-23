<script>
  import { onMount, onDestroy } from 'svelte';
  import { getState } from '$lib/api.js';

  import WelcomeStep  from '$lib/components/WelcomeStep.svelte';
  import VaultStep    from '$lib/components/VaultStep.svelte';
  import FirewallStep from '$lib/components/FirewallStep.svelte';
  import ThreatStep   from '$lib/components/ThreatStep.svelte';
  import GamingStep   from '$lib/components/GamingStep.svelte';
  import VerifyStep   from '$lib/components/VerifyStep.svelte';

  const STEPS = ['welcome', 'vault', 'firewall', 'threat', 'gaming', 'verify'];
  const STEP_LABELS = {
    welcome:  'Welcome',
    vault:    'Vault',
    firewall: 'Firewall',
    threat:   'Threat Engine',
    gaming:   'Gaming',
    verify:   'Verify',
  };

  // Session timeout mirror: warn the user at 25 min, server kills at 30.
  const WARN_TIMEOUT_MS  = 25 * 60 * 1000;
  const HARD_TIMEOUT_MS  = 30 * 60 * 1000;

  let currentStep   = 'welcome';
  let loading       = true;
  let completed     = false;
  let showTimeout   = false;   // "continue later" warning
  let sessionExpired = false;  // hard deadline hit

  let sessionStart  = Date.now();
  let timerInterval;

  onMount(async () => {
    try {
      const s = await getState();
      if (s.completed) {
        completed = true;
      } else {
        currentStep = s.current_step ?? 'welcome';
      }
    } catch (e) {
      console.error('Failed to load state:', e);
    } finally {
      loading = false;
    }

    // Start client-side session clock.
    timerInterval = setInterval(checkTimeout, 30_000);
  });

  onDestroy(() => clearInterval(timerInterval));

  function checkTimeout() {
    const elapsed = Date.now() - sessionStart;
    if (elapsed >= HARD_TIMEOUT_MS) {
      sessionExpired = true;
      showTimeout = false;
      clearInterval(timerInterval);
    } else if (elapsed >= WARN_TIMEOUT_MS) {
      showTimeout = true;
    }
  }

  function advance(step) {
    currentStep = step;
    showTimeout = false; // dismiss if user acts before timeout
  }

  function finish() {
    completed = true;
    clearInterval(timerInterval);
  }

  function dismissTimeout() {
    showTimeout = false;
    // Reset the warning clock for another 5-min window.
    sessionStart = Date.now() - WARN_TIMEOUT_MS + 5 * 60 * 1000;
  }

  $: stepIndex = STEPS.indexOf(currentStep);
</script>

{#if loading}
  <div class="splash">
    <div class="spinner"></div>
    <p>Loading…</p>
  </div>

{:else if sessionExpired}
  <!-- Hard timeout: server has already shut down. -->
  <div class="card done-card">
    <div class="logo">HisnOS</div>
    <h2>Session expired</h2>
    <p>
      The setup wizard timed out after 30 minutes. Your progress is saved.
      To continue, open a terminal and run:
    </p>
    <pre>systemctl --user start hisnos-onboarding</pre>
    <p class="hint">Or reopen <code>http://localhost:9444</code> if the service is still running.</p>
  </div>

{:else if completed}
  <div class="card done-card">
    <div class="logo">HisnOS</div>
    <h2>Setup complete</h2>
    <p>Your secure workstation is ready. You may close this window.</p>
  </div>

{:else}
  <!-- Timeout warning banner (non-blocking) -->
  {#if showTimeout}
    <div class="timeout-banner">
      <span>⏱ Session expires in ~5 minutes. Your progress is saved automatically.</span>
      <button class="btn-secondary" on:click={dismissTimeout}>Dismiss</button>
    </div>
  {/if}

  <div class="shell">
    <!-- Sidebar progress -->
    <aside class="sidebar">
      <div class="logo">HisnOS</div>
      <nav>
        {#each STEPS as s, i}
          <div class="step-item"
               class:done={i < stepIndex}
               class:active={s === currentStep}
               class:future={i > stepIndex}>
            <span class="step-num">{i < stepIndex ? '✓' : i + 1}</span>
            <span class="step-label">{STEP_LABELS[s]}</span>
          </div>
        {/each}
      </nav>
      <div class="sidebar-footer">
        <span class="os-tag">Fedora Kinoite</span>
      </div>
    </aside>

    <!-- Main content area -->
    <main class="content">
      {#if currentStep === 'welcome'}
        <WelcomeStep  on:advance={(e) => advance(e.detail)} />
      {:else if currentStep === 'vault'}
        <VaultStep    on:advance={(e) => advance(e.detail)} />
      {:else if currentStep === 'firewall'}
        <FirewallStep on:advance={(e) => advance(e.detail)} />
      {:else if currentStep === 'threat'}
        <ThreatStep   on:advance={(e) => advance(e.detail)} />
      {:else if currentStep === 'gaming'}
        <GamingStep   on:advance={(e) => advance(e.detail)} />
      {:else if currentStep === 'verify'}
        <VerifyStep   on:finish={finish} />
      {/if}
    </main>
  </div>
{/if}

<style>
  .splash {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 1rem;
    color: #556677;
  }

  .spinner {
    width: 36px;
    height: 36px;
    border: 3px solid #1a2030;
    border-top-color: #00c8ff;
    border-radius: 50%;
    animation: spin 0.8s linear infinite;
  }

  @keyframes spin { to { transform: rotate(360deg); } }

  /* Timeout warning banner */
  .timeout-banner {
    position: fixed;
    top: 0;
    left: 0;
    right: 0;
    background: #2a1a00;
    border-bottom: 1px solid #ffa94d44;
    color: #ffa94d;
    font-size: 0.85rem;
    padding: 0.5rem 1.25rem;
    display: flex;
    align-items: center;
    justify-content: space-between;
    z-index: 100;
  }

  .shell {
    display: flex;
    width: 860px;
    min-height: 520px;
    background: #0d1117;
    border-radius: 12px;
    border: 1px solid #1a2030;
    overflow: hidden;
    box-shadow: 0 24px 64px rgba(0,0,0,0.6);
    /* Push down when timeout banner is showing */
    margin-top: var(--banner-offset, 0);
  }

  .sidebar {
    width: 200px;
    background: #080c12;
    border-right: 1px solid #1a2030;
    display: flex;
    flex-direction: column;
    padding: 1.75rem 1.25rem;
    gap: 2rem;
  }

  .logo {
    font-size: 1.25rem;
    font-weight: 700;
    color: #00c8ff;
    letter-spacing: 0.05em;
  }

  nav {
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
    flex: 1;
  }

  .step-item {
    display: flex;
    align-items: center;
    gap: 0.6rem;
    padding: 0.4rem 0.5rem;
    border-radius: 6px;
    transition: background 0.15s;
  }

  .step-num {
    width: 22px;
    height: 22px;
    border-radius: 50%;
    display: flex;
    align-items: center;
    justify-content: center;
    font-size: 0.75rem;
    font-weight: 600;
    background: #1a2030;
    color: #556677;
    flex-shrink: 0;
  }

  .step-label { font-size: 0.85rem; color: #556677; }

  .step-item.done .step-num  { background: #00c8ff22; color: #00c8ff; }
  .step-item.done .step-label { color: #8892b0; }

  .step-item.active { background: #00c8ff11; }
  .step-item.active .step-num  { background: #00c8ff; color: #0a0a14; }
  .step-item.active .step-label { color: #ccd6f6; font-weight: 500; }

  .sidebar-footer { font-size: 0.75rem; color: #2a3a50; }
  .os-tag { font-family: monospace; }

  .content {
    flex: 1;
    padding: 2.5rem 2rem;
    overflow-y: auto;
  }

  /* Session expired / done card */
  .card {
    width: 480px;
    background: #0d1117;
    border: 1px solid #1a2030;
    border-radius: 12px;
    padding: 2.5rem 2rem;
    text-align: center;
  }

  .done-card .logo { font-size: 2rem; margin-bottom: 1rem; }
  .done-card h2    { color: #ccd6f6; margin-bottom: 0.75rem; }
  .done-card p     { color: #556677; font-size: 0.95rem; margin-bottom: 0.5rem; }
  .done-card pre   {
    background: #0a0a14;
    border: 1px solid #1a2030;
    border-radius: 6px;
    color: #00c8ff;
    font-family: monospace;
    font-size: 0.85rem;
    padding: 0.6rem 1rem;
    margin: 0.75rem 0;
    text-align: left;
  }
  .hint { font-size: 0.8rem; color: #3a4a60; }
  code  { font-family: monospace; color: #556677; }
</style>
