<script lang="ts">
  // Call log ("query log"): filters live in the URL so views can be linked.
  import { api } from '../lib/api'
  import { live } from '../lib/live.svelte'
  import { router } from '../lib/router.svelte'
  import { session } from '../lib/session.svelte'
  import type { CallRow } from '../lib/types'
  import CallTable from '../components/CallTable.svelte'

  const statuses = [
    { id: 'answered', label: 'Answered' },
    { id: 'missed,cancelled', label: 'Missed' },
    { id: 'blocked', label: 'Blocked' },
    { id: 'busy,failed', label: 'Busy / failed' },
  ]

  let calls = $state<CallRow[]>([])
  let next = $state<number | null>(null)
  let loading = $state(false)
  let error = $state('')

  const q = $derived(router.query)
  let search = $state(router.query.get('q') ?? '')

  function setParam(name: string, value: string) {
    const p = new URLSearchParams(router.search)
    if (value) p.set(name, value)
    else p.delete(name)
    const s = p.toString()
    router.go(`/calls${s ? `?${s}` : ''}`, true)
  }

  function rangeFrom(r: string | null): string {
    const h = r === '7d' ? 168 : r === '30d' ? 720 : r === '24h' ? 24 : 0
    return h ? String(Date.now() - h * 3600_000) : ''
  }

  function params(before?: number): string {
    const p = new URLSearchParams()
    for (const k of ['direction', 'status', 'q', 'ext']) {
      const v = q.get(k)
      if (v) p.set(k, v)
    }
    const from = rangeFrom(q.get('range'))
    if (from) p.set('from', from)
    if (before) p.set('before', String(before))
    p.set('limit', '50')
    return p.toString()
  }

  async function load(more = false) {
    loading = true
    try {
      const res = await api<{ calls: CallRow[]; next_before?: number }>(
        `/calls?${params(more ? (next ?? undefined) : undefined)}`,
      )
      calls = more ? [...calls, ...res.calls] : res.calls
      next = res.next_before ?? null
      error = ''
    } catch (e) {
      error = (e as Error).message
    } finally {
      loading = false
    }
  }

  $effect(() => {
    void router.search // reload whenever the filters change
    void load()
  })

  // New calls appear at the top without losing the scroll position.
  $effect(() => {
    let t: ReturnType<typeof setTimeout> | undefined
    const off = live.on('call.ended', () => {
      clearTimeout(t)
      t = setTimeout(() => {
        if (!q.get('before')) void load()
      }, 1200)
    })
    return () => {
      off()
      clearTimeout(t)
    }
  })

  function submitSearch(e: SubmitEvent) {
    e.preventDefault()
    setParam('q', search.trim())
  }
</script>

<div class="page">
  <div class="head">
    <h1>{session.isAdmin ? 'Call log' : 'My calls'}</h1>
  </div>

  <section class="card">
    <form class="filters" onsubmit={submitSearch}>
      <label>
        Search
        <input type="search" placeholder="Number or name" bind:value={search} />
      </label>
      <label>
        Period
        <select value={q.get('range') ?? ''} onchange={(e) => setParam('range', e.currentTarget.value)}>
          <option value="">All time</option>
          <option value="24h">Last 24 hours</option>
          <option value="7d">Last 7 days</option>
          <option value="30d">Last 30 days</option>
        </select>
      </label>
      <label>
        Direction
        <select value={q.get('direction') ?? ''} onchange={(e) => setParam('direction', e.currentTarget.value)}>
          <option value="">All</option>
          <option value="inbound">Incoming</option>
          <option value="outbound">Outgoing</option>
          <option value="internal">Internal</option>
        </select>
      </label>
      <label>
        Outcome
        <select value={q.get('status') ?? ''} onchange={(e) => setParam('status', e.currentTarget.value)}>
          <option value="">All</option>
          {#each statuses as s (s.id)}<option value={s.id}>{s.label}</option>{/each}
        </select>
      </label>
      {#if session.isAdmin}
        <label>
          Extension
          <input
            class="ext"
            inputmode="numeric"
            placeholder="e.g. 10"
            value={q.get('ext') ?? ''}
            onchange={(e) => setParam('ext', e.currentTarget.value.trim())}
          />
        </label>
      {/if}
      <div class="actions">
        <button type="submit" class="primary">Search</button>
        {#if router.search}
          <button type="button" onclick={() => { search = ''; router.go('/calls', true) }}>Reset</button>
        {/if}
      </div>
    </form>
  </section>

  {#if error}<div class="error-box">{error}</div>{/if}

  <section class="card">
    <div class="body flush"><CallTable {calls} /></div>
  </section>

  {#if next}
    <div class="more"><button onclick={() => load(true)} disabled={loading}>Load more</button></div>
  {/if}
</div>

<style>
  .page {
    display: grid;
    gap: 1rem;
  }
  .filters {
    display: flex;
    flex-wrap: wrap;
    gap: 0.75rem 1rem;
    align-items: end;
    padding: 0.9rem 1rem;
  }
  .filters input[type='search'] {
    width: 16rem;
    max-width: 100%;
  }
  .ext {
    width: 6rem;
  }
  .actions {
    display: flex;
    gap: 0.5rem;
  }
  .more {
    text-align: center;
  }
</style>
