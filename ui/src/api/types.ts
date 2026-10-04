// Domain types from the OpenAPI contract (npm run gen:api).
import type { components } from './schema.d'
type S = components['schemas']

export type Provider = S['Provider']
export type ProviderCreated = S['ProviderCreated']
export type ProviderType = S['ProviderType']
export type ProviderField = S['ProviderField']
export type Template = S['Template']
export type Preview = S['Preview']
export type Encoding = S['Encoding']
export type ApiClient = S['APIClient']
export type ApiClientCreated = S['APIClientCreated']
export type PasswordResetResult = S['PasswordResetResult']
export type Block = S['Block']
export type Message = S['Message']
export type MessageDetail = S['MessageDetail']
export type Receipt = S['Receipt']
export type SendResult = S['SendResult']
export type Window = S['Window']
export type InstantResult = S['InstantResult']
export type RangeResult = S['RangeResult']
export type InstantSample = S['InstantSample']
export type RangeSeries = S['RangeSeries']
export type Channel = S['Channel']

export interface Page<T> { items: T[]; total: number; page: number; page_size: number; sort: string; order: 'asc' | 'desc' }

/** A stored credential reads as this marker; sending it back keeps the value. */
export const SET_MARKER = '__set__'
export const CHANNELS: Channel[] = ['sms', 'viber']
export const ENCODINGS: Encoding[] = ['utf-8', 'gsm-03-38']
export const WINDOWS: Window[] = ['15m', '1h', '6h', '24h', '7d']
