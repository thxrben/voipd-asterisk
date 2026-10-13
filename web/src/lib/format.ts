// Display helpers. Numbers are stored as E.164; German numbers are shown in
// national format (as people dial them), others in international format.

const ZERO = '0001-01-01T00:00:00Z'

/** Go's zero time.Time marshals as year 1; treat it as "no value". */
export function time(s: string | null | undefined): Date | null {
  if (!s || s === ZERO || s.startsWith('0001-01-01')) return null
  const d = new Date(s)
  return Number.isNaN(d.getTime()) ? null : d
}

export function phone(n: string | null | undefined): string {
  if (!n) return '–'
  if (n === 'anonymous') return 'Anonymous'
  if (n.startsWith('+49')) return '0' + n.slice(3)
  return n
}

const dtf = new Intl.DateTimeFormat(undefined, { dateStyle: 'short', timeStyle: 'medium' })
const tf = new Intl.DateTimeFormat(undefined, { timeStyle: 'short' })
const df = new Intl.DateTimeFormat(undefined, { weekday: 'short', day: 'numeric', month: 'short' })

export function dateTime(s: string | null | undefined): string {
  const d = time(s)
  return d ? dtf.format(d) : '–'
}

export function clock(s: string | Date | null | undefined): string {
  const d = s instanceof Date ? s : time(s)
  return d ? tf.format(d) : '–'
}

export function day(d: Date): string {
  return df.format(d)
}

/** Human time since a moment: "just now", "5 min ago", "3 h ago", "2 d ago". */
export function ago(s: string | null | undefined, now = Date.now()): string {
  const d = time(s)
  if (!d) return '–'
  const sec = Math.max(0, (now - d.getTime()) / 1000)
  if (sec < 60) return 'just now'
  if (sec < 3600) return `${Math.floor(sec / 60)} min ago`
  if (sec < 86400) return `${Math.floor(sec / 3600)} h ago`
  return `${Math.floor(sec / 86400)} d ago`
}

/** Compact duration: 45 s, 3:07, 1:02:09, 3 d 4 h. */
export function duration(seconds: number | null | undefined): string {
  if (seconds == null || !Number.isFinite(seconds)) return '–'
  const s = Math.max(0, Math.round(seconds))
  if (s < 60) return `${s} s`
  if (s >= 2 * 86400) return `${Math.floor(s / 86400)} d ${Math.floor((s % 86400) / 3600)} h`
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  const sec = String(s % 60).padStart(2, '0')
  return h > 0 ? `${h}:${String(m).padStart(2, '0')}:${sec}` : `${m}:${sec}`
}

/** Readable uptime: "27 min", "5 h 3 min", "2 d 4 h". */
export function uptime(seconds: number | null | undefined): string {
  if (seconds == null || !Number.isFinite(seconds)) return '–'
  const s = Math.max(0, Math.floor(seconds))
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  if (d > 0) return `${d} d ${h} h`
  if (h > 0) return `${h} h ${m} min`
  return m > 0 ? `${m} min` : `${s} s`
}

export function bytes(b: number | null | undefined): string {
  if (b == null) return '–'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let v = b
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(v < 10 && i > 0 ? 1 : 0)} ${units[i]}`
}

export function num(n: number | null | undefined, digits = 0): string {
  if (n == null || !Number.isFinite(n)) return '–'
  return n.toLocaleString(undefined, { maximumFractionDigits: digits, minimumFractionDigits: digits })
}

export const statusLabel: Record<string, string> = {
  ringing: 'Ringing',
  answered: 'Answered',
  missed: 'Missed',
  cancelled: 'Caller hung up',
  blocked: 'Blocked',
  busy: 'Busy',
  failed: 'Failed',
  voicemail: 'Voicemail',
}

export const directionLabel: Record<string, string> = {
  inbound: 'Incoming',
  outbound: 'Outgoing',
  internal: 'Internal',
}

/** Quality rating for a MOS value (1–4.5). */
export function mosRating(mos: number | null | undefined): { label: string; tone: string } {
  if (mos == null) return { label: '–', tone: 'muted' }
  if (mos >= 4.0) return { label: 'Good', tone: 'good' }
  if (mos >= 3.6) return { label: 'Fair', tone: 'warn' }
  return { label: 'Poor', tone: 'bad' }
}
