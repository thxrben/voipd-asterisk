<script lang="ts">
  // Direction icon + status badge for a call.
  import PhoneIncoming from '@lucide/svelte/icons/phone-incoming'
  import PhoneOutgoing from '@lucide/svelte/icons/phone-outgoing'
  import ArrowLeftRight from '@lucide/svelte/icons/arrow-left-right'
  import { directionLabel, statusLabel } from '../lib/format'

  interface Props {
    direction: string
    status: string
  }
  let { direction, status }: Props = $props()

  const tone: Record<string, string> = {
    answered: 'good',
    ringing: 'info',
    missed: 'warn',
    cancelled: 'warn',
    voicemail: 'info',
    blocked: 'bad',
    busy: '',
    failed: 'bad',
  }
</script>

<span class="wrap">
  <span class="dir" title={directionLabel[direction] ?? direction}>
    {#if direction === 'inbound'}<PhoneIncoming size={15} />
    {:else if direction === 'outbound'}<PhoneOutgoing size={15} />
    {:else}<ArrowLeftRight size={15} />{/if}
    <span class="sr-only">{directionLabel[direction] ?? direction}</span>
  </span>
  <span class="badge {tone[status] ?? ''}">{statusLabel[status] ?? status}</span>
</span>

<style>
  .wrap {
    display: inline-flex;
    align-items: center;
    gap: 0.4rem;
  }
  .dir {
    color: var(--muted);
    display: inline-flex;
  }
</style>
