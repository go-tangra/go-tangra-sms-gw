import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { useConfirm } from '@go-tangra/ui'
import { ApiError } from '@go-tangra/ui/api'
import { call, explain, resolve } from '@/api/client'
import { buildConfig, initialConfig, validateConfig } from '@/components/providerForm'
import { apiClientSchema, blockSchema, sendSchema, templateSchema } from '@/components/schemas'
import { bars, headline, percent, sparkPath } from '@/components/dashboard'
import { statusLabel } from '@/components/format'
import { routes } from '@/routes'
import { nav } from '@/nav'
import type { ProviderField } from '@/api/types'
import Providers from '@/pages/Providers.vue'
import ApiClients from '@/pages/ApiClients.vue'
import Templates from '@/pages/Templates.vue'
import Blocks from '@/pages/Blocks.vue'
import Messages from '@/pages/Messages.vue'
import Dashboard from '@/pages/Dashboard.vue'
import { click, drawer, page, plugins, q, roles, set, stubFetch, type Call } from './helpers'

const mountPage = (c: unknown, perms?: string[]) => mount(c as never, { global: { plugins: plugins(perms) }, attachTo: document.body })
beforeEach(() => setActivePinia(createPinia()))
afterEach(() => {
  vi.unstubAllGlobals()
  document.body.innerHTML = ''
})

const fields: ProviderField[] = [
  { key: 'url', label: 'Endpoint URL', type: 'url', required: true, default: 'https://carrier.test/send' },
  { key: 'sid', label: 'SID', type: 'int', required: true },
  { key: 'encoding', label: 'Encoding', type: 'select', required: true, default: 'utf-8', options: ['utf-8', 'gsm-03-38'] },
  { key: 'callback_url', label: 'DLR Callback URL', type: 'url', required: true },
  { key: 'token', label: 'API Token', type: 'secret', required: false },
  { key: 'dlr_token', label: 'DLR Token', type: 'secret', required: false },
]
const providerType = { type: 'voicecom', label: 'Voicecom', fields }
const provider = { id: 7, name: 'main', type: 'voicecom', channel: 'sms' as const, enabled: true, retention_days: 0, config: { url: 'https://carrier.test/send', sid: '9999', encoding: 'utf-8', callback_url: 'https://gw.test/dlr?dlr_token=__set__', token: '__set__', dlr_token: '__set__' }, created_at: '2026-10-01T00:00:00Z', updated_at: '2026-10-01T00:00:00Z' }
const template = { id: 3, name: 'otp', channel: 'sms' as const, enabled: true, fragments: { body: 'Code {{.code}}', subject: 'legacy' }, variables: ['code'], created_at: '2026-10-01T00:00:00Z', updated_at: '2026-10-01T00:00:00Z' }
const client = { id: 4, username: 'shop_api', email: 'ops@shop.test', authority: 'API_CLIENT' as const, enabled: true, callback_url: 'https://shop.test/cb', callback_secret_set: true, created_at: '2026-10-01T00:00:00Z', updated_at: '2026-10-01T00:00:00Z' }
const message = { id: '0b0e9f5e-1c5e-4c1e-9b1e-3f1a2b3c4d5e', recipient: '359888123456', provider_id: 7, provider_name: 'main', template_id: 3, sid: 9999, priority: 2, text: 'Code 1234', status_code: 1, status_message: 'delivered', actor: { kind: 'api_client' as const, api_client_id: 4, api_client_username: 'shop_api' }, dlr_ts: 0, created_at: '2026-10-01T00:00:00Z', updated_at: '2026-10-01T00:00:00Z' }
const preview = { text: 'Code 1234', characters: 9, parts: 1, limit: 160, encoding: 'utf-8', variables: ['code'], missing: [] }

/** Every request stays on the module API and carries no tenant: the session token decides it. */
function expectTenantSafe(calls: Call[]): void {
  expect(calls.length).toBeGreaterThan(0)
  for (const c of calls) {
    expect(c.url.startsWith('/api/sms-gw/v1/')).toBe(true)
    expect(c.url).not.toMatch(/tenant/i)
    expect(Object.keys(c.headers).filter((h) => /tenant|x-md-|x-user|x-roles|x-authority/i.test(h))).toEqual([])
    expect(JSON.stringify(c.body ?? {})).not.toMatch(/tenant/i)
  }
}

describe('federation exports', () => {
  it('exposes one permission-gated route per manifest nav entry and no dynamic nav', () => {
    expect(routes.map((r) => r.path)).toEqual(['/sms-gw/providers', '/sms-gw/templates', '/sms-gw/api-clients', '/sms-gw/blocks', '/sms-gw/messages', '/sms-gw/dashboard'])
    expect(routes.map((r) => r.meta?.requires)).toEqual(['providers:read', 'templates:read', 'clients:read', 'blocks:read', 'messages:read', 'dashboard:read'])
    expect(new Set(routes.map((r) => r.name)).size).toBe(routes.length)
    expect(nav()).toEqual([])
  })
  it('shows the dashboard only with dashboard:read: never to a viewer, alone to monitoring', () => {
    const visible = (r: string) => routes.filter((x) => roles[r]!.includes(x.meta?.requires as string)).map((x) => x.name)
    expect(visible('administrator')).toContain('sms-gw-dashboard')
    expect(visible('viewer')).not.toContain('sms-gw-dashboard')
    expect(visible('sender')).not.toContain('sms-gw-dashboard')
    expect(visible('monitoring')).toEqual(['sms-gw-dashboard'])
  })
})

describe('api client', () => {
  it('encodes path parameters, uses the portal cookie and refuses a missing parameter', async () => {
    const calls = stubFetch(() => ({ status: 200, body: {} }))
    await call('GET', '/api/sms-gw/v1/messages/{message_id}', { params: { message_id: 'a/../b' } })
    expect(calls[0]?.url).toBe('/api/sms-gw/v1/messages/a%2F..%2Fb')
    expect(() => resolve('/api/sms-gw/v1/providers/{provider_id}')).toThrow()
  })
  it('explains refusals with sanitised detail only where the contract defines it', () => {
    expect(explain(new ApiError(400, 'send_rejected', { message: 'recipient is blocked' }))).toContain('recipient is blocked')
    expect(explain(new ApiError(502, 'carrier_failed', { message: 'carrier answered 1003' }))).toContain('carrier answered 1003')
    expect(explain(new ApiError(400, 'validation_failed', { field: 'config.sid', message: 'not a number' }))).toBe('Check the form (config.sid: not a number).')
    expect(explain(new ApiError(409, 'in_use'))).toMatch(/referenced/)
    expect(explain(new ApiError(503, 'temporarily_unavailable'))).toMatch(/unavailable/)
    expect(explain(new ApiError(403, 'forbidden', { secret: 'x' }))).not.toContain('x')
  })
})

describe('form rules', () => {
  it('provider config: defaults, checks and write-only credentials', () => {
    const fresh = initialConfig(fields)
    expect(fresh).toMatchObject({ url: 'https://carrier.test/send', encoding: 'utf-8', sid: '', token: '' })
    expect(validateConfig(fields, fresh)).toEqual({ sid: 'Required', callback_url: 'Required' })
    expect(validateConfig(fields, { ...fresh, sid: '9x', callback_url: 'ftp://x' })).toEqual({ sid: 'Enter a whole number', callback_url: 'Enter an http(s) URL' })
    // A shown URL embedding the marker is valid and goes back unchanged.
    const stored = initialConfig(fields, provider.config)
    expect(validateConfig(fields, stored)).toEqual({})
    // Create: blank credentials are left out (the server generates dlr_token).
    expect(buildConfig(fields, { ...fresh, sid: '1', callback_url: 'https://x.test' }, true)).toEqual({ url: 'https://carrier.test/send', sid: '1', encoding: 'utf-8', callback_url: 'https://x.test' })
    // Update: the marker keeps, "" clears.
    expect(buildConfig(fields, { ...stored, token: '' }, false)).toMatchObject({ token: '', dlr_token: '__set__', callback_url: 'https://gw.test/dlr?dlr_token=__set__' })
  })
  it('schemas mirror the contract', () => {
    expect(apiClientSchema.safeParse({ username: 'abc' }).success).toBe(false)
    expect(apiClientSchema.safeParse({ username: 'shop_api', password: 'short' }).success).toBe(false)
    expect(apiClientSchema.parse({ username: 'shop_api' })).toMatchObject({ password: '', authority: 'API_CLIENT' })
    expect(apiClientSchema.safeParse({ username: 'shop_api', callback_url: 'javascript:alert(1)' }).success).toBe(false)
    expect(templateSchema.safeParse({ name: 'x', body: '' }).success).toBe(false)
    expect(blockSchema.safeParse({ recipient: '+359' }).success).toBe(false)
    expect(sendSchema.safeParse({ provider_id: '7', template_id: '3', to: '3598881234567890' }).success).toBe(false)
    expect(sendSchema.parse({ provider_id: '7', template_id: '3', to: '359888123456' })).toMatchObject({ concatenate: null, encoding: '' })
  })
  it('dashboard helpers keep absent values absent', () => {
    expect(headline({ success_rate: [{ labels: {}, has_value: false }] }, 'success_rate')).toBeNull()
    expect(percent(headline({ success_rate: [{ labels: {}, value: 0.987, has_value: true }] }, 'success_rate'))).toBe('98.7%')
    expect(bars([{ labels: { outcome: 'ok' }, value: 3.2, has_value: true }, { labels: { outcome: 'failed' }, value: 9, has_value: true }]).map((b) => b.label)).toEqual(['failed', 'ok'])
    expect(sparkPath({ labels: {}, timestamps: [1, 2, 3], values: [1, null, 2] }, 100, 10, 2)).toBe('M0.0 5.0M100.0 0.0')
    expect(statusLabel(1004)).toBe('Carrier error 1004')
  })
})

describe('roles', () => {
  const handler = (url: string): { status: number; body?: unknown } => {
    if (url.includes('/provider-types')) return { status: 200, body: { items: [providerType] } }
    if (url.includes('/dashboard/')) return { status: 200, body: { available: false, reason: 'not_configured', window: '1h', results: {} } }
    return { status: 200, body: page([]) }
  }
  const cases: Array<[string, unknown, string, string]> = [
    ['providers', Providers, '[data-test="provider-new"]', 'administrator'],
    ['templates', Templates, '[data-test="template-new"]', 'administrator'],
    ['api clients', ApiClients, '[data-test="client-new"]', 'administrator'],
    ['blocks', Blocks, '[data-test="block-new"]', 'administrator'],
    ['messages', Messages, '[data-test="message-send"]', 'sender'],
  ]
  for (const [name, view, action, role] of cases) {
    it(`${name}: the write action follows the role`, async () => {
      const calls = stubFetch(handler)
      for (const r of ['administrator', 'sender', 'viewer']) {
        const w = mountPage(view, roles[r])
        await flushPromises()
        const allowed = r === 'administrator' || r === role
        expect(!!q(action), `${r} ${action}`).toBe(allowed)
        w.unmount()
      }
      expectTenantSafe(calls)
    })
  }
  it('a viewer opens a provider read-only and sees no credential values', async () => {
    stubFetch((url) => (url.includes('/provider-types') ? { status: 200, body: { items: [providerType] } } : { status: 200, body: page([provider]) }))
    const w = mountPage(Providers, roles.viewer)
    await flushPromises()
    click('[data-test="provider-7"]')
    await flushPromises()
    expect(q('[data-test="provider-save"]')).toBeNull()
    expect(q('[data-test="provider-delete"]')).toBeNull()
    const token = q<HTMLInputElement>('[data-test="provider-config-token"] input')!
    expect(token.type).toBe('password')
    expect(token.disabled).toBe(true)
    expect(token.value).toBe('__set__')
    w.unmount()
  })
})

describe('providers', () => {
  it('keeps stored credentials on save, clears a blanked one, and shows a generated receipt token once', async () => {
    const created = { ...provider, id: 8, generated_secrets: { dlr_token: 'f00dfeedf00dfeedf00dfeedf00dfeed' } }
    const calls = stubFetch((url, method) => {
      if (url.includes('/provider-types')) return { status: 200, body: { items: [providerType] } }
      if (method === 'PATCH') return { status: 200, body: provider }
      if (method === 'POST') return { status: 201, body: created }
      return { status: 200, body: page([provider]) }
    })
    const w = mountPage(Providers)
    await flushPromises()
    // Keyboard: Enter on a focused row opens it.
    q('[data-test="provider-7"]')!.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true }))
    await flushPromises()
    expect(drawer()).not.toBeNull()
    set('[data-test="provider-config-token"] input', '')
    click('[data-test="provider-save"]')
    await flushPromises()
    const patch = calls.find((c) => c.method === 'PATCH')!
    expect(patch.url).toBe('/api/sms-gw/v1/providers/7')
    expect((patch.body as { config: Record<string, string> }).config).toMatchObject({ token: '', dlr_token: '__set__', callback_url: 'https://gw.test/dlr?dlr_token=__set__', sid: '9999' })

    click('[data-test="provider-new"]')
    await flushPromises()
    set('[data-test="provider-name"] input', 'backup')
    set('[data-test="provider-config-sid"] input', '1234')
    set('[data-test="provider-config-callback_url"] input', 'https://gw.test/dlr')
    click('[data-test="provider-save"]')
    await flushPromises()
    const post = calls.find((c) => c.method === 'POST')!
    expect(post.body).toMatchObject({ name: 'backup', type: 'voicecom', config: { sid: '1234', callback_url: 'https://gw.test/dlr' } })
    expect((post.body as { config: Record<string, string> }).config).not.toHaveProperty('dlr_token')
    expect(q('[data-test="one-time-secret-value"]')?.textContent).toBe('f00dfeedf00dfeedf00dfeedf00dfeed')
    click('[data-test="one-time-secret-done"]')
    await flushPromises()
    expect(document.body.innerHTML).not.toContain('f00dfeedf00dfeedf00dfeedf00dfeed')
    expectTenantSafe(calls)
    w.unmount()
  })
  it('blocks an incomplete provider client-side and maps a server field refusal', async () => {
    const calls = stubFetch((url, method) => {
      if (url.includes('/provider-types')) return { status: 200, body: { items: [providerType] } }
      if (method === 'POST') return { status: 400, body: { reason: 'validation_failed', detail: { field: 'config.sid', message: 'sid out of range' } } }
      return { status: 200, body: page([]) }
    })
    const w = mountPage(Providers)
    await flushPromises()
    click('[data-test="provider-new"]')
    await flushPromises()
    click('[data-test="provider-save"]')
    await flushPromises()
    expect(calls.some((c) => c.method === 'POST')).toBe(false)
    set('[data-test="provider-name"] input', 'x')
    set('[data-test="provider-config-sid"] input', '99999999999')
    set('[data-test="provider-config-callback_url"] input', 'https://gw.test/dlr')
    click('[data-test="provider-save"]')
    await flushPromises()
    expect(q('[data-test="provider-error"]')?.textContent).toContain('config.sid: sid out of range')
    expect(drawer()?.textContent).toContain('sid out of range')
    w.unmount()
  })
})

describe('api clients', () => {
  it('creates with a generated password shown once and keeps the callback secret by marker on update', async () => {
    const calls = stubFetch((_url, method) => {
      if (method === 'POST') return { status: 201, body: { client: { ...client, id: 5, username: 'new_api' }, password: 'Gen3rated-Pass' } }
      if (method === 'PATCH') return { status: 200, body: client }
      return { status: 200, body: page([client]) }
    })
    const w = mountPage(ApiClients)
    await flushPromises()
    click('[data-test="client-new"]')
    await flushPromises()
    set('[data-test="client-username"] input', 'new_api')
    click('[data-test="client-save"]')
    await flushPromises()
    const post = calls.find((c) => c.method === 'POST')!
    expect(post.body).toMatchObject({ username: 'new_api', authority: 'API_CLIENT' })
    expect(post.body).not.toHaveProperty('password')
    expect(q('[data-test="one-time-secret-value"]')?.textContent).toBe('Gen3rated-Pass')
    click('[data-test="one-time-secret-done"]')
    await flushPromises()
    expect(document.body.innerHTML).not.toContain('Gen3rated-Pass')

    click('[data-test="client-4"]')
    await flushPromises()
    expect(q<HTMLInputElement>('[data-test="client-username"] input')!.disabled).toBe(true)
    click('[data-test="client-save"]')
    await flushPromises()
    const patch = calls.find((c) => c.method === 'PATCH')!
    expect(patch.body).toMatchObject({ callback_secret: '__set__', authority: 'API_CLIENT' })
    expect(patch.body).not.toHaveProperty('username')
    w.unmount()
  })
  it('resets a password and never offers API_ADMIN; a legacy admin is updated without an authority', async () => {
    const legacy = { ...client, authority: 'API_ADMIN' as const, callback_secret_set: false }
    const calls = stubFetch((url, method) => {
      if (url.endsWith('/reset-password')) return { status: 200, body: { password: 'Once-Only-Pass' } }
      if (method === 'PATCH') return { status: 200, body: legacy }
      return { status: 200, body: page([legacy]) }
    })
    const w = mountPage(ApiClients)
    await flushPromises()
    click('[data-test="client-4"]')
    await flushPromises()
    expect(q('[data-test="client-legacy"]')).not.toBeNull()
    expect([...document.querySelectorAll('[data-test="client-authority"] option')].map((o) => (o as HTMLOptionElement).value)).not.toContain('API_ADMIN')
    click('[data-test="client-save"]')
    await flushPromises()
    expect(calls.find((c) => c.method === 'PATCH')!.body).not.toHaveProperty('authority')
    click('[data-test="client-4"]')
    await flushPromises()
    click('[data-test="client-reset"]')
    await flushPromises()
    click('[data-test="client-reset-confirm"]')
    await flushPromises()
    expect(calls.find((c) => c.url.endsWith('/api-clients/4/reset-password'))?.body).toEqual({})
    expect(q('[data-test="one-time-secret-value"]')?.textContent).toBe('Once-Only-Pass')
    w.unmount()
  })
})

describe('templates and blocks', () => {
  it('saves the body keeping other fragments and previews with properties and encoding', async () => {
    const calls = stubFetch((url, method) => {
      if (url.endsWith('/preview')) return { status: 200, body: { ...preview, parts: 2, missing: ['code'] } }
      if (method === 'PATCH') return { status: 200, body: template }
      return { status: 200, body: page([template]) }
    })
    const w = mountPage(Templates)
    await flushPromises()
    click('[data-test="template-3"]')
    await flushPromises()
    set('[data-test="template-prop-code"] input', '1234')
    click('[data-test="template-preview-run"]')
    await flushPromises()
    expect(calls.find((c) => c.url.endsWith('/templates/3/preview'))?.body).toEqual({ properties: { code: '1234' }, encoding: 'utf-8' })
    set('[data-test="template-prop-code"] input', '')
    click('[data-test="template-preview-run"]')
    await flushPromises()
    // A blank field is not supplied, so the service can report it missing.
    expect(calls.filter((c) => c.url.endsWith('/templates/3/preview'))[1]?.body).toEqual({ properties: {}, encoding: 'utf-8' })
    expect(q('[data-test="preview-parts"]')?.textContent).toContain('2 parts')
    expect(q('[data-test="preview-missing"]')?.textContent).toContain('code')
    set('[data-test="template-body"] textarea', 'New {{.code}}')
    click('[data-test="template-save"]')
    await flushPromises()
    expect(calls.find((c) => c.method === 'PATCH')?.body).toMatchObject({ fragments: { body: 'New {{.code}}', subject: 'legacy' } })
    w.unmount()
  })
  it('a block for every provider sends provider_id null and deletes after confirmation', async () => {
    const block = { id: 9, recipient: '359888000000', description: 'opt-out', provider_id: null, channel: 'sms' as const, enabled: true, created_by: 'op', created_at: '2026-10-01T00:00:00Z', updated_at: '2026-10-01T00:00:00Z' }
    const calls = stubFetch((url, method) => {
      if (method === 'POST') return { status: 201, body: block }
      if (method === 'DELETE') return { status: 204 }
      if (url.includes('/providers')) return { status: 200, body: page([provider]) }
      return { status: 200, body: page([block]) }
    })
    const w = mountPage(Blocks)
    await flushPromises()
    expect(document.body.textContent).toContain('All providers')
    click('[data-test="block-new"]')
    await flushPromises()
    set('[data-test="block-recipient"] input', '359888000001')
    click('[data-test="block-save"]')
    await flushPromises()
    expect(calls.find((c) => c.method === 'POST')?.body).toMatchObject({ recipient: '359888000001', provider_id: null })
    click('[data-test="block-9"]')
    await flushPromises()
    click('[data-test="block-delete"]')
    await flushPromises()
    // The drawer steps aside so the shell's confirmation is not covered, and returns on "no".
    expect(drawer()).toBeNull()
    useConfirm().answer(false)
    await flushPromises()
    expect(drawer()).not.toBeNull()
    expect(calls.some((c) => c.method === 'DELETE')).toBe(false)
    click('[data-test="block-delete"]')
    await flushPromises()
    expect(drawer()).toBeNull()
    useConfirm().answer(true)
    await flushPromises()
    expect(calls.some((c) => c.method === 'DELETE' && c.url.endsWith('/blocks/9'))).toBe(true)
    expect(drawer()).toBeNull()
    w.unmount()
  })
})

describe('messages and dashboard', () => {
  it('filters on the server, opens details with receipt history', async () => {
    const receipt = { id: 1, status: 1, status_text: 'DELIVRD', channel: 'sms', sid: 9999, recipient: '359888123456', sender: 'SHOP', timestamp: 0, parts_received: 1, created_at: '2026-10-01T00:01:00Z', updated_at: '2026-10-01T00:01:00Z' }
    const calls = stubFetch((url) => {
      if (url.includes('/dlrs')) return { status: 200, body: { items: [
        { ...receipt, id: 1, status: 8, status_text: 'sms_sent', timestamp: 1759276800, parts_received: 2 },
        { ...receipt, id: 2, timestamp: 0 },
      ], total: 2 } }
      if (url.includes('/messages/')) return { status: 200, body: { ...message, evidence: { request: 'POST /send token=__set__', response: '{"ok":1}' } } }
      return { status: 200, body: page([message]) }
    })
    const w = mountPage(Messages, roles.viewer)
    await flushPromises()
    set('[data-test="filter-recipient"] input', '3598')
    set('[data-test="filter-status"] select', '2')
    click('[data-test="filter-apply"]')
    await flushPromises()
    const last = calls.filter((c) => c.url.startsWith('/api/sms-gw/v1/messages?')).at(-1)!
    expect(last.url).toContain('recipient=3598')
    expect(last.url).toContain('status=2')
    expect(last.url).toContain('sort=created_at')
    expect(last.url).toContain('order=desc')
    set('[data-test="filter-recipient"] input', '+359')
    click('[data-test="filter-apply"]')
    await flushPromises()
    expect(calls.filter((c) => c.url.startsWith('/api/sms-gw/v1/messages?')).at(-1)).toBe(last)
    click('[data-test="message-' + message.id + '"]')
    await flushPromises()
    // The whole receipt history, oldest first, as in v3: status, text,
    // parts and the carrier's time (ingest time when the carrier sent none).
    const rows = [...document.querySelectorAll('[data-test="message-receipts"] tbody tr')].map((r) => [...r.querySelectorAll('td')].map((c) => c.textContent?.trim()))
    expect(rows).toHaveLength(2)
    expect(rows[0]!.slice(0, 4)).toEqual([new Date(1759276800 * 1000).toLocaleString(), '8 · Delivered to SMSC', 'Sms sent', '2'])
    expect(rows[1]!.slice(0, 4)).toEqual([new Date('2026-10-01T00:01:00Z').toLocaleString(), '1 · Delivered', 'DELIVRD', '1'])
    expect(q('[data-test="message-evidence-request"]')?.textContent).toContain('token=__set__')
    expectTenantSafe(calls)
    w.unmount()
  })
  it('manual send previews parts and reports a rejection with its reason', async () => {
    const calls = stubFetch((url, method) => {
      if (url.endsWith('/preview')) return { status: 200, body: preview }
      if (method === 'POST') return { status: 400, body: { reason: 'send_rejected', detail: { message: 'recipient is blocked' } } }
      if (url.includes('/providers')) return { status: 200, body: page([provider]) }
      if (url.includes('/templates')) return { status: 200, body: page([template]) }
      return { status: 200, body: page([]) }
    })
    const w = mountPage(Messages, roles.sender)
    await flushPromises()
    click('[data-test="message-send"]')
    await flushPromises()
    set('[data-test="send-provider"] select', '7')
    set('[data-test="send-template"] select', '3')
    await flushPromises()
    set('[data-test="send-to"] input', '359888123456')
    set('[data-test="send-prop-code"] input', '1234')
    await new Promise((r) => setTimeout(r, 450))
    await flushPromises()
    expect(calls.filter((c) => c.url.endsWith('/templates/3/preview')).at(-1)?.body).toEqual({ properties: { code: '1234' } })
    expect(q('[data-test="preview-parts"]')?.textContent).toContain('1 part')
    click('[data-test="send-submit"]')
    await flushPromises()
    expect(calls.find((c) => c.method === 'POST' && c.url === '/api/sms-gw/v1/messages')?.body).toEqual({ provider_id: 7, template_id: 3, to: '359888123456', properties: { code: '1234' } })
    expect(q('[data-test="send-error"]')?.textContent).toContain('recipient is blocked')
    w.unmount()
  })
  it('shows the unavailable state, then data for another window', async () => {
    let available = false
    const calls = stubFetch((url) => {
      if (!available) return { status: 200, body: { available: false, reason: 'unreachable', window: '1h', results: {} } }
      if (url.endsWith('/instant')) return { status: 200, body: { available: true, window: '24h', results: { success_rate: [{ labels: {}, value: 0.5, has_value: true }], send_by_outcome: [{ labels: { outcome: 'ok' }, value: 4, has_value: true }] } } }
      return { status: 200, body: { available: true, window: '24h', results: { send_latency_p95: [{ labels: {}, timestamps: [1, 2], values: [0.2, 0.4] }] } } }
    })
    const w = mountPage(Dashboard, roles.monitoring)
    await flushPromises()
    expect(q('[data-test="dashboard-unavailable"]')?.textContent).toContain('unreachable')
    expect(calls[0]?.body).toEqual({ window: '1h' })
    available = true
    const tab = [...document.querySelectorAll('[data-test="dashboard-window"] [role="tab"], [data-test="dashboard-window"] button')].find((b) => b.textContent?.trim() === '24h') as HTMLElement
    tab.click()
    await flushPromises()
    expect(calls.at(-1)?.body).toEqual({ window: '24h' })
    expect(q('[data-test="dashboard-data"]')?.textContent).toContain('50.0%')
    expect(q('[data-test="dashboard-data"]')?.textContent).toContain('400 ms')
    w.unmount()
  })
})
