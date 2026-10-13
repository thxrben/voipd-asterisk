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
  voicemail_new: number
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
  modules: Record<string, ModuleStatus>
}

// --- telephony configuration ---

export interface TrunkNumber {
  id: number
  trunk_id: number
  number: string
  sip_user: string
  register: boolean
  label: string
  ring_group_id: number
}

export interface Trunk {
  id: number
  name: string
  enabled: boolean
  profile: 'generic' | 'telekom'
  registrar: string
  outbound_proxy: string
  transport: 'udp' | 'tcp' | 'tls'
  address_family: 'auto' | 'v4' | 'v6'
  auth_username: string
  password?: string
  password_set: boolean
  srtp: boolean
  dial_format: 'e164' | 'national'
  codecs: string[]
  reg_expiration: number
  numbers: TrunkNumber[]
}

export interface Extension {
  id: number
  number: string
  name: string
  enabled: boolean
  sip_username: string
  password?: string
  transport: 'udp' | 'tcp' | 'tls'
  srtp: boolean
  codecs: string[]
  max_contacts: number
  outbound_number_id: number
  clir: boolean
  dnd: boolean
  owner_subject: string
  online: number
}

export interface RingGroup {
  id: number
  name: string
  ring_timeout: number
  fallback: 'hangup' | 'keep_ringing' | 'voicemail'
  fallback_extension_id: number
  members: number[]
}

export interface User {
  subject: string
  username: string
  name: string
  email: string
  role: Role
}

export interface Telephony {
  trunks: Trunk[]
  extensions: Extension[]
  ring_groups: RingGroup[]
  users: User[]
  codecs: string[]
}

export interface Credentials {
  username: string
  password: string
  auth_username: string
  transport: string
  port: number
}

// --- screening ---

export type ListTarget = 'block' | 'allow' | 'contacts'

export interface ScreeningSettings {
  anonymous_block: boolean
  contacts_allowed: boolean
  paused_until: string | null
}

export interface ListSource {
  id: number
  name: string
  kind: 'manual' | 'file' | 'url' | 'carddav' | 'phoneblock'
  target: ListTarget
  url: string
  username: string
  password?: string
  password_set: boolean
  interval_min: number
  enabled: boolean
  last_sync_at: string | null
  last_status: '' | 'ok' | 'failed'
  last_error: string
  failures: number
  /** PhoneBlock: blocklist version of the last sync ('' = next sync downloads the full list). */
  cursor: string
  /** PhoneBlock: minimum votes, 0 = the PhoneBlock account's setting. */
  min_votes: number
  entries: number
  cards: number
}

/** Sources that are downloaded periodically. */
export const syncedKinds: ListSource['kind'][] = ['url', 'carddav', 'phoneblock']

export interface Screening {
  settings: ScreeningSettings
  sizes: Record<ListTarget, number>
  sources: ListSource[]
}

export interface ListEntry {
  id: number
  source_id: number
  source_name: string
  target: ListTarget
  pattern: string
  label: string
  card_name: string
  hits: number
  last_hit_at: string | null
  created_at: string
}

export interface Decision {
  block: boolean
  reason: '' | 'allowlist' | 'contact' | 'paused' | 'anonymous' | 'blocklist'
  name?: string
  source?: string
  entry_id?: number
  pattern?: string
}

export interface CheckResult {
  number: string
  kind: string
  display: string
  decision: Decision
}

export interface ImportPreview {
  format: 'text' | 'vcard'
  valid: number
  cards: number
  already_listed: number
  duplicates: number
  invalid_count: number
  invalid: { line?: number; value: string; reason: string }[]
  sample: { number: string; label: string }[]
  added?: number
  source_id?: number
}

export interface ContactNumber {
  number: string
  label: string
}

export interface Contact {
  id: number
  source_id: number
  source_name?: string
  name: string
  org: string
  numbers: ContactNumber[]
}

// --- integrations ---

export type ModuleState = 'disabled' | 'starting' | 'ok' | 'error'

export interface ModuleStatus {
  state: ModuleState
  detail: string
  since: string
}

export interface ModuleView<C = Record<string, unknown>> {
  name: string
  enabled: boolean
  config: C
  status: ModuleStatus
}

export interface NotifyConfig {
  host: string
  port: number
  security: 'none' | 'starttls' | 'tls'
  username: string
  password?: string
  password_set?: boolean
  from: string
  missed_calls: boolean
  missed_to: string[]
  blocked_calls: boolean
  trunk_alerts: boolean
  admin_to: string[]
  voicemail: boolean
  voicemail_attach: boolean
}

export interface WebhookEndpoint {
  id: number
  name: string
  url: string
  events: string[]
  enabled: boolean
}

export interface WebhookDelivery {
  id: number
  delivery: string
  event_type: string
  at: string
  attempt: number
  status_code: number
  latency_ms: number
  error: string
  response: string
}

export interface MqttConfig {
  url: string
  username: string
  password?: string
  password_set?: boolean
  client_id: string
  base_topic: string
  insecure: boolean
  commands: boolean
  discovery: boolean
  discovery_prefix: string
}

// --- voicemail ---

export interface Mailbox {
  extension_id: number
  extension: string
  name: string
  new: number
  old: number
  greeting: boolean
}

export interface VoicemailMessage {
  id: string
  extension_id: number
  extension: string
  call_id: string
  caller: string
  caller_name: string
  received_at: string
  duration_ms: number
  heard: boolean
  heard_at: string | null
}

export interface PhoneCode {
  code: string
  name: string
  description: string
}
