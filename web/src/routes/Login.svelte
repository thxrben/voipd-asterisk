<script lang="ts">
  import Phone from '@lucide/svelte/icons/phone'
  import { session } from '../lib/session.svelte'

  let config = $state<{ oidc: boolean; local: boolean } | null>(null)
  let username = $state('admin')
  let password = $state('')
  let error = $state('')
  let busy = $state(false)
  let showLocal = $state(false)

  $effect(() => {
    fetch('/auth/config')
      .then((r) => r.json())
      .then((c: { oidc: boolean; local: boolean }) => {
        config = c
        showLocal = c.local && !c.oidc
      })
      .catch(() => (error = 'voipd is not reachable.'))
  })

  const returnTo = encodeURIComponent(window.location.pathname + window.location.search)

  async function localLogin(e: SubmitEvent) {
    e.preventDefault()
    busy = true
    error = ''
    try {
      const res = await fetch('/auth/local', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ username, password }),
      })
      if (res.status === 204) {
        await session.load()
        return
      }
      error = ((await res.json()) as { error?: string }).error ?? 'Login failed.'
    } catch {
      error = 'voipd is not reachable.'
    } finally {
      busy = false
      password = ''
    }
  }
</script>

<main>
  <div class="card box">
    <div class="brand"><Phone size={26} /> <span>voipd</span></div>
    <p class="muted">Home telephone system</p>

    {#if config?.oidc}
      <a class="button primary wide" href="/auth/login?return={returnTo}">Sign in with authentik</a>
    {/if}

    {#if config?.local}
      {#if showLocal}
        <form onsubmit={localLogin}>
          <label>Username <input autocomplete="username" bind:value={username} required /></label>
          <label>Password <input type="password" autocomplete="current-password" bind:value={password} required /></label>
          <button class="primary wide" disabled={busy}>Sign in</button>
        </form>
      {:else}
        <button class="link" onclick={() => (showLocal = true)}>Use local administrator account</button>
      {/if}
    {/if}

    {#if config && !config.oidc && !config.local}
      <p class="error-box">
        No login method is configured. Set <code>VOIPD_OIDC_ISSUER</code> (authentik) or
        <code>VOIPD_ADMIN_PASSWORD_HASH</code>.
      </p>
    {/if}
    {#if error}<p class="error-box">{error}</p>{/if}
  </div>
</main>

<style>
  main {
    min-height: 100vh;
    display: grid;
    place-items: center;
    padding: 1rem;
  }
  .box {
    width: min(100%, 360px);
    padding: 2rem 1.75rem;
    display: grid;
    gap: 1rem;
  }
  .brand {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    font-size: 1.5rem;
    font-weight: 700;
    color: var(--accent);
  }
  p {
    margin: 0;
  }
  form {
    display: grid;
    gap: 0.75rem;
  }
  .wide {
    width: 100%;
    justify-content: center;
    padding: 0.6rem;
  }
  .link {
    border: 0;
    background: none;
    color: var(--muted);
    padding: 0;
    justify-self: center;
    font-size: 0.85rem;
  }
  .link:hover:not(:disabled) {
    background: none;
    color: var(--text);
    text-decoration: underline;
  }
</style>
