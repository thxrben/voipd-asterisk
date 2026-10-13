<script lang="ts">
  // Phone codes: everything that can be dialed on a phone besides numbers.
  import { api } from '../lib/api'
  import type { PhoneCode } from '../lib/types'

  let codes = $state<PhoneCode[]>([])
  let error = $state('')
  $effect(() => {
    api<{ codes: PhoneCode[] }>('/codes')
      .then((r) => (codes = r.codes))
      .catch((e: Error) => (error = e.message))
  })
</script>

<div class="page">
  <h1>Phone codes</h1>
  {#if error}<div class="error-box">{error}</div>{/if}

  <section class="card">
    <header><h2>Codes</h2></header>
    <div class="body flush table-wrap">
      <table>
        <thead><tr><th>Dial</th><th>Function</th><th>How it works</th></tr></thead>
        <tbody>
          {#each codes as c (c.code)}
            <tr>
              <td class="mono code">{c.code}</td>
              <td><b>{c.name}</b></td>
              <td>{c.description}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  </section>

  <section class="card">
    <header><h2>Dialing</h2></header>
    <div class="body">
      <table class="plain">
        <tbody>
          <tr><td class="mono code">10, 11, …</td><td>Call another phone in the house by its extension number.</td></tr>
          <tr><td class="mono code">0201 1234567</td><td>Call out with the area code …</td></tr>
          <tr><td class="mono code">1234567</td><td>… or without it for local numbers.</td></tr>
          <tr><td class="mono code">+49 …, 0049 …</td><td>International format works too.</td></tr>
          <tr><td class="mono code">110, 112</td><td>Emergency and other short numbers are dialed as they are.</td></tr>
        </tbody>
      </table>
    </div>
  </section>
</div>

<style>
  .page {
    display: grid;
    gap: 1rem;
  }
  h1 {
    margin: 0;
  }
  .code {
    white-space: nowrap;
    font-size: 1.05rem;
  }
  .plain td {
    padding: 0.35rem 1rem 0.35rem 0;
    vertical-align: top;
  }
</style>
