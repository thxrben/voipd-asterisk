<script lang="ts">
  // List sources: the built-in lists, imports, URL lists and CardDAV
  // address books (e.g. PhoneBlock), with sync status.
  import Plus from '@lucide/svelte/icons/plus'
  import Pencil from '@lucide/svelte/icons/pencil'
  import Trash from '@lucide/svelte/icons/trash-2'
  import RefreshCw from '@lucide/svelte/icons/refresh-cw'
  import { api } from '../../lib/api'
  import { ago, dateTime } from '../../lib/format'
  import { live } from '../../lib/live.svelte'
  import { link } from '../../lib/router.svelte'
  import { emptySave, runSave } from '../../lib/save'
  import { toast } from '../../lib/toast.svelte'
  import { syncedKinds, type ListSource, type ListTarget } from '../../lib/types'
  import Modal from '../Modal.svelte'
  import Field from '../Field.svelte'

  interface Props {
    sources: ListSource[]
    /** Targets that can be chosen for new sources (first = default). */
    targets?: ListTarget[]
    onchange: () => void
  }
  let { sources, targets = ['block', 'allow'], onchange }: Props = $props()

  const targetLabel: Record<ListTarget, string> = { block: 'Blocklists', allow: 'Allowlists', contacts: 'Address books' }
  const kindLabel: Record<ListSource['kind'], string> = {
    manual: 'managed here', file: 'imported file', url: 'download', carddav: 'CardDAV', phoneblock: 'PhoneBlock API',
  }
  const PHONEBLOCK_API = 'https://phoneblock.net/phoneblock/api'
  const voteChoices = [
    [0, 'as in my PhoneBlock account'], [2, '2 votes'], [4, '4 votes'], [10, '10 votes'], [20, '20 votes'],
    [50, '50 votes'], [100, '100 votes'],
  ] as const
  const groups = $derived(targets.map((t) => ({ target: t, items: sources.filter((s) => s.target === t) })))

  // Sources with a running sync (cleared by the list.sync_* event).
  let syncing = $state<number[]>([])
  $effect(() =>
    live.on('list.sync_', (ev) => {
      const id = Number(ev.data?.['source_id'])
      syncing = syncing.filter((s) => s !== id)
      const name = String(ev.data?.['source'] ?? '')
      if (ev.type === 'list.sync_ok') toast.show(`${name} synced: ${Number(ev.data?.['entries'] ?? 0).toLocaleString()} numbers`)
      else toast.show(`${name}: sync failed – ${String(ev.data?.['error'] ?? '')}`, 'bad')
    }),
  )

  async function sync(s: ListSource) {
    try {
      await api(`/lists/sources/${s.id}/sync`, { method: 'POST' })
      syncing = [...syncing, s.id]
    } catch (e) {
      toast.show((e as Error).message, 'bad')
    }
  }

  async function toggle(s: ListSource) {
    try {
      const { password: _p, ...body } = s
      await api(`/lists/sources/${s.id}`, { method: 'PUT', json: { ...body, enabled: !s.enabled } })
      onchange()
    } catch (e) {
      toast.show((e as Error).message, 'bad')
    }
  }

  async function remove(s: ListSource) {
    if (!confirm(`Delete "${s.name}" and its ${s.entries.toLocaleString()} numbers?`)) return
    try {
      await api(`/lists/sources/${s.id}`, { method: 'DELETE' })
      toast.show(`${s.name} deleted`)
      onchange()
    } catch (e) {
      toast.show((e as Error).message, 'bad')
    }
  }

  // --- add / edit ---
  let src = $state<ListSource | null>(null)
  let save = $state(emptySave())

  function edit(s?: ListSource) {
    src = s
      ? { ...s, password: '' }
      : {
          id: 0, name: '', kind: 'carddav', target: targets[0] ?? 'block', url: '', username: '', password: '',
          password_set: false, interval_min: 360, enabled: true, last_sync_at: null, last_status: '', last_error: '',
          failures: 0, cursor: '', min_votes: 0, entries: 0, cards: 0,
        }
    advanced = !!s && s.kind === 'phoneblock' && s.url !== PHONEBLOCK_API
    save = emptySave()
  }

  let advanced = $state(false)

  function phoneBlock() {
    if (!src) return
    Object.assign(src, {
      name: src.name || 'PhoneBlock', kind: 'phoneblock', target: 'block', interval_min: 1440, url: '', username: '',
    })
  }

  function setKind(kind: 'carddav' | 'url') {
    if (!src) return
    if (src.kind === 'phoneblock') Object.assign(src, { url: '', interval_min: 360 })
    src.kind = kind
  }

  // A CardDAV source pointing at PhoneBlock may only see part of the list.
  const phoneBlockDAV = (s: ListSource) => s.kind === 'carddav' && s.url.includes('phoneblock.net')

  async function submit(e: SubmitEvent) {
    e.preventDefault()
    if (!src) return
    const s = src
    const ok = await runSave(save, () =>
      s.id ? api(`/lists/sources/${s.id}`, { method: 'PUT', json: s }) : api('/lists/sources', { method: 'POST', json: s }),
    )
    if (ok) {
      toast.show(s.id ? `${s.name} saved` : `${s.name} added – first sync started`)
      src = null
      onchange()
    }
  }

  function intervalLabel(min: number): string {
    return min % 1440 === 0 ? `${min / 1440} d` : min % 60 === 0 ? `${min / 60} h` : `${min} min`
  }
</script>

<div class="toolbar">
  <p class="muted">Lists are checked in this order: allowlists and contacts first, then blocklists.</p>
  <button class="primary" onclick={() => edit()}><Plus size={16} /> Add list</button>
</div>

{#each groups as g (g.target)}
  <h2 class="group">{targetLabel[g.target]}</h2>
  <div class="sources">
    {#each g.items as s (s.id)}
      {@const busy = syncing.includes(s.id)}
      <section class="card" class:off={!s.enabled}>
        <header>
          <h2>{s.name}</h2>
          <span class="badge">{kindLabel[s.kind]}</span>
          <div class="actions">
            {#if syncedKinds.includes(s.kind)}
              <button onclick={() => sync(s)} disabled={busy} aria-label="Sync {s.name} now" title="Sync now">
                <RefreshCw size={15} class={busy ? 'spin' : ''} />
              </button>
            {/if}
            {#if s.kind !== 'manual'}
              <button onclick={() => edit(s)} aria-label="Edit {s.name}"><Pencil size={15} /></button>
              <button onclick={() => remove(s)} aria-label="Delete {s.name}"><Trash size={15} /></button>
            {/if}
          </div>
        </header>
        <div class="body info">
          <span class="count">
            <a href="/blocking?tab=numbers&source={s.id}" onclick={link}><b>{s.entries.toLocaleString()}</b> {s.entries === 1 ? 'number' : 'numbers'}</a>
            {#if s.cards && s.kind !== 'phoneblock'}<span class="muted">from {s.cards.toLocaleString()} entries</span>{/if}
          </span>
          {#if syncedKinds.includes(s.kind)}
            <span class="sync">
              {#if busy}
                <span class="dot warn"></span> syncing…
              {:else if s.last_status === 'ok'}
                <span class="dot good"></span> synced <span title={dateTime(s.last_sync_at)}>{ago(s.last_sync_at)}</span>
              {:else if s.last_status === 'failed'}
                <span class="dot bad"></span> sync failed <span title={dateTime(s.last_sync_at)}>{ago(s.last_sync_at)}</span>
              {:else}
                <span class="dot"></span> not synced yet
              {/if}
              <span class="muted">· every {intervalLabel(s.interval_min)}</span>
            </span>
            {#if s.last_status === 'failed' && !busy}<p class="error-box small">{s.last_error}</p>{/if}
            {#if s.kind === 'phoneblock'}
              <span class="muted small">
                {s.cards.toLocaleString()} numbers in the PhoneBlock list ·
                {s.min_votes ? `blocked from ${s.min_votes} votes` : 'threshold from your PhoneBlock account'}
                {#if s.cursor} · version {s.cursor}{/if}
              </span>
            {:else}
              <span class="url mono muted" title={s.url}>{s.url}</span>
            {/if}
            {#if phoneBlockDAV(s)}
              <p class="hint small">
                PhoneBlock's address book may only contain part of the blocklist. Add a list with the <b>PhoneBlock</b>
                preset to sync the complete list through the PhoneBlock API, then delete this one.
              </p>
            {/if}
          {:else if s.kind === 'file'}
            <span class="url muted" title={s.url}>from {s.url}</span>
          {/if}
          {#if s.kind !== 'manual'}
            <label class="enabled">
              <input type="checkbox" checked={s.enabled} onchange={() => toggle(s)} /> Active
            </label>
          {/if}
        </div>
      </section>
    {/each}
  </div>
{/each}

<Modal title={src?.id ? `Edit ${src.name}` : 'Add list'} open={!!src} onclose={() => (src = null)}>
  {#if src}
    <form id="source-form" class="form" onsubmit={submit}>
      {#if !src.id}
        <div class="presets">
          <button type="button" class:selected={src.kind === 'phoneblock'} onclick={phoneBlock}>PhoneBlock</button>
          <button type="button" class:selected={src.kind === 'carddav'} onclick={() => setKind('carddav')}>
            CardDAV address book
          </button>
          <button type="button" class:selected={src.kind === 'url'} onclick={() => setKind('url')}>
            Download (CSV, text, vCard)
          </button>
        </div>
        {#if src.kind === 'phoneblock'}
          <p class="hint">
            The complete PhoneBlock blocklist is downloaded once; after that only the changes, once a day. Create an
            <b>API key</b> in your PhoneBlock account settings. Your own PhoneBlock block- and allowlist are applied too.
          </p>
        {/if}
      {/if}
      <div class="cols">
        <Field label="Name" error={save.fields['name']}><input bind:value={src.name} required /></Field>
        {#if !src.id && targets.length > 1}
          <Field label="Use the numbers to" error={save.fields['target']}>
            <select bind:value={src.target}>
              {#each targets as t (t)}<option value={t}>{t === 'block' ? 'Block calls' : t === 'allow' ? 'Always allow calls' : 'Show caller names'}</option>{/each}
            </select>
          </Field>
        {/if}
      </div>
      {#if src.kind === 'phoneblock'}
        <Field label="API key" error={save.fields['password']} help={src.password_set ? 'Leave empty to keep it' : ''}>
          <input type="password" bind:value={src.password} autocomplete="new-password"
            placeholder={src.password_set ? '•••••••• (unchanged)' : ''} required={!src.password_set} />
        </Field>
        <div class="cols">
          <Field label="Block numbers from" error={save.fields['min_votes']}
            help="More votes: fewer numbers, fewer false positives">
            <select bind:value={src.min_votes}>
              {#each voteChoices as [v, l] (v)}<option value={v}>{l}</option>{/each}
            </select>
          </Field>
          <Field label="Sync every" error={save.fields['interval_min']}>
            <select bind:value={src.interval_min}>
              {#each [[1440, 'day'], [2880, '2 days'], [10080, 'week']] as const as [v, l] (v)}
                <option value={v}>{l}</option>
              {/each}
            </select>
          </Field>
        </div>
        <button type="button" class="link" onclick={() => (advanced = !advanced)} aria-expanded={advanced}>
          {advanced ? 'Hide' : 'Show'} advanced settings
        </button>
        {#if advanced}
          <Field label="API URL" error={save.fields['url']} help="Leave empty for {PHONEBLOCK_API}">
            <input bind:value={src.url} type="url" placeholder={PHONEBLOCK_API} />
          </Field>
        {/if}
      {:else if src.kind !== 'file'}
        <Field label="URL" error={save.fields['url']}
          help={src.kind === 'carddav' ? 'Address book or address book home URL' : 'Downloaded on every sync; replaces the list'}>
          <input bind:value={src.url} type="url" required placeholder="https://" />
        </Field>
        <div class="cols">
          <Field label="Username" error={save.fields['username']}><input bind:value={src.username} autocomplete="off" /></Field>
          <Field label={src.url.includes('phoneblock.net') ? 'CardDAV token' : 'Password'} error={save.fields['password']}
            help={src.password_set ? 'Leave empty to keep it' : ''}>
            <input type="password" bind:value={src.password} autocomplete="new-password"
              placeholder={src.password_set ? '•••••••• (unchanged)' : ''} />
          </Field>
        </div>
        <Field label="Sync every" error={save.fields['interval_min']}>
          <select bind:value={src.interval_min}>
            {#each [[60, '1 hour'], [180, '3 hours'], [360, '6 hours'], [720, '12 hours'], [1440, 'day'], [10080, 'week']] as const as [v, l] (v)}
              <option value={v}>{l}</option>
            {/each}
          </select>
        </Field>
      {/if}
      {#if save.error && Object.keys(save.fields).length === 0}<p class="error-box">{save.error}</p>{/if}
    </form>
  {/if}
  {#snippet footer()}
    <button onclick={() => (src = null)}>Cancel</button>
    <button class="primary" type="submit" form="source-form" disabled={save.busy}>{src?.id ? 'Save' : 'Add and sync'}</button>
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
  h2.group {
    font-size: 0.85rem;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--muted);
    margin: 0.4rem 0 -0.4rem;
  }
  .sources {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
    gap: 1rem;
  }
  .card.off {
    opacity: 0.65;
  }
  header {
    gap: 0.5rem;
  }
  .actions {
    margin-left: auto;
    display: flex;
    gap: 0.35rem;
  }
  .actions button {
    padding: 0.3rem 0.45rem;
  }
  .info {
    display: grid;
    gap: 0.4rem;
  }
  .count {
    display: flex;
    gap: 0.5rem;
    align-items: baseline;
  }
  .count b {
    font-size: 1.3rem;
  }
  .sync {
    display: flex;
    align-items: center;
    gap: 0.35rem;
    flex-wrap: wrap;
  }
  .url {
    font-size: 0.8rem;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .small {
    font-size: 0.82rem;
    margin: 0;
  }
  .enabled {
    display: flex;
    flex-direction: row;
    align-items: center;
    gap: 0.4rem;
    color: var(--text);
  }
  .presets {
    display: grid;
    grid-template-columns: repeat(3, 1fr);
    gap: 0.5rem;
  }
  @media (max-width: 600px) {
    .presets {
      grid-template-columns: 1fr;
    }
  }
  .presets button.selected {
    border-color: var(--accent);
    background: color-mix(in srgb, var(--accent) 10%, transparent);
    color: var(--accent);
    font-weight: 600;
  }
  .hint {
    margin: 0;
    font-size: 0.88rem;
    color: var(--muted);
  }
  .hint.small {
    font-size: 0.8rem;
  }
  .link {
    justify-self: start;
    background: none;
    border: none;
    padding: 0;
    color: var(--accent);
    text-decoration: underline;
  }
  :global(.spin) {
    animation: spin 1s linear infinite;
  }
  @keyframes spin {
    to {
      transform: rotate(360deg);
    }
  }
</style>
