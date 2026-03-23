<script>
  import { createEventDispatcher } from 'svelte';
  import { postVault } from '$lib/api.js';

  const dispatch = createEventDispatcher();

  let passphrase  = '';
  let confirm     = '';
  let loading     = false;
  let error       = '';

  $: mismatch  = confirm.length > 0 && passphrase !== confirm;
  $: tooShort  = passphrase.length > 0 && passphrase.length < 12;
  $: canSubmit = passphrase.length >= 12 && passphrase === confirm && !loading;

  async function init() {
    error = '';
    loading = true;
    try {
      const res = await postVault({ passphrase });
      dispatch('advance', res.next);
    } catch (e) {
      error = e.message;
    } finally {
      loading = false;
    }
  }

  async function skip() {
    loading = true;
    error = '';
    try {
      const res = await postVault({ skip: true });
      dispatch('advance', res.next);
    } catch (e) {
      error = e.message;
    } finally {
      loading = false;
    }
  }
</script>

<div class="step">
  <h1>Encrypted Vault</h1>
  <p class="desc">
    Your vault is a gocryptfs-encrypted directory for sensitive files.
    Choose a strong passphrase — it cannot be recovered if lost.
  </p>

  <div class="field">
    <label for="pp">Passphrase</label>
    <input id="pp" type="password" bind:value={passphrase} placeholder="12+ characters" autocomplete="new-password" />
    {#if tooShort}
      <p class="error-msg">Must be at least 12 characters.</p>
    {/if}
  </div>

  <div class="field">
    <label for="pp2">Confirm passphrase</label>
    <input id="pp2" type="password" bind:value={confirm} placeholder="Repeat passphrase" autocomplete="new-password" />
    {#if mismatch}
      <p class="error-msg">Passphrases do not match.</p>
    {/if}
  </div>

  {#if error}
    <p class="error-msg">{error}</p>
  {/if}

  <div class="actions">
    <button class="btn-primary" on:click={init} disabled={!canSubmit}>
      {loading ? 'Initialising…' : 'Create Vault'}
    </button>
    <button class="btn-secondary" on:click={skip} disabled={loading}>
      Skip for now
    </button>
  </div>

  <p class="hint">
    Skipping leaves your home directory unencrypted.
    You can initialise the vault later with <code>hisnos-vault init</code>.
  </p>
</div>

<style>
  .step h1 { font-size: 1.4rem; color: #ccd6f6; margin-bottom: 0.5rem; }
  .desc { color: #8892b0; font-size: 0.9rem; line-height: 1.6; margin-bottom: 1.5rem; }
  .field { display: flex; flex-direction: column; gap: 0.4rem; margin-bottom: 1rem; }
  label { font-size: 0.85rem; color: #8892b0; }
  .actions { display: flex; gap: 0.75rem; margin-top: 1.25rem; }
  .hint { margin-top: 1.25rem; font-size: 0.8rem; color: #3a4a60; line-height: 1.5; }
  code { font-family: monospace; color: #556677; }
</style>
