// Live connection to /api/v1/events (Server-Sent Events). Keeps the latest
// status snapshot and a short feed of recent events; components can
// subscribe to event types (e.g. refresh the dashboard when a call ends).

import type { BusEvent, LiveStatus } from './types'

type Listener = (ev: BusEvent) => void

class Live {
  connected = $state(false)
  status = $state<LiveStatus | null>(null)
  recent = $state<BusEvent[]>([])

  private es: EventSource | null = null
  private listeners = new Set<{ prefix: string; fn: Listener }>()
  private retry = 1000

  start(): void {
    if (this.es) return
    const es = new EventSource('/api/v1/events')
    this.es = es
    es.addEventListener('open', () => {
      this.connected = true
      this.retry = 1000
    })
    es.addEventListener('status', (m) => {
      this.status = JSON.parse((m as MessageEvent).data) as LiveStatus
    })
    es.addEventListener('bus', (m) => {
      const ev = JSON.parse((m as MessageEvent).data) as BusEvent
      this.recent = [ev, ...this.recent].slice(0, 50)
      for (const l of this.listeners) if (ev.type.startsWith(l.prefix)) l.fn(ev)
    })
    es.addEventListener('error', () => {
      this.connected = false
      // EventSource retries on its own, except after fatal errors (e.g. 401
      // when the session expired): then reconnect with backoff ourselves.
      if (es.readyState === EventSource.CLOSED) {
        this.es = null
        setTimeout(() => this.start(), this.retry)
        this.retry = Math.min(this.retry * 2, 30000)
      }
    })
  }

  stop(): void {
    this.es?.close()
    this.es = null
    this.connected = false
  }

  /** Calls fn for events whose type starts with prefix; returns unsubscribe. */
  on(prefix: string, fn: Listener): () => void {
    const l = { prefix, fn }
    this.listeners.add(l)
    return () => this.listeners.delete(l)
  }
}

export const live = new Live()
