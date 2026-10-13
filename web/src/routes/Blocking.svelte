<script lang="ts">
  // Call blocking: global switches, lists and their sources, numbers, import.
  import ShieldCheck from '@lucide/svelte/icons/shield-check'
  import ShieldOff from '@lucide/svelte/icons/shield-off'
  import { api } from '../lib/api'
  import { clock } from '../lib/format'
  import { live } from '../lib/live.svelte'
  import { router } from '../lib/router.svelte'
  import { toast } from '../lib/toast.svelte'
  import type { Screening, ScreeningSettings } from '../lib/types'
  import NumberCheck from '../components/blocking/NumberCheck.svelte'
  import SourcesTab from '../components/blocking/SourcesTab.svelte'
  import EntriesTab from '../components/blocking/EntriesTab.svelte'
  import ImportPanel from '../components/blocking/ImportPanel.svelte'

  const tabs = [
    { id: 'lists', label: 'Lists' },
    { id: 'numbers', label: 'Numbers' },
    { id: 'import', label: 'Import' },
  ] as const
  const tab = $derived(router.query.get('tab') ?? 'lists')

  let data = $state<Screening | null>(null)
  let error = $state('')
  let busy = $state(false)

  async function reload() {
    try {
      data = await api<Screening>('/screening')
      error = ''
    } catch (e) {
      error = (e as Error).message
    }
  }
  $effect(() => {
    void reload()
    let t: ReturnType<typeof setTimeout>
    const debounced = () => {
      clearTimeout(t)
      t = setTimeout(() => void reload(), 300)
    }
    const offs = [live.on('list.', debounced), live.on('blocking.', debounced)]
    return () => offs.forEach((off) => off())
  })

  // The pause ends by itself; refresh then so the banner disappears.
  $effect(() => {
    const until = data?.settings.paused_until
    if (!until) return
    const ms = new Date(until).getTime() - Date.now()
    const t = setTimeout(() => void reload(), Math.max(ms + 500, 500))
    return () => clearTimeout(t)
  })

  async function setSetting(patch: Partial<ScreeningSettings>) {
    busy = true
    try {
      await api('/screening/settings', { method: 'PUT', json: patch })
      await reload()
    } catch (e) {
      toast.show((e as Error).message, 'bad')
    } finally {
      busy = false
    }
  }

  async function pause(minutes: number) {
    busy = true
    try {
      await api('/screening/pause', { method: 'POST', json: { minutes } })
      toast.show(minutes ? `Blocking paused for ${minutes < 60 ? `${minutes} minutes` : `${minutes / 60} hour(s)`}` : 'Blocking resumed')
      await reload()
    } catch (e) {
      toast.show((e as Error).message, 'bad')
    } finally {
      busy = false
    }
  }
</script>

<div class="page">
  <div class="head">
    <h1>Blocking</h1>
    <div class="segmented" role="tablist" aria-label="Section">
      {#each tabs as t (t.id)}
        <button role="tab" aria-selected={tab === t.id} onclick={() => router.go(`/blocking?tab=${t.id}`, true)}>{t.label}</button>
      {/each}
    </div>
  </div>

  {#if error}<div class="error-box">{error}</div>{/if}

  {#if data}
    {@const paused = !!data.settings.paused_until}
    <section class="card status" class:paused>
      <div class="body top">
        <div class="state">
          {#if paused}
            <ShieldOff size={30} />
            <div>
              <b>Blocking is paused</b>
              <span class="muted">until {clock(data.settings.paused_until)} – all calls ring</span>
            </div>
          {:else}
            <ShieldCheck size={30} />
            <div>
              <b>Blocking is active</b>
              <span class="muted">
                {data.sizes.block.toLocaleString()} blocked numbers · {data.sizes.allow.toLocaleString()} allowed ·
                {data.sizes.contacts.toLocaleString()} contact numbers
              </span>
            </div>
          {/if}
        </div>
        <div class="pause">
          {#if paused}
            <button class="primary" disabled={busy} onclick={() => pause(0)}>Resume blocking</button>
          {:else}
            <span class="muted">Pause for</span>
            {#each [5, 30, 60] as m (m)}
              <button disabled={busy} onclick={() => pause(m)}>{m < 60 ? `${m} min` : '1 hour'}</button>
            {/each}
          {/if}
        </div>
      </div>
      <div class="body switches">
        <label class="switch">
          <input type="checkbox" checked={data.settings.anonymous_block} disabled={busy}
            onchange={(e) => setSetting({ anonymous_block: e.currentTarget.checked })} />
          <span><b>Block anonymous callers</b><span class="muted">Calls without a number ring silently for the caller</span></span>
        </label>
        <label class="switch">
          <input type="checkbox" checked={data.settings.contacts_allowed} disabled={busy}
            onchange={(e) => setSetting({ contacts_allowed: e.currentTarget.checked })} />
          <span><b>Never block contacts</b><span class="muted">Numbers in your contacts always ring, even if a list blocks them</span></span>
        </label>
      </div>
      <div class="body">
        <NumberCheck />
      </div>
    </section>

    {#if tab === 'numbers'}
      <EntriesTab sources={data.sources} onchange={reload} />
    {:else if tab === 'import'}
      <ImportPanel targets={['block', 'allow']} onchange={reload} />
    {:else}
      <SourcesTab sources={data.sources.filter((s) => s.target !== 'contacts')} onchange={reload} />
    {/if}
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
    justify-content: space-between;
    align-items: center;
    gap: 1rem;
    flex-wrap: wrap;
  }
  .status {
    border-left: 4px solid var(--good);
  }
  .status.paused {
    border-left-color: var(--warn);
  }
  .top {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 1rem;
    flex-wrap: wrap;
  }
  .state {
    display: flex;
    align-items: center;
    gap: 0.8rem;
    color: var(--good);
  }
  .paused .state {
    color: var(--warn);
  }
  .state div {
    display: grid;
    color: var(--text);
  }
  .state b {
    font-size: 1.1rem;
  }
  .pause {
    display: flex;
    align-items: center;
    gap: 0.4rem;
    flex-wrap: wrap;
  }
  .switches {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 0.8rem 1.5rem;
    border-top: 1px solid var(--border);
  }
  @media (max-width: 700px) {
    .switches {
      grid-template-columns: 1fr;
    }
  }
  .switch {
    display: flex;
    gap: 0.6rem;
    align-items: flex-start;
  }
  .switch input {
    margin-top: 0.25rem;
  }
  .switch > span {
    display: grid;
    gap: 0.1rem;
  }
  .switch .muted {
    font-size: 0.85rem;
  }
  .card > .body + .body {
    border-top: 1px solid var(--border);
  }
</style>
