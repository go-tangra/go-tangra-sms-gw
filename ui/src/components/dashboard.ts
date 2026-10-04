import type { BarItem } from '@go-tangra/ui'
import type { InstantSample, RangeSeries } from '@/api/types'
import { labelText } from './format'

// Rendering helpers for the predefined monitoring queries
// (internal/metrics/dashboard.go). Values are absent, not zero, when the
// series has no sample.

export function headline(results: Record<string, InstantSample[]> | undefined, name: string): number | null {
  const s = results?.[name]?.[0]
  return s && s.has_value && typeof s.value === 'number' && Number.isFinite(s.value) ? s.value : null
}

export function rate(v: number | null): string {
  return v === null ? '—' : v.toFixed(v < 10 ? 2 : 1) + '/s'
}
export function percent(v: number | null): string {
  return v === null ? '—' : (v * 100).toFixed(1) + '%'
}
export function seconds(v: number | null): string {
  if (v === null) return '—'
  return v < 1 ? Math.round(v * 1000) + ' ms' : v.toFixed(2) + ' s'
}

/** Counts by label set, largest first. */
export function bars(samples: InstantSample[] | undefined): BarItem[] {
  return (samples ?? [])
    .filter((s) => s.has_value && typeof s.value === 'number')
    .map((s) => ({ label: labelText(s.labels), value: Math.round(s.value ?? 0) }))
    .sort((a, b) => b.value - a.value)
}

/** SVG path for a series scaled into width×height; gaps (null) break the line. */
export function sparkPath(series: RangeSeries, width: number, height: number, max: number): string {
  const n = series.values.length
  if (!n || max <= 0) return ''
  const step = n > 1 ? width / (n - 1) : 0
  let d = ''
  let pen = false
  series.values.forEach((v, i) => {
    if (v === null || v === undefined || !Number.isFinite(v)) {
      pen = false
      return
    }
    const x = (i * step).toFixed(1)
    const y = (height - (v / max) * height).toFixed(1)
    d += (pen ? 'L' : 'M') + x + ' ' + y
    pen = true
  })
  return d
}

export function seriesMax(series: RangeSeries[] | undefined): number {
  let m = 0
  for (const s of series ?? []) for (const v of s.values) if (typeof v === 'number' && v > m) m = v
  return m
}

export function last(series: RangeSeries): number | null {
  for (let i = series.values.length - 1; i >= 0; i--) {
    const v = series.values[i]
    if (typeof v === 'number') return v
  }
  return null
}
