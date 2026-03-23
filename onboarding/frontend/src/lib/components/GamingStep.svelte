<script>
  import { createEventDispatcher } from 'svelte';
  import { postGaming } from '$lib/api.js';

  const dispatch = createEventDispatcher();

  let enable   = false;
  let loading  = false;
  let error    = '';
  let warnings = [];

  async function save() {
    loading = true;
    error = '';
    warnings = [];
    try {
      const res = await postGaming(enable);
      if (res.warnings && res.warnings.length) {
        warnings = res.warnings;
      }
      dispatch('advance', res.next);
    } catch (e) {
      error = e.message;
    } finally {
      loading = false;
    }
  }
</script>

<div class="step">
  <h1>Gaming Performance</h1>
  <p class="desc">
    The <code>hisnos-gaming</code> group grants access to GameMode, MangoHud,
    and the <code>hispowerd</code> performance runtime.  Membership requires a
    re-login to take effect.
  </p>

  <div class="toggle-card" class:enabled={enable}>
    <div class="toggle-info">
      <p class="toggle-label">Join <code>hisnos-gaming</code> group</p>
      <p class="toggle-sub">
        Enables CPU isolation, IRQ affinity, and nftables fast path for games
        detected by hispowerd.  Does not affect security policies.
      </p>
    </div>
    <label class="switch">
      <input type="checkbox" bind:checked={enable} />
      <span class="slider"></span>
    </label>
  </div>

  {#if warnings.length}
    {#each warnings as w}
      <p class="warning-msg">⚠ {w}</p>
    {/each}
  {/if}

  {#if error}
    <p class="error-msg">{error}</p>
  {/if}

  <div class="actions">
    <button class="btn-primary" on:click={save} disabled={loading}>
      {loading ? 'Saving…' : 'Continue →'}
    </button>
  </div>
</div>

<style>
  .step h1 { font-size: 1.4rem; color: #ccd6f6; margin-bottom: 0.5rem; }
  .desc { color: #8892b0; font-size: 0.9rem; line-height: 1.6; margin-bottom: 1.5rem; }
  code { font-family: monospace; color: #556677; }

  .toggle-card {
    display: flex;
    align-items: center;
    gap: 1rem;
    padding: 1rem 1.1rem;
    border: 1px solid #1a2030;
    border-radius: 8px;
    margin-bottom: 1rem;
    transition: border-color 0.15s, background 0.15s;
  }

  .toggle-card.enabled {
    border-color: #00c8ff33;
    background: #00c8ff06;
  }

  .toggle-info { flex: 1; }
  .toggle-label { color: #ccd6f6; font-weight: 500; margin-bottom: 0.25rem; }
  .toggle-sub { color: #556677; font-size: 0.83rem; line-height: 1.5; }

  .switch { position: relative; display: inline-block; width: 44px; height: 24px; flex-shrink: 0; }
  .switch input { opacity: 0; width: 0; height: 0; }

  .slider {
    position: absolute;
    inset: 0;
    background: #1a2030;
    border-radius: 24px;
    transition: background 0.2s;
    cursor: pointer;
  }

  .slider::before {
    content: '';
    position: absolute;
    width: 18px;
    height: 18px;
    left: 3px;
    bottom: 3px;
    background: #556677;
    border-radius: 50%;
    transition: transform 0.2s, background 0.2s;
  }

  .switch input:checked + .slider { background: #00c8ff22; }
  .switch input:checked + .slider::before {
    transform: translateX(20px);
    background: #00c8ff;
  }

  .actions { display: flex; gap: 0.75rem; margin-top: 1rem; }
</style>
