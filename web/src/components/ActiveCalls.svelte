<script lang="ts">
  // Live table of calls in progress (from the event stream's status).
  import { onMount } from 'svelte'
  import Activity from '@lucide/svelte/icons/activity'
  import type { ActiveCall } from '../lib/types'
  import { duration, phone, time } from '../lib/format'
  import CallBadges from './CallBadges.svelte'

  interface Props {
    calls: ActiveCall[]
  }
  let { calls }: Props = $props()

  // Tick every second so durations count up between status updates.
  let now = $state(Date.now())
  onMount(() => {
    const t = setInterval(() => (now = Date.now()), 1000)
    return () => clearInterval(t)
  })

  function since(c: ActiveCall): string {
    const t = time(c.answered_at) ?? time(c.started_at)
    return t ? duration((now - t.getTime()) / 1000) : '–'
  }
</script>

<section class="card">
  <header>
    <h2><Activity size={16} /> Active calls</h2>
    <span class="muted">{calls.length} in progress</span>
  </header>
  {#if calls.length === 0}
    <div class="empty">No calls right now.</div>
  {:else}
    <div class="table-wrap">
      <table>
        <thead>
          <tr><th>Call</th><th>From</th><th>To</th><th>Where</th><th>Time</th></tr>
        </thead>
        <tbody>
          {#each calls as c (c.id)}
            <tr>
              <td><CallBadges direction={c.direction} status={c.status} /></td>
              <td>
                {c.caller_name || phone(c.caller)}
                {#if c.caller_name}<div class="muted mono small">{phone(c.caller)}</div>{/if}
              </td>
              <td class="mono">{phone(c.callee)}</td>
              <td>
                {#if c.status === 'answered' && c.answered_by}Ext. {c.answered_by}
                {:else if c.ringing?.length}<span class="ringing">ringing {c.ringing.join(', ')}</span>
                {:else if c.status === 'blocked'}<span class="muted">ringing silently</span>
                {:else}–{/if}
              </td>
              <td class="num">{since(c)}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
</section>

<style>
  h2 {
    display: flex;
    align-items: center;
    gap: 0.4rem;
  }
  .small {
    font-size: 0.8rem;
  }
  .ringing {
    color: var(--accent);
    animation: pulse 1.4s ease-in-out infinite;
  }
  @keyframes pulse {
    50% {
      opacity: 0.5;
    }
  }
  @media (prefers-reduced-motion: reduce) {
    .ringing {
      animation: none;
    }
  }
</style>
