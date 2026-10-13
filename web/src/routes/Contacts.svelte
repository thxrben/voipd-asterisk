<script lang="ts">
  // Contacts: shown as caller names (and never blocked, if enabled).
  // Managed here, imported from vCard files, or synced via CardDAV.
  import Plus from '@lucide/svelte/icons/plus'
  import Pencil from '@lucide/svelte/icons/pencil'
  import Trash from '@lucide/svelte/icons/trash-2'
  import X from '@lucide/svelte/icons/x'
  import { api } from '../lib/api'
  import { phone } from '../lib/format'
  import { live } from '../lib/live.svelte'
  import { router } from '../lib/router.svelte'
  import { emptySave, runSave } from '../lib/save'
  import { toast } from '../lib/toast.svelte'
  import type { Contact, Screening } from '../lib/types'
  import Modal from '../components/Modal.svelte'
  import Field from '../components/Field.svelte'
  import SourcesTab from '../components/blocking/SourcesTab.svelte'
  import ImportPanel from '../components/blocking/ImportPanel.svelte'

  const tabs = [
    { id: 'contacts', label: 'Contacts' },
    { id: 'sources', label: 'Address books' },
    { id: 'import', label: 'Import' },
  ] as const
  const tab = $derived(router.query.get('tab') ?? 'contacts')

  let contacts = $state<Contact[]>([])
  let total = $state(0)
  let search = $state('')
  let loading = $state(false)
  let error = $state('')
  let screening = $state<Screening | null>(null)

  async function load(more = false) {
    loading = true
    try {
      const p = new URLSearchParams({ limit: '100', offset: String(more ? contacts.length : 0) })
      if (search.trim()) p.set('q', search.trim())
      const res = await api<{ contacts: Contact[]; total: number }>(`/contacts?${p}`)
      contacts = more ? [...contacts, ...res.contacts] : res.contacts
      total = res.total
      error = ''
    } catch (e) {
      error = (e as Error).message
    } finally {
      loading = false
    }
  }
  async function loadSources() {
    try {
      screening = await api<Screening>('/screening')
    } catch (e) {
      error = (e as Error).message
    }
  }
  function reload() {
    void load()
    void loadSources()
  }
  $effect(() => {
    reload()
    return live.on('list.', reload)
  })

  let timer: ReturnType<typeof setTimeout>
  function onSearch() {
    clearTimeout(timer)
    timer = setTimeout(() => void load(), 250)
  }

  // --- edit ---
  let c = $state<Contact | null>(null)
  let save = $state(emptySave())
  function edit(x?: Contact) {
    c = x
      ? { ...x, numbers: x.numbers.map((n) => ({ ...n, number: phone(n.number) })) }
      : { id: 0, source_id: 3, name: '', org: '', numbers: [{ number: '', label: 'mobile' }] }
    save = emptySave()
  }
  async function submit(e: SubmitEvent) {
    e.preventDefault()
    if (!c) return
    const body = { id: c.id, name: c.name, org: c.org, numbers: c.numbers.filter((n) => n.number.trim()) }
    const ok = await runSave(save, () =>
      body.id ? api(`/contacts/${body.id}`, { method: 'PUT', json: body }) : api('/contacts', { method: 'POST', json: body }),
    )
    if (ok) {
      toast.show(`${body.name} saved`)
      c = null
      void load()
    }
  }
  async function remove(x: Contact) {
    if (!confirm(`Delete ${x.name}?`)) return
    try {
      await api(`/contacts/${x.id}`, { method: 'DELETE' })
      void load()
    } catch (e) {
      toast.show((e as Error).message, 'bad')
    }
  }
  const manual = (x: Contact) => x.source_id === 3
</script>

<div class="page">
  <div class="head">
    <h1>Contacts</h1>
    <div class="segmented" role="tablist" aria-label="Section">
      {#each tabs as t (t.id)}
        <button role="tab" aria-selected={tab === t.id} onclick={() => router.go(`/contacts?tab=${t.id}`, true)}>{t.label}</button>
      {/each}
    </div>
  </div>
  {#if error}<div class="error-box">{error}</div>{/if}

  {#if tab === 'sources'}
    {#if screening}
      <SourcesTab sources={screening.sources.filter((s) => s.target === 'contacts')} targets={['contacts']} onchange={reload} />
    {/if}
  {:else if tab === 'import'}
    <ImportPanel targets={['contacts']} onchange={reload} />
  {:else}
    <div class="toolbar">
      <input type="search" placeholder="Search name or number" bind:value={search} oninput={onSearch} aria-label="Search contacts" />
      <button class="primary" onclick={() => edit()}><Plus size={16} /> Add contact</button>
    </div>
    <section class="card">
      {#if contacts.length === 0}
        <div class="empty">{loading ? 'Loading…' : search ? 'No contacts found.' : 'No contacts yet. Their names are shown on the phones when they call.'}</div>
      {:else}
        <div class="table-wrap">
          <table>
            <thead><tr><th>Name</th><th>Numbers</th><th>Address book</th><th></th></tr></thead>
            <tbody>
              {#each contacts as x (x.id)}
                <tr>
                  <td><b>{x.name}</b>{#if x.org}<br /><span class="muted">{x.org}</span>{/if}</td>
                  <td>
                    {#each x.numbers as n (n.number)}
                      <div><span class="mono">{phone(n.number)}</span> {#if n.label}<span class="muted">{n.label}</span>{/if}</div>
                    {/each}
                  </td>
                  <td><span class="badge">{x.source_name}</span></td>
                  <td class="right">
                    {#if manual(x)}
                      <button onclick={() => edit(x)} aria-label="Edit {x.name}"><Pencil size={14} /></button>
                      <button onclick={() => remove(x)} aria-label="Delete {x.name}"><Trash size={14} /></button>
                    {/if}
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
        <footer class="more">
          <span class="muted">{contacts.length.toLocaleString()} of {total.toLocaleString()}</span>
          {#if contacts.length < total}<button onclick={() => load(true)} disabled={loading}>Load more</button>{/if}
        </footer>
      {/if}
    </section>
  {/if}
</div>

<Modal title={c?.id ? `Edit ${c.name}` : 'Add contact'} open={!!c} onclose={() => (c = null)}>
  {#if c}
    <form id="contact-form" class="form" onsubmit={submit}>
      <div class="cols">
        <Field label="Name" error={save.fields['name']}><input bind:value={c.name} required /></Field>
        <Field label="Company" error={save.fields['org']}><input bind:value={c.org} /></Field>
      </div>
      <Field label="Numbers" error={save.fields['numbers']} group>
        <div class="numbers">
          {#each c.numbers as n, i (i)}
            <div class="num-row">
              <input bind:value={n.number} placeholder="Number" inputmode="tel" aria-label="Number {i + 1}"
                class:invalid={!!save.fields[`numbers.${i}`]} />
              <select bind:value={n.label} aria-label="Type of number {i + 1}">
                <option value="mobile">mobile</option><option value="home">home</option>
                <option value="work">work</option><option value="">other</option>
              </select>
              <button type="button" onclick={() => c!.numbers.splice(i, 1)} aria-label="Remove number {i + 1}"><X size={14} /></button>
              {#if save.fields[`numbers.${i}`]}<span class="error">{save.fields[`numbers.${i}`]}</span>{/if}
            </div>
          {/each}
          <button type="button" class="link" onclick={() => c!.numbers.push({ number: '', label: 'mobile' })}>+ Add number</button>
        </div>
      </Field>
      {#if save.error && Object.keys(save.fields).length === 0}<p class="error-box">{save.error}</p>{/if}
    </form>
  {/if}
  {#snippet footer()}
    <button onclick={() => (c = null)}>Cancel</button>
    <button class="primary" type="submit" form="contact-form" disabled={save.busy}>Save</button>
  {/snippet}
</Modal>

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
  .toolbar {
    display: flex;
    justify-content: space-between;
    gap: 0.75rem;
    flex-wrap: wrap;
  }
  .toolbar input {
    width: 18rem;
    max-width: 100%;
  }
  .right {
    text-align: right;
    white-space: nowrap;
  }
  td button {
    padding: 0.25rem 0.4rem;
  }
  .more {
    display: flex;
    justify-content: space-between;
    align-items: center;
    padding: 0.6rem 1rem;
    border-top: 1px solid var(--border);
  }
  .numbers {
    display: grid;
    gap: 0.4rem;
  }
  .num-row {
    display: grid;
    grid-template-columns: 1fr 7rem auto;
    gap: 0.4rem;
    align-items: center;
  }
  .num-row .error {
    grid-column: 1 / -1;
    color: var(--bad);
    font-size: 0.82rem;
  }
  input.invalid {
    border-color: var(--bad);
  }
  .num-row button {
    padding: 0.3rem;
  }
  button.link {
    justify-self: start;
  }
</style>
