// jsdom lacks a few browser APIs the kit touches on mount.
class RO {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}
globalThis.ResizeObserver = RO as unknown as typeof ResizeObserver
window.matchMedia = (query: string): MediaQueryList => {
  const m = /min-width:\s*(\d+)px/.exec(query)
  const matches = m ? 1280 >= Number(m[1]) : false
  return { matches, media: query, onchange: null, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false } as MediaQueryList
}
