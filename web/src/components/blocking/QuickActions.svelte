<script lang="ts">
  // Block / always allow a caller with one click (call details).
  import Ban from '@lucide/svelte/icons/ban'
  import Check from '@lucide/svelte/icons/check'
  import { api } from '../../lib/api'
  import { phone } from '../../lib/format'
  import { toast } from '../../lib/toast.svelte'
  import type { CheckResult } from '../../lib/types'

  interface Props {
    number: string // E.164
  }
  let { number }: Props = $props()

  let result = $state<CheckResult | null>(null)
  let busy = $state(false)

  async function refresh() {
    try {
      result = await api<CheckResult>(`/screening/check?number=${encodeURIComponent(number)}`)
    } catch {
      result = null
    }
  }
  $effect(() => {
    void number
    void refresh()
  })

  async function add(target: 'block' | 'allow') {
    busy = true
    try {
      await api('/lists/entries', { method: 'POST', json: { target, number, label: '' } })
      toast.show(`${phone(number)} ${target === 'block' ? 'will be blocked' : 'will always ring'}`)
      await refresh()
    } catch (e) {
      toast.show((e as Error).message, 'bad')
    } finally {
      busy = false
    }
  }
</script>

{#if result}
  {@const d = result.decision}
  <div class="qa">
    {#if d.block}
      <span class="badge bad">Blocked{d.source ? ` (${d.source})` : ''}</span>
    {:else if d.reason === 'allowlist' || d.reason === 'contact'}
      <span class="badge good">{d.reason === 'contact' ? 'Contact' : 'Allowed'}</span>
    {/if}
    {#if !d.block && d.reason !== 'allowlist'}
      <button disabled={busy} onclick={() => add('block')}><Ban size={15} /> Block number</button>
    {/if}
    {#if d.reason !== 'allowlist'}
      <button disabled={busy} onclick={() => add('allow')}><Check size={15} /> Always allow</button>
    {/if}
  </div>
{/if}

<style>
  .qa {
    display: flex;
    gap: 0.4rem;
    align-items: center;
    flex-wrap: wrap;
  }
</style>
