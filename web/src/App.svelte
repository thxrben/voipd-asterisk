<script lang="ts">
  import LayoutDashboard from '@lucide/svelte/icons/layout-dashboard'
  import List from '@lucide/svelte/icons/list'
  import Server from '@lucide/svelte/icons/server'
  import Settings from '@lucide/svelte/icons/settings'
  import ShieldBan from '@lucide/svelte/icons/shield-ban'
  import BookUser from '@lucide/svelte/icons/book-user'
  import Plug from '@lucide/svelte/icons/plug'
  import BookOpen from '@lucide/svelte/icons/book-open'
  import VoicemailIcon from '@lucide/svelte/icons/voicemail'
  import Hash from '@lucide/svelte/icons/hash'
  import LogOut from '@lucide/svelte/icons/log-out'
  import Sun from '@lucide/svelte/icons/sun'
  import Moon from '@lucide/svelte/icons/moon'
  import Menu from '@lucide/svelte/icons/menu'
  import Phone from '@lucide/svelte/icons/phone'
  import Wrench from '@lucide/svelte/icons/wrench'
  import type { Component } from 'svelte'
  import { session } from './lib/session.svelte'
  import { live } from './lib/live.svelte'
  import { link, router } from './lib/router.svelte'
  import Login from './routes/Login.svelte'
  import Dashboard from './routes/Dashboard.svelte'
  import Calls from './routes/Calls.svelte'
  import CallDetail from './routes/CallDetail.svelte'
  import Status from './routes/Status.svelte'
  import Telephony from './routes/Telephony.svelte'
  import Blocking from './routes/Blocking.svelte'
  import Contacts from './routes/Contacts.svelte'
  import Integrations from './routes/Integrations.svelte'
  import Voicemail from './routes/Voicemail.svelte'
  import Codes from './routes/Codes.svelte'
  import Directory from './routes/Directory.svelte'
  import System from './routes/System.svelte'
  import Toasts from './components/Toasts.svelte'

  let loadError = $state('')
  session.load().catch((e: Error) => (loadError = e.message))

  $effect(() => {
    if (session.state === 'ready') live.start()
    else live.stop()
  })

  interface NavItem {
    href: string
    label: string
    icon: Component
    admin?: boolean
  }
  const nav: NavItem[] = [
    { href: '/', label: 'Dashboard', icon: LayoutDashboard, admin: true },
    { href: '/calls', label: 'Calls', icon: List },
    { href: '/voicemail', label: 'Voicemail', icon: VoicemailIcon },
    { href: '/blocking', label: 'Blocking', icon: ShieldBan, admin: true },
    { href: '/contacts', label: 'Contacts', icon: BookUser, admin: true },
    { href: '/telephony', label: 'Telephony', icon: Settings, admin: true },
    { href: '/integrations', label: 'Integrations', icon: Plug, admin: true },
    { href: '/directory', label: 'Directory', icon: BookOpen, admin: true },
    { href: '/system', label: 'System', icon: Wrench, admin: true },
    { href: '/status', label: 'Status', icon: Server, admin: true },
    { href: '/codes', label: 'Phone codes', icon: Hash },
  ]
  const items = $derived(nav.filter((n) => !n.admin || session.isAdmin))

  function active(href: string): boolean {
    return href === '/' ? router.path === '/' : router.path.startsWith(href)
  }

  // Users without admin rights start on their own call list.
  $effect(() => {
    const adminPaths = ['/', '/status', '/telephony', '/blocking', '/contacts', '/integrations', '/directory', '/system']
    if (session.state === 'ready' && !session.isAdmin && adminPaths.includes(router.path)) {
      router.go('/calls', true)
    }
  })

  const callId = $derived(router.path.match(/^\/calls\/([^/]+)$/)?.[1])

  // Theme: system by default; the toggle stores an explicit choice.
  let theme = $state(localStorage.getItem('theme') ?? 'system')
  $effect(() => {
    if (theme === 'system') document.documentElement.removeAttribute('data-theme')
    else document.documentElement.setAttribute('data-theme', theme)
  })
  function toggleTheme() {
    const dark =
      theme === 'dark' || (theme === 'system' && window.matchMedia('(prefers-color-scheme: dark)').matches)
    theme = dark ? 'light' : 'dark'
    try {
      localStorage.setItem('theme', theme)
    } catch {
      /* storage unavailable: keep for this page only */
    }
  }

  let navOpen = $state(false)
  $effect(() => {
    void router.path
    navOpen = false
  })
</script>

<Toasts />

{#if session.state === 'loading'}
  <div class="center muted">{loadError || 'Loading…'}</div>
{:else if session.state === 'anonymous'}
  <Login />
{:else}
  <div class="layout" class:open={navOpen}>
    <aside>
      <a class="brand" href={session.isAdmin ? '/' : '/calls'} onclick={link}><Phone size={20} /> voipd</a>
      <nav>
        {#each items as n (n.href)}
          <a href={n.href} onclick={link} class:active={active(n.href)} aria-current={active(n.href) ? 'page' : undefined}>
            <n.icon size={18} />
            {n.label}
          </a>
        {/each}
      </nav>
      <div class="side-foot">
        <span class="dot {live.connected ? 'good' : 'bad'}"></span>
        {live.connected ? 'Live' : 'Reconnecting…'}
      </div>
    </aside>
    <div class="main">
      <header class="top">
        <button class="icon menu" onclick={() => (navOpen = !navOpen)} aria-label="Menu"><Menu size={18} /></button>
        <div class="spacer"></div>
        <button class="icon" onclick={toggleTheme} aria-label="Toggle dark mode">
          <Sun size={17} class="sun" /><Moon size={17} class="moon" />
        </button>
        <span class="user" title={session.me?.email}>
          {session.me?.name || session.me?.username}
          <span class="badge">{session.me?.role}</span>
        </span>
        <button onclick={() => session.logout()}><LogOut size={16} /> Sign out</button>
      </header>
      <main>
        {#if callId}
          <CallDetail id={decodeURIComponent(callId)} />
        {:else if router.path === '/calls'}
          <Calls />
        {:else if router.path === '/voicemail'}
          <Voicemail />
        {:else if router.path === '/codes'}
          <Codes />
        {:else if router.path === '/status' && session.isAdmin}
          <Status />
        {:else if router.path === '/blocking' && session.isAdmin}
          <Blocking />
        {:else if router.path === '/contacts' && session.isAdmin}
          <Contacts />
        {:else if router.path === '/telephony' && session.isAdmin}
          <Telephony />
        {:else if router.path === '/integrations' && session.isAdmin}
          <Integrations />
        {:else if router.path === '/directory' && session.isAdmin}
          <Directory />
        {:else if router.path === '/system' && session.isAdmin}
          <System />
        {:else if router.path === '/' && session.isAdmin}
          <Dashboard />
        {:else}
          <div class="empty">Page not found. <a href="/" onclick={link}>Go to the start page</a></div>
        {/if}
      </main>
    </div>
  </div>
{/if}

<style>
  .center {
    min-height: 100vh;
    display: grid;
    place-items: center;
  }
  .layout {
    display: grid;
    grid-template-columns: 220px minmax(0, 1fr);
    min-height: 100vh;
  }
  aside {
    background: var(--sidebar);
    color: var(--sidebar-text);
    display: flex;
    flex-direction: column;
    position: sticky;
    top: 0;
    height: 100vh;
  }
  .brand {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    padding: 1rem 1.1rem;
    font-size: 1.2rem;
    font-weight: 700;
    color: #fff;
    border-bottom: 1px solid rgb(255 255 255 / 8%);
  }
  .brand:hover {
    text-decoration: none;
  }
  nav {
    display: grid;
    padding: 0.6rem 0;
  }
  nav a {
    display: flex;
    align-items: center;
    gap: 0.65rem;
    padding: 0.6rem 1.1rem;
    color: var(--sidebar-text);
    border-left: 3px solid transparent;
  }
  nav a:hover {
    background: var(--sidebar-2);
    text-decoration: none;
    color: #fff;
  }
  nav a.active {
    background: var(--sidebar-2);
    color: var(--sidebar-active);
    border-left-color: var(--accent);
  }
  .side-foot {
    margin-top: auto;
    padding: 0.9rem 1.1rem;
    font-size: 0.82rem;
    display: flex;
    align-items: center;
    gap: 0.45rem;
    border-top: 1px solid rgb(255 255 255 / 8%);
  }
  .main {
    min-width: 0;
    display: flex;
    flex-direction: column;
  }
  .top {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    padding: 0.6rem 1.25rem;
    background: var(--panel);
    border-bottom: 1px solid var(--border);
    position: sticky;
    top: 0;
    z-index: 5;
  }
  .spacer {
    flex: 1;
  }
  .user {
    display: inline-flex;
    align-items: center;
    gap: 0.4rem;
    white-space: nowrap;
  }
  button.icon {
    padding: 0.4rem;
  }
  .menu {
    display: none;
  }
  :global(:root .moon) {
    display: none;
  }
  :global(:root[data-theme='dark'] .sun) {
    display: none;
  }
  :global(:root[data-theme='dark'] .moon) {
    display: inline;
  }
  @media (prefers-color-scheme: dark) {
    :global(:root:not([data-theme='light']) .sun) {
      display: none;
    }
    :global(:root:not([data-theme='light']) .moon) {
      display: inline;
    }
  }
  main {
    padding: 1.25rem;
    max-width: 1600px;
    width: 100%;
  }
  @media (max-width: 800px) {
    .layout {
      grid-template-columns: 1fr;
    }
    aside {
      position: fixed;
      inset: 0 auto 0 0;
      width: 240px;
      z-index: 20;
      transform: translateX(-100%);
      transition: transform 0.2s;
    }
    .layout.open aside {
      transform: none;
      box-shadow: 0 0 0 100vmax rgb(0 0 0 / 35%);
    }
    .menu {
      display: inline-flex;
    }
    .user {
      display: none;
    }
    main {
      padding: 1rem;
    }
  }
</style>
