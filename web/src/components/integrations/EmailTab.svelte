<script lang="ts">
  // E-mail notifications: SMTP server, who gets which mails, test mail.
  import { api } from '../../lib/api'
  import { emptySave, runSave } from '../../lib/save'
  import { session } from '../../lib/session.svelte'
  import { toast } from '../../lib/toast.svelte'
  import type { ModuleView, NotifyConfig } from '../../lib/types'
  import Field from '../Field.svelte'
  import ModuleState from './ModuleState.svelte'

  interface Props {
    mod: ModuleView
    onchange: () => void
  }
  let { mod, onchange }: Props = $props()

  const defaults: NotifyConfig = {
    host: '', port: 587, security: 'starttls', username: '', password: '', from: '',
    missed_calls: true, missed_to: [], blocked_calls: false, trunk_alerts: true, admin_to: [],
    voicemail: true, voicemail_attach: true,
  }
  const ports = { none: 25, starttls: 587, tls: 465 }

  let enabled = $state(false)
  let cfg = $state<NotifyConfig>({ ...defaults })
  let missedTo = $state('')
  let adminTo = $state('')
  let save = $state(emptySave())
  let testTo = $state(session.me?.email ?? '')
  let testing = $state(false)

  // Load the stored settings whenever the module is (re)loaded.
  $effect(() => {
    const c = { ...defaults, ...(mod.config as Partial<NotifyConfig>), password: '' }
    enabled = mod.enabled
    cfg = c
    missedTo = (c.missed_to ?? []).join('\n')
    adminTo = (c.admin_to ?? []).join('\n')
  })

  function setSecurity(s: NotifyConfig['security']) {
    if (cfg.port === ports[cfg.security]) cfg.port = ports[s]
    cfg.security = s
  }

  const list = (s: string) => s.split(/[\s,;]+/).map((x) => x.trim()).filter(Boolean)
  const body = () => ({ ...cfg, port: Number(cfg.port) || 0, missed_to: list(missedTo), admin_to: list(adminTo) })

  async function submit(e: SubmitEvent) {
    e.preventDefault()
    const ok = await runSave(save, () => api('/modules/notify', { method: 'PUT', json: { enabled, config: body() } }))
    if (ok) {
      toast.show(enabled ? 'E-mail notifications saved' : 'E-mail notifications are off')
      onchange()
    }
  }

  async function test() {
    testing = true
    save.fields = {}
    try {
      const res = await api<{ result: { ok: boolean; error?: string; to?: string } }>('/modules/notify/test', {
        method: 'POST', json: { config: body(), to: testTo },
      })
      if (res.result.ok) toast.show(`Test mail sent to ${res.result.to}`)
      else toast.show(`Sending failed: ${res.result.error}`, 'bad')
    } catch (e) {
      const err = e as { fields?: Record<string, string>; message: string }
      if (err.fields) save.fields = err.fields
      toast.show(err.message, 'bad')
    } finally {
      testing = false
    }
  }
</script>

<section class="card">
  <header>
    <h2>E-mail notifications</h2>
    <ModuleState status={mod.status} />
  </header>
  <form class="body form" onsubmit={submit} aria-label="E-mail settings">
    <label class="switch"><input type="checkbox" bind:checked={enabled} /> Send e-mail notifications</label>

    <h3>Mail server</h3>
    <div class="cols">
      <Field label="SMTP server" error={save.fields['host']}><input bind:value={cfg.host} placeholder="smtp.example.org" /></Field>
      <div class="cols">
        <Field label="Encryption" error={save.fields['security']}>
          <select value={cfg.security} onchange={(e) => setSecurity(e.currentTarget.value as NotifyConfig['security'])}>
            <option value="starttls">STARTTLS</option>
            <option value="tls">TLS</option>
            <option value="none">none</option>
          </select>
        </Field>
        <Field label="Port" error={save.fields['port']}><input type="number" min="1" max="65535" bind:value={cfg.port} /></Field>
      </div>
    </div>
    <div class="cols">
      <Field label="Username" error={save.fields['username']}><input bind:value={cfg.username} autocomplete="off" /></Field>
      <Field label="Password" error={save.fields['password']} help={cfg.password_set ? 'Leave empty to keep it' : ''}>
        <input type="password" bind:value={cfg.password} autocomplete="new-password"
          placeholder={cfg.password_set ? '•••••••• (unchanged)' : ''} />
      </Field>
    </div>
    <Field label="Sender" error={save.fields['from']} help="e.g. Telefon <pbx@example.org>">
      <input bind:value={cfg.from} placeholder="pbx@example.org" />
    </Field>

    <h3>Notifications</h3>
    <Field label="Missed calls to the owners of the phones that rang" inline>
      <input type="checkbox" bind:checked={cfg.missed_calls} />
    </Field>
    <Field label="New voicemail messages to the mailbox owner" inline>
      <input type="checkbox" bind:checked={cfg.voicemail} />
    </Field>
    {#if cfg.voicemail}
      <Field label="Attach the recording (WAV)" inline>
        <input type="checkbox" bind:checked={cfg.voicemail_attach} />
      </Field>
    {/if}
    <Field label="Missed calls and voicemail also to" error={save.fields['missed_to']} help="One address per line">
      <textarea rows="2" bind:value={missedTo}></textarea>
    </Field>
    <Field label="Line outages, failed backups and SIP bans to the admins" inline>
      <input type="checkbox" bind:checked={cfg.trunk_alerts} />
    </Field>
    <Field label="Blocked calls to the admins" inline>
      <input type="checkbox" bind:checked={cfg.blocked_calls} />
    </Field>
    <Field label="Admin addresses besides the admins' own" error={save.fields['admin_to']} help="One address per line">
      <textarea rows="2" bind:value={adminTo}></textarea>
    </Field>

    {#if save.error && Object.keys(save.fields).length === 0}<p class="error-box">{save.error}</p>{/if}
    <div class="actions">
      <div class="test">
        <input type="email" bind:value={testTo} placeholder="test address" aria-label="Test address" />
        <button type="button" onclick={test} disabled={testing}>Send test mail</button>
      </div>
      <button class="primary" type="submit" disabled={save.busy}>Save</button>
    </div>
  </form>
</section>

<style>
  h3 {
    margin: 0.4rem 0 -0.3rem;
    font-size: 0.8rem;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--muted);
  }
  .switch {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    font-weight: 600;
  }
  textarea {
    resize: vertical;
    font: inherit;
  }
  .actions {
    display: flex;
    justify-content: space-between;
    gap: 0.75rem;
    flex-wrap: wrap;
  }
  .test {
    display: flex;
    gap: 0.5rem;
    flex-wrap: wrap;
  }
  .test input {
    width: 16rem;
    max-width: 100%;
  }
</style>
