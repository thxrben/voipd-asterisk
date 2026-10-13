<script lang="ts">
  import type { CallRow } from '../lib/types'
  import { dateTime, duration, phone } from '../lib/format'
  import { router } from '../lib/router.svelte'
  import Ellipsis from '@lucide/svelte/icons/ellipsis'
  import CallBadges from './CallBadges.svelte'
  import QuickActions from './blocking/QuickActions.svelte'

  interface Props {
    calls: CallRow[]
    compact?: boolean
    actions?: boolean // block / allow menu on inbound callers (admins)
  }
  let { calls, compact = false, actions = false }: Props = $props()

  // Only one row's menu is open; QuickActions checks the number when shown.
  let menuFor = $state<string | null>(null)
  const canAct = (c: CallRow) => actions && c.direction === 'inbound' && c.caller.startsWith('+')

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
          {#if actions}<th><span class="sr-only">Actions</span></th>{/if}
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
            {#if actions}
              <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_noninteractive_element_interactions -->
              <td class="menu-cell" onclick={(e) => e.stopPropagation()} onkeydown={(e) => e.stopPropagation()}>
                {#if canAct(c)}
                  <button
                    class="icon"
                    title="Block or allow caller"
                    aria-label="Block or allow caller"
                    aria-expanded={menuFor === c.id}
                    onclick={() => (menuFor = menuFor === c.id ? null : c.id)}><Ellipsis size={16} /></button>
                  {#if menuFor === c.id}
                    <div class="menu card"><QuickActions number={c.caller} /></div>
                  {/if}
                {/if}
              </td>
            {/if}
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
  button.icon {
    padding: 0.3rem;
  }
  .menu-cell {
    position: relative;
    width: 1%;
    cursor: default;
  }
  .menu {
    position: absolute;
    right: 0.5rem;
    top: calc(100% - 0.3rem);
    z-index: 5;
    padding: 0.5rem;
    white-space: nowrap;
    box-shadow: 0 6px 20px rgb(0 0 0 / 0.18);
  }
</style>
