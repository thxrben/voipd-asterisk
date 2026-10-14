<script lang="ts">
  import Server from '@lucide/svelte/icons/server'
  import Cpu from '@lucide/svelte/icons/cpu'
  import { api } from '../lib/api'
  import { live } from '../lib/live.svelte'
  import { ago, bytes, dateTime, duration, num, phone, uptime } from '../lib/format'
  import type { Status } from '../lib/types'
  import ActiveCalls from '../components/ActiveCalls.svelte'
  import ModuleState from '../components/integrations/ModuleState.svelte'
  import { link } from '../lib/router.svelte'

  const moduleLabel: Record<string, string> = { notify: 'E-mail', webhook: 'Webhooks', mqtt: 'MQTT' }
  const moduleTab: Record<string, string> = { notify: 'email', webhook: 'webhooks', mqtt: 'mqtt' }

  let status = $state<Status | null>(null)
  let error = $state('')

  async function load() {
    try {
      status = await api<Status>('/status')
      error = ''
    } catch (e) {
      error = (e as Error).message
    }
  }
  $effect(() => {
    void load()
    const t = setInterval(load, 10_000)
    const off = live.on('trunk.', () => void load())
    const off2 = live.on('engine.', () => void load())
    return () => {
      clearInterval(t)
      off()
      off2()
    }
  })

  const eventLabel: Record<string, string> = {
    'trunk.up': 'Line registered',
    'trunk.down': 'Line down',
    'trunk.reconnecting': 'Reconnecting',
    'engine.up': 'Asterisk connected',
    'engine.down': 'Asterisk connection lost',
    'trunk.family_changed': 'Line switched address family',
    'security.banned': 'SIP sender banned',
  }
  function eventTone(t: string): string {
    if (t.endsWith('.up')) return 'good'
    if (t.endsWith('.down')) return 'bad'
    return 'warn'
  }
  function detailText(detail: Record<string, unknown> | null): string {
    const d = detail ?? {}
    const p: string[] = []
    if (d['number']) p.push(phone(String(d['number'])))
    if (d['action']) p.push(`${d['action'] as string}${d['attempt'] ? ` #${d['attempt'] as number}` : ''}`)
    if (d['reason'] && d['reason'] !== 'escalation') p.push(String(d['reason']))
    if (d['detail']) p.push(String(d['detail']))
    if (d['from'] && d['to']) p.push(`IP${d['from'] as string} → IP${d['to'] as string}`)
    if (d['down_seconds'] != null) p.push(`was down ${duration(Number(d['down_seconds']))}`)
    if (d['error']) p.push(String(d['error']))
    return p.join(' · ')
  }
  const mem = $derived(
    status?.system.mem_total && status.system.mem_available != null
      ? 1 - status.system.mem_available / status.system.mem_total
      : null,
  )
  const disk = $derived(
    status?.system.disk_total && status.system.disk_free != null
      ? 1 - status.system.disk_free / status.system.disk_total
      : null,
  )
</script>

<div class="page">
  <h1>Status</h1>
  {#if error}<div class="error-box">{error}</div>{/if}

  {#if status}
    <div class="cols3">
      <section class="card">
        <header><h2><Server size={16} /> Asterisk</h2></header>
        <div class="body kv">
          <span>Connection</span>
          <span><span class="dot {status.engine.connected ? 'good' : 'bad'}"></span>
            {status.engine.connected ? 'connected' : 'disconnected'}</span>
          <span>Version</span><span>{status.engine.version || '–'}</span>
          <span>Uptime</span><span>{status.engine.connected ? uptime(status.engine.uptime_seconds) : '–'}</span>
          <span>Calls now</span><span class="num">{status.active_calls}</span>
        </div>
      </section>
      <section class="card">
        <header><h2><Cpu size={16} /> System</h2></header>
        <div class="body kv">
          <span>Load</span>
          <span class="num">{num(status.system.load1, 2)} · {num(status.system.load5, 2)} · {num(status.system.load15, 2)}
            <span class="muted">({status.system.cpus} CPUs)</span></span>
          <span>Temperature</span>
          <span class="num {status.system.temp_c != null && status.system.temp_c > 75 ? 'hot' : ''}">
            {status.system.temp_c != null ? `${num(status.system.temp_c, 1)} °C` : '–'}</span>
          <span>Memory</span>
          <span class="num">{mem != null ? `${Math.round(mem * 100)} % used of ${bytes(status.system.mem_total)}` : '–'}</span>
          <span>Disk (data)</span>
          <span class="num">{disk != null ? `${Math.round(disk * 100)} % used, ${bytes(status.system.disk_free)} free` : '–'}</span>
          <span>Host uptime</span><span>{uptime(status.system.uptime_seconds)}</span>
        </div>
      </section>
      <section class="card">
        <header><h2>Lines</h2><span class="muted">{status.trunks_up}/{status.trunks.length} registered</span></header>
        {#if status.trunks.length === 0}
          <div class="empty">No lines configured.</div>
        {:else}
          <ul class="lines">
            {#each status.trunks as t (t.registration)}
              <li>
                <span class="dot {t.up ? 'good' : 'bad'}"></span>
                <span class="mono">{phone(t.number)}</span>
                <span class="muted">{t.trunk}</span>
                {#if t.family}<span class="badge info" title="Address family in use">IP{t.family}</span>{/if}
                <span class="right small {t.up ? 'muted' : 'down'}">
                  {t.up ? `up ${ago(t.since).replace(" ago", "")}` : `${t.state}${t.attempts ? `, ${t.attempts} tries` : ''}`}
                </span>
              </li>
            {/each}
          </ul>
        {/if}
      </section>
    </div>

    <ActiveCalls calls={live.status?.active_calls ?? []} />

    <section class="card">
      <header><h2>Phones</h2></header>
      <div class="body flush table-wrap">
        <table>
          <thead><tr><th>Extension</th><th>Device</th><th>Address</th><th>Reachability</th></tr></thead>
          <tbody>
            {#each status.phones as p (p.username)}
              {#if p.contacts.length === 0}
                <tr>
                  <td><b>{p.extension}</b> {p.name}</td>
                  <td colspan="3"><span class="dot"></span> <span class="muted">not registered</span></td>
                </tr>
              {:else}
                {#each p.contacts as c, i (c.addr + i)}
                  <tr>
                    <td>{#if i === 0}<b>{p.extension}</b> {p.name}{/if}</td>
                    <td>{c.user_agent || '–'}</td>
                    <td class="mono">{c.addr}</td>
                    <td>
                      <span class="dot {c.status === 'Reachable' ? 'good' : c.status === 'Unreachable' ? 'bad' : ''}"></span>
                      {c.status}{#if c.rtt_ms} <span class="muted num">{num(c.rtt_ms, 1)} ms</span>{/if}
                    </td>
                  </tr>
                {/each}
              {/if}
            {/each}
          </tbody>
        </table>
      </div>
    </section>

    {#if Object.keys(status.modules ?? {}).length}
      <section class="card">
        <header><h2>Integrations</h2></header>
        <ul class="lines">
          {#each Object.entries(status.modules).sort() as [name, m] (name)}
            <li>
              <a href="/integrations?tab={moduleTab[name] ?? name}" onclick={link}>{moduleLabel[name] ?? name}</a>
              <span class="right"><ModuleState status={m} /></span>
            </li>
          {/each}
        </ul>
      </section>
    {/if}

    <section class="card">
      <header><h2>Line and engine events</h2></header>
      {#if status.events.length === 0}
        <div class="empty">No events yet.</div>
      {:else}
        <div class="body flush table-wrap">
          <table>
            <tbody>
              {#each status.events as ev, i (i)}
                <tr>
                  <td class="num nowrap">{dateTime(ev.at)}</td>
                  <td><span class="badge {eventTone(ev.type)}">{eventLabel[ev.type] ?? ev.type}</span></td>
                  <td class="mono small">{ev.subject}</td>
                  <td class="small">{detailText(ev.detail)}</td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/if}
    </section>
  {:else if !error}
    <div class="empty">Loading…</div>
  {/if}
</div>

<style>
  .page {
    display: grid;
    gap: 1rem;
  }
  h2 {
    display: flex;
    align-items: center;
    gap: 0.4rem;
  }
  .cols3 {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 1rem;
  }
  @media (max-width: 1000px) {
    .cols3 {
      grid-template-columns: 1fr;
    }
  }
  .kv {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: 0.45rem 1rem;
  }
  .kv > span:nth-child(odd) {
    color: var(--muted);
  }
  .kv > span {
    display: flex;
    align-items: center;
    gap: 0.4rem;
    flex-wrap: wrap;
  }
  .lines {
    list-style: none;
    margin: 0;
    padding: 0.5rem 1rem;
  }
  .lines li {
    display: flex;
    align-items: center;
    gap: 0.6rem;
    padding: 0.35rem 0;
  }
  .right {
    margin-left: auto;
  }
  .down {
    color: var(--bad);
  }
  .hot {
    color: var(--bad);
  }
  .small {
    font-size: 0.82rem;
  }
  .nowrap {
    white-space: nowrap;
  }
</style>
