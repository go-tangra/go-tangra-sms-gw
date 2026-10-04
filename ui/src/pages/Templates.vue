<script setup lang="ts">
import { computed, reactive, ref } from 'vue'
import { useAbility } from '@casl/vue'
import { UiPage, UiAlert, UiCard, UiButton, UiDataTable, UiBadge, UiStatusChip, UiDrawer, UiForm, UiInput, UiSelect, UiSwitch, UiTextarea, UiSection, useConfirm, useToast, type Column, type SelectOption } from '@go-tangra/ui'
import { useZodForm } from '@go-tangra/ui/forms'
import { call, errorField, explain } from '@/api/client'
import { CHANNELS, ENCODINGS, type Encoding, type Preview, type Template } from '@/api/types'
import { debounce, usePagedList } from '@/components/lists'
import { useRowActivation } from '@/components/rows'
import { when } from '@/components/format'
import { templateSchema } from '@/components/schemas'
import PreviewResult from '@/components/PreviewResult.vue'

const ability = useAbility()
const confirm = useConfirm()
const toast = useToast()
const q = ref('')
const list = usePagedList<Template>('templates', '/api/sms-gw/v1/templates', ['name', 'status', 'created_at', 'updated_at'], 'name', 'asc', () => ({ q: q.value.trim() }))
const search = debounce(list.refilter)

const drawer = ref(false)
const selected = ref<Template | null>(null)
const formError = ref('')
const canEdit = computed(() => (selected.value ? ability.can('update', 'SmsTemplate') : ability.can('create', 'SmsTemplate')))
const channelOptions: SelectOption[] = CHANNELS.map((c) => ({ title: c === 'sms' ? 'SMS' : 'Viber (reserved, not sent)', value: c }))
const encodingOptions: SelectOption[] = ENCODINGS.map((e) => ({ title: e, value: e }))

const form = useZodForm(templateSchema, {
  onSubmit: async (v) => {
    formError.value = ''
    // Fragments other than the SMS body (legacy data) are kept as they are.
    const body = { name: v.name, channel: v.channel, enabled: v.enabled, fragments: { ...(selected.value?.fragments ?? {}), body: v.body } }
    try {
      const saved = selected.value
        ? await call<Template>('PATCH', '/api/sms-gw/v1/templates/{template_id}', { params: { template_id: selected.value.id }, body })
        : await call<Template>('POST', '/api/sms-gw/v1/templates', { body })
      toast.success(selected.value ? 'Template saved' : 'Template created')
      // Stay open on the saved template so it can be previewed right away.
      select(saved)
      void list.load()
    } catch (e) {
      const f = errorField(e)
      if (f) form.setFieldError(f.field === 'fragments.body' ? 'body' : f.field, f.message)
      formError.value = explain(e)
    }
  },
})

const preview = ref<Preview | null>(null)
const previewError = ref('')
const previewing = ref(false)
const properties = reactive<Record<string, string>>({})
const encoding = ref<Encoding>('utf-8')

function select(t: Template | null): void {
  selected.value = t
  formError.value = ''
  preview.value = null
  previewError.value = ''
  for (const k of Object.keys(properties)) delete properties[k]
  for (const v of t?.variables ?? []) properties[v] = ''
  form.reset({ name: t?.name ?? '', channel: t?.channel ?? 'sms', enabled: t?.enabled ?? true, body: t?.fragments.body ?? '' })
}
function open(t: Template | null): void {
  select(t)
  drawer.value = true
}
async function runPreview(): Promise<void> {
  const t = selected.value
  if (!t) return
  previewing.value = true
  previewError.value = ''
  try {
    preview.value = await call<Preview>('POST', '/api/sms-gw/v1/templates/{template_id}/preview', { params: { template_id: t.id }, body: { properties: { ...properties }, encoding: encoding.value } })
  } catch (e) {
    preview.value = null
    previewError.value = explain(e)
  } finally {
    previewing.value = false
  }
}
async function remove(): Promise<void> {
  const t = selected.value
  if (!t || !(await confirm.ask({ title: `Delete ${t.name}?`, text: 'Templates referenced by messages cannot be deleted; disable them instead.', danger: true, confirmLabel: 'Delete' }))) return
  try {
    await call('DELETE', '/api/sms-gw/v1/templates/{template_id}', { params: { template_id: t.id } })
    drawer.value = false
    void list.load()
  } catch (e) {
    formError.value = explain(e)
  }
}
const unsaved = computed(() => !!selected.value && form.dirty.value)
const rows = useRowActivation<Template>((t) => String(t.id), (t) => 'Open template ' + t.name, () => list.items.value, open, 'template')
const columns: Column<Template>[] = [
  { key: 'name', label: 'Name', sortable: true },
  { key: 'channel', label: 'Channel', width: 'sm' },
  { key: 'status', label: 'Status', width: 'sm', sortable: true },
  { key: 'variables', label: 'Variables', format: (t) => t.variables.join(', ') },
  { key: 'updated_at', label: 'Updated', sortable: true, defaultDir: 'desc', hideOnStack: true, format: (t) => when(t.updated_at) },
]
</script>

<template>
  <UiPage title="SMS templates">
    <template #actions>
      <UiButton v-if="ability.can('create', 'SmsTemplate')" icon="mdi-plus" data-test="template-new" @click="open(null)">New template</UiButton>
    </template>
    <template #filters>
      <UiInput id="template-search" v-model="q" type="search" label="Search" sr-only-label placeholder="Search by name" size="sm" data-test="template-search" @update:model-value="search()" />
    </template>
    <UiAlert v-if="list.error.value" kind="error" class="mb-3">{{ list.error.value }}</UiAlert>
    <UiCard :padded="false">
      <div @keydown="rows.onKeydown">
        <UiDataTable :items="list.items.value" :columns="columns" row-key="id" :row-attrs="rows.rowAttrs" :loading="list.loading.value" :total="list.total.value" :page="list.lq.page.value" :page-size="list.lq.pageSize.value" :sort="list.lq.sort.value" caption="SMS templates" empty-title="No templates yet" clickable data-test="templates-table" @row-click="open" @update:page="list.lq.setPage" @update:page-size="list.lq.setPageSize" @update:sort="list.lq.setSort">
          <template #cell-channel="{ row }"><UiBadge>{{ row.channel }}</UiBadge></template>
          <template #cell-status="{ row }"><UiStatusChip :status="row.enabled ? 'enabled' : 'disabled'" /></template>
        </UiDataTable>
      </div>
    </UiCard>

    <UiDrawer v-model="drawer" :title="selected ? selected.name : 'New template'" size="xl" data-test="template-drawer">
      <UiAlert v-if="formError" kind="error" class="mb-3" data-test="template-error">{{ formError }}</UiAlert>
      <UiForm :form="form">
        <div class="flex flex-col gap-3">
          <UiInput v-bind="form.field('name')" label="Name" required :disabled="!canEdit" data-test="template-name" />
          <UiSelect v-bind="form.field('channel')" label="Channel" :options="channelOptions" :clearable="false" :disabled="!canEdit" />
          <UiSwitch v-bind="form.field('enabled')" label="Enabled" :disabled="!canEdit" />
          <UiTextarea v-bind="form.field('body')" label="Body" :rows="6" required :disabled="!canEdit" hint="Go template text; refer to send properties as {{ .name }}." data-test="template-body" />
        </div>
      </UiForm>
      <UiSection v-if="selected" title="Preview" class="mt-4" data-test="template-preview">
        <UiAlert v-if="unsaved" kind="info" class="mb-3">The preview renders the saved body. Save to preview your changes.</UiAlert>
        <form class="flex flex-col gap-3" @submit.prevent="runPreview">
          <p v-if="!selected.variables.length" class="text-sm text-base-content/70">This template uses no properties.</p>
          <UiInput v-for="v in selected.variables" :id="'template-prop-' + v" :key="v" v-model="properties[v]" :label="v" :data-test="'template-prop-' + v" />
          <UiSelect id="template-encoding" v-model="encoding" label="Encoding" :options="encodingOptions" :clearable="false" />
          <div><UiButton type="submit" variant="soft" icon="mdi-eye-outline" :loading="previewing" data-test="template-preview-run">Preview</UiButton></div>
        </form>
        <UiAlert v-if="previewError" kind="error" class="mt-3">{{ previewError }}</UiAlert>
        <PreviewResult v-if="preview" :preview="preview" class="mt-3" />
      </UiSection>
      <template #actions>
        <UiButton v-if="selected && ability.can('delete', 'SmsTemplate')" variant="text" color="error" data-test="template-delete" @click="remove">Delete</UiButton>
        <UiButton variant="text" @click="drawer = false">Close</UiButton>
        <UiButton v-if="canEdit" :loading="form.submitting.value" data-test="template-save" @click="form.submit()">Save</UiButton>
      </template>
    </UiDrawer>
  </UiPage>
</template>
