// Thin fetch wrapper for /api/v1: JSON in/out, CSRF header on changes,
// and a global "logged out" signal on 401.

import { session } from './session.svelte'

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
    /** Field errors of a 422 response, keyed by JSON field name. */
    public fields?: Record<string, string>,
  ) {
    super(message)
  }
}

export async function api<T>(path: string, init: RequestInit & { json?: unknown } = {}): Promise<T> {
  const headers = new Headers(init.headers)
  const method = (init.method ?? 'GET').toUpperCase()
  if (method !== 'GET' && method !== 'HEAD') headers.set('X-CSRF-Token', session.csrf)
  let body = init.body
  if (init.json !== undefined) {
    headers.set('Content-Type', 'application/json')
    body = JSON.stringify(init.json)
  }
  const res = await fetch(`/api/v1${path}`, { ...init, method, headers, body, credentials: 'same-origin' })
  if (res.status === 401) {
    session.loggedOut()
    throw new ApiError(401, 'not logged in')
  }
  if (!res.ok) {
    let msg = res.statusText
    let fields: Record<string, string> | undefined
    try {
      const body = (await res.json()) as { error?: string; fields?: Record<string, string> }
      msg = body.error ?? msg
      fields = body.fields
    } catch {
      /* not JSON */
    }
    throw new ApiError(res.status, msg, fields)
  }
  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}
