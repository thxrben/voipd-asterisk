<script lang="ts">
  import Plus from '@lucide/svelte/icons/plus'
  import Pencil from '@lucide/svelte/icons/pencil'
  import Trash from '@lucide/svelte/icons/trash-2'
  import { api } from '../../lib/api'
  import { phone } from '../../lib/format'
  import { emptySave, runSave } from '../../lib/save'
  import { toast } from '../../lib/toast.svelte'
  import type { Status, Telephony, Trunk, TrunkNumber } from '../../lib/types'
  import Modal from '../Modal.svelte'
  import Field from '../Field.svelte'
  import CodecPicker from './CodecPicker.svelte'

  interface Props {
    data: Telephony
    status: Status | null
    onchange: () => void
  }
  let { data, status, onchange }: Props = $props()

  // Registration state per number, from the trunk supervisor.
  const regState = $derived(new Map((status?.trunks ?? []).map((t) => [t.number, t])))
  const groupName = $derived(new Map(data.ring_groups.map((g) => [g.id, g.name])))

  // --- trunk form ---
  const blankTrunk = (): Trunk => ({
    id: 0, name: '', enabled: true, profile: 'generic', registrar: '', outbound_proxy: '', transport: 'udp',
    address_family: 'auto', auth_username: '', password: '', password_set: false, srtp: false,
    dial_format: 'e164', codecs: ['g722', 'alaw', 'ulaw'], reg_expiration: 480, numbers: [],
  })
  let trunk = $state<Trunk | null>(null)
  let trunkSave = $state(emptySave())
  let advanced = $state(false)

  function telekomPreset(t: Trunk) {
    Object.assign(t, {
      profile: 'telekom', registrar: 'tel.t-online.de', transport: 'tls', srtp: true, dial_format: 'e164',
      codecs: ['g722', 'alaw'], outbound_proxy: '',
    })
    if (!t.name) t.name = 'telekom'
  }

  function editTrunk(t?: Trunk) {
    trunk = t ? { ...t, password: '', codecs: [...t.codecs] } : blankTrunk()
    trunkSave = emptySave()
    advanced = false
  }

  async function saveTrunk(e: SubmitEvent) {
    e.preventDefault()
    if (!trunk) return
    const { numbers: _n, password_set: _p, ...body } = trunk
    const ok = await runSave(trunkSave, () =>
      trunk!.id
        ? api(`/trunks/${trunk!.id}`, { method: 'PUT', json: body })
        : api('/trunks', { method: 'POST', json: { ...body, id: 0 } }),
    )
    if (ok) {
      toast.show(`Line ${trunk.name} saved`)
      trunk = null
      onchange()
    }
  }

  async function deleteTrunk(t: Trunk) {
    if (!confirm(`Delete line "${t.name}" and its ${t.numbers.length} number(s)? Calls to them will stop working.`)) return
    try {
      await api(`/trunks/${t.id}`, { method: 'DELETE' })
      toast.show(`Line ${t.name} deleted`)
      onchange()
    } catch (e) {
      toast.show((e as Error).message, 'bad')
    }
  }

  // --- number form ---
  let num = $state<TrunkNumber | null>(null)
  let numSave = $state(emptySave())

  function editNumber(trunkID: number, n?: TrunkNumber) {
    num = n
      ? { ...n }
      : { id: 0, trunk_id: trunkID, number: '', sip_user: '', register: true, label: '', ring_group_id: data.ring_groups[0]?.id ?? 0 }
    numSave = emptySave()
  }

  async function saveNumber(e: SubmitEvent) {
    e.preventDefault()
    if (!num) return
    const body = { number: num.number, sip_user: num.sip_user, register: num.register, label: num.label, ring_group_id: num.ring_group_id }
    const ok = await runSave(numSave, () =>
      num!.id
        ? api(`/numbers/${num!.id}`, { method: 'PUT', json: body })
        : api(`/trunks/${num!.trunk_id}/numbers`, { method: 'POST', json: body }),
    )
    if (ok) {
      toast.show('Number saved')
      num = null
      onchange()
    }
  }

  async function deleteNumber(n: TrunkNumber) {
    if (!confirm(`Delete number ${phone(n.number)}? Calls to it will no longer be accepted.`)) return
    try {
      await api(`/numbers/${n.id}`, { method: 'DELETE' })
      onchange()
    } catch (e) {
      toast.show((e as Error).message, 'bad')
    }
  }
</script>

<div class="toolbar">
  <p class="muted">Lines are your SIP accounts at a provider. Each number registers separately and rings a ring group.</p>
  <button class="primary" onclick={() => editTrunk()}><Plus size={16} /> Add line</button>
</div>

{#if data.trunks.length === 0}
  <section class="card"><div class="empty">No lines yet. Add your Telekom (or other) SIP account to make and receive calls.</div></section>
{/if}

{#each data.trunks as t (t.id)}
  <section class="card">
    <header>
      <div class="title">
        <h2>{t.name}</h2>
        {#if t.profile === 'telekom'}<span class="badge info">Telekom</span>{/if}
        {#if !t.enabled}<span class="badge">disabled</span>{/if}
        <span class="muted mono small">{t.registrar} · {t.transport.toUpperCase()}{t.srtp ? ' + SRTP' : ''}</span>
      </div>
      <div class="actions">
        <button onclick={() => editNumber(t.id)}><Plus size={15} /> Number</button>
        <button onclick={() => editTrunk(t)} aria-label="Edit line {t.name}"><Pencil size={15} /></button>
        <button onclick={() => deleteTrunk(t)} aria-label="Delete line {t.name}"><Trash size={15} /></button>
      </div>
    </header>
    {#if t.numbers.length === 0}
      <div class="empty">No numbers on this line yet.</div>
    {:else}
      <div class="table-wrap">
        <table>
          <thead><tr><th>Number</th><th>Label</th><th>Rings</th><th>Registration</th><th></th></tr></thead>
          <tbody>
            {#each t.numbers as n (n.id)}
              {@const st = regState.get(n.number)}
              <tr>
                <td class="mono">{phone(n.number)}</td>
                <td>{n.label || '–'}</td>
                <td>
                  {#if n.ring_group_id}{groupName.get(n.ring_group_id)}
                  {:else}<span class="badge warn">no route – calls are rejected</span>{/if}
                </td>
                <td>
                  {#if !n.register}<span class="muted">not registered (by design)</span>
                  {:else if st}<span class="dot {st.up ? 'good' : 'bad'}"></span> {st.up ? 'registered' : st.state}
                  {:else}<span class="muted">pending</span>{/if}
                </td>
                <td><div class="row-actions">
                  <button onclick={() => editNumber(t.id, n)} aria-label="Edit {n.number}"><Pencil size={14} /></button>
                  <button onclick={() => deleteNumber(n)} aria-label="Delete {n.number}"><Trash size={14} /></button>
                </div></td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </section>
{/each}

<Modal title={trunk?.id ? `Edit line ${trunk.name}` : 'Add line'} open={!!trunk} onclose={() => (trunk = null)} wide>
  {#if trunk}
    <form id="trunk-form" class="form" onsubmit={saveTrunk}>
      {#if !trunk.id}
        <div class="presets">
          <button type="button" class:selected={trunk.profile === 'telekom'} onclick={() => telekomPreset(trunk!)}>
            Deutsche Telekom
          </button>
          <button type="button" class:selected={trunk.profile === 'generic'} onclick={() => Object.assign(trunk!, { profile: 'generic' })}>
            Other provider
          </button>
        </div>
      {/if}
      <div class="cols">
        <Field label="Name" error={trunkSave.fields['name']} help="Short identifier, e.g. telekom">
          <input bind:value={trunk.name} required />
        </Field>
        <Field label="Registrar" error={trunkSave.fields['registrar']} help="Provider SIP domain, e.g. tel.t-online.de">
          <input bind:value={trunk.registrar} required />
        </Field>
        <Field label="Username" error={trunkSave.fields['auth_username']}
          help={trunk.profile === 'telekom' ? 'Your Telekom login (email address)' : 'SIP authentication username'}>
          <input bind:value={trunk.auth_username} autocomplete="off" />
        </Field>
        <Field label="Password" error={trunkSave.fields['password']}
          help={trunk.password_set ? 'Leave empty to keep the current password' : ''}>
          <input type="password" bind:value={trunk.password} autocomplete="new-password"
            placeholder={trunk.password_set ? '•••••••• (unchanged)' : ''} />
        </Field>
        <Field label="Transport" error={trunkSave.fields['transport']}>
          <select bind:value={trunk.transport}>
            <option value="udp">UDP</option><option value="tcp">TCP</option><option value="tls">TLS (encrypted)</option>
          </select>
        </Field>
        <Field label="Number format sent to the provider" error={trunkSave.fields['dial_format']}>
          <select bind:value={trunk.dial_format}>
            <option value="e164">International (+49…)</option><option value="national">National (0…)</option>
          </select>
        </Field>
      </div>
      <Field label="Encrypt calls (SRTP)" inline error={trunkSave.fields['srtp']} help="Requires TLS">
        <input type="checkbox" bind:checked={trunk.srtp} />
      </Field>
      <Field label="Enabled" inline><input type="checkbox" bind:checked={trunk.enabled} /></Field>
      <Field label="Codecs (in order of preference)" error={trunkSave.fields['codecs']} group>
        <CodecPicker bind:value={trunk.codecs} all={data.codecs} />
      </Field>
      <button type="button" class="link" onclick={() => (advanced = !advanced)}>{advanced ? 'Hide' : 'Show'} advanced settings</button>
      {#if advanced}
        <div class="cols">
          <Field label="Outbound proxy" error={trunkSave.fields['outbound_proxy']} help="Usually empty">
            <input bind:value={trunk.outbound_proxy} />
          </Field>
          <Field label="IP version" error={trunkSave.fields['address_family']}>
            <select bind:value={trunk.address_family}>
              <option value="auto">Automatic</option><option value="v4">IPv4</option><option value="v6">IPv6</option>
            </select>
          </Field>
          <Field label="Registration interval (s)" error={trunkSave.fields['reg_expiration']}
            help="Shorter notices a dead provider sooner">
            <input type="number" min="60" max="3600" bind:value={trunk.reg_expiration} />
          </Field>
          <Field label="Profile" error={trunkSave.fields['profile']}>
            <select bind:value={trunk.profile}>
              <option value="generic">Generic</option><option value="telekom">Deutsche Telekom</option>
            </select>
          </Field>
        </div>
      {/if}
      {#if trunkSave.error && Object.keys(trunkSave.fields).length === 0}<p class="error-box">{trunkSave.error}</p>{/if}
    </form>
  {/if}
  {#snippet footer()}
    <button onclick={() => (trunk = null)}>Cancel</button>
    <button class="primary" type="submit" form="trunk-form" disabled={trunkSave.busy}>Save</button>
  {/snippet}
</Modal>

<Modal title={num?.id ? 'Edit number' : 'Add number'} open={!!num} onclose={() => (num = null)}>
  {#if num}
    <form id="number-form" class="form" onsubmit={saveNumber}>
      <Field label="Phone number" error={numSave.fields['number']} help="Any format, e.g. 0201 1234567">
        <input bind:value={num.number} required inputmode="tel" />
      </Field>
      <Field label="Label" error={numSave.fields['label']} help="e.g. Home, Office">
        <input bind:value={num.label} />
      </Field>
      <Field label="Calls ring" error={numSave.fields['ring_group_id']}>
        <select bind:value={num.ring_group_id}>
          <option value={0}>Nobody (reject calls)</option>
          {#each data.ring_groups as g (g.id)}<option value={g.id}>{g.name}</option>{/each}
        </select>
      </Field>
      <Field label="Register this number at the provider" inline><input type="checkbox" bind:checked={num.register} /></Field>
      <Field label="SIP user (if different from the number)" error={numSave.fields['sip_user']} help="Usually empty">
        <input bind:value={num.sip_user} />
      </Field>
      {#if numSave.error && Object.keys(numSave.fields).length === 0}<p class="error-box">{numSave.error}</p>{/if}
    </form>
  {/if}
  {#snippet footer()}
    <button onclick={() => (num = null)}>Cancel</button>
    <button class="primary" type="submit" form="number-form" disabled={numSave.busy}>Save</button>
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
  .title {
    display: flex;
    align-items: center;
    gap: 0.6rem;
    flex-wrap: wrap;
  }
  .actions,
  .row-actions {
    display: flex;
    gap: 0.35rem;
  }
  .row-actions {
    justify-content: flex-end;
  }
  .row-actions button {
    padding: 0.3rem;
  }
  .small {
    font-size: 0.82rem;
  }
  :global(.form) {
    display: grid;
    gap: 0.9rem;
  }
  :global(.form .cols) {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 0.9rem;
  }
  @media (max-width: 600px) {
    :global(.form .cols) {
      grid-template-columns: 1fr;
    }
  }
  .presets {
    display: flex;
    gap: 0.5rem;
  }
  .presets button {
    flex: 1;
    justify-content: center;
    padding: 0.7rem;
  }
  .presets button.selected {
    border-color: var(--accent);
    background: color-mix(in srgb, var(--accent) 10%, transparent);
    color: var(--accent);
    font-weight: 600;
  }
  .link {
    border: 0;
    background: none;
    color: var(--accent);
    padding: 0;
    justify-self: start;
  }
  .link:hover:not(:disabled) {
    background: none;
    text-decoration: underline;
  }
</style>
