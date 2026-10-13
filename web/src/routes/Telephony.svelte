<script lang="ts">
  // Telephony configuration: lines (trunks + numbers), extensions, ring groups.
  import { api } from '../lib/api'
  import { router } from '../lib/router.svelte'
  import type { Status, Telephony } from '../lib/types'
  import LinesTab from '../components/telephony/LinesTab.svelte'
  import ExtensionsTab from '../components/telephony/ExtensionsTab.svelte'
  import RingGroupsTab from '../components/telephony/RingGroupsTab.svelte'

  const tabs = [
    { id: 'lines', label: 'Lines' },
    { id: 'extensions', label: 'Extensions' },
    { id: 'groups', label: 'Ring groups' },
  ] as const
  const tab = $derived(router.query.get('tab') ?? 'lines')

  let data = $state<Telephony | null>(null)
  let status = $state<Status | null>(null)
  let error = $state('')

  async function reload() {
    try {
      const [t, s] = await Promise.all([api<Telephony>('/telephony'), api<Status>('/status')])
      data = t
      status = s
      error = ''
    } catch (e) {
      error = (e as Error).message
    }
  }
  $effect(() => {
    void reload()
  })
</script>

<div class="page">
  <div class="head">
    <h1>Telephony</h1>
    <div class="segmented" role="tablist" aria-label="Section">
      {#each tabs as t (t.id)}
        <button role="tab" aria-selected={tab === t.id}
          onclick={() => router.go(`/telephony?tab=${t.id}`, true)}>{t.label}</button>
      {/each}
    </div>
  </div>

  {#if error}<div class="error-box">{error}</div>{/if}

  {#if data}
    {#if tab === 'extensions'}
      <ExtensionsTab {data} onchange={reload} />
    {:else if tab === 'groups'}
      <RingGroupsTab {data} onchange={reload} />
    {:else}
      <LinesTab {data} {status} onchange={reload} />
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
</style>
