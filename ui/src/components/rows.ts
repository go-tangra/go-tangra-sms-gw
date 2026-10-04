// Keyboard access for clickable UiDataTable rows. The kit only wires a mouse
// click, so every opening row is made focusable through rowAttrs and the
// table's wrapper handles Enter/Space for the focused row (keydown bubbles).
export const focusRing = 'outline-none focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-primary'

export function useRowActivation<T>(key: (row: T) => string, label: (row: T) => string, items: () => T[], open: (row: T) => void, test = 'row') {
  const rowAttrs = (row: T): Record<string, string> => ({ tabindex: '0', 'aria-label': label(row), 'data-row-key': key(row), 'data-test': test + '-' + key(row), class: focusRing })
  function onKeydown(event: KeyboardEvent): void {
    if (event.key !== 'Enter' && event.key !== ' ') return
    const target = event.target
    if (!(target instanceof HTMLElement) || !target.hasAttribute('data-row-key')) return
    const row = items().find((r) => key(r) === target.getAttribute('data-row-key'))
    if (!row) return
    event.preventDefault()
    open(row)
  }
  return { rowAttrs, onKeydown }
}
