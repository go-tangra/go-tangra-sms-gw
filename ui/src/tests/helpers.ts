import { vi } from 'vitest'
import type { Plugin } from 'vue'
import { createRouter, createMemoryHistory } from 'vue-router'
import { abilitiesPlugin } from '@casl/vue'
import { createMongoAbility } from '@casl/ability'

export type Reply = { status: number; body?: unknown }
export type Call = { url: string; method: string; headers: Record<string, string>; body: unknown }

/** Stubs fetch with a request handler and records every call. */
export function stubFetch(handler: (url: string, method: string, body: unknown) => Reply) {
  const calls: Call[] = []
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    const method = init?.method ?? 'GET'
    const body = init?.body ? JSON.parse(String(init.body)) : undefined
    calls.push({ url, method, headers: Object.fromEntries(new Headers(init?.headers).entries()), body })
    const { status, body: out } = handler(url, method, body)
    return new Response(status === 204 ? null : JSON.stringify(out ?? {}), { status, headers: { 'Content-Type': 'application/json' } })
  })
  vi.stubGlobal('fetch', fn)
  return calls
}

// The CASL rules the gateway derives from pkg/smsgwmanifest Abilities for
// the module roles (administrator, sender, viewer).
const abilities: Array<{ action: string[]; subject: string; requires: string }> = [
  { action: ['read'], subject: 'SmsProvider', requires: 'providers:read' },
  { action: ['create', 'update', 'delete'], subject: 'SmsProvider', requires: 'providers:manage' },
  { action: ['read'], subject: 'SmsTemplate', requires: 'templates:read' },
  { action: ['create', 'update', 'delete'], subject: 'SmsTemplate', requires: 'templates:manage' },
  { action: ['read'], subject: 'SmsApiClient', requires: 'clients:read' },
  { action: ['create', 'update', 'delete'], subject: 'SmsApiClient', requires: 'clients:manage' },
  { action: ['read'], subject: 'SmsBlock', requires: 'blocks:read' },
  { action: ['create', 'update', 'delete'], subject: 'SmsBlock', requires: 'blocks:manage' },
  { action: ['read'], subject: 'SmsMessage', requires: 'messages:read' },
  { action: ['send'], subject: 'SmsMessage', requires: 'messages:send' },
  { action: ['read'], subject: 'SmsDashboard', requires: 'dashboard:read' },
]
export const roles: Record<string, string[]> = {
  administrator: [...new Set(abilities.map((a) => a.requires))],
  sender: ['providers:read', 'templates:read', 'messages:send', 'messages:read'],
  viewer: ['providers:read', 'templates:read', 'messages:read', 'dashboard:read'],
}
export function rulesFor(perms: string[]) {
  return abilities.filter((a) => perms.includes(a.requires)).map((a) => ({ action: a.action, subject: a.subject }))
}

export function plugins(perms: string[] = roles.administrator!): Array<Plugin | [Plugin, ...unknown[]]> {
  const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/:pathMatch(.*)*', component: { template: '<div/>' } }] })
  return [router, [abilitiesPlugin as Plugin, createMongoAbility(rulesFor(perms)), { useGlobalProperties: true }]]
}

export const page = <T>(items: T[]) => ({ items, total: items.length, page: 1, page_size: 25, sort: 'name', order: 'asc' })
export const drawer = () => document.body.querySelector('aside[role=dialog]')
export const q = <T extends Element = HTMLElement>(sel: string) => document.body.querySelector<T>(sel)
export function set(sel: string, v: string): void {
  const el = q<HTMLInputElement>(sel)
  if (!el) throw new Error('missing ' + sel)
  el.value = v
  el.dispatchEvent(new Event(el.tagName === 'SELECT' ? 'change' : 'input'))
}
export function click(sel: string): void {
  const el = q(sel)
  if (!el) throw new Error('missing ' + sel)
  el.click()
}
