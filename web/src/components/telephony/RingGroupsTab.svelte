<script lang="ts">
  import Plus from '@lucide/svelte/icons/plus'
  import Pencil from '@lucide/svelte/icons/pencil'
  import Trash from '@lucide/svelte/icons/trash-2'
  import { api } from '../../lib/api'
  import { phone } from '../../lib/format'
  import { emptySave, runSave } from '../../lib/save'
  import { toast } from '../../lib/toast.svelte'
  import type { RingGroup, Telephony } from '../../lib/types'
  import Modal from '../Modal.svelte'
  import Field from '../Field.svelte'

  interface Props {
    data: Telephony
    onchange: () => void
  }
  let { data, onchange }: Props = $props()

  const extById = $derived(new Map(data.extensions.map((x) => [x.id, x])))
  const numbersOf = $derived(
    new Map(
      data.ring_groups.map((g) => [
        g.id,
        data.trunks.flatMap((t) => t.numbers).filter((n) => n.ring_group_id === g.id),
      ]),
    ),
  )
  const fallbackLabel: Record<string, string> = {
    hangup: 'Hang up',
    keep_ringing: 'Caller keeps hearing ringing',
    voicemail: 'Voicemail',
  }

  let group = $state<RingGroup | null>(null)
  let save = $state(emptySave())

  function edit(g?: RingGroup) {
    group = g ? { ...g, members: [...g.members] } : { id: 0, name: '', ring_timeout: 25, fallback: 'hangup', fallback_extension_id: 0, members: [] }
    save = emptySave()
  }
  function toggle(id: number) {
    if (!group) return
    group.members = group.members.includes(id) ? group.members.filter((m) => m !== id) : [...group.members, id]
  }

  async function submit(e: SubmitEvent) {
    e.preventDefault()
    if (!group) return
    const ok = await runSave(save, () =>
      group!.id ? api(`/ringgroups/${group!.id}`, { method: 'PUT', json: group }) : api('/ringgroups', { method: 'POST', json: group }),
    )
    if (ok) {
      toast.show(`Ring group ${group.name} saved`)
      group = null
      onchange()
    }
  }

  async function remove(g: RingGroup) {
    if (!confirm(`Delete ring group "${g.name}"?`)) return
    try {
      await api(`/ringgroups/${g.id}`, { method: 'DELETE' })
      onchange()
    } catch (e) {
      toast.show(`${(e as Error).message} – numbers still ring this group.`, 'bad')
    }
  }
</script>

<div class="toolbar">
  <p class="muted">A ring group is a set of phones that ring at the same time; the first to answer takes the call.</p>
  <button class="primary" onclick={() => edit()}><Plus size={16} /> Add ring group</button>
</div>

<div class="groups">
  {#each data.ring_groups as g (g.id)}
    <section class="card">
      <header>
        <h2>{g.name}</h2>
        <div class="actions">
          <button onclick={() => edit(g)} aria-label="Edit {g.name}"><Pencil size={15} /></button>
          <button onclick={() => remove(g)} aria-label="Delete {g.name}"><Trash size={15} /></button>
        </div>
      </header>
      <div class="body info">
        <span class="muted">Rings</span>
        <span>
          {#each g.members as m (m)}
            {@const x = extById.get(m)}
            {#if x}<span class="badge {x.dnd ? '' : 'info'}" title={x.dnd ? 'Do not disturb' : ''}>{x.number} {x.name}</span>{/if}
          {:else}<span class="badge warn">nobody</span>{/each}
        </span>
        <span class="muted">For</span>
        <span>{g.ring_timeout} seconds, then: {fallbackLabel[g.fallback]}</span>
        <span class="muted">Numbers</span>
        <span>
          {#each numbersOf.get(g.id) ?? [] as n (n.id)}<span class="mono num-chip">{phone(n.number)}</span>{:else}<span class="muted">none</span>{/each}
        </span>
      </div>
    </section>
  {:else}
    <section class="card"><div class="empty">No ring groups yet. Create one, then route your numbers to it.</div></section>
  {/each}
</div>

<Modal title={group?.id ? `Edit ${group.name}` : 'Add ring group'} open={!!group} onclose={() => (group = null)}>
  {#if group}
    <form id="group-form" class="form" onsubmit={submit}>
      <Field label="Name" error={save.fields['name']}><input bind:value={group.name} required /></Field>
      <Field label="Extensions that ring" error={save.fields['members']} group>
        <div class="members">
          {#each data.extensions as x (x.id)}
            <label class="member">
              <input type="checkbox" checked={group.members.includes(x.id)} onchange={() => toggle(x.id)} />
              <b>{x.number}</b> {x.name}
            </label>
          {:else}
            <span class="muted">Create extensions first.</span>
          {/each}
        </div>
      </Field>
      <div class="cols">
        <Field label="Ring for (seconds)" error={save.fields['ring_timeout']}>
          <input type="number" min="5" max="300" bind:value={group.ring_timeout} />
        </Field>
        <Field label="If nobody answers" error={save.fields['fallback']}>
          <select bind:value={group.fallback}>
            <option value="hangup">Hang up</option>
            <option value="keep_ringing">Let the caller keep ringing</option>
            <option value="voicemail">Voicemail</option>
          </select>
        </Field>
      </div>
      {#if group.fallback === 'voicemail'}
        <Field label="Voicemail of" error={save.fields['fallback_extension_id']}>
          <select bind:value={group.fallback_extension_id}>
            <option value={0}>Choose…</option>
            {#each data.extensions as x (x.id)}<option value={x.id}>{x.number} {x.name}</option>{/each}
          </select>
        </Field>
      {/if}
      {#if save.error && Object.keys(save.fields).length === 0}<p class="error-box">{save.error}</p>{/if}
    </form>
  {/if}
  {#snippet footer()}
    <button onclick={() => (group = null)}>Cancel</button>
    <button class="primary" type="submit" form="group-form" disabled={save.busy}>Save</button>
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
  .groups {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(340px, 1fr));
    gap: 1rem;
  }
  .actions {
    display: flex;
    gap: 0.35rem;
  }
  .info {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: 0.5rem 1rem;
    align-items: start;
  }
  .info > span:nth-child(even) {
    display: flex;
    flex-wrap: wrap;
    gap: 0.3rem;
  }
  .num-chip {
    font-size: 0.85rem;
  }
  .members {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(160px, 1fr));
    gap: 0.35rem;
  }
  .member {
    display: flex;
    flex-direction: row;
    align-items: center;
    gap: 0.45rem;
    color: var(--text);
    padding: 0.35rem 0.5rem;
    border: 1px solid var(--border);
    border-radius: 7px;
  }
</style>
