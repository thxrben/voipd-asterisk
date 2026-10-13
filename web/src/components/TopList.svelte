<script lang="ts">
  import type { TopEntry } from '../lib/types'
  import { ago, phone } from '../lib/format'
  import { link } from '../lib/router.svelte'

  interface Props {
    title: string
    entries: TopEntry[]
    color: string
    empty?: string
    showReason?: boolean
  }
  let { title, entries, color, empty = 'Nothing yet', showReason = false }: Props = $props()
  const max = $derived(Math.max(1, ...entries.map((e) => e.count)))
</script>

<section class="card">
  <header><h2>{title}</h2></header>
  {#if entries.length === 0}
    <div class="empty">{empty}</div>
  {:else}
    <ol>
      {#each entries as e (e.number)}
        <li>
          <div class="row">
            <a href="/calls?q={encodeURIComponent(e.number)}" onclick={link} class="who" title={e.number}>
              {e.name || phone(e.number)}
              {#if e.name}<span class="muted mono">{phone(e.number)}</span>{/if}
            </a>
            <b class="num">{e.count}</b>
          </div>
          <div class="bar"><span style="width: {(e.count / max) * 100}%; background: {color}"></span></div>
          <div class="meta muted">
            last {ago(e.last)}{#if showReason && e.reason} · {e.reason}{/if}
          </div>
        </li>
      {/each}
    </ol>
  {/if}
</section>

<style>
  ol {
    list-style: none;
    margin: 0;
    padding: 0.4rem 1rem 0.8rem;
  }
  li {
    padding: 0.45rem 0;
  }
  li + li {
    border-top: 1px dashed var(--border);
  }
  .row {
    display: flex;
    justify-content: space-between;
    gap: 0.5rem;
  }
  .who {
    color: var(--text);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .who .muted {
    margin-left: 0.35rem;
    font-size: 0.85em;
  }
  .bar {
    height: 4px;
    background: var(--panel-2);
    border-radius: 2px;
    margin: 0.3rem 0 0.2rem;
    overflow: hidden;
  }
  .bar span {
    display: block;
    height: 100%;
    border-radius: 2px;
  }
  .meta {
    font-size: 0.78rem;
  }
</style>
