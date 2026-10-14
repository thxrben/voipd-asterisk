<script lang="ts">
  // System: backups and configuration transfer.
  import { router } from '../lib/router.svelte'
  import BackupTab from '../components/system/BackupTab.svelte'

  const tabs = [{ id: 'backup', label: 'Backup' }] as const
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
