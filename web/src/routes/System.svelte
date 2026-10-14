<script lang="ts">
  // System: backups and configuration transfer, SIP access control, call
  // limits and history retention.
  import { router } from '../lib/router.svelte'
  import BackupTab from '../components/system/BackupTab.svelte'
  import SecurityTab from '../components/system/SecurityTab.svelte'
  import RetentionTab from '../components/system/RetentionTab.svelte'

  const tabs = [
    { id: 'backup', label: 'Backup' },
    { id: 'security', label: 'Security' },
    { id: 'calls', label: 'Calls & history' },
  ] as const
  const tab = $derived(tabs.find((t) => t.id === router.query.get('tab')) ?? tabs[0])
</script>

<div class="page">
  <div class="head">
    <h1>System</h1>
    <div class="segmented" role="tablist" aria-label="Section">
      {#each tabs as t (t.id)}
        <button role="tab" aria-selected={tab.id === t.id} onclick={() => router.go(`/system?tab=${t.id}`, true)}>
          {t.label}
        </button>
      {/each}
    </div>
  </div>

  {#if tab.id === 'backup'}
    <BackupTab />
  {:else if tab.id === 'security'}
    <SecurityTab />
  {:else}
    <RetentionTab />
  {/if}
</div>

<style>
  .page {
    display: grid;
    gap: 1rem;
  }
  .head {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 1rem;
    flex-wrap: wrap;
  }
</style>
