import type { Message } from '@/api/types'

export function when(ts?: string | null): string {
  if (!ts) return ''
  const d = new Date(ts)
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleString()
}

/** Message status codes (contracts/management-api.md). */
export function statusLabel(code: number): string {
  switch (code) {
    case -1: return 'Accepted by gateway'
    case 0: return 'Accepted by carrier'
    case 1: return 'Delivered'
    case 2: return 'Failed'
    case 8: return 'Delivered to SMSC'
    case 16: return 'Rejected'
    case 500: return 'Carrier exchange failed'
  }
  return code >= 1000 ? 'Carrier error ' + code : 'Status ' + code
}

export function statusColor(code: number): 'success' | 'error' | 'info' | 'warning' {
  if (code === 1) return 'success'
  if (code === 2 || code === 16 || code === 500 || code >= 1000) return 'error'
  if (code === 0) return 'info'
  return 'warning'
}

export function actorLabel(m: Pick<Message, 'actor'>): string {
  if (m.actor.kind === 'platform') return 'Portal' + (m.actor.user_id ? ' (' + m.actor.user_id + ')' : '')
  return m.actor.api_client_username || 'API client ' + (m.actor.api_client_id ?? '')
}

export function labelText(labels: Record<string, string>): string {
  return Object.values(labels).filter(Boolean).join(' · ') || 'total'
}

/** Carrier delivery time (unix seconds) as v3 showed it; 0 falls back to the ingest time. */
export function carrierTime(r: { timestamp: number; created_at: string }): string {
  return r.timestamp > 0 ? new Date(r.timestamp * 1000).toLocaleString() : when(r.created_at)
}

/** "sms_delivered" → "Sms delivered" (v3 humanizeSlug). */
export function humanize(s: string): string {
  const t = s.replace(/[_-]+/g, ' ').trim()
  return t ? t[0]!.toUpperCase() + t.slice(1) : ''
}
