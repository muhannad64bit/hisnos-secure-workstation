<script lang="ts">
  import { onMount }       from 'svelte';
  import { page }          from '$app/stores';
  import { startPolling, stopPolling, systemStateStore, displayState } from '$lib/state.js';
  import StatusBadge from '$lib/components/StatusBadge.svelte';
  import '../app.css';

  onMount(() => {
    startPolling();
    return stopPolling;
  });

  const navLinks = [
    { href: '/',          label: 'Overview'  },
    { href: '/vault',     label: 'Vault'     },
    { href: '/firewall',  label: 'Firewall'  },
    { href: '/update',    label: 'Update'    },
    { href: '/system',    label: 'System'    },
  ];

  $: currentPath = $page.url.pathname;
</script>

<div class="layout">
  <!-- ── Top bar ─────────────────────────────────────────────────────────── -->
  <header class="topbar">
    <span class="topbar-title">🔒 HisnOS</span>
    <StatusBadge display={$displayState} />
    {#if $systemStateStore === null}
      <span class="muted" style="font-size:.75rem">connecting…</span>
    {/if}
  </header>

  <!-- ── Sidebar ────────────────────────────────────────────────────────── -->
  <nav class="sidebar">
    {#each navLinks as { href, label }}
      <a {href} class:active={currentPath === href}>{label}</a>
    {/each}
  </nav>

  <!-- ── Main content ───────────────────────────────────────────────────── -->
  <main class="main">
    <slot />
  </main>
</div>
