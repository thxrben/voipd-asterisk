<script lang="ts">
  import Phone from '@lucide/svelte/icons/phone'
  import ShieldBan from '@lucide/svelte/icons/shield-ban'
  import Percent from '@lucide/svelte/icons/percent'
  import List from '@lucide/svelte/icons/list'
  import { api } from '../lib/api'
  import { live } from '../lib/live.svelte'
  import { router } from '../lib/router.svelte'
  import { duration, num } from '../lib/format'
  import type { Dashboard, Status, CallRow } from '../lib/types'
  import Tile from '../components/Tile.svelte'
  import StackedBars from '../components/StackedBars.svelte'
  import Donut from '../components/Donut.svelte'
  import TopList from '../components/TopList.svelte'
  import ActiveCalls from '../components/ActiveCalls.svelte'
  import CallTable from '../components/CallTable.svelte'
  import { session } from '../lib/session.svelte'
  import HealthStrip from '../components/HealthStrip.svelte'

  const ranges = [
    { id: 'today', label: 'Today' },
    { id: '24h', label: '24 h' },
    { id: '7d', label: '7 days' },
    { id: '30d', label: '30 days' },
  ] as const
  const range = $derived(router.query.get('range') ?? '24h')
  const rangeLabel = $derived(ranges.find((r) => r.id === range)?.label ?? '24 h')

  let data = $state<Dashboard | null>(null)
  let status = $state<Status | null>(null)
  let recent = $state<CallRow[]>([])
  let error = $state('')

  async function load(r: string) {
    try {
      const [d, s, c] = await Promise.all([
        api<Dashboard>(`/stats/dashboard?range=${r}`),
        api<Status>('/status'),
        api<{ calls: CallRow[] }>('/calls?limit=8'),
      ])
      data = d
      status = s
      recent = c.calls
      error = ''
    } catch (e) {
      error = (e as Error).message
    }
  }

  $effect(() => {
    void load(range)
  })

  // Refresh when calls end (debounced) and every minute.
  $effect(() => {
    let timer: ReturnType<typeof setTimeout> | undefined
    const off = live.on('call.ended', () => {
      clearTimeout(timer)
      timer = setTimeout(() => load(range), 1500)
    })
    const every = setInterval(() => load(range), 60_000)
    return () => {
      off()
      clearTimeout(timer)
      clearInterval(every)
    }
  })

  const c = {
    answered: 'var(--c-answered)',
    missed: 'var(--c-missed)',
    blocked: 'var(--c-blocked)',
    outbound: 'var(--c-outbound)',
    internal: 'var(--c-internal)',
  }
  const outcomeParts = $derived.by(() => {
    const o = data?.outcomes ?? {}
    return [
      { label: 'Answered', value: (o['answered'] ?? 0) + (o['voicemail'] ?? 0), color: c.answered },
      { label: 'Missed', value: (o['missed'] ?? 0) + (o['cancelled'] ?? 0), color: c.missed },
      { label: 'Busy / failed', value: (o['busy'] ?? 0) + (o['failed'] ?? 0), color: c.internal },
      { label: 'Blocked', value: o['blocked'] ?? 0, color: c.blocked },
    ]
  })
</script>

<div class="page">
  <div class="head">
    <h1>Dashboard</h1>
    <div class="segmented" role="group" aria-label="Time range">
      {#each ranges as r (r.id)}
        <button aria-pressed={range === r.id} onclick={() => router.go(`/?range=${r.id}`, true)}>{r.label}</button>
      {/each}
    </div>
  </div>

  {#if error}<div class="error-box">Could not load the dashboard: {error}</div>{/if}

  <div class="tiles">
    <Tile
      label="Total calls"
      value={num(data?.summary.total)}
      sub="{num(data?.summary.inbound)} in · {num(data?.summary.outbound)} out · {rangeLabel}"
      color="var(--c-calls)"
      icon={Phone}
      href="/calls"
    />
    <Tile
      label="Calls blocked"
      value={num(data?.summary.blocked)}
      sub="silently, caller heard ringing"
      color="var(--c-blocked)"
      icon={ShieldBan}
      href="/calls?status=blocked"
    />
    <Tile
      label="Blocked"
      value="{num(data?.summary.blocked_pct, 1)} %"
      sub="of incoming calls"
      color="var(--c-pct)"
      icon={Percent}
    />
    <Tile
      label="Numbers on blocklist"
      value={num(data?.summary.blocklist_size)}
      sub="across all lists"
      color="var(--c-list)"
      icon={List}
    />
  </div>

  <HealthStrip {status} summary={data?.summary ?? null} voicemailNew={data?.voicemail_new ?? 0} />

  <div class="row two">
    <section class="card">
      <header>
        <h2>Calls over time</h2>
        <span class="muted">{rangeLabel} · talk time {duration(data?.summary.talk_seconds)}</span>
      </header>
      <div class="body">
        {#if data}
          <StackedBars
            data={data.series}
            bucketSeconds={data.bucket_seconds}
            series={[
              { key: 'answered', label: 'Answered', color: c.answered },
              { key: 'missed', label: 'Missed', color: c.missed },
              { key: 'blocked', label: 'Blocked', color: c.blocked },
              { key: 'outbound', label: 'Outgoing', color: c.outbound },
              { key: 'internal', label: 'Internal', color: c.internal },
            ]}
          />
        {:else}
          <div class="empty">Loading…</div>
        {/if}
      </div>
    </section>
    <section class="card">
      <header><h2>Incoming calls</h2></header>
      <div class="body"><Donut parts={outcomeParts} center="incoming" /></div>
    </section>
  </div>

  <ActiveCalls calls={live.status?.active_calls ?? []} />

  <div class="row four">
    <TopList title="Top blocked callers" entries={data?.top.blocked ?? []} color={c.blocked} showReason
      empty="No blocked calls" />
    <TopList title="Top callers" entries={data?.top.callers ?? []} color={c.answered} />
    <TopList title="Most missed" entries={data?.top.missed ?? []} color={c.missed} empty="No missed calls" />
    <TopList title="Your numbers called" entries={data?.top.called ?? []} color={c.outbound} />
  </div>

  <section class="card">
    <header>
      <h2>Recent calls</h2>
      <a href="/calls" onclick={(e) => { e.preventDefault(); router.go('/calls') }}>Call log →</a>
    </header>
    <div class="body flush"><CallTable calls={recent} compact actions={session.isAdmin} /></div>
  </section>
</div>

<style>
  .page {
    display: grid;
    gap: 1rem;
  }
  .head {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 1rem;
    flex-wrap: wrap;
  }
  .tiles {
    display: grid;
    gap: 1rem;
    grid-template-columns: repeat(4, minmax(0, 1fr));
  }
  .row {
    display: grid;
    gap: 1rem;
  }
  .two {
    grid-template-columns: minmax(0, 2.2fr) minmax(0, 1fr);
  }
  .four {
    grid-template-columns: repeat(4, minmax(0, 1fr));
  }
  @media (max-width: 1200px) {
    .four {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
  }
  @media (max-width: 1000px) {
    .tiles {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
    .two {
      grid-template-columns: 1fr;
    }
  }
  @media (max-width: 560px) {
    .tiles {
      gap: 0.6rem;
    }
    .four {
      grid-template-columns: 1fr;
    }
  }
</style>
