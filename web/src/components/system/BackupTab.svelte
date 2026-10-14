<script lang="ts">
  // Backups: schedule and passphrase, backup files (download, restore,
  // delete), configuration export and import with a preview.
  import Download from '@lucide/svelte/icons/download'
  import Trash from '@lucide/svelte/icons/trash-2'
  import History from '@lucide/svelte/icons/history'
  import { api, ApiError } from '../../lib/api'
  import { emptySave, runSave } from '../../lib/save'
  import { toast } from '../../lib/toast.svelte'
  import { bytes, dateTime, ago } from '../../lib/format'
  import type { BackupInfo, BackupManifest, ConfigDiff } from '../../lib/types'
  import Field from '../Field.svelte'
  import Modal from '../Modal.svelte'

  let info = $state<BackupInfo | null>(null)
  let error = $state('')
  let enabled = $state(false)
  let time = $state('03:30')
  let keep = $state(14)
  let passphrase = $state('')
  let save = $state(emptySave())
  let running = $state(false)

  async function reload() {
    try {
      info = await api<BackupInfo>('/backup')
      error = ''
    } catch (e) {
      error = (e as Error).message
    }
  }
  $effect(() => {
    void reload().then(() => {
      if (!info) return
      enabled = info.settings.enabled
      time = info.settings.time
      keep = info.settings.keep
    })
  })

  async function submit(e: SubmitEvent) {
    e.preventDefault()
    const ok = await runSave(save, () =>
      api('/backup/settings', { method: 'PUT', json: { enabled, time, keep: Number(keep) || 0, passphrase } }),
    )
    if (ok) {
      passphrase = ''
      toast.show(enabled ? `Backups run daily at ${time}` : 'Backup settings saved')
      void reload()
    }
  }

  async function runNow() {
    running = true
    try {
      const res = await api<{ result: { name: string } }>('/backup/run', { method: 'POST' })
      toast.show(`Backup ${res.result.name} written`)
    } catch (e) {
      toast.show((e as ApiError).fields?.passphrase ?? (e as Error).message, 'bad')
    } finally {
      running = false
      void reload()
    }
  }

  async function remove(name: string) {
    if (!confirm(`Delete the backup ${name}?`)) return
    try {
      await api(`/backup/files/${encodeURIComponent(name)}`, { method: 'DELETE' })
      void reload()
    } catch (e) {
      toast.show((e as Error).message, 'bad')
    }
  }

  // Restore: from a stored file (name) or an uploaded one.
  let restoring = $state<{ name?: string } | null>(null)
  let restorePass = $state('')
  let restoreFile = $state<FileList | null>(null)
  let restoreSave = $state(emptySave())
  let restarting = $state(false)

  async function restore(e: SubmitEvent) {
    e.preventDefault()
    const target = restoring
    if (!target) return
    const ok = await runSave(restoreSave, async () => {
      if (target.name) {
        return api<{ result: BackupManifest }>(`/backup/files/${encodeURIComponent(target.name)}/restore`, {
          method: 'POST', json: { passphrase: restorePass },
        })
      }
      const f = restoreFile?.[0]
      if (!f) throw new ApiError(422, 'choose a backup file', { file: 'choose a backup file' })
      const form = new FormData()
      form.append('passphrase', restorePass) // before the file: the server streams it
      form.append('file', f)
      return api<{ result: BackupManifest }>('/backup/restore', { method: 'POST', body: form })
    })
    if (ok) {
      restoring = null
      restarting = true
      void waitForRestart()
    }
  }

  // voipd exits to swap the backup in; reload once it answers again.
  async function waitForRestart() {
    await new Promise((r) => setTimeout(r, 3000))
    for (let i = 0; i < 120; i++) {
      try {
        const res = await fetch('/healthz', { cache: 'no-store' })
        if (res.ok) {
          location.reload()
          return
        }
      } catch {
        /* still down */
      }
      await new Promise((r) => setTimeout(r, 2000))
    }
    restarting = false
    toast.show('voipd did not come back; check the container logs', 'bad')
  }

  // Configuration import with a preview.
  let importFile = $state<FileList | null>(null)
  let importPass = $state('')
  let importSave = $state(emptySave())
  let diff = $state<ConfigDiff | null>(null)

  function importForm(token?: string) {
    const form = new FormData()
    form.append('passphrase', importPass)
    if (token) form.append('token', token)
    const f = importFile?.[0]
    if (f) form.append('file', f)
    return form
  }

  async function preview(e: SubmitEvent) {
    e.preventDefault()
    await runSave(importSave, async () => {
      diff = (await api<{ result: ConfigDiff }>('/backup/config/plan', { method: 'POST', body: importForm() })).result
      return undefined
    })
  }

  async function applyImport() {
    if (!diff) return
    const token = diff.token
    const ok = await runSave(importSave, () => api('/backup/config/apply', { method: 'POST', body: importForm(token) }))
    if (ok) {
      diff = null
      importFile = null
      importPass = ''
      toast.show('Configuration imported')
      void reload()
    } else if (importSave.error) {
      diff = null
      toast.show(importSave.error, 'bad')
    }
  }

  const sectionNames: Record<string, string> = {
    settings: 'Settings', trunks: 'Lines', numbers: 'Numbers', extensions: 'Phones', ring_groups: 'Ring groups',
    ring_group_members: 'Ring group members', routes: 'Incoming routes', list_sources: 'Lists and address books',
    cards: 'Contacts', list_entries: 'List entries', modules: 'Integrations', webhooks: 'Webhooks',
    directory_accounts: 'Directory accounts',
  }
  const preview3 = (l: string[]) => l.slice(0, 3).join(', ') + (l.length > 3 ? ` and ${l.length - 3} more` : '')
</script>

{#if error}<div class="error-box">{error}</div>{/if}

{#if restarting}
  <section class="card"><div class="body restarting" role="status">
    <b>Restoring the backup.</b> voipd restarts and the page reloads when it is back.
  </div></section>
{:else if info}
  {#if info.restore_pending}
    <div class="error-box">A restore is waiting for the next start of voipd. Restart the container to finish it.</div>
  {/if}
  <div class="row">
    <section class="card">
      <header><h2>Scheduled backups</h2></header>
      <form class="body form" onsubmit={submit} aria-label="Backup settings">
        <p class="muted">
          A backup holds the database (configuration, call history, contacts) and all voicemail. It is encrypted with
          the passphrase; without it a backup cannot be restored, so keep it somewhere safe.
        </p>
        <label class="switch"><input type="checkbox" bind:checked={enabled} /> Back up daily</label>
        <div class="cols">
          <Field label="Time" error={save.fields['time']}><input type="time" bind:value={time} /></Field>
          <Field label="Keep" error={save.fields['keep']} help="newest backups">
            <input type="number" min="1" max="365" bind:value={keep} />
          </Field>
        </div>
        <Field label="Passphrase" error={save.fields['passphrase']}
          help={info.settings.passphrase_set ? 'Set. Leave empty to keep it.' : 'At least 12 characters'}>
          <input type="password" bind:value={passphrase} autocomplete="new-password"
            placeholder={info.settings.passphrase_set ? '•••••••• (unchanged)' : ''} />
        </Field>
        {#if save.error && Object.keys(save.fields).length === 0}<p class="error-box">{save.error}</p>{/if}
        <div class="actions"><button class="primary" type="submit" disabled={save.busy}>Save</button></div>
      </form>
    </section>

    <section class="card">
      <header><h2>Status</h2></header>
      <div class="body kv">
        <span>Last backup</span>
        <span>
          {#if info.status.last_at}
            {#if info.status.last_error}<span class="dot bad"></span> failed {ago(info.status.last_at)}
            {:else}<span class="dot good"></span> {ago(info.status.last_at)}{/if}
          {:else}never{/if}
        </span>
        {#if info.status.last_error}<span></span><span class="error small">{info.status.last_error}</span>{/if}
        <span>Next</span><span>{info.status.next_at ? dateTime(info.status.next_at) : 'not scheduled'}</span>
        <span>Directory</span><span class="mono small">{info.status.dir}</span>
      </div>
      {#if info.same_disk}
        <p class="body warn small">
          The backups are on the same disk as the data. Mount another disk at this directory, or download backups
          regularly, so a broken SD card does not take them along.
        </p>
      {/if}
    </section>
  </div>

  <section class="card">
    <header>
      <h2>Backups</h2>
      <div class="head-actions">
        <button onclick={() => { restoring = {}; restorePass = ''; restoreFile = null; restoreSave = emptySave() }}>
          <History size={16} /> Restore from file
        </button>
        <button class="primary" onclick={runNow} disabled={running || info.status.running}>
          {running || info.status.running ? 'Backing up…' : 'Back up now'}
        </button>
      </div>
    </header>
    {#if info.files.length === 0}
      <div class="empty">No backups yet.</div>
    {:else}
      <div class="body flush table-wrap">
        <table>
          <thead><tr><th>Backup</th><th class="num">Size</th><th></th></tr></thead>
          <tbody>
            {#each info.files as f (f.name)}
              <tr>
                <td>{dateTime(f.modified)} <span class="muted small mono">{f.name}</span></td>
                <td class="num">{bytes(f.size)}</td>
                <td class="row-actions">
                  <a class="button" href="/api/v1/backup/files/{encodeURIComponent(f.name)}" download aria-label="Download {f.name}"
                    title="Download"><Download size={15} /></a>
                  <button onclick={() => { restoring = { name: f.name }; restorePass = ''; restoreSave = emptySave() }}
                    aria-label="Restore {f.name}" title="Restore"><History size={15} /></button>
                  <button onclick={() => remove(f.name)} aria-label="Delete {f.name}" title="Delete"><Trash size={15} /></button>
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </section>

  <section class="card">
    <header><h2>Configuration</h2></header>
    <div class="body form">
      <p class="muted">
        The configuration as a readable YAML file: lines, phones, ring groups, lists, integrations and directory
        accounts, without call history or voicemail. Use it to review settings, keep them in version control or move
        them to another voipd.
      </p>
      <div class="exports">
        <a class="button" href="/api/v1/backup/config" download><Download size={15} /> Export without passwords</a>
        {#if info.settings.passphrase_set}
          <a class="button" href="/api/v1/backup/config?secrets=1" download><Download size={15} /> Export with passwords (encrypted)</a>
        {:else}
          <span class="muted small">Set a passphrase to export with passwords.</span>
        {/if}
      </div>
      <form class="import" onsubmit={preview} aria-label="Import configuration">
        <h3>Import</h3>
        <div class="cols">
          <Field label="Configuration file" error={importSave.fields['file']}>
            <input type="file" accept=".yaml,.yml,.age" bind:files={importFile} />
          </Field>
          <Field label="Passphrase" error={importSave.fields['passphrase']} help="Only for encrypted files">
            <input type="password" bind:value={importPass} autocomplete="off" />
          </Field>
        </div>
        {#if importSave.error && Object.keys(importSave.fields).length === 0 && !diff}<p class="error-box">{importSave.error}</p>{/if}
        <div class="actions"><button type="submit" disabled={importSave.busy || !importFile?.length}>Preview changes</button></div>
      </form>
    </div>
  </section>
{:else if !error}
  <div class="empty">Loading…</div>
{/if}

<Modal title="Import configuration" open={diff !== null} onclose={() => (diff = null)} wide>
  {#if diff}
    {#if diff.sections.length === 0}
      <p>The file matches the current configuration. Nothing to change.</p>
    {:else}
      <p>
        These changes are applied together.{#if !diff.secrets} Passwords are not in the file; existing ones are kept.{/if}
      </p>
      <ul class="diff">
        {#each diff.sections as s (s.section)}
          <li>
            <b>{sectionNames[s.section] ?? s.section}</b>
            {#if s.added.length}<div class="good">+ {s.added.length} new: {preview3(s.added)}</div>{/if}
            {#if s.changed.length}
              <div class="warn">~ {s.changed.length} changed: {preview3(s.changed.map((c) => `${c.key} (${c.fields.join(', ')})`))}</div>
            {/if}
            {#if s.removed.length}<div class="bad">− {s.removed.length} removed: {preview3(s.removed)}</div>{/if}
          </li>
        {/each}
      </ul>
      {#if diff.warnings.length}
        <ul class="warnings">{#each diff.warnings as w}<li>{w}</li>{/each}</ul>
      {/if}
    {/if}
  {/if}
  {#snippet footer()}
    <button onclick={() => (diff = null)}>Cancel</button>
    {#if diff?.sections.length}
      <button class="primary" onclick={applyImport} disabled={importSave.busy}>Apply changes</button>
    {/if}
  {/snippet}
</Modal>

<Modal title="Restore a backup" open={restoring !== null} onclose={() => (restoring = null)}>
  <form id="restore-form" class="form" onsubmit={restore}>
    <p>
      Restoring replaces <b>everything</b> — configuration, call history, contacts and voicemail — with the backup.
      voipd restarts; calls in progress are cut off. The current data is kept aside until the next restore.
    </p>
    {#if restoring?.name}
      <p class="mono small">{restoring.name}</p>
    {:else}
      <Field label="Backup file" error={restoreSave.fields['file']}>
        <input type="file" accept=".age" bind:files={restoreFile} />
      </Field>
    {/if}
    <Field label="Passphrase of the backup" error={restoreSave.fields['passphrase']}>
      <input type="password" bind:value={restorePass} autocomplete="off" />
    </Field>
    {#if restoreSave.error && Object.keys(restoreSave.fields).length === 0}<p class="error-box">{restoreSave.error}</p>{/if}
  </form>
  {#snippet footer()}
    <button onclick={() => (restoring = null)}>Cancel</button>
    <button class="primary danger" type="submit" form="restore-form" disabled={restoreSave.busy}>
      {restoreSave.busy ? 'Checking…' : 'Restore and restart'}
    </button>
  {/snippet}
</Modal>

<style>
  .row {
    display: grid;
    grid-template-columns: 3fr 2fr;
    gap: 1rem;
  }
  @media (max-width: 900px) {
    .row {
      grid-template-columns: 1fr;
    }
  }
  .form {
    display: grid;
    gap: 0.9rem;
  }
  .cols {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 0.9rem;
  }
  @media (max-width: 600px) {
    .cols {
      grid-template-columns: 1fr;
    }
  }
  .muted,
  p {
    margin: 0;
  }
  .switch {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    font-weight: 600;
  }
  .actions,
  .head-actions,
  .exports {
    display: flex;
    gap: 0.5rem;
    flex-wrap: wrap;
    align-items: center;
  }
  .actions {
    justify-content: flex-end;
  }
  .kv {
    display: grid;
    grid-template-columns: max-content 1fr;
    gap: 0.4rem 1rem;
    align-items: baseline;
  }
  .small {
    font-size: 0.82rem;
  }
  .error {
    color: var(--bad);
  }
  .warn {
    color: var(--warn);
  }
  .good {
    color: var(--good);
  }
  .bad {
    color: var(--bad);
  }
  .row-actions {
    display: flex;
    gap: 0.3rem;
    justify-content: flex-end;
  }
  .row-actions :is(a, button) {
    padding: 0.3rem;
  }
  h3 {
    margin: 0.4rem 0 0;
    font-size: 0.8rem;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--muted);
  }
  .import {
    display: grid;
    gap: 0.9rem;
  }
  .diff {
    display: grid;
    gap: 0.6rem;
    padding-left: 1.1rem;
  }
  .warnings {
    color: var(--warn);
    padding-left: 1.1rem;
  }
  .restarting {
    padding: 1.5rem;
  }
  button.danger {
    background: var(--bad);
    border-color: var(--bad);
  }
</style>
