import { describe, expect, it } from 'vitest'
import { ICONS } from '@go-tangra/ui'
import { EXTRA_ICONS, KIT_MODIFIERS } from '../safelist'

const sources = import.meta.glob<string>(['../**/*.{vue,ts}', '!./**'], { query: '?raw', import: 'default', eager: true })
const kit = new Set<string>(ICONS)
const safelisted = new Set<string>(EXTRA_ICONS.map((c) => c.replace('icon-[mdi--', 'mdi-').replace(']', '')))

// Tailwind emits only classes it sees; the kit builds icon classes at
// runtime, so every icon is either in the shell's kit set or safelisted in
// this remote's stylesheet. Navigation icons are checked against the kit set
// in pkg/smsgwmanifest (TestNavIconsInKitSet).
describe('styles', () => {
  it('reads the module sources', () => expect(Object.keys(sources).length).toBeGreaterThan(10))
  it('every icon the pages use is in the kit set or the remote safelist', () => {
    const used = new Set(Object.values(sources).flatMap((src) => [...src.matchAll(/\bmdi-[a-z0-9][a-z0-9-]*/g)].map((m) => m[0])))
    expect([...used].filter((i) => !kit.has(i) && !safelisted.has(i))).toEqual([])
    expect([...safelisted].filter((i) => kit.has(i))).toEqual([])
  })
  it('the remote re-emits the kit modifiers after the FlyonUI bases', () => {
    expect(KIT_MODIFIERS).toEqual(['switch-primary', 'switch-sm', 'input-sm', 'select-sm', 'link-primary'])
  })
})
