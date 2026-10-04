<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useAbility } from '@casl/vue'
import { UiPage, UiAlert, UiCard, UiButton, UiDataTable, UiBadge, UiStatusChip, UiDrawer, UiInput, UiSelect, UiSecretField, UiSwitch, UiNumberInput, UiSection, useConfirm, useToast, type Column, type SelectOption } from '@go-tangra/ui'
import { call, errorField, explain } from '@/api/client'
import { CHANNELS, SET_MARKER, type Channel, type Provider, type ProviderCreated, type ProviderField, type ProviderType } from '@/api/types'
import { debounce, usePagedList } from '@/components/lists'
import { useRowActivation } from '@/components/rows'
import { when } from '@/components/format'
import { buildConfig, initialConfig, validateConfig } from '@/components/providerForm'
import OneTimeSecret from '@/components/OneTimeSecret.vue'

const ability = useAbility()
const confirm = useConfirm()
const toast = useToast()
const q = ref('')
const list = usePagedList<Provider>('providers', '/api/sms-gw/v1/providers', ['name', 'type', 'status', 'created_at', 'updated_at'], 'name', 'asc', () => ({ q: q.value.trim() }))
const search = debounce(list.refilter)

const types = ref<ProviderType[]>([])
onMounted(async () => {
  try {
    types.value = (await call<{ items: ProviderType[] }>('GET', '/api/sms-gw/v1/provider-types')).items
  } catch (e) {
    list.error.value = explain(e)
  }
})
const typeOptions = computed<SelectOption[]>(() => types.value.map((t) => ({ title: t.label, value: t.type })))
const typeLabel = (t: string) => types.value.find((x) => x.type === t)?.label ?? t

const drawer = ref(false)
const selected = ref<Provider | null>(null)
const canEdit = computed(() => (selected.value ? ability.can('update', 'SmsProvider') : ability.can('create', 'SmsProvider')))
const form = reactive({ name: '', type: '', channel: 'sms' as Channel, enabled: true, retention_days: 0 as number | null, config: {} as Record<string, string> })
const errors = ref<Record<string, string>>({})
const formError = ref('')
const saving = ref(false)
const fields = computed<ProviderField[]>(() => types.value.find((t) => t.type === form.type)?.fields ?? [])
const channelOptions: SelectOption[] = CHANNELS.map((c) => ({ title: c === 'sms' ? 'SMS' : 'Viber (reserved, not sent)', value: c }))
const generated = ref<{ label: string; value: string } | null>(null)

function open(p: Provider | null): void {
  selected.value = p
  errors.value = {}
  formError.value = ''
  const type = p?.type ?? types.value[0]?.type ?? ''
  Object.assign(form, { name: p?.name ?? '', type, channel: p?.channel ?? 'sms', enabled: p?.enabled ?? true, retention_days: p?.retention_days ?? 0 })
  const fs = types.value.find((t) => t.type === type)?.fields ?? []
  form.config = initialConfig(fs, p?.config)
  drawer.value = true
}
function changeType(v: unknown): void {
  form.type = String(v ?? '')
  form.config = initialConfig(fields.value, selected.value?.type === form.type ? selected.value.config : undefined)
}
function validate(): boolean {
  const e: Record<string, string> = {}
  if (!form.name.trim()) e.name = 'Required'
  if (!form.type) e.type = 'Required'
  const r = form.retention_days ?? 0
  if (!Number.isInteger(r) || r < 0 || r > 36500) e.retention_days = 'Between 0 and 36500 days'
  for (const [k, v] of Object.entries(validateConfig(fields.value, form.config))) e['config.' + k] = v
  errors.value = e
  return Object.keys(e).length === 0
}
async function save(): Promise<void> {
  formError.value = ''
  if (!validate()) return
  const creating = !selected.value
  const body = { name: form.name.trim(), type: form.type, channel: form.channel, enabled: form.enabled, retention_days: form.retention_days ?? 0, config: buildConfig(fields.value, form.config, creating) }
  saving.value = true
  try {
    if (creating) {
      const res = await call<ProviderCreated>('POST', '/api/sms-gw/v1/providers', { body })
      const token = res.generated_secrets?.dlr_token
      if (token) generated.value = { label: fields.value.find((f) => f.key === 'dlr_token')?.label ?? 'DLR token', value: token }
    } else {
      await call<Provider>('PATCH', '/api/sms-gw/v1/providers/{provider_id}', { params: { provider_id: selected.value!.id }, body })
    }
    drawer.value = false
    toast.success(creating ? 'Provider created' : 'Provider saved')
    void list.load()
  } catch (e) {
    const f = errorField(e)
    if (f) errors.value = { ...errors.value, [f.field]: f.message }
    formError.value = explain(e)
  } finally {
    saving.value = false
  }
}
async function remove(): Promise<void> {
  const p = selected.value
  if (!p || !(await confirm.ask({ title: `Delete ${p.name}?`, text: 'Providers referenced by messages or blocks cannot be deleted.', danger: true, confirmLabel: 'Delete' }))) return
  try {
    await call('DELETE', '/api/sms-gw/v1/providers/{provider_id}', { params: { provider_id: p.id } })
    drawer.value = false
    void list.load()
  } catch (e) {
    formError.value = explain(e)
  }
}
const err = (k: string) => errors.value[k]
const rows = useRowActivation<Provider>((p) => String(p.id), (p) => 'Open provider ' + p.name, () => list.items.value, open, 'provider')
const columns: Column<Provider>[] = [
  { key: 'name', label: 'Name', sortable: true },
  { key: 'type', label: 'Type', sortable: true, format: (p) => typeLabel(p.type) },
  { key: 'channel', label: 'Channel', width: 'sm' },
  { key: 'status', label: 'Status', width: 'sm', sortable: true },
  { key: 'retention_days', label: 'Retention', width: 'sm', align: 'end', format: (p) => (p.retention_days ? p.retention_days + ' d' : 'kept') },
  { key: 'updated_at', label: 'Updated', sortable: true, defaultDir: 'desc', hideOnStack: true, format: (p) => when(p.updated_at) },
]
</script>

<template>
  <UiPage title="SMS providers">
    <template #actions>
      <UiButton v-if="ability.can('create', 'SmsProvider')" icon="mdi-plus" data-test="provider-new" :disabled="!types.length" @click="open(null)">New provider</UiButton>
    </template>
    <template #filters>
      <UiInput id="provider-search" v-model="q" type="search" label="Search" sr-only-label placeholder="Search by name" size="sm" data-test="provider-search" @update:model-value="search()" />
    </template>
    <UiAlert v-if="list.error.value" kind="error" class="mb-3" data-test="provider-list-error">{{ list.error.value }}</UiAlert>
    <UiCard :padded="false">
      <div @keydown="rows.onKeydown">
        <UiDataTable :items="list.items.value" :columns="columns" row-key="id" :row-attrs="rows.rowAttrs" :loading="list.loading.value" :total="list.total.value" :page="list.lq.page.value" :page-size="list.lq.pageSize.value" :sort="list.lq.sort.value" caption="SMS providers" empty-title="No providers yet" clickable data-test="providers-table" @row-click="open" @update:page="list.lq.setPage" @update:page-size="list.lq.setPageSize" @update:sort="list.lq.setSort">
          <template #cell-channel="{ row }"><UiBadge>{{ row.channel }}</UiBadge></template>
          <template #cell-status="{ row }"><UiStatusChip :status="row.enabled ? 'enabled' : 'disabled'" /></template>
        </UiDataTable>
      </div>
    </UiCard>

    <UiDrawer v-model="drawer" :title="selected ? (canEdit ? 'Edit provider' : selected.name) : 'New provider'" size="lg" data-test="provider-drawer">
      <UiAlert v-if="formError" kind="error" class="mb-3" data-test="provider-error">{{ formError }}</UiAlert>
      <form class="flex flex-col gap-3" novalidate @submit.prevent="save">
        <UiInput id="provider-name" v-model="form.name" label="Name" required :disabled="!canEdit" :error="err('name')" data-test="provider-name" />
        <UiSelect id="provider-type" :model-value="form.type" label="Type" :options="typeOptions" :clearable="false" required :disabled="!canEdit" :error="err('type')" data-test="provider-type" @update:model-value="changeType" />
        <UiSelect id="provider-channel" v-model="form.channel" label="Channel" :options="channelOptions" :clearable="false" :disabled="!canEdit" />
        <UiNumberInput id="provider-retention" v-model="form.retention_days" label="Retention (days, 0 keeps messages)" :min="0" :max="36500" :disabled="!canEdit" :error="err('retention_days')" />
        <UiSwitch id="provider-enabled" v-model="form.enabled" label="Enabled" :disabled="!canEdit" data-test="provider-enabled" />
        <UiSection v-if="fields.length" title="Configuration">
          <div class="flex flex-col gap-3">
            <template v-for="f in fields" :key="f.key">
              <UiSecretField v-if="f.type === 'secret'" :id="'provider-config-' + f.key" v-model="form.config[f.key]" :label="f.label" :required="f.required" :disabled="!canEdit" autocomplete="new-password" :error="err('config.' + f.key)" :hint="(form.config[f.key] === SET_MARKER ? 'Stored. Keep the masked value to keep it, clear the field to remove it. ' : '') + (f.help ?? '')" :data-test="'provider-config-' + f.key" />
              <UiSelect v-else-if="f.type === 'select'" :id="'provider-config-' + f.key" v-model="form.config[f.key]" :label="f.label" :options="(f.options ?? []).map((o) => ({ title: o, value: o }))" :required="f.required" :clearable="!f.required" :disabled="!canEdit" :error="err('config.' + f.key)" :hint="f.help" :data-test="'provider-config-' + f.key" />
              <UiInput v-else :id="'provider-config-' + f.key" v-model="form.config[f.key]" :label="f.label" :type="f.type === 'url' ? 'url' : 'text'" :inputmode="f.type === 'int' ? 'numeric' : undefined" :placeholder="f.placeholder" :required="f.required" :disabled="!canEdit" :error="err('config.' + f.key)" :hint="f.help" :data-test="'provider-config-' + f.key" />
            </template>
          </div>
        </UiSection>
        <UiAlert v-if="err('config')" kind="error">{{ err('config') }}</UiAlert>
        <p v-if="selected" class="text-xs text-base-content/70">Created {{ when(selected.created_at) }} · updated {{ when(selected.updated_at) }}</p>
      </form>
      <template #actions>
        <UiButton v-if="selected && ability.can('delete', 'SmsProvider')" variant="text" color="error" data-test="provider-delete" @click="remove">Delete</UiButton>
        <UiButton variant="text" @click="drawer = false">{{ canEdit ? 'Cancel' : 'Close' }}</UiButton>
        <UiButton v-if="canEdit" :loading="saving" data-test="provider-save" @click="save">Save</UiButton>
      </template>
    </UiDrawer>
    <OneTimeSecret :model-value="!!generated" title="Receipt token generated" :label="generated?.label ?? ''" :value="generated?.value ?? ''" @update:model-value="(v) => { if (!v) generated = null }" />
  </UiPage>
</template>
