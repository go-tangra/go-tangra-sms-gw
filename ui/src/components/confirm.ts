import type { Ref } from 'vue'
import type { ConfirmOptions } from '@go-tangra/ui'

// The shell hosts the confirmation dialog; it and a page drawer are both body
// overlays on the same layer and the shell's comes first, so an open drawer
// would cover the question. The drawer steps aside while it is asked and
// comes back when the answer is no.
export async function askOverDrawer(confirm: { ask: (o: ConfirmOptions) => Promise<boolean> }, drawer: Ref<boolean>, o: ConfirmOptions): Promise<boolean> {
  drawer.value = false
  const ok = await confirm.ask(o)
  if (!ok) drawer.value = true
  return ok
}

// Preview properties the operator actually filled in: a blank field is not
// supplied, so the preview reports it as missing.
export function supplied(properties: Record<string, string>): Record<string, string> {
  return Object.fromEntries(Object.entries(properties).filter(([, v]) => v !== ''))
}
