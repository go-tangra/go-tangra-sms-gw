import { onMounted, ref, watch } from 'vue'
import { useListQuery, type SortDir } from '@go-tangra/ui'
import { call, explain, type ApiPath, type Query } from '@/api/client'
import type { Page } from '@/api/types'

// Server-paged lists (go-tangra 032): every table asks the server for one page
// (page, page_size, sort, order, q and filters) and shows its total. Page,
// size and sort live in the URL (?<key>.page=…).
export const PAGE_SIZE = 25
/** The largest page the server answers: option lists for selects. */
export const OPTIONS_SIZE = 200

function compact(f: Query): Query {
  const out: Query = {}
  for (const [k, v] of Object.entries(f)) if (v !== undefined && v !== '') out[k] = v
  return out
}

export function usePagedList<T>(key: string, path: ApiPath, sortable: string[], sort: string, dir: SortDir = 'asc', filters: () => Query = () => ({})) {
  const lq = useListQuery(key, { sortable, defaultSort: { key: sort, dir }, defaultSize: PAGE_SIZE })
  const items = ref<T[]>([]) as { value: T[] }
  const total = ref(0)
  const loading = ref(false)
  const error = ref('')
  let seq = 0
  async function load(): Promise<void> {
    const mine = ++seq
    loading.value = true
    error.value = ''
    try {
      const res = await call<Page<T>>('GET', path, { query: { ...compact(filters()), ...lq.query.value } })
      if (mine !== seq) return
      items.value = res.items ?? []
      total.value = res.total ?? 0
      if (res.page) lq.clampTo(res.page) // a page beyond the end answers the last page
    } catch (e) {
      if (mine === seq) error.value = explain(e)
    } finally {
      if (mine === seq) loading.value = false
    }
  }
  /** Filters or search changed: back to page 1 (which reloads), or reload in place. */
  function refilter(): void {
    if (lq.page.value !== 1) lq.resetPage()
    else void load()
  }
  watch(lq.query, () => void load())
  onMounted(() => void load())
  return { lq, items, total, loading, error, load, refilter }
}

/** Up to OPTIONS_SIZE records of a list for a select; [] when the caller may not read them. */
export async function loadOptions<T>(path: ApiPath, sort: string): Promise<T[]> {
  try {
    const res = await call<Page<T>>('GET', path, { query: { page: 1, page_size: OPTIONS_SIZE, sort, order: 'asc' } })
    return res.items ?? []
  } catch {
    return []
  }
}

/** Calls fn once input settles (search boxes). */
export function debounce<A extends unknown[]>(fn: (...a: A) => void, ms = 300): (...a: A) => void {
  let t: ReturnType<typeof setTimeout> | undefined
  return (...a: A) => {
    clearTimeout(t)
    t = setTimeout(() => fn(...a), ms)
  }
}
