<script lang="ts">
  import ArrowLeft from '@lucide/svelte/icons/arrow-left'
  import { api } from '../lib/api'
  import { clock, dateTime, duration, mosRating, num, phone, time } from '../lib/format'
  import type { CallDetail, Leg } from '../lib/types'
  import { session } from '../lib/session.svelte'
  import CallBadges from '../components/CallBadges.svelte'
  import QuickActions from '../components/blocking/QuickActions.svelte'

  interface Props {
    id: string
  }
  let { id }: Props = $props()

  let detail = $state<CallDetail | null>(null)
  let error = $state('')

  $effect(() => {
    detail = null
    api<CallDetail>(`/calls/${encodeURIComponent(id)}`)
      .then((d) => (detail = d))
      .catch((e: Error) => (error = e.message))
  })

  function lossPct(l: Leg): string {
    if (l.rx_packets == null || l.rx_lost == null) return '–'
    const total = l.rx_packets + l.rx_lost
    return total ? `${num((l.rx_lost / total) * 100, 1)} %` : '0 %'
  }
  function legName(l: Leg): string {
    return l.kind === 'trunk' ? `Line ${l.endpoint.replace(/^trunk-/, '')}` : `Phone ${l.endpoint}`
  }
  const eventLabel: Record<string, string> = {
    'call.incoming': 'Call arrived',
    'call.ringing': 'Ringing',
    'call.answered': 'Answered',
    'call.missed': 'Missed',
    'call.blocked': 'Blocked (silently ringing)',
    'call.ended': 'Ended',
    'route.none': 'No route for this number',
    'route.no_targets': 'No phone available',
    'dial.trunk': 'Dialing out',
    'dial.invalid': 'Invalid number dialed',
    'dial.no_line': 'No outgoing line',
  }
  function eventText(t: string, d: Record<string, unknown>): string {
    const parts: string[] = []
    if (Array.isArray(d['extensions'])) parts.push(`extensions ${(d['extensions'] as string[]).join(', ')}`)
    if (d['answered_by']) parts.push(`by ext. ${d['answered_by'] as string}`)
    if (d['reason'] && t !== 'call.ended') parts.push(String(d['reason']))
    if (d['dial']) parts.push(String(d['dial']))
    if (d['status'] && t === 'call.ended') parts.push(String(d['status']))
    return parts.join(' · ')
  }
</script>

<div class="page">
  <div class="head">
    <button onclick={() => history.back()}><ArrowLeft size={16} /> Back</button>
    <h1>Call details</h1>
  </div>

  {#if error}<div class="error-box">{error}</div>{/if}

  {#if detail}
    {@const c = detail.call}
    <section class="card">
      <div class="body summary">
        <div>
          <div class="muted small">Call</div>
          <CallBadges direction={c.direction} status={c.status} />
        </div>
        <div>
          <div class="muted small">From</div>
          <div>{c.caller_name || ''} <span class="mono">{c.direction === 'inbound' ? phone(c.caller) : `Ext. ${c.caller}`}</span></div>
        </div>
        <div>
          <div class="muted small">To</div>
          <div class="mono">{c.direction === 'internal' ? `Ext. ${c.callee}` : phone(c.callee)}</div>
        </div>
        <div>
          <div class="muted small">Started</div>
          <div class="num">{dateTime(c.started_at)}</div>
        </div>
        <div>
          <div class="muted small">Talk time</div>
          <div class="num">
            {#if time(c.answered_at) && time(c.ended_at)}
              {duration((time(c.ended_at)!.getTime() - time(c.answered_at)!.getTime()) / 1000)}
            {:else}–{/if}
          </div>
        </div>
        {#if c.answered_by}
          <div><div class="muted small">Answered by</div><div>Ext. {c.answered_by}</div></div>
        {/if}
        {#if c.block_reason}
          <div><div class="muted small">Blocked by</div><div>{c.block_reason}</div></div>
        {/if}
        {#if session.isAdmin && c.direction === 'inbound' && c.caller.startsWith('+')}
          <div class="actions"><div class="muted small">Caller</div><QuickActions number={c.caller} /></div>
        {/if}
      </div>
    </section>

    <div class="cols">
      <section class="card">
        <header><h2>Timeline</h2></header>
        {#if detail.events.length === 0}
          <div class="empty">No events recorded.</div>
        {:else}
          <ol class="timeline">
            {#each detail.events as ev, i (i)}
              <li>
                <span class="num muted">{clock(ev.at)}</span>
                <span><b>{eventLabel[ev.type] ?? ev.type}</b> <span class="muted">{eventText(ev.type, ev.detail)}</span></span>
              </li>
            {/each}
          </ol>
        {/if}
      </section>

      <section class="card">
        <header><h2>Connections and quality</h2></header>
        <div class="body flush table-wrap">
          <table>
            <thead>
              <tr><th>Side</th><th>Codec</th><th>Loss</th><th>Jitter</th><th>RTT</th><th>Quality</th><th>Result</th></tr>
            </thead>
            <tbody>
              {#each detail.legs as l (l.id)}
                {@const q = mosRating(l.mos)}
                <tr>
                  <td>
                    {legName(l)}
                    <div class="muted small mono">
                      {l.remote_addr || ''}{#if l.transport} · {l.transport.toUpperCase()}{/if}{#if l.srtp} · SRTP{/if}
                    </div>
                  </td>
                  <td class="mono">{l.codec || '–'}</td>
                  <td class="num">{lossPct(l)}</td>
                  <td class="num">{l.jitter_ms != null ? `${num(l.jitter_ms, 1)} ms` : '–'}</td>
                  <td class="num">{l.rtt_ms != null ? `${num(l.rtt_ms, 0)} ms` : '–'}</td>
                  <td>
                    {#if l.mos != null}<span class="badge {q.tone}">{q.label} · {l.mos.toFixed(2)}</span>{:else}–{/if}
                  </td>
                  <td class="small">{time(l.answered_at) ? 'answered' : l.hangup_cause || '–'}</td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      </section>
    </div>
  {:else if !error}
    <div class="empty">Loading…</div>
  {/if}
</div>

<style>
  .page {
    display: grid;
    gap: 1rem;
  }
  .head {
    display: flex;
    align-items: center;
    gap: 1rem;
  }
  .summary {
    display: flex;
    flex-wrap: wrap;
    gap: 1rem 2.5rem;
  }
  .small {
    font-size: 0.8rem;
  }
  .cols {
    display: grid;
    grid-template-columns: minmax(0, 1fr) minmax(0, 2fr);
    gap: 1rem;
  }
  @media (max-width: 1000px) {
    .cols {
      grid-template-columns: 1fr;
    }
  }
  .timeline {
    list-style: none;
    margin: 0;
    padding: 0.75rem 1rem;
    display: grid;
    gap: 0.5rem;
  }
  .timeline li {
    display: grid;
    grid-template-columns: 4.5rem 1fr;
    gap: 0.5rem;
  }
</style>
