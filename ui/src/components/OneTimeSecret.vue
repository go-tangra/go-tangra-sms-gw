<script setup lang="ts">
import { UiAlert, UiButton, UiCopyButton, UiDialog } from '@go-tangra/ui'

// A secret the server returns exactly once (a generated password or receipt
// token). The parent clears `value` on close, so it never lingers in state.
defineProps<{ modelValue: boolean; title: string; label: string; value: string }>()
const emit = defineEmits<{ 'update:modelValue': [boolean] }>()
</script>

<template>
  <UiDialog :model-value="modelValue" :title="title" persistent data-test="one-time-secret" @update:model-value="emit('update:modelValue', $event)">
    <UiAlert kind="warning" class="mb-3">Copy it now: it is shown only once and cannot be displayed again. Anyone holding it can use it.</UiAlert>
    <p class="mb-1 text-sm font-medium">{{ label }}</p>
    <div class="flex items-center gap-2">
      <code class="grow break-all rounded bg-base-200 px-2 py-1 font-mono text-sm" data-test="one-time-secret-value">{{ value }}</code>
      <UiCopyButton :value="value" :label="'Copy ' + label" />
    </div>
    <template #actions>
      <UiButton data-test="one-time-secret-done" @click="emit('update:modelValue', false)">I have saved it</UiButton>
    </template>
  </UiDialog>
</template>
