<script lang="ts">
  // Status badge of an integration module.
  import { ago } from '../../lib/format'
  import type { ModuleStatus } from '../../lib/types'

  interface Props {
    status: ModuleStatus
  }
  let { status }: Props = $props()

  const label: Record<string, string> = { disabled: 'off', starting: 'starting', ok: 'running', error: 'error' }
  const tone: Record<string, string> = { disabled: '', starting: 'warn', ok: 'good', error: 'bad' }
</script>

<span class="state" title={status.since ? `since ${ago(status.since)}` : ''}>
  <span class="badge {tone[status.state] ?? ''}">{label[status.state] ?? status.state}</span>
  {#if status.detail}<span class="muted detail">{status.detail}</span>{/if}
</span>

<style>
  .state {
    display: inline-flex;
    align-items: center;
    gap: 0.45rem;
    flex-wrap: wrap;
    min-width: 0;
  }
  .detail {
    font-size: 0.82rem;
    overflow-wrap: anywhere;
  }
</style>
