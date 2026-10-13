<script lang="ts">
  // Form field: label, control (children), help text and error message.
  import type { Snippet } from 'svelte'

  interface Props {
    label: string
    error?: string
    help?: string
    children: Snippet
    inline?: boolean
    /** Several controls (e.g. checkboxes): render a group instead of a label. */
    group?: boolean
  }
  let { label, error, help, children, inline = false, group = false }: Props = $props()
</script>

<!-- svelte-ignore a11y_label_has_associated_control: the control is the child snippet -->
<svelte:element this={group ? 'div' : 'label'} role={group ? 'group' : undefined} aria-label={group ? label : undefined}
  class="field" class:inline class:invalid={!!error}>
  {#if !inline}<span class="label">{label}</span>{/if}
  {@render children()}
  {#if inline}<span class="label">{label}</span>{/if}
  {#if error}<span class="error">{error}</span>{:else if help}<span class="help">{help}</span>{/if}
</svelte:element>

<style>
  .field {
    display: grid;
    align-content: start;
    gap: 0.3rem;
    color: var(--text);
    font-size: 0.9rem;
  }
  .label {
    font-weight: 600;
    font-size: 0.85rem;
  }
  .field.inline {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.5rem;
  }
  .field.inline .label {
    font-weight: 500;
  }
  .help {
    color: var(--muted);
    font-size: 0.8rem;
  }
  .error {
    color: var(--bad);
    font-size: 0.8rem;
  }
  .invalid :global(input),
  .invalid :global(select) {
    border-color: var(--bad);
  }
  .field.inline .help,
  .field.inline .error {
    flex-basis: 100%;
  }
</style>
