<script>
  import { createEventDispatcher } from 'svelte';
  import { postWelcome } from '$lib/api.js';

  const dispatch = createEventDispatcher();

  let loading = false;
  let error = '';

  async function accept() {
    loading = true;
    error = '';
    try {
      const res = await postWelcome();
      dispatch('advance', res.next);
    } catch (e) {
      error = e.message;
    } finally {
      loading = false;
    }
  }
</script>

<div class="step">
  <h1>Welcome to HisnOS</h1>
  <p class="subtitle">Secure Workstation Setup</p>

  <div class="overview">
    <p>
      This wizard will configure your secure workstation in a few short steps.
      Each step can be skipped — you can complete it later from the control panel.
    </p>
    <ul>
      <li><span class="icon">🔒</span> Encrypted vault (gocryptfs)</li>
      <li><span class="icon">🛡</span> Firewall profile (nftables)</li>
      <li><span class="icon">⚠️</span> Threat engine notifications</li>
      <li><span class="icon">🎮</span> Gaming performance group</li>
      <li><span class="icon">✅</span> System verification</li>
    </ul>
  </div>

  {#if error}
    <p class="error-msg">{error}</p>
  {/if}

  <div class="actions">
    <button class="btn-primary" on:click={accept} disabled={loading}>
      {loading ? 'Please wait…' : 'Get Started →'}
    </button>
  </div>
</div>

<style>
  .step h1 {
    font-size: 1.6rem;
    color: #ccd6f6;
    margin-bottom: 0.25rem;
  }

  .subtitle {
    color: #00c8ff;
    font-size: 0.9rem;
    margin-bottom: 1.75rem;
    letter-spacing: 0.05em;
    text-transform: uppercase;
  }

  .overview p {
    color: #8892b0;
    font-size: 0.95rem;
    line-height: 1.6;
    margin-bottom: 1.25rem;
  }

  ul {
    list-style: none;
    display: flex;
    flex-direction: column;
    gap: 0.6rem;
    margin-bottom: 2rem;
  }

  li {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    color: #8892b0;
    font-size: 0.9rem;
  }

  .icon {
    font-size: 1rem;
  }

  .actions {
    display: flex;
    gap: 0.75rem;
  }
</style>
