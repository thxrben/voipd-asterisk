<script lang="ts">
  // Choose codecs and their order (first = preferred).
  import ArrowUp from '@lucide/svelte/icons/arrow-up'

  interface Props {
    value: string[]
    all: string[]
  }
  let { value = $bindable(), all }: Props = $props()

  const labels: Record<string, string> = {
    g722: 'G.722 (HD voice)',
    alaw: 'G.711 A-law',
    ulaw: 'G.711 µ-law',
  }
  const ordered = $derived([...value, ...all.filter((c) => !value.includes(c))])

  function toggle(c: string) {
    value = value.includes(c) ? value.filter((x) => x !== c) : [...value, c]
  }
  function up(c: string) {
    const i = value.indexOf(c)
    if (i > 0) {
      const v = [...value]
      ;[v[i - 1], v[i]] = [v[i]!, v[i - 1]!]
      value = v
    }
  }
</script>

<ul>
  {#each ordered as c (c)}
    <li class:off={!value.includes(c)}>
      <label class="check"><input type="checkbox" checked={value.includes(c)} onchange={() => toggle(c)} /> {labels[c] ?? c}</label>
      {#if value.indexOf(c) > 0}
        <button type="button" onclick={() => up(c)} aria-label="Prefer {labels[c] ?? c}"><ArrowUp size={14} /></button>
      {/if}
    </li>
  {/each}
</ul>

<style>
  ul {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 0.3rem;
  }
  li {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.5rem;
    padding: 0.3rem 0.5rem;
    border: 1px solid var(--border);
    border-radius: 7px;
  }
  li.off {
    opacity: 0.6;
  }
  .check {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    color: var(--text);
  }
  button {
    padding: 0.2rem;
  }
</style>
