<script lang="ts">
  // Phone directory: LDAP (desk phones, DECT) and CardDAV (softphones,
  // FritzBox) over the contacts and extensions, with read-only accounts.
  import Plus from '@lucide/svelte/icons/plus'
  import Trash from '@lucide/svelte/icons/trash-2'
  import KeyRound from '@lucide/svelte/icons/key-round'
  import Copy from '@lucide/svelte/icons/copy'
  import { api } from '../lib/api'
  import { ago } from '../lib/format'
  import { emptySave, runSave } from '../lib/save'
  import { toast } from '../lib/toast.svelte'
  import type { DirectoryEntry, DirectoryInfo } from '../lib/types'
  import Modal from '../components/Modal.svelte'
  import Field from '../components/Field.svelte'

  let info = $state<DirectoryInfo | null>(null)
  let error = $state('')
  async function load() {
    try {
      info = await api<DirectoryInfo>('/directory')
      error = ''
    } catch (e) {
      error = (e as Error).message
    }
  }
  $effect(() => {
    void load()
  })

  const host = location.hostname
  const davURL = $derived(`${location.origin}${info?.carddav_path ?? '/dav/'}`)
  const user = $derived(info?.accounts[0]?.username ?? '<account>')

  // --- accounts ---
  let adding = $state(false)
  let username = $state('')
  let save = $state(emptySave())
  let shown = $state<{ username: string; password: string } | null>(null)

  async function create(e: SubmitEvent) {
    e.preventDefault()
    let res: { result: { password: string; account: { username: string } } } | undefined
    const ok = await runSave(save, async () => {
      res = await api('/directory/accounts', { method: 'POST', json: { username: username.trim() } })
      return res
    })
    if (ok && res) {
      shown = { username: res.result.account.username, password: res.result.password }
      adding = false
      username = ''
      void load()
    }
  }

  async function regenerate(id: number, name: string) {
    if (!confirm(`Create a new password for "${name}"? Phones using the old one stop finding contacts until updated.`)) return
    try {
      const res = await api<{ result: { password: string } }>(`/directory/accounts/${id}/password`, { method: 'POST' })
      shown = { username: name, password: res.result.password }
    } catch (e) {
      toast.show((e as Error).message, 'bad')
    }
  }

  async function remove(id: number, name: string) {
    if (!confirm(`Delete the directory account "${name}"?`)) return
    try {
      await api(`/directory/accounts/${id}`, { method: 'DELETE' })
      void load()
    } catch (e) {
      toast.show((e as Error).message, 'bad')
    }
  }

  // --- search preview ---
  let q = $state('')
  let results = $state<DirectoryEntry[] | null>(null)
  async function search(e: SubmitEvent) {
    e.preventDefault()
    try {
      results = (await api<{ entries: DirectoryEntry[] }>(`/directory/search?q=${encodeURIComponent(q)}`)).entries
    } catch (err) {
      toast.show((err as Error).message, 'bad')
    }
  }

  function copy(v: string) {
    void navigator.clipboard?.writeText(v).then(() => toast.show('Copied'))
  }

  const ldapPort = $derived(info?.ldap.port || 389)
  const clients = $derived([
    {
      name: 'Yealink', where: 'Directory → LDAP',
      rows: [
        ['Server', host], ['Port', String(ldapPort)], ['Base', info?.base_dn ?? 'dc=voipd'],
        ['User name', user], ['Name filter', '(|(cn=%)(sn=%))'],
        ['Number filter', '(|(telephoneNumber=%)(mobile=%)(homePhone=%))'], ['Display name', 'cn'],
        ['Number attributes', 'telephoneNumber mobile homePhone'], ['LDAP version', '3'], ['Lookup for incoming calls', 'on'],
      ],
    },
    {
      name: 'Snom', where: 'Directory → LDAP',
      rows: [
        ['Server', host], ['Port', String(ldapPort)], ['Base', info?.base_dn ?? 'dc=voipd'], ['Bind DN', user],
        ['Search filter', '(|(cn=*%*)(telephoneNumber=*%*))'], ['Number filter', '(|(telephoneNumber=%)(mobile=%))'],
        ['Name attributes', 'cn'], ['Number attributes', 'telephoneNumber mobile homePhone'],
      ],
    },
    {
      name: 'Grandstream', where: 'Phonebook → LDAP',
      rows: [
        ['Server', host], ['Port', String(ldapPort)], ['Base DN', info?.base_dn ?? 'dc=voipd'], ['User name', user],
        ['Name filter', '(|(cn=%)(sn=%))'], ['Number filter', '(|(telephoneNumber=%)(mobile=%))'],
        ['Display name attributes', 'cn'], ['Number attributes', 'telephoneNumber mobile homePhone'],
      ],
    },
    {
      name: 'Gigaset (DECT)', where: 'Telephony → Directories → LDAP',
      rows: [
        ['Server', host], ['Port', String(ldapPort)], ['Search base', info?.base_dn ?? 'dc=voipd'], ['User name', user],
        ['Filter', '(&(objectClass=person)(|(cn=%)(sn=%)))'],
      ],
    },
    {
      name: 'FritzBox', where: 'Telefonie → Telefonbücher → Neues Telefonbuch → Online-Telefonbuch (CardDAV)',
      rows: [['URL', `${davURL}${user}/books/contacts/`], ['Benutzer', user]],
    },
    {
      name: 'Softphones, DAVx5, iPhone', where: 'Add a CardDAV account',
      rows: [['Server', davURL], ['User name', user]],
    },
  ])
</script>

<div class="page">
  <h1>Directory</h1>
  <p class="muted intro">
    Phones look up names in voipd's contacts and extensions, and show caller names on incoming calls.
    Desk phones use LDAP, softphones and the FritzBox use CardDAV. Both need a directory account.
  </p>

  {#if error}<div class="error-box">{error}</div>{/if}

  {#if info}
    <div class="row">
      <section class="card">
        <header><h2>Status</h2></header>
        <div class="body kv">
          <span>LDAP</span>
          <span>
            {#if info.ldap.listening}<span class="dot good"></span> listening on port {info.ldap.port}
            {:else}<span class="dot bad"></span> not available{/if}
          </span>
          {#if !info.ldap.listening && info.ldap.error}<span></span><span class="error small">{info.ldap.error}</span>{/if}
          <span>CardDAV</span><span class="mono small">{davURL}</span>
          <span>Entries</span><span>{info.contacts} contacts · {info.extensions} phones</span>
        </div>
      </section>

      <section class="card">
        <header>
          <h2>Accounts</h2>
          <button class="primary add" onclick={() => { adding = true; save = emptySave() }}><Plus size={16} /> Add account</button>
        </header>
        {#if info.accounts.length === 0}
          <div class="empty">No accounts yet. Create one for your phones.</div>
        {:else}
          <ul class="accounts">
            {#each info.accounts as a (a.id)}
              <li>
                <b class="mono">{a.username}</b>
                <span class="muted small">{a.last_used_at ? `used ${ago(a.last_used_at)}` : 'never used'}</span>
                <span class="actions">
                  <button onclick={() => regenerate(a.id, a.username)} aria-label="New password for {a.username}"
                    title="New password"><KeyRound size={15} /></button>
                  <button onclick={() => remove(a.id, a.username)} aria-label="Delete {a.username}"><Trash size={15} /></button>
                </span>
              </li>
            {/each}
          </ul>
        {/if}
      </section>
    </div>

    <section class="card">
      <header><h2>Try a search</h2></header>
      <form class="body search" onsubmit={search}>
        <input bind:value={q} placeholder="Name or number, e.g. 0170 1234567" aria-label="Search the directory" />
        <button type="submit">Search</button>
      </form>
      {#if results}
        {#if results.length === 0}
          <div class="empty">Nothing found.</div>
        {:else}
          <div class="body flush table-wrap">
            <table>
              <thead><tr><th>Name</th><th>Numbers as the phone dials them</th></tr></thead>
              <tbody>
                {#each results as r (r.id)}
                  <tr>
                    <td><b>{r.name}</b>{#if r.org}<span class="muted"> · {r.org}</span>{/if}</td>
                    <td class="mono">{r.numbers.map((n) => `${n.dial} (${n.type})`).join(', ')}</td>
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
        {/if}
      {/if}
    </section>

    <h2 class="group">Phone setup</h2>
    <div class="clients">
      {#each clients as c (c.name)}
        <section class="card" aria-label="Setup {c.name}">
          <header><h2>{c.name}</h2></header>
          <div class="body">
            <p class="muted small where">{c.where}</p>
            <table class="setup">
              <tbody>
                {#each c.rows as [k, v] (k)}
                  <tr>
                    <td class="muted">{k}</td>
                    <td class="mono value">{v}</td>
                    <td><button class="icon" onclick={() => copy(v!)} aria-label="Copy {c.name} {k}"><Copy size={13} /></button></td>
                  </tr>
                {/each}
                <tr><td class="muted">Password</td><td colspan="2" class="small">the account's password</td></tr>
              </tbody>
            </table>
          </div>
        </section>
      {/each}
    </div>
  {/if}
</div>

<Modal title="Add directory account" open={adding} onclose={() => (adding = false)}>
  <form id="account-form" class="form" onsubmit={create}>
    <Field label="User name" error={save.fields['username']} help="Lowercase letters, digits, . _ - (e.g. phones)">
      <input bind:value={username} required autocomplete="off" />
    </Field>
    <p class="muted small">voipd generates the password and shows it once.</p>
    {#if save.error && Object.keys(save.fields).length === 0}<p class="error-box">{save.error}</p>{/if}
  </form>
  {#snippet footer()}
    <button onclick={() => (adding = false)}>Cancel</button>
    <button class="primary" type="submit" form="account-form" disabled={save.busy}>Create</button>
  {/snippet}
</Modal>

<Modal title="Directory account" open={!!shown} onclose={() => (shown = null)}>
  {#if shown}
    <p>Enter these on your phones. The password is shown only now.</p>
    <table class="setup">
      <tbody>
        <tr><td class="muted">User name</td><td class="mono value">{shown.username}</td><td></td></tr>
        <tr>
          <td class="muted">Password</td>
          <td class="mono value" data-testid="directory-password">{shown.password}</td>
          <td><button class="icon" onclick={() => copy(shown!.password)} aria-label="Copy password"><Copy size={13} /></button></td>
        </tr>
      </tbody>
    </table>
  {/if}
  {#snippet footer()}
    <button class="primary" onclick={() => (shown = null)}>Done</button>
  {/snippet}
</Modal>

<style>
  .page {
    display: grid;
    gap: 1rem;
  }
  h1 {
    margin: 0;
  }
  .intro {
    margin: 0;
  }
  .row {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 1rem;
  }
  @media (max-width: 800px) {
    .row {
      grid-template-columns: 1fr;
    }
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
  header .add {
    margin-left: auto;
  }
  .accounts {
    list-style: none;
    margin: 0;
    padding: 0.4rem 1rem;
  }
  .accounts li {
    display: flex;
    align-items: center;
    gap: 0.7rem;
    padding: 0.35rem 0;
  }
  .actions {
    margin-left: auto;
    display: flex;
    gap: 0.3rem;
  }
  .actions button,
  .icon {
    padding: 0.3rem;
  }
  .search {
    display: flex;
    gap: 0.5rem;
  }
  .search input {
    flex: 1;
    max-width: 26rem;
  }
  h2.group {
    font-size: 0.85rem;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--muted);
    margin: 0.4rem 0 -0.4rem;
  }
  .clients {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(340px, 1fr));
    gap: 1rem;
  }
  .where {
    margin: 0 0 0.5rem;
  }
  .setup td {
    padding: 0.2rem 0.6rem 0.2rem 0;
    vertical-align: top;
  }
  .value {
    overflow-wrap: anywhere;
    font-size: 0.85rem;
  }
</style>
