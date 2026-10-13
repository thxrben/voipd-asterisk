<script lang="ts">
  // "What happens if this number calls?"
  import Search from '@lucide/svelte/icons/search'
  import { api } from '../../lib/api'
  import type { CheckResult } from '../../lib/types'

  let number = $state('')
  let result = $state<CheckResult | null>(null)
  let error = $state('')

  async function check(e: SubmitEvent) {
    e.preventDefault()
    if (!number.trim()) return
    try {
      result = await api<CheckResult>(`/screening/check?number=${encodeURIComponent(number)}`)
      error = ''
    } catch (err) {
      error = (err as Error).message
    }
  }

  const reasons: Record<string, string> = {
    allowlist: 'on the allowlist',
    contact: 'a contact',
    paused: 'blocking is paused',
    anonymous: 'anonymous callers are blocked',
    blocklist: 'on a blocklist',
  }
</script>

<form class="check" onsubmit={check}>
  <label for="check-number" class="muted">Test a number</label>
  <input id="check-number" bind:value={number} placeholder="e.g. 0170 1234567" inputmode="tel" />
  <button type="submit" aria-label="Test number"><Search size={16} /> Test</button>
  {#if result}
    {@const d = result.decision}
    <p class="result" role="status">
      {#if result.kind === 'invalid'}
        <span class="badge warn">not a valid number</span>
      {:else}
        <span class="badge {d.block ? 'bad' : 'good'}">{d.block ? 'Blocked' : 'Rings'}</span>
        <span>
          <b>{result.display}</b>{d.name ? ` (${d.name})` : ''}
          – {d.reason ? reasons[d.reason] : 'not on any list'}{d.source ? ' ' : ''}{#if d.source}<span class="muted">({d.source}{d.pattern?.endsWith('*') ? `, range ${d.pattern}` : ''})</span>{/if}
        </span>
      {/if}
    </p>
  {/if}
  {#if error}<p class="result error-box">{error}</p>{/if}
</form>

<style>
  .check {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.5rem;
  }
  input {
    width: 16rem;
    max-width: 100%;
  }
  .result {
    flex-basis: 100%;
    margin: 0.2rem 0 0;
    display: flex;
    gap: 0.5rem;
    align-items: baseline;
    flex-wrap: wrap;
  }
</style>
