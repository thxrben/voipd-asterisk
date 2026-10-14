<script lang="ts">
  // SIP access: where phones may connect from, and bans after failed logins.
  import Trash from '@lucide/svelte/icons/trash-2'
  import { api } from '../../lib/api'
  import { emptySave, runSave } from '../../lib/save'
  import { toast } from '../../lib/toast.svelte'
  import { ago, dateTime } from '../../lib/format'
  import type { SecurityInfo, SecuritySettings } from '../../lib/types'
  import Field from '../Field.svelte'

  let info = $state<SecurityInfo | null>(null)
  let error = $state('')
  let set = $state<SecuritySettings>({
    acl_enabled: true, allowed_nets: [], ban_enabled: true, max_failures: 10, window_minutes: 10, ban_minutes: 60,
  })
  let custom = $state(false)
  let nets = $state('')
  let save = $state(emptySave())
  let banIP = $state('')
  let banMinutes = $state(0)
  let banReason = $state('')
  let banSave = $state(emptySave())

  async function reload(resetForm = false) {
    try {
      info = await api<SecurityInfo>('/security')
      error = ''
      if (resetForm) fill(info)
    } catch (e) {
      error = (e as Error).message
    }
  }
  function fill(i: SecurityInfo) {
    set = { ...i.settings }
    custom = i.settings.allowed_nets.length > 0
    nets = i.settings.allowed_nets.join('\n')
  }
  $effect(() => {
    void reload(true)
    const t = setInterval(() => reload(), 20_000)
    return () => clearInterval(t)
  })

  async function submit(e: SubmitEvent) {
    e.preventDefault()
    const body = {
      ...set,
      allowed_nets: custom ? nets.split(/[\s,;]+/).map((x) => x.trim()).filter(Boolean) : [],
      max_failures: Number(set.max_failures) || 0,
      window_minutes: Number(set.window_minutes) || 0,
      ban_minutes: Number(set.ban_minutes) || 0,
    }
    const ok = await runSave(save, async () => {
      const res = await api<{ result: SecurityInfo }>('/security/settings', { method: 'PUT', json: body })
      info = res.result
      fill(res.result)
      return res
    })
    if (ok) toast.show('SIP access settings applied')
  }

  async function ban(e: SubmitEvent) {
    e.preventDefault()
    const ok = await runSave(banSave, () =>
      api('/security/bans', { method: 'POST', json: { ip: banIP, minutes: Number(banMinutes) || 0, reason: banReason } }),
    )
    if (ok) {
      toast.show(`${banIP} banned`)
      banIP = ''
      banReason = ''
      void reload()
    }
  }

  async function unban(ip: string) {
    try {
      await api(`/security/bans?ip=${encodeURIComponent(ip)}`, { method: 'DELETE' })
      toast.show(`${ip} unbanned`)
      void reload()
    } catch (e) {
      toast.show((e as Error).message, 'bad')
    }
  }

  const kinds = { password: 'wrong password', unknown_account: 'unknown account', acl: 'outside allowed networks' }
</script>

{#if error}<div class="error-box">{error}</div>{/if}

{#if info}
  <section class="card">
    <header><h2>Where phones may connect from</h2></header>
    <form class="body form" onsubmit={submit} aria-label="SIP access settings">
      <p class="muted">
        Phones (and softphones) may only register and call from these networks; requests from anywhere else are refused
        before the password is even checked. The line to your carrier is not affected.
      </p>
      <label class="switch"><input type="checkbox" bind:checked={set.acl_enabled} /> Limit phones to allowed networks</label>
      {#if set.acl_enabled}
        <div class="choice" role="radiogroup" aria-label="Allowed networks">
          <label><input type="radio" name="nets" checked={!custom} onchange={() => (custom = false)} />
            Automatic: this host's networks, <span class="mono">VOIPD_LOCAL_NETS</span> (WireGuard) and loopback</label>
          {#if !custom}<p class="mono small nets">{info.auto_nets.join(' · ')}</p>{/if}
          <label><input type="radio" name="nets" checked={custom} onchange={() => (custom = true)} /> Only these networks</label>
          {#if custom}
            <Field label="Networks" error={save.fields['allowed_nets']} help="One address or network per line, e.g. 192.168.1.0/24 or 10.8.0.0/24">
              <textarea rows="4" bind:value={nets} class="mono"></textarea>
            </Field>
          {/if}
        </div>
        {#if !info.client_allowed}
          <p class="warn-box">
            This browser's address <span class="mono">{info.client_ip}</span> is outside the allowed networks: a phone there
            could not register.
          </p>
        {/if}
      {/if}

      <h3>Failed logins</h3>
      <label class="switch"><input type="checkbox" bind:checked={set.ban_enabled} /> Ban senders of repeated failed logins</label>
      {#if set.ban_enabled}
        <div class="cols3">
          <Field label="After failures" error={save.fields['max_failures']}><input type="number" min="2" bind:value={set.max_failures} /></Field>
          <Field label="Within (minutes)" error={save.fields['window_minutes']}><input type="number" min="1" bind:value={set.window_minutes} /></Field>
          <Field label="Ban for (minutes)" error={save.fields['ban_minutes']}><input type="number" min="1" bind:value={set.ban_minutes} /></Field>
        </div>
      {/if}
      {#if save.error && Object.keys(save.fields).length === 0}<p class="error-box">{save.error}</p>{/if}
      <div class="actions"><button class="primary" type="submit" disabled={save.busy}>Save</button></div>
    </form>
  </section>

  <div class="row">
    <section class="card">
      <header><h2>Banned</h2></header>
      {#if info.bans.length === 0}
        <div class="empty">Nobody is banned.</div>
      {:else}
        <div class="body flush table-wrap">
          <table>
            <thead><tr><th>Address</th><th>Why</th><th>Until</th><th></th></tr></thead>
            <tbody>
              {#each info.bans as b (b.ip)}
                <tr>
                  <td class="mono">{b.ip}</td>
                  <td>{b.reason}{#if b.account}<span class="muted small"> · {b.account}</span>{/if}</td>
                  <td>{b.expires_at ? dateTime(b.expires_at) : 'unbanned by hand'}</td>
                  <td class="row-actions">
                    <button onclick={() => unban(b.ip)} aria-label="Unban {b.ip}" title="Unban"><Trash size={15} /></button>
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/if}
      <form class="body ban" onsubmit={ban} aria-label="Ban an address">
        <Field label="Address or network" error={banSave.fields['ip']}><input bind:value={banIP} placeholder="203.0.113.9" class="mono" /></Field>
        <Field label="Minutes" error={banSave.fields['minutes']} help="0 = until unbanned"><input type="number" min="0" bind:value={banMinutes} /></Field>
        <Field label="Reason"><input bind:value={banReason} /></Field>
        <button type="submit" disabled={banSave.busy || !banIP}>Ban</button>
      </form>
    </section>

    <section class="card">
      <header><h2>Recent failed logins</h2></header>
      {#if info.recent.length === 0}
        <div class="empty">None since voipd started.</div>
      {:else}
        <ul class="recent">
          {#each info.recent as f, i (i)}
            <li>
              <span class="mono">{f.ip}</span>
              <span>{kinds[f.kind]}{#if f.account}: <span class="mono">{f.account}</span>{/if}</span>
              <span class="muted small">{ago(f.at)}</span>
            </li>
          {/each}
        </ul>
      {/if}
    </section>
  </div>
{:else if !error}
  <div class="empty">Loading…</div>
{/if}

<style>
  .form {
    display: grid;
    gap: 0.9rem;
  }
  p {
    margin: 0;
  }
  .switch {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    font-weight: 600;
  }
  .choice {
    display: grid;
    gap: 0.5rem;
    padding-left: 1.6rem;
  }
  .choice label {
    display: flex;
    gap: 0.5rem;
    align-items: baseline;
    font-weight: normal;
  }
  .nets {
    padding-left: 1.6rem;
    overflow-wrap: anywhere;
  }
  .small {
    font-size: 0.82rem;
  }
  h3 {
    margin: 0.4rem 0 0;
    font-size: 0.8rem;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--muted);
  }
  .cols3 {
    display: grid;
    grid-template-columns: repeat(3, 1fr);
    gap: 0.9rem;
  }
  @media (max-width: 600px) {
    .cols3 {
      grid-template-columns: 1fr;
    }
  }
  .actions {
    display: flex;
    justify-content: flex-end;
  }
  .warn-box {
    padding: 0.6rem 0.8rem;
    border: 1px solid var(--warn);
    border-radius: var(--radius);
    color: var(--warn);
  }
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
  .row-actions {
    text-align: right;
  }
  .row-actions button {
    padding: 0.3rem;
  }
  .ban {
    display: grid;
    grid-template-columns: 2fr 1fr 2fr auto;
    gap: 0.6rem;
    align-items: end;
    border-top: 1px solid var(--border);
  }
  @media (max-width: 600px) {
    .ban {
      grid-template-columns: 1fr;
    }
  }
  .recent {
    list-style: none;
    margin: 0;
    padding: 0.5rem 1rem;
    display: grid;
    gap: 0.4rem;
  }
  .recent li {
    display: flex;
    gap: 0.75rem;
    flex-wrap: wrap;
    align-items: baseline;
  }
  textarea {
    resize: vertical;
    font: inherit;
  }
</style>
