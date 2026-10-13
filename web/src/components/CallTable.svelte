<script lang="ts">
  import type { CallRow } from '../lib/types'
  import { dateTime, duration, phone } from '../lib/format'
  import { router } from '../lib/router.svelte'
  import CallBadges from './CallBadges.svelte'

  interface Props {
    calls: CallRow[]
    compact?: boolean
  }
  let { calls, compact = false }: Props = $props()

  function open(c: CallRow) {
    router.go(`/calls/${c.id}`)
  }
  function onKey(e: KeyboardEvent, c: CallRow) {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault()
      open(c)
    }
  }
</script>

{#if calls.length === 0}
  <div class="empty">No calls found.</div>
{:else}
  <div class="table-wrap">
    <table>
      <thead>
        <tr>
          <th>Time</th>
          <th>Call</th>
          <th>From</th>
          <th>To</th>
          {#if !compact}<th>Answered by</th>{/if}
          <th class="right">Duration</th>
        </tr>
      </thead>
      <tbody>
        {#each calls as c (c.id)}
          <tr class="clickable" tabindex="0" onclick={() => open(c)} onkeydown={(e) => onKey(e, c)}>
            <td class="num nowrap">{dateTime(c.started_at)}</td>
            <td>
              <CallBadges direction={c.direction} status={c.status} />
              {#if c.block_reason && !compact}<div class="muted small">{c.block_reason}</div>{/if}
            </td>
            <td>
              {#if c.caller_name}{c.caller_name}<div class="muted mono small">{phone(c.caller)}</div>
              {:else}<span class="mono">{c.direction === 'inbound' ? phone(c.caller) : `Ext. ${c.caller}`}</span>{/if}
            </td>
            <td class="mono">{c.direction === 'internal' ? `Ext. ${c.callee}` : phone(c.callee)}</td>
            {#if !compact}<td>{c.answered_by ? `Ext. ${c.answered_by}` : '–'}</td>{/if}
            <td class="num right">
              {#if c.status === 'answered'}{duration(c.duration_ms / 1000)}
              {:else}<span class="muted">rang {duration(c.ring_ms / 1000)}</span>{/if}
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
{/if}

<style>
  .small {
    font-size: 0.8rem;
  }
  .nowrap {
    white-space: nowrap;
  }
  .right {
    text-align: right;
  }
</style>
