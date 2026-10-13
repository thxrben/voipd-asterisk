<script lang="ts">
  // Second row of the dashboard: live calls, missed, trunks, phones, quality.
  import { live } from '../lib/live.svelte'
  import { link } from '../lib/router.svelte'
  import { mosRating, num } from '../lib/format'
  import type { Status, Summary } from '../lib/types'

  interface Props {
    status: Status | null
    summary: Summary | null
  }
  let { status, summary }: Props = $props()

  const active = $derived(live.status?.active_calls.length ?? status?.active_calls ?? 0)
  const trunksUp = $derived(live.status?.trunks_up ?? status?.trunks_up ?? 0)
  const trunksTotal = $derived(live.status?.trunks_total ?? status?.trunks.length ?? 0)
  const engineUp = $derived(live.status?.engine_connected ?? status?.engine.connected ?? false)
  const phonesUp = $derived(status?.phones.filter((p) => p.contacts.some((c) => c.status === 'Reachable')).length ?? 0)
  const phones = $derived(status?.phones.length ?? 0)
  const mos = $derived(mosRating(summary?.avg_mos))
</script>

<div class="strip">
  <a class="item" href="/status" onclick={link}>
    <span class="dot {active > 0 ? 'good' : ''}"></span>
    <span><b class="num">{active}</b> active {active === 1 ? 'call' : 'calls'}</span>
  </a>
  <a class="item" href="/calls?status=missed,cancelled&direction=inbound" onclick={link}>
    <span class="dot {summary?.missed ? 'warn' : ''}"></span>
    <span><b class="num">{num(summary?.missed)}</b> missed</span>
  </a>
  <a class="item" href="/status" onclick={link}>
    <span class="dot {!engineUp ? 'bad' : trunksUp === trunksTotal ? 'good' : 'bad'}"></span>
    <span>
      {#if !engineUp}<b>Engine offline</b>
      {:else}<b class="num">{trunksUp}/{trunksTotal}</b> lines registered{/if}
    </span>
  </a>
  <a class="item" href="/status" onclick={link}>
    <span class="dot {phonesUp > 0 ? 'good' : ''}"></span>
    <span><b class="num">{phonesUp}/{phones}</b> phones online</span>
  </a>
  <div class="item">
    <span class="dot {mos.tone}"></span>
    <span>
      Call quality <b>{mos.label}</b>
      {#if summary?.avg_mos != null}<span class="muted num">(MOS {summary.avg_mos.toFixed(2)})</span>{/if}
    </span>
  </div>
</div>

<style>
  .strip {
    display: grid;
    grid-template-columns: repeat(5, minmax(0, 1fr));
    gap: 1rem;
  }
  .item {
    display: flex;
    align-items: center;
    gap: 0.6rem;
    padding: 0.7rem 0.9rem;
    background: var(--panel);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    box-shadow: var(--shadow);
    color: var(--text);
    min-width: 0;
  }
  a.item:hover {
    text-decoration: none;
    background: var(--panel-2);
  }
  @media (max-width: 1000px) {
    .strip {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
  }
  @media (max-width: 560px) {
    .strip {
      gap: 0.6rem;
    }
    .item {
      padding: 0.55rem 0.7rem;
      font-size: 0.9rem;
    }
  }
</style>
