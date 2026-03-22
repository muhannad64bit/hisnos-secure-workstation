<script lang="ts">
  import type { DisplayState } from '$lib/api.js';

  export let display: DisplayState | null = null;

  const labels: Record<string, string> = {
    'normal':                'Normal',
    'vault-mounted':         'Vault Mounted',
    'gaming':                'Gaming',
    'lab-active':            'Lab Active',
    'update-preparing':      'Update Preparing',
    'update-pending-reboot': 'Reboot Pending',
    'rollback-mode':         'Rollback Mode',
  };
</script>

{#if display}
  <span class="badge {display}">{labels[display] ?? display}</span>
{:else}
  <span class="badge" style="opacity:.4">—</span>
{/if}
