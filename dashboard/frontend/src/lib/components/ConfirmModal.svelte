<script lang="ts">
  import { createEventDispatcher } from 'svelte';

  /** Whether the modal is visible. */
  export let open     = false;
  export let title    = 'Confirm action';
  export let message  = '';
  /** Guard warnings from the control plane (shown as amber list). */
  export let warnings: string[] = [];
  /** Use 'danger' (red) or 'warn' (amber) for the confirm button. */
  export let variant: 'danger' | 'warn' = 'danger';

  const dispatch = createEventDispatcher<{ confirm: void; cancel: void }>();

  function onBackdrop(e: MouseEvent) {
    if (e.target === e.currentTarget) dispatch('cancel');
  }

  function onKey(e: KeyboardEvent) {
    if (e.key === 'Escape') dispatch('cancel');
  }
</script>

<svelte:window on:keydown={onKey} />

{#if open}
  <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
  <div class="backdrop" on:click={onBackdrop}>
    <div class="modal" role="dialog" aria-modal="true" aria-labelledby="modal-title">
      <h3 id="modal-title">{title}</h3>

      {#if message}
        <p class="message">{message}</p>
      {/if}

      {#if warnings.length > 0}
        <ul class="warnings">
          {#each warnings as w}
            <li>⚠ {w}</li>
          {/each}
        </ul>
      {/if}

      <div class="actions">
        <button on:click={() => dispatch('cancel')}>Cancel</button>
        <button class={variant} on:click={() => dispatch('confirm')}>Confirm</button>
      </div>
    </div>
  </div>
{/if}

<style>
  .backdrop {
    position:        fixed;
    inset:           0;
    background:      rgba(0, 0, 0, .7);
    display:         flex;
    align-items:     center;
    justify-content: center;
    z-index:         100;
  }

  .modal {
    background:    var(--surface);
    border:        1px solid var(--border);
    border-radius: 6px;
    padding:       1.5rem;
    width:         min(480px, 90vw);
    display:       flex;
    flex-direction: column;
    gap:           .75rem;
  }

  h3 { font-size: 1rem; font-weight: 600; }

  .message { font-size: .85rem; color: var(--text-muted); }

  .warnings {
    list-style: none;
    display:    flex;
    flex-direction: column;
    gap:        .3rem;
    font-size:  .82rem;
    color:      var(--c-update-preparing);
    background: #2d2200;
    border:     1px solid var(--c-update-preparing);
    border-radius: var(--radius);
    padding:    .6rem .75rem;
  }

  .actions {
    display:         flex;
    justify-content: flex-end;
    gap:             .5rem;
    margin-top:      .25rem;
  }
</style>
