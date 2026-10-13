// Types mirroring voipd's JSON API (internal/web, internal/store, internal/stats).

export type Role = 'admin' | 'user'

export interface Me {
  subject: string
  username: string
  name: string
  email: string
  role: Role
  csrf_token: string
  extensions: { number: string; name: string; dnd: boolean }[]
}

export type Direction = 'inbound' | 'outbound' | 'internal'
export type CallStatus =
  | 'ringing'
  | 'answered'
  | 'missed'
  | 'cancelled'
  | 'blocked'
  | 'busy'
  | 'failed'
  | 'voicemail'

export interface CallRow {
  id: string
  direction: Direction
  status: CallStatus
  caller: string
  caller_name: string
  callee: string
  trunk_id: number
  answered_by: string
  block_reason: string
  started_at: string
  answered_at: string
  ended_at: string
  hangup_cause: string
  duration_ms: number
  ring_ms: number
}

export interface Leg {
  id: string
  kind: 'trunk' | 'extension'
  endpoint: string
  created_at: string
  answered_at: string
  ended_at: string
  hangup_cause: string
  codec: string
  transport: string
  srtp: boolean
  remote_addr: string
  rx_packets: number | null
  tx_packets: number | null
  rx_lost: number | null
  tx_lost: number | null
  jitter_ms: number | null
  rtt_ms: number | null
  mos: number | null
}

export interface CallEvent {
  at: string
  type: string
  detail: Record<string, unknown>
}

export interface CallDetail {
  call: CallRow
  legs: Leg[]
  events: CallEvent[]
}

export interface Summary {
  total: number
  inbound: number
  outbound: number
  internal: number
  answered: number
  missed: number
  blocked: number
  blocked_pct: number
  voicemail: number
  talk_seconds: number
  avg_mos: number | null
  blocklist_size: number
}

export interface Bucket {
  start: string
  answered: number
  missed: number
  blocked: number
  outbound: number
  internal: number
}

export interface TopEntry {
  number: string
  name: string
  count: number
  last: string
  reason?: string
}

export interface Dashboard {
  from: string
  to: string
  bucket_seconds: number
  summary: Summary
  series: Bucket[]
  outcomes: Record<string, number>
  top: Record<'blocked' | 'callers' | 'missed' | 'called', TopEntry[]>
}

export interface ActiveCall {
  id: string
  direction: Direction
  status: string
  caller: string
  caller_name: string
  callee: string
  trunk?: string
  ringing?: string[]
  answered_by?: string
  started_at: string
  answered_at: string
}

export interface LiveStatus {
  engine_connected: boolean
  active_calls: ActiveCall[]
  trunks_up?: number
  trunks_total?: number
}

export interface BusEvent {
  type: string
  at: string
  call_id?: string
  data?: Record<string, unknown>
}

export interface TrunkStatus {
  registration: string
  trunk: string
  number: string
  state: string
  detail: string
  up: boolean
  since: string
  attempts: number
  announced_down: boolean
}

export interface PhoneContact {
  addr: string
  user_agent: string
  status: string
  rtt_ms: number
  expires: string
}

export interface Status {
  engine: { connected: boolean; version: string; uptime_seconds: number }
  trunks: TrunkStatus[]
  trunks_up: number
  phones: { extension: string; name: string; username: string; contacts: PhoneContact[] }[]
  active_calls: number
  system: {
    load1: number | null
    load5: number | null
    load15: number | null
    cpus: number
    mem_total: number | null
    mem_available: number | null
    temp_c: number | null
    disk_total: number | null
    disk_free: number | null
    uptime_seconds: number | null
  }
  events: { at: string; type: string; subject: string; detail: Record<string, unknown> }[]
}
