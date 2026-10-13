<script lang="ts">
  // Bulk import from a file or URL: check first (preview), then import.
  import Upload from '@lucide/svelte/icons/upload'
  import { ApiError, api } from '../../lib/api'
  import { phone } from '../../lib/format'
  import { toast } from '../../lib/toast.svelte'
  import type { ImportPreview, ListTarget } from '../../lib/types'
  import Field from '../Field.svelte'

  interface Props {
    targets: ListTarget[]
    onchange: () => void
  }
  let { targets, onchange }: Props = $props()

  // svelte-ignore state_referenced_locally: the first target is only the default
  let target = $state<ListTarget>(targets[0] ?? 'block')
  let from = $state<'file' | 'url'>('file')
  let file = $state<File | null>(null)
  let url = $state('')
  let into = $state<'manual' | 'new'>('new')
  let name = $state('')
  let preview = $state<ImportPreview | null>(null)
  let busy = $state(false)
  let error = $state('')
  let fields = $state<Record<string, string>>({})

  const manualName: Record<ListTarget, string> = { block: 'My blocklist', allow: 'My allowlist', contacts: 'My contacts' }

  function reset() {
    preview = null
    error = ''
    fields = {}
  }

  function form(dryRun: boolean): FormData {
    const f = new FormData()
    f.set('target', target)
    f.set('dry_run', dryRun ? '1' : '0')
    if (from === 'file' && file) f.set('file', file)
    else f.set('url', url)
    if (!dryRun && into === 'new' && target !== 'contacts') f.set('name', name.trim())
    return f
  }

  async function run(dryRun: boolean) {
    busy = true
    error = ''
    fields = {}
    try {
      const res = await api<{ result: ImportPreview }>('/lists/import', { method: 'POST', body: form(dryRun) })
      if (dryRun) {
        preview = res.result
      } else {
        const n = res.result.added ?? 0
        toast.show(`${n.toLocaleString()} ${target === 'contacts' ? 'contacts' : 'numbers'} imported`)
        preview = null
        file = null
        url = ''
        name = ''
        onchange()
      }
    } catch (e) {
      error = (e as Error).message
      if (e instanceof ApiError && e.fields) fields = e.fields
    } finally {
      busy = false
    }
  }

  function onFile(e: Event) {
    const f = (e.currentTarget as HTMLInputElement).files?.[0] ?? null
    file = f
    if (f && !name) name = f.name.replace(/\.[^.]+$/, '')
    reset()
  }

  const ready = $derived((from === 'file' ? !!file : url.trim() !== '') && (into === 'manual' || target === 'contacts' || name.trim() !== ''))
</script>

<section class="card">
  <header><h2>Import numbers</h2></header>
  <div class="body form">
    <p class="muted intro">
      Text or CSV files with one number per line (an optional comment after a comma, semicolon or tab), or vCard files
      (.vcf) – every phone number of a contact is imported. Ranges are written with a trailing *, e.g. <span class="mono">0201 123*</span>.
    </p>
    <div class="cols">
      {#if targets.length > 1}
        <Field label="Use the numbers to">
          <select bind:value={target} onchange={reset}>
            {#each targets as t (t)}<option value={t}>{t === 'block' ? 'Block calls' : t === 'allow' ? 'Always allow calls' : 'Show caller names'}</option>{/each}
          </select>
        </Field>
      {/if}
      <Field label="Source">
        <select bind:value={from} onchange={reset}>
          <option value="file">Upload a file</option>
          <option value="url">Download from a URL (once)</option>
        </select>
      </Field>
    </div>
    {#if from === 'file'}
      <Field label="File" error={fields['file']}>
        <input type="file" accept=".csv,.txt,.vcf,.vcard,text/*" onchange={onFile} />
      </Field>
    {:else}
      <Field label="URL" error={fields['url']} help="To keep a list up to date automatically, add it under Lists instead">
        <input type="url" bind:value={url} oninput={reset} placeholder="https://" />
      </Field>
    {/if}
    {#if target !== 'contacts'}
      <div class="cols">
        <Field label="Add the numbers to">
          <select bind:value={into}>
            <option value="new">A new list (can be removed as a whole)</option>
            <option value="manual">{manualName[target]}</option>
          </select>
        </Field>
        {#if into === 'new'}
          <Field label="Name of the new list" error={fields['name']}><input bind:value={name} /></Field>
        {/if}
      </div>
    {/if}

    {#if error && Object.keys(fields).length === 0}<p class="error-box">{error}</p>{/if}

    {#if preview}
      <div class="preview" role="status">
        <div class="stats">
          <div><b>{preview.valid.toLocaleString()}</b><span>valid numbers{preview.format === 'vcard' ? ` in ${preview.cards.toLocaleString()} contacts` : ''}</span></div>
          <div><b>{preview.already_listed.toLocaleString()}</b><span>already listed</span></div>
          <div><b>{preview.duplicates.toLocaleString()}</b><span>duplicates in the file</span></div>
          <div class:bad={preview.invalid_count > 0}><b>{preview.invalid_count.toLocaleString()}</b><span>not usable</span></div>
        </div>
        {#if preview.sample.length}
          <details open>
            <summary>First numbers</summary>
            <ul class="sample">
              {#each preview.sample as s (s.number)}<li><span class="mono">{phone(s.number)}</span> <span class="muted">{s.label}</span></li>{/each}
            </ul>
          </details>
        {/if}
        {#if preview.invalid.length}
          <details>
            <summary>Not usable ({preview.invalid_count.toLocaleString()})</summary>
            <ul class="sample">
              {#each preview.invalid as inv, i (i)}
                <li>{#if inv.line}<span class="muted">line {inv.line}:</span>{/if} <span class="mono">{inv.value}</span> – {inv.reason}</li>
              {/each}
            </ul>
          </details>
        {/if}
      </div>
    {/if}

    <div class="buttons">
      {#if preview}
        <button onclick={reset}>Back</button>
        <button class="primary" disabled={busy || preview.valid === 0 || !ready} onclick={() => run(false)}>
          <Upload size={16} /> Import {preview.valid.toLocaleString()} {target === 'contacts' ? 'numbers' : 'numbers'}
        </button>
      {:else}
        <button class="primary" disabled={busy || !ready} onclick={() => run(true)}>{busy ? 'Checking…' : 'Check file'}</button>
      {/if}
    </div>
  </div>
</section>

<style>
  .intro {
    margin: 0;
  }
  .preview {
    display: grid;
    gap: 0.6rem;
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 0.8rem;
    background: var(--panel-2);
  }
  .stats {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(130px, 1fr));
    gap: 0.6rem;
  }
  .stats div {
    display: grid;
  }
  .stats b {
    font-size: 1.4rem;
  }
  .stats span {
    color: var(--muted);
    font-size: 0.85rem;
  }
  .stats .bad b {
    color: var(--bad);
  }
  .sample {
    margin: 0.4rem 0 0;
    padding-left: 1.2rem;
    font-size: 0.88rem;
    max-height: 14rem;
    overflow: auto;
  }
  summary {
    cursor: pointer;
  }
  .buttons {
    display: flex;
    justify-content: flex-end;
    gap: 0.5rem;
  }
</style>
