<script>
  import { createEventDispatcher } from 'svelte';
  import { postThreat } from '$lib/api.js';

  const dispatch = createEventDispatcher();

  let notifications = true;
  let loading = false;
  let error = '';

  async function save() {
    loading = true;
    error = '';
    try {
      const res = await postThreat(notifications);
      dispatch('advance', res.next);
    } catch (e) {
      error = e.message;
    } finally {
      loading = false;
    }
  }
</script>

<div class="step">
  <h1>Threat Engine</h1>
  <p class="desc">
    <code>hisnos-threatd</code> monitors running processes, open ports, and
    audit logs for anomalies. When an anomaly is detected it can notify you via
    a desktop notification.
  </p>

  <div class="toggle-card">
    <div class="toggle-info">
      <p class="toggle-label">Desktop notifications</p>
      <p class="toggle-sub">
        Show a pop-up when a high-severity threat event is detected.
        Notifications are local-only and never sent off-device.
      </p>
    </div>
    <label class="switch">
      <input type="checkbox" bind:checked={notifications} />
      <span class="slider"></span>
    </label>
  </div>

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
    margin-bottom: 1.5rem;
  }

  .toggle-info { flex: 1; }
  .toggle-label { color: #ccd6f6; font-weight: 500; margin-bottom: 0.25rem; }
  .toggle-sub { color: #556677; font-size: 0.83rem; line-height: 1.5; }

  /* Toggle switch */
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

  .actions { display: flex; gap: 0.75rem; }
</style>
