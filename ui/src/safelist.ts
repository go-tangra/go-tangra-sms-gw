// Classes the kit builds at runtime that this remote's stylesheet must carry
// (Tailwind emits only the class names it finds in the sources).
//
// FlyonUI re-emits the base classes the module's sources mention (.switch,
// .input, .select, …) in this sheet's utilities layer after the shell's; the
// kit's modifiers are emitted here too so they always follow those bases.
export const KIT_MODIFIERS = ['switch-primary', 'switch-sm', 'input-sm', 'select-sm', 'link-primary'] as const

// Icons outside the kit's set (@go-tangra/ui ICONS) used by the pages; the
// shell's stylesheet carries only the kit set.
export const EXTRA_ICONS = [
  'icon-[mdi--send]',
  'icon-[mdi--lock-reset]',
  'icon-[mdi--chart-line-variant]',
  'icon-[mdi--email-check-outline]',
  'icon-[mdi--timer-outline]',
  'icon-[mdi--timer-sand]',
] as const
