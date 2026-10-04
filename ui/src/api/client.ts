// The sms-gw management API through the gateway: the kit client bound to this
// module's base. Paths are the OpenAPI contract's (api/openapi/sms-gw.yaml),
// checked at compile time; the tenant always comes from the session token.
import { createApi, ApiError, describe, type Method } from '@go-tangra/ui/api'
import { registerReasons } from '@go-tangra/ui/forms'
import type { paths } from './schema.d'

export { ApiError, describe }
export const BASE = '/api/sms-gw/v1'
export const api = createApi({ base: BASE })
export type ApiPath = keyof paths
export type Query = Record<string, string | number | boolean | undefined>

registerReasons({
  send_rejected: 'The gateway refused to send this message.',
  carrier_failed: 'The carrier did not accept the message.',
  in_use: 'This record is still referenced by messages or blocks.',
})

/** Fills {param} placeholders of a contract path, encoding every value. */
export function resolve(path: ApiPath, params: Record<string, string | number> = {}): string {
  return path.replace(/\{([^}]+)\}/g, (_, key: string) => {
    const v = params[key]
    if (v === undefined || v === '') throw new Error('missing path parameter ' + key)
    return encodeURIComponent(String(v))
  })
}

export function call<T>(method: Method, path: ApiPath, opts: { params?: Record<string, string | number>; body?: unknown; query?: Query; signal?: AbortSignal } = {}): Promise<T> {
  return api<T>(method, resolve(path, opts.params), opts.body, { ...(opts.query ? { query: opts.query } : {}), ...(opts.signal ? { signal: opts.signal } : {}) })
}

function detailText(d: Record<string, unknown> | undefined): string {
  if (!d) return ''
  const where = typeof d.field === 'string' ? d.field : typeof d.param === 'string' ? d.param : ''
  const msg = typeof d.message === 'string' ? d.message : ''
  return [where, msg].filter(Boolean).join(': ')
}

/** User wording for a refusal; server detail is shown only where the contract sanitises it. */
export function explain(err: unknown): string {
  if (err instanceof ApiError) {
    const detail = detailText(err.detail)
    switch (err.reason) {
      case 'send_rejected':
      case 'carrier_failed':
        return describe(err) + (typeof err.detail?.message === 'string' ? ' ' + err.detail.message : '')
      case 'validation_failed':
        return 'Check the form' + (detail ? ' (' + detail + ').' : '.')
      case 'conflict':
        return 'The name or username is already taken.'
    }
    if (err.status === 401) return 'Your session has ended. Sign in again.'
    if (err.status === 403) return 'You do not have permission for this operation.'
    if (err.status === 404) return 'This record no longer exists.'
    if (err.status === 503) return 'The SMS gateway is temporarily unavailable. Try again shortly.'
  }
  return describe(err)
}

/** The form field a validation refusal names, if any (e.g. "config.sid", "fragments.body"). */
export function errorField(err: unknown): { field: string; message: string } | undefined {
  if (!(err instanceof ApiError) || err.reason !== 'validation_failed' || typeof err.detail?.field !== 'string') return undefined
  return { field: err.detail.field, message: typeof err.detail.message === 'string' ? err.detail.message : 'Invalid value' }
}
