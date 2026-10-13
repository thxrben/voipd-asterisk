<script lang="ts">
  import { toast } from '../lib/toast.svelte'
</script>

<div class="toasts" role="status" aria-live="polite">
  {#each toast.items as t (t.id)}
    <button class="toast {t.tone}" onclick={() => toast.dismiss(t.id)}>{t.text}</button>
  {/each}
</div>

<style>
  .toasts {
    position: fixed;
    right: 1rem;
    bottom: 1rem;
    display: grid;
    gap: 0.5rem;
    z-index: 50;
    max-width: min(420px, calc(100vw - 2rem));
  }
  .toast {
    text-align: left;
    padding: 0.7rem 0.9rem;
    border-radius: 8px;
    box-shadow: var(--shadow);
    background: var(--panel);
    border: 1px solid var(--border);
    border-left: 4px solid var(--good);
  }
  .toast.warn {
    border-left-color: var(--warn);
  }
  .toast.bad {
    border-left-color: var(--bad);
  }
</style>
