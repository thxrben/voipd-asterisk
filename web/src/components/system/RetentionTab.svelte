<script lang="ts">
  // Call duration limit and how long history is kept.
  import { api } from '../../lib/api'
  import { emptySave, runSave } from '../../lib/save'
  import { toast } from '../../lib/toast.svelte'
  import { ago } from '../../lib/format'
  import type { RetentionInfo, RetentionSettings } from '../../lib/types'
  import Field from '../Field.svelte'

  let info = $state<RetentionInfo | null>(null)
  let error = $state('')
  let set = $state<RetentionSettings>({
    max_call_minutes: 240, calls_days: 730, audit_days: 365, sip_events_days: 30, webhook_days: 30, voicemail_heard_days: 0,
  })
  let save = $state(emptySave())
  let pruning = $state(false)

  async function reload(fill = false) {
    try {
      info = await api<RetentionInfo>('/retention')
      if (fill) set = { ...info.settings }
      error = ''
    } catch (e) {
      error = (e as Error).message
    }
  }
  $effect(() => {
    void reload(true)
  })

  async function submit(e: SubmitEvent) {
    e.preventDefault()
    const body = Object.fromEntries(Object.entries(set).map(([k, v]) => [k, Number(v) || 0]))
    const ok = await runSave(save, () => api('/retention', { method: 'PUT', json: body }))
    if (ok) {
      toast.show('Saved')
      void reload()
    }
  }

  async function prune() {
    pruning = true
    try {
      await api('/retention/prune', { method: 'POST' })
      toast.show('Old history removed')
      void reload()
    } catch (e) {
      toast.show((e as Error).message, 'bad')
    } finally {
      pruning = false
    }
  }

  const rows: { key: keyof RetentionSettings; label: string }[] = [
    { key: 'calls_days', label: 'Call history (with quality data)' },
    { key: 'voicemail_heard_days', label: 'Voicemail messages after they were heard' },
    { key: 'audit_days', label: 'Audit log' },
    { key: 'sip_events_days', label: 'Line and security events' },
    { key: 'webhook_days', label: 'Webhook delivery log' },
  ]
</script>

{#if error}<div class="error-box">{error}</div>{/if}

<form class="card" onsubmit={submit} aria-label="Call limits and history">
  <header><h2>Call limit</h2></header>
  <div class="body form">
    <p class="muted">
      Answered calls end after this long, so a phone left off the hook or a stuck line does not keep the line busy or run
      up costs. Calls in progress keep the limit they started with.
    </p>
    <Field label="Maximum call duration (minutes)" error={save.fields['max_call_minutes']} help="0 = no limit">
      <input type="number" min="0" bind:value={set.max_call_minutes} />
    </Field>
  </div>

  <header class="sub"><h2>Keep history</h2></header>
  <div class="body form">
    <p class="muted">Older entries are removed once a day. 0 keeps them forever.</p>
    <div class="days">
      {#each rows as r (r.key)}
        <Field label={`${r.label} (days)`} error={save.fields[r.key]}>
          <input type="number" min="0" bind:value={set[r.key]} />
        </Field>
      {/each}
    </div>
    {#if info?.last_run}
      {@const l = info.last_run}
      <p class="muted small">
        Last cleanup {ago(l.at)}:
        {#if l.error}<span class="error">{l.error}</span>
        {:else}{l.calls} calls, {l.voicemails} messages, {l.audit} audit entries, {l.sip_events} events and
          {l.webhook_deliveries} webhook deliveries removed{/if}
      </p>
    {/if}
    {#if save.error && Object.keys(save.fields).length === 0}<p class="error-box">{save.error}</p>{/if}
    <div class="actions">
      <button type="button" onclick={prune} disabled={pruning}>Clean up now</button>
      <button class="primary" type="submit" disabled={save.busy}>Save</button>
    </div>
  </div>
</form>

<style>
  .form {
    display: grid;
    gap: 0.9rem;
  }
  p {
    margin: 0;
  }
  .sub {
    border-top: 1px solid var(--border);
  }
  .days {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(16rem, 1fr));
    gap: 0.9rem;
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: 0.5rem;
  }
  .small {
    font-size: 0.82rem;
  }
  .error {
    color: var(--bad);
  }
</style>
