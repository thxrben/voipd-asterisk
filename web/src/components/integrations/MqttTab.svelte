<script lang="ts">
  // MQTT: broker connection, commands and Home Assistant discovery.
  import { api } from '../../lib/api'
  import { emptySave, runSave } from '../../lib/save'
  import { toast } from '../../lib/toast.svelte'
  import type { ModuleView, MqttConfig } from '../../lib/types'
  import Field from '../Field.svelte'
  import ModuleState from './ModuleState.svelte'

  interface Props {
    mod: ModuleView
    onchange: () => void
  }
  let { mod, onchange }: Props = $props()

  const defaults: MqttConfig = {
    url: '', username: '', password: '', client_id: 'voipd', base_topic: 'voipd', insecure: false,
    commands: true, discovery: true, discovery_prefix: 'homeassistant',
  }
  let enabled = $state(false)
  let cfg = $state<MqttConfig>({ ...defaults })
  let save = $state(emptySave())
  let advanced = $state(false)

  $effect(() => {
    enabled = mod.enabled
    cfg = { ...defaults, ...(mod.config as Partial<MqttConfig>), password: '' }
  })

  const base = $derived(cfg.base_topic || 'voipd')
  const tls = $derived(/^(mqtts|ssl|tls|wss):/.test(cfg.url))

  async function submit(e: SubmitEvent) {
    e.preventDefault()
    const ok = await runSave(save, () => api('/modules/mqtt', { method: 'PUT', json: { enabled, config: cfg } }))
    if (ok) {
      toast.show(enabled ? 'MQTT saved – connecting' : 'MQTT is off')
      onchange()
    }
  }
</script>

<section class="card">
  <header>
    <h2>MQTT</h2>
    <ModuleState status={mod.status} />
  </header>
  <form class="body form" onsubmit={submit} aria-label="MQTT settings">
    <label class="switch"><input type="checkbox" bind:checked={enabled} /> Connect to an MQTT broker</label>

    <Field label="Broker" error={save.fields['url']}
      help="mqtt://host:1883, mqtts://host:8883 or wss://host/mqtt – e.g. the Mosquitto add-on of Home Assistant (MQTT 5)">
      <input bind:value={cfg.url} placeholder="mqtt://homeassistant.local:1883" />
    </Field>
    <div class="cols">
      <Field label="Username" error={save.fields['username']}><input bind:value={cfg.username} autocomplete="off" /></Field>
      <Field label="Password" error={save.fields['password']} help={cfg.password_set ? 'Leave empty to keep it' : ''}>
        <input type="password" bind:value={cfg.password} autocomplete="new-password"
          placeholder={cfg.password_set ? '•••••••• (unchanged)' : ''} />
      </Field>
    </div>
    <Field label="Home Assistant discovery: add voipd as a device with switches and sensors" inline>
      <input type="checkbox" bind:checked={cfg.discovery} />
    </Field>
    <Field label="Accept commands (blocking, do not disturb, hang up)" inline>
      <input type="checkbox" bind:checked={cfg.commands} />
    </Field>

    <button type="button" class="link" onclick={() => (advanced = !advanced)} aria-expanded={advanced}>
      {advanced ? 'Hide' : 'Show'} advanced settings
    </button>
    {#if advanced}
      <div class="cols">
        <Field label="Base topic" error={save.fields['base_topic']}><input bind:value={cfg.base_topic} /></Field>
        <Field label="Client ID" error={save.fields['client_id']}><input bind:value={cfg.client_id} /></Field>
      </div>
      <div class="cols">
        <Field label="Discovery prefix" error={save.fields['discovery_prefix']}><input bind:value={cfg.discovery_prefix} /></Field>
        {#if tls}
          <Field label="Accept any certificate (self-signed broker)" inline>
            <input type="checkbox" bind:checked={cfg.insecure} />
          </Field>
        {/if}
      </div>
    {/if}

    {#if save.error && Object.keys(save.fields).length === 0}<p class="error-box">{save.error}</p>{/if}
    <div class="actions"><button class="primary" type="submit" disabled={save.busy}>Save</button></div>
  </form>
</section>

<section class="card">
  <header><h2>Topics</h2></header>
  <div class="body topics">
    <p class="muted">State topics are retained; events are sent as they happen.</p>
    <table>
      <tbody>
        <tr><td class="mono">{base}/status</td><td>online / offline</td></tr>
        <tr><td class="mono">{base}/line/&lt;number&gt;/state</td><td>up / down</td></tr>
        <tr><td class="mono">{base}/extension/&lt;ext&gt;/dnd</td><td>ON / OFF</td></tr>
        <tr><td class="mono">{base}/extension/&lt;ext&gt;/registered</td><td>ON / OFF</td></tr>
        <tr><td class="mono">{base}/calls/active</td><td>calls in progress (list: …/active/list)</td></tr>
        <tr><td class="mono">{base}/calls/last_caller</td><td>name or number</td></tr>
        <tr><td class="mono">{base}/stats/today</td><td>JSON: total, missed, blocked, …</td></tr>
        <tr><td class="mono">{base}/blocking/anonymous</td><td>ON / OFF</td></tr>
        <tr><td class="mono">{base}/blocking/paused</td><td>ON / OFF</td></tr>
        <tr><td class="mono">{base}/event/&lt;type&gt;</td><td>every event as JSON, e.g. call.missed</td></tr>
      </tbody>
    </table>
    {#if cfg.commands}
      <h3>Commands</h3>
      <table>
        <tbody>
          <tr><td class="mono">{base}/blocking/anonymous/set</td><td>ON / OFF</td></tr>
          <tr><td class="mono">{base}/blocking/pause/set</td><td>minutes, 0 = resume</td></tr>
          <tr><td class="mono">{base}/blocklist/add</td><td>number or JSON {'{'}"number", "label"{'}'}</td></tr>
          <tr><td class="mono">{base}/allowlist/add</td><td>number or JSON</td></tr>
          <tr><td class="mono">{base}/extension/&lt;ext&gt;/dnd/set</td><td>ON / OFF</td></tr>
          <tr><td class="mono">{base}/call/&lt;id&gt;/hangup</td><td>ends a call in progress</td></tr>
        </tbody>
      </table>
    {/if}
  </div>
</section>

<style>
  .switch {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    font-weight: 600;
  }
  .link {
    justify-self: start;
    background: none;
    border: none;
    padding: 0;
    color: var(--accent);
    text-decoration: underline;
  }
  .actions {
    display: flex;
    justify-content: flex-end;
  }
  .topics p {
    margin: 0 0 0.5rem;
  }
  .topics h3 {
    margin: 1rem 0 0.3rem;
    font-size: 0.8rem;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--muted);
  }
  .topics td {
    padding: 0.25rem 0.75rem 0.25rem 0;
    font-size: 0.86rem;
    overflow-wrap: anywhere;
  }
</style>
