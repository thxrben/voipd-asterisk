<script lang="ts">
  import Plus from '@lucide/svelte/icons/plus'
  import Pencil from '@lucide/svelte/icons/pencil'
  import Trash from '@lucide/svelte/icons/trash-2'
  import KeyRound from '@lucide/svelte/icons/key-round'
  import Copy from '@lucide/svelte/icons/copy'
  import { api } from '../../lib/api'
  import { phone } from '../../lib/format'
  import { emptySave, runSave } from '../../lib/save'
  import { toast } from '../../lib/toast.svelte'
  import type { Credentials, Extension, Telephony } from '../../lib/types'
  import Modal from '../Modal.svelte'
  import Field from '../Field.svelte'
  import CodecPicker from './CodecPicker.svelte'

  interface Props {
    data: Telephony
    onchange: () => void
  }
  let { data, onchange }: Props = $props()

  const numbers = $derived(data.trunks.flatMap((t) => t.numbers.map((n) => ({ ...n, trunk: t.name }))))
  const userName = $derived(new Map(data.users.map((u) => [u.subject, u.name || u.username])))
  const groupsOf = $derived(
    new Map(data.extensions.map((x) => [x.id, data.ring_groups.filter((g) => g.members.includes(x.id)).map((g) => g.name)])),
  )

  let ext = $state<Extension | null>(null)
  let save = $state(emptySave())
  let usernameTouched = $state(false)

  function edit(x?: Extension) {
    const next = Math.max(9, ...data.extensions.map((e) => Number(e.number) || 0)) + 1
    ext = x
      ? { ...x, password: '', codecs: [...x.codecs] }
      : {
          id: 0, number: String(next), name: '', enabled: true, sip_username: '', password: '', transport: 'udp',
          srtp: false, codecs: [...data.codecs], max_contacts: 3, outbound_number_id: 0, clir: false, dnd: false,
          owner_subject: '', online: 0,
        }
    usernameTouched = !!x
    save = emptySave()
  }

  // Suggest a SIP username from the name for new extensions.
  function onName() {
    if (ext && !usernameTouched && !ext.id) {
      ext.sip_username = ext.name
        .toLowerCase()
        .normalize('NFKD')
        .replace(/[̀-ͯ]/g, '')
        .replace(/ß/g, 'ss')
        .replace(/[^a-z0-9]+/g, '-')
        .replace(/^-+|-+$/g, '')
        .slice(0, 32)
    }
  }

  async function submit(e: SubmitEvent) {
    e.preventDefault()
    if (!ext) return
    const { online: _o, ...body } = ext
    const ok = await runSave(save, () =>
      ext!.id ? api(`/extensions/${ext!.id}`, { method: 'PUT', json: body }) : api('/extensions', { method: 'POST', json: body }),
    )
    if (ok) {
      const created = !ext.id
      toast.show(`Extension ${ext.number} saved`)
      ext = null
      onchange()
      if (created) toast.show('Use "Credentials" to set up the phone.', 'good', 6000)
    }
  }

  async function remove(x: Extension) {
    if (!confirm(`Delete extension ${x.number} (${x.name})? Its phones will be logged out.`)) return
    try {
      await api(`/extensions/${x.id}`, { method: 'DELETE' })
      onchange()
    } catch (e) {
      toast.show((e as Error).message, 'bad')
    }
  }

  async function toggleDND(x: Extension) {
    try {
      await api(`/extensions/${x.id}/dnd`, { method: 'PUT', json: { dnd: !x.dnd } })
      onchange()
    } catch (e) {
      toast.show((e as Error).message, 'bad')
    }
  }

  // --- credentials ---
  let creds = $state<(Credentials & { ext: Extension }) | null>(null)
  async function showCredentials(x: Extension) {
    try {
      creds = { ...(await api<Credentials>(`/extensions/${x.id}/credentials`)), ext: x }
    } catch (e) {
      toast.show((e as Error).message, 'bad')
    }
  }
  async function regenerate() {
    if (!creds || !confirm('Generate a new password? The phone must be updated with it.')) return
    try {
      const res = await api<{ result: { password: string } }>(`/extensions/${creds.ext.id}/password`, { method: 'POST' })
      creds.password = res.result.password
      toast.show('New password generated')
    } catch (e) {
      toast.show((e as Error).message, 'bad')
    }
  }
  function copy(v: string) {
    navigator.clipboard?.writeText(v).then(
      () => toast.show('Copied'),
      () => toast.show('Copy failed – select and copy manually', 'warn'),
    )
  }
  const server = window.location.hostname
</script>

<div class="toolbar">
  <p class="muted">Extensions are your phones. Several devices can log in to the same extension.</p>
  <button class="primary" onclick={() => edit()}><Plus size={16} /> Add extension</button>
</div>

<section class="card">
  {#if data.extensions.length === 0}
    <div class="empty">No extensions yet.</div>
  {:else}
    <div class="table-wrap">
      <table>
        <thead>
          <tr><th>Ext.</th><th>Name</th><th>Username</th><th>Ring groups</th><th>Owner</th><th>Devices</th><th>Do not disturb</th><th></th></tr>
        </thead>
        <tbody>
          {#each data.extensions as x (x.id)}
            <tr class:disabled={!x.enabled}>
              <td><b>{x.number}</b></td>
              <td>{x.name}{#if !x.enabled} <span class="badge">disabled</span>{/if}</td>
              <td class="mono">{x.sip_username}</td>
              <td>{groupsOf.get(x.id)?.join(', ') || '–'}</td>
              <td>{x.owner_subject ? (userName.get(x.owner_subject) ?? x.owner_subject) : '–'}</td>
              <td><span class="dot {x.online > 0 ? 'good' : ''}"></span> {x.online > 0 ? `${x.online} online` : 'offline'}</td>
              <td>
                <label class="switch">
                  <input type="checkbox" checked={x.dnd} onchange={() => toggleDND(x)} aria-label="Do not disturb for {x.number}" />
                  <span>{x.dnd ? 'on' : 'off'}</span>
                </label>
              </td>
              <td><div class="row-actions">
                <button onclick={() => showCredentials(x)}><KeyRound size={14} /> Credentials</button>
                <button onclick={() => edit(x)} aria-label="Edit {x.number}"><Pencil size={14} /></button>
                <button onclick={() => remove(x)} aria-label="Delete {x.number}"><Trash size={14} /></button>
              </div></td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
</section>

<Modal title={ext?.id ? `Edit extension ${ext.number}` : 'Add extension'} open={!!ext} onclose={() => (ext = null)} wide>
  {#if ext}
    <form id="ext-form" class="form" onsubmit={submit}>
      <div class="cols">
        <Field label="Extension number" error={save.fields['number']} help="2–6 digits, dialed internally">
          <input bind:value={ext.number} required inputmode="numeric" />
        </Field>
        <Field label="Name" error={save.fields['name']} help="Shown as caller name, e.g. Kitchen">
          <input bind:value={ext.name} oninput={onName} required />
        </Field>
        <Field label="SIP username" error={save.fields['sip_username']} help="What the phone logs in with">
          <input bind:value={ext.sip_username} oninput={() => (usernameTouched = true)} required autocomplete="off" />
        </Field>
        <Field label="SIP password" error={save.fields['password']}
          help={ext.id ? 'Leave empty to keep it' : 'Leave empty to generate a strong one'}>
          <input type="password" bind:value={ext.password} autocomplete="new-password" />
        </Field>
        <Field label="Outgoing calls via" error={save.fields['outbound_number_id']}>
          <select bind:value={ext.outbound_number_id}>
            <option value={0}>Default (first number)</option>
            {#each numbers as n (n.id)}<option value={n.id}>{phone(n.number)}{n.label ? ` – ${n.label}` : ''}</option>{/each}
          </select>
        </Field>
        <Field label="Owner (web login)" error={save.fields['owner_subject']} help="Sees this extension's calls">
          <select bind:value={ext.owner_subject}>
            <option value="">Nobody</option>
            {#each data.users as u (u.subject)}<option value={u.subject}>{u.name || u.username}</option>{/each}
          </select>
        </Field>
        <Field label="Preferred transport" error={save.fields['transport']}>
          <select bind:value={ext.transport}>
            <option value="udp">UDP</option><option value="tcp">TCP</option><option value="tls">TLS</option>
          </select>
        </Field>
        <Field label="Max. devices" error={save.fields['max_contacts']}>
          <input type="number" min="1" max="10" bind:value={ext.max_contacts} />
        </Field>
      </div>
      <div class="checks">
        <Field label="Enabled" inline><input type="checkbox" bind:checked={ext.enabled} /></Field>
        <Field label="Hide my number on outgoing calls (CLIR)" inline><input type="checkbox" bind:checked={ext.clir} /></Field>
        <Field label="Encrypt calls to the phone (SRTP)" inline><input type="checkbox" bind:checked={ext.srtp} /></Field>
        <Field label="Do not disturb" inline><input type="checkbox" bind:checked={ext.dnd} /></Field>
      </div>
      <Field label="Codecs (in order of preference)" error={save.fields['codecs']} group>
        <CodecPicker bind:value={ext.codecs} all={data.codecs} />
      </Field>
      {#if save.error && Object.keys(save.fields).length === 0}<p class="error-box">{save.error}</p>{/if}
    </form>
  {/if}
  {#snippet footer()}
    <button onclick={() => (ext = null)}>Cancel</button>
    <button class="primary" type="submit" form="ext-form" disabled={save.busy}>Save</button>
  {/snippet}
</Modal>

<Modal title={creds ? `Phone setup for ${creds.ext.number} ${creds.ext.name}` : ''} open={!!creds} onclose={() => (creds = null)}>
  {#if creds}
    <p class="muted intro">Enter these into the phone's SIP account settings.</p>
    <dl>
      {#each [['Server / registrar', server], ['Port', String(creds.port)], ['Transport', creds.transport.toUpperCase()],
        ...(creds.srtp ? [['Media encryption', 'SRTP (SDES), mandatory']] : []),
        ['Username', creds.username], ['Authentication user', creds.auth_username], ['Password', creds.password]] as [k, v] (k)}
        <dt>{k}</dt>
        <dd>
          <code>{v}</code>
          <button onclick={() => copy(v!)} aria-label="Copy {k}"><Copy size={14} /></button>
        </dd>
      {/each}
    </dl>
    {#if creds.transport === 'tls'}
      <p class="muted intro">
        For TLS the phone must trust voipd's certificate authority: import
        <a href={creds.ca_url} download>voipd-ca.pem</a> (or <a href="/ca.crt" download>.crt</a>) as a trusted
        certificate, or switch off server certificate verification.
      </p>
    {/if}
  {/if}
  {#snippet footer()}
    <button onclick={regenerate}>New password</button>
    <button class="primary" onclick={() => (creds = null)}>Done</button>
  {/snippet}
</Modal>

<style>
  .toolbar {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 1rem;
    flex-wrap: wrap;
  }
  .toolbar p {
    margin: 0;
  }
  tr.disabled td {
    opacity: 0.6;
  }
  .row-actions {
    display: flex;
    justify-content: flex-end;
    gap: 0.35rem;
    white-space: nowrap;
  }
  .row-actions button {
    padding: 0.3rem 0.5rem;
  }
  .switch {
    display: inline-flex;
    flex-direction: row;
    align-items: center;
    gap: 0.4rem;
    color: var(--text);
  }
  .checks {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 0.4rem 1rem;
  }
  @media (max-width: 600px) {
    .checks {
      grid-template-columns: 1fr;
    }
  }
  .intro {
    margin: 0 0 0.8rem;
  }
  dl {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: 0.5rem 1rem;
    margin: 0;
    align-items: center;
  }
  dt {
    color: var(--muted);
  }
  dd {
    margin: 0;
    display: flex;
    align-items: center;
    gap: 0.4rem;
    min-width: 0;
  }
  dd code {
    background: var(--panel-2);
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.2rem 0.45rem;
    overflow-wrap: anywhere;
  }
  dd button {
    padding: 0.25rem;
  }
</style>
