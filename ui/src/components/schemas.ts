import { z } from 'zod'

// Client-side form rules mirroring api/openapi/sms-gw.yaml; the server
// validates every request again.
const optionalUrl = z.string().trim().max(2048).refine((v) => !v || /^https?:\/\/[^\s]+$/i.test(v), 'Enter an http(s) URL')
const optionalPassword = z.string().refine((v) => !v || (v.length >= 8 && v.length <= 72), 'Between 8 and 72 characters, or blank to generate one')

export const apiClientSchema = z.object({
  username: z.string().trim().regex(/^[A-Za-z0-9_]{4,50}$/, '4–50 letters, digits or underscores'),
  password: optionalPassword.default(''),
  email: z.string().trim().max(255).refine((v) => !v || /^[^\s@]+@[^\s@]+$/.test(v), 'Enter an email address').default(''),
  authority: z.enum(['API_CLIENT', 'API_VIEWER', '']).default('API_CLIENT'),
  enabled: z.boolean().default(true),
  callback_url: optionalUrl.default(''),
  callback_secret: z.string().max(256).default(''),
})
export type ApiClientForm = z.output<typeof apiClientSchema>

export const passwordResetSchema = z.object({ password: optionalPassword.default('') })

export const templateSchema = z.object({
  name: z.string().trim().min(1).max(128),
  channel: z.enum(['sms', 'viber']).default('sms'),
  enabled: z.boolean().default(true),
  body: z.string().min(1, 'An SMS template needs a body').max(16384),
})

export const blockSchema = z.object({
  recipient: z.string().trim().regex(/^[0-9]{1,20}$/, 'Digits only (up to 20)'),
  description: z.string().trim().max(255).default(''),
  provider_id: z.string().default(''),
  channel: z.enum(['sms', 'viber']).default('sms'),
  enabled: z.boolean().default(true),
})

export const sendSchema = z.object({
  provider_id: z.string().min(1, 'Choose a provider'),
  template_id: z.string().min(1, 'Choose a template'),
  to: z.string().trim().regex(/^[0-9]{1,15}$/, 'International number, digits only (up to 15)'),
  from: z.string().trim().max(16).default(''),
  encoding: z.enum(['utf-8', 'gsm-03-38', '']).default(''),
  concatenate: z.union([z.number().int().min(1).max(10), z.null()]).default(null),
})
