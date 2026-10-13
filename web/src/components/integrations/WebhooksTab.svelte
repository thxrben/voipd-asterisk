<script lang="ts">
  // Webhooks: endpoints that receive events as signed HTTP POSTs.
  import Plus from '@lucide/svelte/icons/plus'
  import Pencil from '@lucide/svelte/icons/pencil'
  import Trash from '@lucide/svelte/icons/trash-2'
  import Send from '@lucide/svelte/icons/send'
  import History from '@lucide/svelte/icons/history'
  import Copy from '@lucide/svelte/icons/copy'
  import { api } from '../../lib/api'
  import { dateTime } from '../../lib/format'
  import { emptySave, runSave } from '../../lib/save'
  import { toast } from '../../lib/toast.svelte'
  import type { ModuleView, WebhookDelivery, WebhookEndpoint } from '../../lib/types'
  import Modal from '../Modal.svelte'
  import Field from '../Field.svelte'
  import ModuleState from './ModuleState.svelte'

  interface Props {
    mod: ModuleView
    onchange: () => void
  }
  let { mod, onchange }: Props = $props()

  const eventChoices = [
    { id: 'call.incoming', label: 'Incoming call' },
    { id: 'call.ringing', label: 'Phones ringing' },
    { id: 'call.answered', label: 'Answered' },
    { id: 'call.missed', label: 'Missed call' },
    { id: 'call.blocked', label: 'Blocked call' },
    { id: 'call.ended', label: 'Call ended' },
    { id: 'trunk.', label: 'Line up / down' },
    { id: 'extension.', label: 'Do not disturb' },
    { id: 'list.', label: 'List syncs' },
    { id: 'blocking.', label: 'Blocking settings' },
  ]

  let endpoints = $state<WebhookEndpoint[]>([])
  let error = $state('')
  async function load() {
    try {
      endpoints = (await api<{ endpoints: WebhookEndpoint[] }>('/webhooks')).endpoints
      error = ''
    } catch (e) {
      error = (e as Error).message
    }
  }
  $effect(() => {
    void load()
  })

  async function setModule(on: boolean) {
    try {
      await api('/modules/webhook', { method: 'PUT', json: { enabled: on, config: {} } })
      onchange()
    } catch (e) {
      toast.show((e as Error).message, 'bad')
    }
  }

  // --- add / edit ---
  let ep = $state<WebhookEndpoint | null>(null)
  let allEvents = $state(true)
  let save = $state(emptySave())
  let secret = $state('') // shown once after create / regenerate

  function edit(e?: WebhookEndpoint) {
    ep = e ? { ...e, events: [...e.events] } : { id: 0, name: '', url: '', events: [], enabled: true }
    allEvents = ep.events.length === 0
    save = emptySave()
  }

  function toggleEvent(id: string, on: boolean) {
    if (!ep) return
    ep.events = on ? [...ep.events, id] : ep.events.filter((x) => x !== id)
  }

  async function submit(e: SubmitEvent) {
    e.preventDefault()
    if (!ep) return
    const body = { name: ep.name, url: ep.url, enabled: ep.enabled, events: allEvents ? [] : ep.events }
    const id = ep.id
    let created: { result: { secret: string } } | undefined
    const ok = await runSave(save, async () => {
      if (id) return api(`/webhooks/${id}`, { method: 'PUT', json: body })
      created = await api<{ result: { secret: string } }>('/webhooks', { method: 'POST', json: body })
      return created
    })
    if (ok) {
      toast.show(`${body.name} saved`)
      ep = null
      if (created) secret = created.result.secret
      void load()
    }
  }

  async function remove(e: WebhookEndpoint) {
    if (!confirm(`Delete webhook "${e.name}"?`)) return
    try {
      await api(`/webhooks/${e.id}`, { method: 'DELETE' })
      void load()
    } catch (err) {
      toast.show((err as Error).message, 'bad')
    }
  }

  async function regenerate(e: WebhookEndpoint) {
    if (!confirm(`Create a new secret for "${e.name}"? The receiver must then use the new one.`)) return
    try {
      secret = (await api<{ result: { secret: string } }>(`/webhooks/${e.id}/secret`, { method: 'POST' })).result.secret
    } catch (err) {
      toast.show((err as Error).message, 'bad')
    }
  }

  async function test(e: WebhookEndpoint) {
    try {
      const d = (await api<{ result: WebhookDelivery }>(`/webhooks/${e.id}/test`, { method: 'POST' })).result
      if (d.status_code >= 200 && d.status_code < 300) toast.show(`${e.name}: test delivered (HTTP ${d.status_code}, ${d.latency_ms} ms)`)
      else toast.show(`${e.name}: ${d.error || `HTTP ${d.status_code}`}`, 'bad')
      if (history?.id === e.id) void showHistory(e)
    } catch (err) {
      toast.show((err as Error).message, 'bad')
    }
  }

  // --- deliveries ---
  let history = $state<WebhookEndpoint | null>(null)
  let deliveries = $state<WebhookDelivery[]>([])
  async function showHistory(e: WebhookEndpoint) {
    history = e
    try {
      deliveries = (await api<{ deliveries: WebhookDelivery[] }>(`/webhooks/${e.id}/deliveries`)).deliveries
    } catch (err) {
      toast.show((err as Error).message, 'bad')
    }
  }

  function copy(text: string) {
    void navigator.clipboard?.writeText(text).then(() => toast.show('Copied'))
  }
  const label = (id: string) => eventChoices.find((c) => c.id === id)?.label ?? id
</script>

<section class="card">
  <header>
    <h2>Webhooks</h2>
    <ModuleState status={mod.status} />
  </header>
  <div class="body intro">
    <label class="switch">
      <input type="checkbox" checked={mod.enabled} onchange={(e) => setModule(e.currentTarget.checked)} /> Send events to webhooks
    </label>
    <p class="muted">
      Each event is POSTed as JSON, signed with the endpoint's secret:
      <span class="mono">X-Voipd-Signature: sha256=HMAC(secret, timestamp + "." + body)</span>, with the timestamp in
      <span class="mono">X-Voipd-Timestamp</span>. Failed deliveries are retried 4 times.
    </p>
  </div>
</section>

{#if error}<div class="error-box">{error}</div>{/if}

<section class="card">
  <header>
    <h2>Endpoints</h2>
    <button class="primary add" onclick={() => edit()}><Plus size={16} /> Add webhook</button>
  </header>
  {#if endpoints.length === 0}
    <div class="empty">No webhooks yet.</div>
  {:else}
    <div class="body flush table-wrap">
      <table>
        <thead><tr><th>Name</th><th>URL</th><th>Events</th><th></th></tr></thead>
        <tbody>
          {#each endpoints as e (e.id)}
            <tr class:off={!e.enabled}>
              <td><b>{e.name}</b>{#if !e.enabled} <span class="badge">paused</span>{/if}</td>
              <td class="mono url" title={e.url}>{e.url}</td>
              <td>{e.events.length === 0 ? 'all' : e.events.map(label).join(', ')}</td>
              <td class="row-actions">
                <button onclick={() => test(e)} aria-label="Send test to {e.name}" title="Send test"><Send size={15} /></button>
                <button onclick={() => showHistory(e)} aria-label="Deliveries of {e.name}" title="Deliveries"><History size={15} /></button>
                <button onclick={() => edit(e)} aria-label="Edit {e.name}"><Pencil size={15} /></button>
                <button onclick={() => remove(e)} aria-label="Delete {e.name}"><Trash size={15} /></button>
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
</section>

<Modal title={ep?.id ? `Edit ${ep.name}` : 'Add webhook'} open={!!ep} onclose={() => (ep = null)}>
  {#if ep}
    <form id="webhook-form" class="form" onsubmit={submit}>
      <Field label="Name" error={save.fields['name']}><input bind:value={ep.name} required /></Field>
      <Field label="URL" error={save.fields['url']} help="e.g. a Home Assistant or Node-RED webhook">
        <input type="url" bind:value={ep.url} required placeholder="https://" />
      </Field>
      <Field label="Events" group error={save.fields['events']}>
        <label class="check"><input type="checkbox" bind:checked={allEvents} /> All events</label>
        {#if !allEvents}
          <div class="events">
            {#each eventChoices as c (c.id)}
              <label class="check">
                <input type="checkbox" checked={ep.events.includes(c.id)} onchange={(e) => toggleEvent(c.id, e.currentTarget.checked)} />
                {c.label}
              </label>
            {/each}
          </div>
        {/if}
      </Field>
      <label class="check"><input type="checkbox" bind:checked={ep.enabled} /> Active</label>
      {#if ep.id}
        <button type="button" class="link" onclick={() => { const e = ep!; ep = null; void regenerate(e) }}>Create a new secret…</button>
      {/if}
      {#if save.error && Object.keys(save.fields).length === 0}<p class="error-box">{save.error}</p>{/if}
    </form>
  {/if}
  {#snippet footer()}
    <button onclick={() => (ep = null)}>Cancel</button>
    <button class="primary" type="submit" form="webhook-form" disabled={save.busy}>{ep?.id ? 'Save' : 'Add'}</button>
  {/snippet}
</Modal>

<Modal title="Webhook secret" open={!!secret} onclose={() => (secret = '')}>
  <p>Configure the receiver with this secret to verify signatures. It is shown only now.</p>
  <div class="secret">
    <code class="mono" data-testid="webhook-secret">{secret}</code>
    <button onclick={() => copy(secret)} aria-label="Copy secret"><Copy size={15} /></button>
  </div>
  {#snippet footer()}
    <button class="primary" onclick={() => (secret = '')}>Done</button>
  {/snippet}
</Modal>

<Modal title={history ? `Deliveries: ${history.name}` : ''} open={!!history} onclose={() => (history = null)} wide>
  {#if deliveries.length === 0}
    <div class="empty">Nothing delivered yet.</div>
  {:else}
    <div class="table-wrap">
      <table>
        <thead><tr><th>Time</th><th>Event</th><th>Try</th><th>Result</th><th class="right">ms</th></tr></thead>
        <tbody>
          {#each deliveries as d (d.id)}
            <tr>
              <td class="num nowrap">{dateTime(d.at)}</td>
              <td class="mono">{d.event_type}</td>
              <td class="num">{d.attempt}</td>
              <td>
                <span class="dot {d.status_code >= 200 && d.status_code < 300 ? 'good' : 'bad'}"></span>
                {d.status_code || ''} {d.error}
                {#if d.response}<div class="muted small mono resp" title={d.response}>{d.response}</div>{/if}
              </td>
              <td class="num right">{d.latency_ms}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
  {#snippet footer()}
    <button onclick={() => (history = null)}>Close</button>
  {/snippet}
</Modal>

<style>
  .intro {
    display: grid;
    gap: 0.6rem;
  }
  .intro p {
    margin: 0;
    font-size: 0.88rem;
  }
  .switch {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    font-weight: 600;
  }
  .add {
    margin-left: auto;
  }
  .off {
    opacity: 0.6;
  }
  .url {
    max-width: 22rem;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .row-actions {
    display: flex;
    gap: 0.3rem;
    justify-content: flex-end;
  }
  .row-actions button {
    padding: 0.3rem;
  }
  .check {
    display: flex;
    align-items: center;
    gap: 0.45rem;
  }
  .events {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(11rem, 1fr));
    gap: 0.35rem 1rem;
  }
  .secret {
    display: flex;
    gap: 0.5rem;
    align-items: center;
  }
  .secret code {
    flex: 1;
    padding: 0.5rem;
    background: var(--bg-soft, rgb(127 127 127 / 0.1));
    border-radius: 6px;
    overflow-wrap: anywhere;
  }
  .link {
    justify-self: start;
    background: none;
    border: none;
    padding: 0;
    color: var(--accent);
    text-decoration: underline;
  }
  .small {
    font-size: 0.8rem;
  }
  .resp {
    max-width: 24rem;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .right {
    text-align: right;
  }
  .nowrap {
    white-space: nowrap;
  }
</style>
