<script lang="ts">
  // Dialog on top of the page (native <dialog>: focus trap, Esc to close).
  import type { Snippet } from 'svelte'
  import X from '@lucide/svelte/icons/x'

  interface Props {
    title: string
    open: boolean
    onclose: () => void
    children: Snippet
    footer?: Snippet
    wide?: boolean
  }
  let { title, open, onclose, children, footer, wide = false }: Props = $props()
  let dialog = $state<HTMLDialogElement>()

  $effect(() => {
    if (!dialog) return
    if (open && !dialog.open) dialog.showModal()
    if (!open && dialog.open) dialog.close()
  })
</script>

<dialog bind:this={dialog} class:wide onclose={onclose} aria-labelledby="modal-title">
  <header>
    <h2 id="modal-title">{title}</h2>
    <button class="close" onclick={onclose} aria-label="Close"><X size={18} /></button>
  </header>
  <div class="content">{@render children()}</div>
  {#if footer}<footer>{@render footer()}</footer>{/if}
</dialog>

<style>
  dialog {
    width: min(560px, calc(100vw - 2rem));
    max-height: calc(100vh - 2rem);
    padding: 0;
    border: 1px solid var(--border);
    border-radius: var(--radius);
    background: var(--panel);
    color: var(--text);
    box-shadow: 0 20px 50px rgb(0 0 0 / 25%);
  }
  dialog.wide {
    width: min(760px, calc(100vw - 2rem));
  }
  dialog::backdrop {
    background: rgb(15 20 30 / 45%);
  }
  header {
    display: flex;
    justify-content: space-between;
    align-items: center;
    padding: 0.9rem 1.1rem;
    border-bottom: 1px solid var(--border);
    position: sticky;
    top: 0;
    background: var(--panel);
  }
  .close {
    border: 0;
    padding: 0.25rem;
  }
  .content {
    padding: 1.1rem;
  }
  footer {
    display: flex;
    justify-content: flex-end;
    gap: 0.5rem;
    padding: 0.8rem 1.1rem;
    border-top: 1px solid var(--border);
    position: sticky;
    bottom: 0;
    background: var(--panel);
  }
</style>
