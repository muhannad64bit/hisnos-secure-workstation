<script>
  import { createEventDispatcher } from 'svelte';
  import { postFirewall } from '$lib/api.js';

  const dispatch = createEventDispatcher();

  const PROFILES = [
    {
      id:   'strict',
      name: 'Strict',
      desc: 'Default-deny egress. Only HTTPS, DNS, and NTP allowed. Best for high-security work.',
      icon: '🔒',
    },
    {
      id:   'balanced',
      name: 'Balanced',
      desc: 'HTTPS, DNS, NTP, plus common developer ports (SSH, git, package managers).',
      icon: '⚖️',
    },
    {
      id:   'gaming-ready',
      name: 'Gaming-Ready',
      desc: 'All of Balanced plus Steam, Proton, and game UDP ports. hispowerd manages the fast path.',
      icon: '🎮',
    },
  ];

  let selected = 'balanced';
  let loading  = false;
  let error    = '';

  async function apply() {
    loading = true;
    error = '';
    try {
      const res = await postFirewall(selected);
      dispatch('advance', res.next);
    } catch (e) {
      error = e.message;
    } finally {
      loading = false;
    }
  }
</script>

<div class="step">
  <h1>Firewall Profile</h1>
  <p class="desc">
    Select an nftables egress profile. You can change this later via the
    governance dashboard.
  </p>

  <div class="profiles">
    {#each PROFILES as p}
      <label class="profile-card" class:selected={selected === p.id}>
        <input type="radio" bind:group={selected} value={p.id} />
        <div class="profile-body">
          <div class="profile-head">
            <span class="profile-icon">{p.icon}</span>
            <span class="profile-name">{p.name}</span>
          </div>
          <p class="profile-desc">{p.desc}</p>
        </div>
      </label>
    {/each}
  </div>

  {#if error}
    <p class="error-msg">{error}</p>
  {/if}

  <div class="actions">
    <button class="btn-primary" on:click={apply} disabled={loading}>
      {loading ? 'Applying…' : 'Apply Profile →'}
    </button>
  </div>
</div>

<style>
  .step h1 { font-size: 1.4rem; color: #ccd6f6; margin-bottom: 0.5rem; }
  .desc { color: #8892b0; font-size: 0.9rem; line-height: 1.6; margin-bottom: 1.5rem; }

  .profiles { display: flex; flex-direction: column; gap: 0.75rem; margin-bottom: 1.5rem; }

  .profile-card {
    display: flex;
    align-items: flex-start;
    gap: 0.75rem;
    padding: 0.9rem 1rem;
    border: 1px solid #1a2030;
    border-radius: 8px;
    cursor: pointer;
    transition: border-color 0.15s, background 0.15s;
  }

  .profile-card input[type="radio"] { margin-top: 3px; accent-color: #00c8ff; }

  .profile-card.selected {
    border-color: #00c8ff55;
    background: #00c8ff08;
  }

  .profile-body { flex: 1; }

  .profile-head {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    margin-bottom: 0.3rem;
  }

  .profile-name { font-weight: 600; color: #ccd6f6; font-size: 0.95rem; }
  .profile-icon { font-size: 1rem; }
  .profile-desc { color: #556677; font-size: 0.83rem; line-height: 1.5; }

  .actions { display: flex; gap: 0.75rem; }
</style>
