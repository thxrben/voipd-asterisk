<script lang="ts">
  // Integrations: MQTT (Home Assistant), e-mail notifications and webhooks.
  import { api } from '../lib/api'
  import { router } from '../lib/router.svelte'
  import type { ModuleView } from '../lib/types'
  import EmailTab from '../components/integrations/EmailTab.svelte'
  import WebhooksTab from '../components/integrations/WebhooksTab.svelte'
  import MqttTab from '../components/integrations/MqttTab.svelte'

  const tabs = [
    { id: 'mqtt', label: 'MQTT', module: 'mqtt' },
    { id: 'email', label: 'E-mail', module: 'notify' },
    { id: 'webhooks', label: 'Webhooks', module: 'webhook' },
  ] as const
  const tab = $derived(tabs.find((t) => t.id === router.query.get('tab')) ?? tabs[0])

  let mods = $state<Record<string, ModuleView>>({})
  let error = $state('')

  async function reload() {
    try {
      const res = await api<{ modules: ModuleView[] }>('/modules')
      mods = Object.fromEntries(res.modules.map((m) => [m.name, m]))
      error = ''
    } catch (e) {
      error = (e as Error).message
    }
  }
  $effect(() => {
    void reload()
    // Statuses change on their own (connecting, errors).
    const t = setInterval(reload, 15_000)
    return () => clearInterval(t)
  })
</script>

<div class="page">
  <div class="head">
    <h1>Integrations</h1>
    <div class="segmented" role="tablist" aria-label="Section">
      {#each tabs as t (t.id)}
        <button role="tab" aria-selected={tab.id === t.id} onclick={() => router.go(`/integrations?tab=${t.id}`, true)}>
          {t.label}
          {#if mods[t.module]?.status.state === 'error'}<span class="dot bad" aria-label="error"></span>{/if}
        </button>
      {/each}
    </div>
  </div>

  {#if error}<div class="error-box">{error}</div>{/if}

  {#if mods[tab.module]}
    {@const mod = mods[tab.module]!}
    {#if tab.id === 'webhooks'}
      <WebhooksTab {mod} onchange={reload} />
    {:else if tab.id === 'email'}
      <EmailTab {mod} onchange={reload} />
    {:else}
      <MqttTab {mod} onchange={reload} />
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
