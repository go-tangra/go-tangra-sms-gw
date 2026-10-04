<script setup lang="ts">
import { UiAlert, UiBadge } from '@go-tangra/ui'
import type { Preview } from '@/api/types'

// A rendered template: the text exactly as it would be sent, its length and
// SMS part count under the chosen encoding, and the properties left unset.
defineProps<{ preview: Preview }>()
</script>

<template>
  <div class="flex flex-col gap-2" data-test="preview-result">
    <pre class="whitespace-pre-wrap break-words rounded bg-base-200 p-3 font-mono text-sm" data-test="preview-text">{{ preview.text }}</pre>
    <div class="flex flex-wrap gap-2 text-sm" aria-live="polite">
      <UiBadge data-test="preview-characters">{{ preview.characters }} characters</UiBadge>
      <UiBadge :color="preview.parts > 1 ? 'warning' : 'info'" data-test="preview-parts">{{ preview.parts }} {{ preview.parts === 1 ? 'part' : 'parts' }}</UiBadge>
      <UiBadge>limit {{ preview.limit }} for this part count</UiBadge>
      <UiBadge>{{ preview.encoding }}</UiBadge>
    </div>
    <UiAlert v-if="preview.missing.length" kind="warning" data-test="preview-missing">Missing properties render as &lt;no value&gt;: {{ preview.missing.join(', ') }}</UiAlert>
  </div>
</template>
