<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { UiAlert, UiButton, UiDrawer, UiForm, UiInput, UiNumberInput, UiSection, UiSelect, type SelectOption } from '@go-tangra/ui'
import { useZodForm } from '@go-tangra/ui/forms'
import { call, errorField, explain } from '@/api/client'
import { ENCODINGS, type Message, type Preview, type Provider, type SendResult, type Template } from '@/api/types'
import { debounce, loadOptions } from '@/components/lists'
import { statusLabel } from '@/components/format'
import { sendSchema } from '@/components/schemas'
import PreviewResult from './PreviewResult.vue'

// Manual send from the portal: the same pipeline as the public API (recipient
// policy, blocks, rendering, carrier), recorded with the operator as actor.
const props = defineProps<{ modelValue: boolean }>()
const emit = defineEmits<{ 'update:modelValue': [boolean]; sent: [Message] }>()

const providers = ref<Provider[]>([])
const templates = ref<Template[]>([])
const properties = reactive<Record<string, string>>({})
const preview = ref<Preview | null>(null)
const previewError = ref('')
const error = ref('')
const result = ref<SendResult | null>(null)

const providerOptions = computed<SelectOption[]>(() => providers.value.map((p) => ({ title: p.name + (p.enabled ? '' : ' (disabled)'), value: String(p.id) })))
const templateOptions = computed<SelectOption[]>(() => templates.value.map((t) => ({ title: t.name + (t.enabled ? '' : ' (disabled)'), value: String(t.id) })))
const encodingOptions: SelectOption[] = [{ title: 'Provider default', value: '' }, ...ENCODINGS.map((e) => ({ title: e, value: e }))]

const form = useZodForm(sendSchema, {
  initial: { provider_id: '', template_id: '', to: '', from: '', encoding: '', concatenate: null },
  onSubmit: async (v) => {
    error.value = ''
    result.value = null
    const sms = { ...(v.from ? { from: v.from } : {}), ...(v.encoding ? { encoding: v.encoding } : {}), ...(v.concatenate ? { concatenate: v.concatenate } : {}) }
    const body = { provider_id: Number(v.provider_id), template_id: Number(v.template_id), to: v.to, properties: { ...properties }, ...(Object.keys(sms).length ? { sms } : {}) }
    try {
      const res = await call<SendResult>('POST', '/api/sms-gw/v1/messages', { body })
      result.value = res
      emit('sent', res.message)
    } catch (e) {
      const f = errorField(e)
      if (f) form.setFieldError(f.field, f.message)
      error.value = explain(e)
    }
  },
})

const template = computed(() => templates.value.find((t) => String(t.id) === form.values.template_id))
watch(template, (t) => {
  for (const k of Object.keys(properties)) delete properties[k]
  for (const v of t?.variables ?? []) properties[v] = ''
  preview.value = null
  void refresh()
})

let seq = 0
async function refresh(): Promise<void> {
  const t = template.value
  const mine = ++seq
  if (!t) return
  try {
    const enc = form.values.encoding
    const res = await call<Preview>('POST', '/api/sms-gw/v1/templates/{template_id}/preview', { params: { template_id: t.id }, body: { properties: { ...properties }, ...(enc ? { encoding: enc } : {}) } })
    if (mine === seq) {
      preview.value = res
      previewError.value = ''
    }
  } catch (e) {
    if (mine === seq) previewError.value = explain(e)
  }
}
const refreshSoon = debounce(() => void refresh(), 400)
watch(() => [{ ...properties }, form.values.encoding], refreshSoon, { deep: true })

watch(
  () => props.modelValue,
  async (open) => {
    if (!open) return
    error.value = ''
    result.value = null
    preview.value = null
    form.reset({ provider_id: '', template_id: '', to: '', from: '', encoding: '', concatenate: null })
    const [p, t] = await Promise.all([loadOptions<Provider>('/api/sms-gw/v1/providers', 'name'), loadOptions<Template>('/api/sms-gw/v1/templates', 'name')])
    providers.value = p
    templates.value = t
  },
  { immediate: true },
)
</script>

<template>
  <UiDrawer :model-value="modelValue" title="Send SMS" size="lg" data-test="send-drawer" @update:model-value="emit('update:modelValue', $event)">
    <UiAlert v-if="error" kind="error" class="mb-3" data-test="send-error">{{ error }}</UiAlert>
    <UiAlert v-if="result" :kind="[-1, 0, 1].includes(result.message.status_code) ? 'success' : 'warning'" class="mb-3" data-test="send-result">
      {{ statusLabel(result.message.status_code) }}<span v-if="result.message.status_message"> — {{ result.message.status_message }}</span>.
      <span v-if="result.carrier"> Carrier answered {{ result.carrier.return_code }}<span v-if="result.carrier.return_message"> ({{ result.carrier.return_message }})</span>.</span>
      Message {{ result.message.id }}.
    </UiAlert>
    <UiAlert v-if="!providers.length || !templates.length" kind="info" class="mb-3">Sending needs at least one provider and one template you can read.</UiAlert>
    <UiForm :form="form">
      <div class="flex flex-col gap-3">
        <UiSelect v-bind="form.field('provider_id')" label="Provider" :options="providerOptions" :clearable="false" required data-test="send-provider" />
        <UiSelect v-bind="form.field('template_id')" label="Template" :options="templateOptions" :clearable="false" required data-test="send-template" />
        <UiInput v-bind="form.field('to')" label="To" inputmode="numeric" required hint="International number, digits only (e.g. 359888123456)." autocomplete="off" data-test="send-to" />
        <UiSection v-if="template" title="Properties">
          <p v-if="!template.variables.length" class="text-sm text-base-content/70">This template uses no properties.</p>
          <div class="flex flex-col gap-3">
            <UiInput v-for="v in template.variables" :id="'send-prop-' + v" :key="v" v-model="properties[v]" :label="v" :data-test="'send-prop-' + v" />
          </div>
        </UiSection>
        <UiSection title="SMS options">
          <div class="grid gap-3 sm:grid-cols-3">
            <UiInput v-bind="form.field('from')" label="Sender (from)" hint="Blank: provider default" />
            <UiSelect v-bind="form.field('encoding')" label="Encoding" :options="encodingOptions" :clearable="false" />
            <UiNumberInput v-bind="form.field('concatenate')" label="Max parts" :min="1" :max="10" />
          </div>
        </UiSection>
      </div>
    </UiForm>
    <UiSection v-if="template" title="Preview" class="mt-4">
      <UiAlert v-if="previewError" kind="error">{{ previewError }}</UiAlert>
      <PreviewResult v-if="preview" :preview="preview" />
    </UiSection>
    <template #actions>
      <UiButton variant="text" @click="emit('update:modelValue', false)">Close</UiButton>
      <UiButton icon="mdi-send" :loading="form.submitting.value" data-test="send-submit" @click="form.submit()">Send</UiButton>
    </template>
  </UiDrawer>
</template>
