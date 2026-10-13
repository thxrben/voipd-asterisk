// Who is logged in. `state` drives the top-level view: loading, login or app.

import type { Me } from './types'

class Session {
  state = $state<'loading' | 'anonymous' | 'ready'>('loading')
  me = $state<Me | null>(null)

  get csrf(): string {
    return this.me?.csrf_token ?? ''
  }

  get isAdmin(): boolean {
    return this.me?.role === 'admin'
  }

  async load(): Promise<void> {
    const res = await fetch('/api/v1/me', { credentials: 'same-origin' })
    if (res.status === 401) {
      this.loggedOut()
      return
    }
    if (!res.ok) throw new Error(`loading session failed: ${res.status}`)
    this.me = (await res.json()) as Me
    this.state = 'ready'
  }

  loggedOut(): void {
    this.me = null
    this.state = 'anonymous'
  }

  async logout(): Promise<void> {
    const res = await fetch('/auth/logout', { method: 'POST', credentials: 'same-origin' })
    const next = res.ok ? ((await res.json()) as { next?: string }).next : '/'
    this.loggedOut()
    window.location.assign(next || '/')
  }
}

export const session = new Session()
