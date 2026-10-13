<script lang="ts">
  // All blocked/allowed numbers: search, filter by list, add, remove.
  import Plus from '@lucide/svelte/icons/plus'
  import Trash from '@lucide/svelte/icons/trash-2'
  import { api } from '../../lib/api'
  import { ago, dateTime, phone } from '../../lib/format'
  import { router } from '../../lib/router.svelte'
  import { emptySave, runSave } from '../../lib/save'
  import { toast } from '../../lib/toast.svelte'
  import type { ListEntry, ListSource } from '../../lib/types'
  import Modal from '../Modal.svelte'
  import Field from '../Field.svelte'

  interface Props {
    sources: ListSource[]
    onchange: () => void
  }
  let { sources, onchange }: Props = $props()

  const q = $derived(router.query)
  const listSources = $derived(sources.filter((s) => s.target !== 'contacts'))
  const synced = $derived(new Set(sources.filter((s) => s.kind === 'url' || s.kind === 'carddav').map((s) => s.id)))

  let entries = $state<ListEntry[]>([])
  let total = $state(0)
  let loading = $state(false)
  let error = $state('')
  let search = $state(router.query.get('q') ?? '')

  function setParam(name: string, value: string) {
    const p = new URLSearchParams(router.search)
    if (value) p.set(name, value)
    else p.delete(name)
    router.go(`/blocking?${p.toString()}`, true)
  }

  function params(offset: number): string {
    const p = new URLSearchParams({ limit: '100', offset: String(offset) })
    for (const k of ['target', 'source', 'q', 'sort']) {
      const v = q.get(k)
      if (v) p.set(k, v)
    }
    if (!p.get('target') && !p.get('source')) p.set('target', 'block')
    return p.toString()
  }

  async function load(more = false) {
    loading = true
    try {
      const res = await api<{ entries: ListEntry[]; total: number }>(`/lists/entries?${params(more ? entries.length : 0)}`)
      entries = more ? [...entries, ...res.entries] : res.entries
      total = res.total
      error = ''
    } catch (e) {
      error = (e as Error).message
    } finally {
      loading = false
    }
  }
  $effect(() => {
    void router.search
    void load()
  })

  let timer: ReturnType<typeof setTimeout>
  function onSearch() {
    clearTimeout(timer)
    timer = setTimeout(() => setParam('q', search.trim()), 300)
  }

  async function remove(e: ListEntry) {
    if (!confirm(`Remove ${phone(e.pattern)} from ${e.source_name}?`)) return
    try {
      await api(`/lists/entries/${e.id}`, { method: 'DELETE' })
      toast.show(`${phone(e.pattern)} removed`)
      await load()
      onchange()
    } catch (err) {
      toast.show((err as Error).message, 'bad')
    }
  }

  // --- add ---
  let add = $state<{ target: 'block' | 'allow'; number: string; label: string } | null>(null)
  let save = $state(emptySave())
  function openAdd() {
    add = { target: q.get('target') === 'allow' ? 'allow' : 'block', number: '', label: '' }
    save = emptySave()
  }
  async function submit(e: SubmitEvent) {
    e.preventDefault()
    if (!add) return
    const a = add
    const ok = await runSave(save, () => api('/lists/entries', { method: 'POST', json: a }))
    if (ok) {
      toast.show(`${a.number} ${a.target === 'block' ? 'blocked' : 'allowed'}`)
      add = null
      await load()
      onchange()
    }
  }
</script>

<div class="toolbar">
  <div class="filters">
    <label class="sr-only" for="entry-search">Search</label>
    <input id="entry-search" type="search" placeholder="Search number or name" bind:value={search} oninput={onSearch} />
    <label>
      <span class="sr-only">List</span>
      <select value={q.get('source') ? `s${q.get('source')}` : (q.get('target') ?? 'block')}
        onchange={(e) => {
          const v = e.currentTarget.value
          const p = new URLSearchParams(router.search)
          p.delete('source')
          p.delete('target')
          if (v.startsWith('s')) p.set('source', v.slice(1))
          else p.set('target', v)
          router.go(`/blocking?${p.toString()}`, true)
        }}>
        <option value="block">All blocked numbers</option>
        <option value="allow">All allowed numbers</option>
        {#each listSources as s (s.id)}<option value="s{s.id}">{s.name}</option>{/each}
      </select>
    </label>
    <label>
      <span class="sr-only">Sort</span>
      <select value={q.get('sort') ?? ''} onchange={(e) => setParam('sort', e.currentTarget.value)}>
        <option value="">Newest first</option>
        <option value="hits">Most blocked calls</option>
      </select>
    </label>
  </div>
  <button class="primary" onclick={openAdd}><Plus size={16} /> Add number</button>
</div>

{#if error}<div class="error-box">{error}</div>{/if}

<section class="card">
  {#if entries.length === 0}
    <div class="empty">{loading ? 'Loading…' : 'No numbers found.'}</div>
  {:else}
    <div class="table-wrap">
      <table>
        <thead>
          <tr><th>Number</th><th>Name / comment</th><th>List</th><th class="num">Calls</th><th>Last call</th><th>Added</th><th></th></tr>
        </thead>
        <tbody>
          {#each entries as e (e.id)}
            <tr>
              <td class="mono">
                {phone(e.pattern)}
                {#if e.pattern.endsWith('*')}<span class="badge" title="All numbers starting with this">range</span>{/if}
              </td>
              <td>{e.card_name || e.label || '–'}</td>
              <td><span class="badge {e.target === 'allow' ? 'good' : ''}">{e.source_name}</span></td>
              <td class="num">{e.hits || ''}</td>
              <td title={dateTime(e.last_hit_at)}>{e.last_hit_at ? ago(e.last_hit_at) : ''}</td>
              <td title={dateTime(e.created_at)}>{ago(e.created_at)}</td>
              <td class="right">
                {#if !synced.has(e.source_id)}
                  <button onclick={() => remove(e)} aria-label="Remove {e.pattern}"><Trash size={14} /></button>
                {/if}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
    <footer class="more">
      <span class="muted">{entries.length.toLocaleString()} of {total.toLocaleString()}</span>
      {#if entries.length < total}<button onclick={() => load(true)} disabled={loading}>Load more</button>{/if}
    </footer>
  {/if}
</section>

<Modal title="Add number" open={!!add} onclose={() => (add = null)}>
  {#if add}
    <form id="entry-form" class="form" onsubmit={submit}>
      <div class="segmented" role="radiogroup" aria-label="List">
        <button type="button" role="radio" aria-checked={add.target === 'block'}
          onclick={() => (add!.target = 'block')}>Block</button>
        <button type="button" role="radio" aria-checked={add.target === 'allow'}
          onclick={() => (add!.target = 'allow')}>Always allow</button>
      </div>
      <Field label="Number" error={save.fields['number']}
        help="Any format. End with * to match a range, e.g. 0201 123*">
        <input bind:value={add.number} required inputmode="tel" />
      </Field>
      <Field label="Comment" error={save.fields['label']}><input bind:value={add.label} /></Field>
      {#if save.error && Object.keys(save.fields).length === 0}<p class="error-box">{save.error}</p>{/if}
    </form>
  {/if}
  {#snippet footer()}
    <button onclick={() => (add = null)}>Cancel</button>
    <button class="primary" type="submit" form="entry-form" disabled={save.busy}>Add</button>
  {/snippet}
</Modal>

<style>
  .toolbar {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 0.75rem;
    flex-wrap: wrap;
  }
  .filters {
    display: flex;
    gap: 0.5rem;
    flex-wrap: wrap;
  }
  .filters input {
    width: 15rem;
  }
  .num {
    text-align: right;
  }
  .right {
    text-align: right;
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
</style>
