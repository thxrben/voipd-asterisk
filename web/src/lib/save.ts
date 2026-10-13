// Helpers for forms that save through the API: field errors from 422
// responses and the "saved but not applied" warning.

import { ApiError } from './api'
import { toast } from './toast.svelte'

export interface SaveState {
  busy: boolean
  error: string
  fields: Record<string, string>
}

export function emptySave(): SaveState {
  return { busy: false, error: '', fields: {} }
}

interface Result {
  result?: unknown
  warning?: string
}

/** Runs a save call, fills state with errors, returns true on success. */
export async function runSave(state: SaveState, fn: () => Promise<Result | undefined>): Promise<boolean> {
  state.busy = true
  state.error = ''
  state.fields = {}
  try {
    const res = await fn()
    if (res?.warning) toast.show(res.warning, 'warn')
    return true
  } catch (e) {
    if (e instanceof ApiError) {
      state.error = e.message
      if (e.fields) state.fields = e.fields
    } else {
      state.error = (e as Error).message
    }
    return false
  } finally {
    state.busy = false
  }
}
