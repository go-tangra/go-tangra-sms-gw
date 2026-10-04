<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useAbility } from '@casl/vue'
import { UiPage, UiAlert, UiCard, UiButton, UiDataTable, UiBadge, UiStatusChip, UiDrawer, UiForm, UiInput, UiSelect, UiSwitch, useConfirm, useToast, type Column, type SelectOption } from '@go-tangra/ui'
import { useZodForm } from '@go-tangra/ui/forms'
import { call, errorField, explain } from '@/api/client'
import { CHANNELS, type Block, type Provider } from '@/api/types'
import { debounce, loadOptions, usePagedList } from '@/components/lists'
import { useRowActivation } from '@/components/rows'
import { askOverDrawer } from '@/components/confirm'
import { when } from '@/components/format'
import { blockSchema } from '@/components/schemas'

const ALL = ''
const ability = useAbility()
const confirm = useConfirm()
const toast = useToast()
const q = ref('')
const list = usePagedList<Block>('blocks', '/api/sms-gw/v1/blocks', ['recipient', 'status', 'created_at'], 'created_at', 'desc', () => ({ q: q.value.trim() }))
const search = debounce(list.refilter)

// Provider names for the scope column and select (empty when the caller cannot read providers).
const providers = ref<Provider[]>([])
onMounted(async () => {
  if (ability.can('read', 'SmsProvider')) providers.value = await loadOptions<Provider>('/api/sms-gw/v1/providers', 'name')
})
function scope(b: Block): string {
  if (b.provider_id === null || b.provider_id === undefined) return 'All providers'
  return providers.value.find((p) => p.id === b.provider_id)?.name ?? 'Provider #' + b.provider_id
}
const providerOptions = computed<SelectOption[]>(() => [{ title: 'All providers', value: ALL }, ...providers.value.map((p) => ({ title: p.name, value: String(p.id) }))])
const channelOptions: SelectOption[] = CHANNELS.map((c) => ({ title: c === 'sms' ? 'SMS' : 'Viber (reserved)', value: c }))

const drawer = ref(false)
const selected = ref<Block | null>(null)
const formError = ref('')
const canEdit = computed(() => (selected.value ? ability.can('update', 'SmsBlock') : ability.can('create', 'SmsBlock')))
const form = useZodForm(blockSchema, {
  onSubmit: async (v) => {
    formError.value = ''
    // provider_id null blocks the recipient on every provider of the tenant.
    const body = { recipient: v.recipient, description: v.description, channel: v.channel, enabled: v.enabled, provider_id: v.provider_id === ALL ? null : Number(v.provider_id) }
    try {
      if (selected.value) await call<Block>('PATCH', '/api/sms-gw/v1/blocks/{block_id}', { params: { block_id: selected.value.id }, body })
      else await call<Block>('POST', '/api/sms-gw/v1/blocks', { body })
      toast.success(selected.value ? 'Block saved' : 'Recipient blocked')
      drawer.value = false
      void list.load()
    } catch (e) {
      const f = errorField(e)
      if (f) form.setFieldError(f.field, f.message)
      formError.value = explain(e)
    }
  },
})
function open(b: Block | null): void {
  selected.value = b
  formError.value = ''
  form.reset({ recipient: b?.recipient ?? '', description: b?.description ?? '', provider_id: b?.provider_id ? String(b.provider_id) : ALL, channel: b?.channel ?? 'sms', enabled: b?.enabled ?? true })
  drawer.value = true
}
async function remove(): Promise<void> {
  const b = selected.value
  if (!b || !(await askOverDrawer(confirm, drawer, { title: `Unblock ${b.recipient}?`, text: 'Messages to this recipient will be sent again.', danger: true, confirmLabel: 'Delete block' }))) return
  try {
    await call('DELETE', '/api/sms-gw/v1/blocks/{block_id}', { params: { block_id: b.id } })
    drawer.value = false
    void list.load()
  } catch (e) {
    formError.value = explain(e)
    drawer.value = true
  }
}
const rows = useRowActivation<Block>((b) => String(b.id), (b) => 'Open block for ' + b.recipient, () => list.items.value, open, 'block')
const columns: Column<Block>[] = [
  { key: 'recipient', label: 'Recipient', sortable: true },
  { key: 'description', label: 'Description', hideOnStack: true },
  { key: 'provider_id', label: 'Scope', format: scope },
  { key: 'channel', label: 'Channel', width: 'sm' },
  { key: 'status', label: 'Status', width: 'sm', sortable: true },
  { key: 'created_by', label: 'Created by', hideOnStack: true },
  { key: 'created_at', label: 'Created', sortable: true, defaultDir: 'desc', format: (b) => when(b.created_at) },
]
</script>

<template>
  <UiPage title="SMS blocks" subtitle="Recipients the gateway refuses to message">
    <template #actions>
      <UiButton v-if="ability.can('create', 'SmsBlock')" icon="mdi-plus" data-test="block-new" @click="open(null)">Block a recipient</UiButton>
    </template>
    <template #filters>
      <UiInput id="block-search" v-model="q" type="search" label="Search" sr-only-label placeholder="Search recipient or description" size="sm" data-test="block-search" @update:model-value="search()" />
    </template>
    <UiAlert v-if="list.error.value" kind="error" class="mb-3">{{ list.error.value }}</UiAlert>
    <UiCard :padded="false">
      <div @keydown="rows.onKeydown">
        <UiDataTable :items="list.items.value" :columns="columns" row-key="id" :row-attrs="rows.rowAttrs" :loading="list.loading.value" :total="list.total.value" :page="list.lq.page.value" :page-size="list.lq.pageSize.value" :sort="list.lq.sort.value" caption="SMS blocks" empty-title="No blocked recipients" clickable data-test="blocks-table" @row-click="open" @update:page="list.lq.setPage" @update:page-size="list.lq.setPageSize" @update:sort="list.lq.setSort">
          <template #cell-provider_id="{ row }">
            <UiBadge :color="row.provider_id === null ? 'warning' : 'neutral'">{{ scope(row) }}</UiBadge>
          </template>
          <template #cell-status="{ row }"><UiStatusChip :status="row.enabled ? 'enabled' : 'disabled'" /></template>
        </UiDataTable>
      </div>
    </UiCard>

    <UiDrawer v-model="drawer" :title="selected ? 'Block ' + selected.recipient : 'Block a recipient'" data-test="block-drawer">
      <UiAlert v-if="formError" kind="error" class="mb-3" data-test="block-error">{{ formError }}</UiAlert>
      <UiForm :form="form">
        <div class="flex flex-col gap-3">
          <UiInput v-bind="form.field('recipient')" label="Recipient" inputmode="numeric" required :disabled="!canEdit" hint="Digits only, as the recipient is sent (e.g. 359888123456)." data-test="block-recipient" />
          <UiInput v-bind="form.field('description')" label="Description" :disabled="!canEdit" />
          <UiSelect v-bind="form.field('provider_id')" label="Applies to" :options="providerOptions" :clearable="false" :disabled="!canEdit" hint="All providers blocks the recipient on every provider of this tenant." data-test="block-provider" />
          <UiSelect v-bind="form.field('channel')" label="Channel" :options="channelOptions" :clearable="false" :disabled="!canEdit" />
          <UiSwitch v-bind="form.field('enabled')" label="Enforced" hint="Disabled blocks are kept but not enforced." :disabled="!canEdit" />
        </div>
      </UiForm>
      <p v-if="selected" class="mt-3 text-xs text-base-content/70">Created by {{ selected.created_by || 'unknown' }} · {{ when(selected.created_at) }}</p>
      <template #actions>
        <UiButton v-if="selected && ability.can('delete', 'SmsBlock')" variant="text" color="error" data-test="block-delete" @click="remove">Delete</UiButton>
        <UiButton variant="text" @click="drawer = false">{{ canEdit ? 'Cancel' : 'Close' }}</UiButton>
        <UiButton v-if="canEdit" :loading="form.submitting.value" data-test="block-save" @click="form.submit()">Save</UiButton>
      </template>
    </UiDrawer>
  </UiPage>
</template>
