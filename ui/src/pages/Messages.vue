<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useAbility } from '@casl/vue'
import { UiPage, UiAlert, UiCard, UiButton, UiDataTable, UiBadge, UiDrawer, UiInput, UiSelect, UiSection, UiKeyValueTable, UiEmptyState, type Column, type KeyValue, type SelectOption } from '@go-tangra/ui'
import { call, explain } from '@/api/client'
import type { Message, MessageDetail, Provider, Receipt } from '@/api/types'
import { loadOptions, usePagedList } from '@/components/lists'
import { useRowActivation } from '@/components/rows'
import { actorLabel, carrierTime, humanize, statusColor, statusLabel, when } from '@/components/format'
import SendDrawer from '@/components/SendDrawer.vue'

const ability = useAbility()
// Applied filters (sent with every page) and the inputs being edited.
const applied = reactive({ recipient: '', sid: '', status: '', provider_id: '', api_client_username: '' })
const draft = reactive({ ...applied })
const filterErrors = ref<Record<string, string>>({})
const list = usePagedList<Message>('messages', '/api/sms-gw/v1/messages', ['created_at', 'recipient', 'status'], 'created_at', 'desc', () => ({ ...applied }))

const providers = ref<Provider[]>([])
onMounted(async () => {
  if (ability.can('read', 'SmsProvider')) providers.value = await loadOptions<Provider>('/api/sms-gw/v1/providers', 'name')
})
const providerOptions = computed<SelectOption[]>(() => [{ title: 'Any provider', value: '' }, ...providers.value.map((p) => ({ title: p.name, value: String(p.id) }))])
const statusOptions: SelectOption[] = [{ title: 'Any status', value: '' }, ...[-1, 0, 1, 2, 16, 500].map((c) => ({ title: statusLabel(c), value: String(c) }))]

function applyFilters(): void {
  const e: Record<string, string> = {}
  const d = { recipient: draft.recipient.trim(), sid: draft.sid.trim(), status: draft.status, provider_id: draft.provider_id, api_client_username: draft.api_client_username.trim() }
  if (d.recipient && !/^[0-9]{1,20}$/.test(d.recipient)) e.recipient = 'Digits only'
  if (d.sid && !/^[1-9][0-9]{0,9}$/.test(d.sid)) e.sid = 'A positive number'
  if (d.api_client_username && !/^[A-Za-z0-9_]{1,50}$/.test(d.api_client_username)) e.api_client_username = 'Letters, digits or underscores'
  filterErrors.value = e
  if (Object.keys(e).length) return
  Object.assign(applied, d)
  list.refilter()
}
function clearFilters(): void {
  Object.assign(draft, { recipient: '', sid: '', status: '', provider_id: '', api_client_username: '' })
  applyFilters()
}

const selected = ref<MessageDetail | null>(null)
const receipts = ref<Receipt[]>([])
const detailError = ref('')
const receiptError = ref('')
let seq = 0
async function open(m: Message): Promise<void> {
  const mine = ++seq
  selected.value = { ...m }
  receipts.value = []
  detailError.value = ''
  receiptError.value = ''
  const params = { message_id: m.id }
  const [detail, dlrs] = await Promise.allSettled([
    call<MessageDetail>('GET', '/api/sms-gw/v1/messages/{message_id}', { params }),
    call<{ items: Receipt[] }>('GET', '/api/sms-gw/v1/messages/{message_id}/dlrs', { params }),
  ])
  if (mine !== seq) return
  if (detail.status === 'fulfilled') selected.value = detail.value
  else detailError.value = explain(detail.reason)
  if (dlrs.status === 'fulfilled') receipts.value = dlrs.value.items
  else receiptError.value = explain(dlrs.reason)
}
function close(): void {
  seq++
  selected.value = null
}
const facts = computed<KeyValue[]>(() => {
  const m = selected.value
  if (!m) return []
  return [
    { label: 'Message id', value: m.id, copyable: true },
    { label: 'Recipient', value: m.recipient },
    { label: 'Status', value: statusLabel(m.status_code) + ' (' + m.status_code + ')' + (m.status_message ? ' — ' + m.status_message : '') },
    { label: 'Provider', value: m.provider_name || '#' + m.provider_id },
    { label: 'Template', value: m.template_id ?? '—' },
    { label: 'SID', value: m.sid },
    { label: 'Priority', value: m.priority },
    { label: 'Sent by', value: actorLabel(m) },
    { label: 'From address', value: m.remote_address || '—' },
    { label: 'Created', value: when(m.created_at) },
    { label: 'Updated', value: when(m.updated_at) },
  ]
})
const json = (v: unknown) => JSON.stringify(v, null, 2)

const sending = ref(false)
function sent(): void {
  void list.load()
}

const rows = useRowActivation<Message>((m) => m.id, (m) => 'Open message to ' + m.recipient + ', ' + statusLabel(m.status_code), () => list.items.value, (m) => void open(m), 'message')
const columns: Column<Message>[] = [
  { key: 'created_at', label: 'Sent', sortable: true, defaultDir: 'desc', format: (m) => when(m.created_at) },
  { key: 'recipient', label: 'Recipient', sortable: true },
  { key: 'provider_name', label: 'Provider', hideOnStack: true },
  { key: 'status', label: 'Status', sortable: true },
  { key: 'status_message', label: 'Detail', hideOnStack: true },
  { key: 'actor', label: 'Sent by', hideOnStack: true, format: actorLabel },
]
// Every receipt of the message, oldest first, as in v3: one row per carrier
// status, repeated part receipts counted in Parts; Time is the carrier's.
const receiptColumns: Column<Receipt>[] = [
  { key: 'timestamp', label: 'Time', format: carrierTime },
  { key: 'status', label: 'Status', format: (r) => r.status + ' · ' + statusLabel(r.status) },
  { key: 'status_text', label: 'Status text', format: (r) => humanize(r.status_text) },
  { key: 'parts_received', label: 'Parts', align: 'end', format: (r) => String(r.parts_received || 1) },
  { key: 'created_at', label: 'Received', hideOnStack: true, format: (r) => when(r.created_at) },
]
</script>

<template>
  <UiPage title="SMS messages">
    <template #actions>
      <UiButton v-if="ability.can('send', 'SmsMessage')" icon="mdi-send" data-test="message-send" @click="sending = true">Send SMS</UiButton>
    </template>
    <!-- One row of filters on wide screens; fields share the width and wrap on narrow ones. -->
    <template #filters>
      <form class="flex w-full flex-wrap items-end gap-2 lg:flex-nowrap" data-test="message-filters" @submit.prevent="applyFilters">
        <UiInput class="min-w-0 grow basis-36" id="filter-recipient" v-model="draft.recipient" label="Recipient starts with" inputmode="numeric" size="sm" :error="filterErrors.recipient" data-test="filter-recipient" />
        <UiInput class="min-w-0 grow basis-36" id="filter-sid" v-model="draft.sid" label="SID" inputmode="numeric" size="sm" :error="filterErrors.sid" />
        <UiSelect class="min-w-0 grow basis-36" id="filter-status" v-model="draft.status" label="Status" :options="statusOptions" :clearable="false" size="sm" data-test="filter-status" />
        <UiSelect class="min-w-0 grow basis-36" v-if="providers.length" id="filter-provider" v-model="draft.provider_id" label="Provider" :options="providerOptions" :clearable="false" size="sm" />
        <UiInput class="min-w-0 grow basis-36" id="filter-client" v-model="draft.api_client_username" label="API client" size="sm" :error="filterErrors.api_client_username" />
        <UiButton class="shrink-0" type="submit" size="sm" icon="mdi-filter-outline" data-test="filter-apply">Apply</UiButton>
        <UiButton class="shrink-0" type="button" size="sm" variant="text" @click="clearFilters">Clear</UiButton>
      </form>
    </template>
    <UiAlert v-if="list.error.value" kind="error" class="mb-3" data-test="message-list-error">{{ list.error.value }}</UiAlert>
    <UiCard :padded="false">
      <div @keydown="rows.onKeydown">
        <UiDataTable :items="list.items.value" :columns="columns" row-key="id" :row-attrs="rows.rowAttrs" :loading="list.loading.value" :total="list.total.value" :page="list.lq.page.value" :page-size="list.lq.pageSize.value" :sort="list.lq.sort.value" caption="SMS messages" empty-title="No messages match" clickable data-test="messages-table" @row-click="(m) => void open(m)" @update:page="list.lq.setPage" @update:page-size="list.lq.setPageSize" @update:sort="list.lq.setSort">
          <template #cell-status="{ row }"><UiBadge :color="statusColor(row.status_code)">{{ statusLabel(row.status_code) }}</UiBadge></template>
        </UiDataTable>
      </div>
    </UiCard>

    <UiDrawer :model-value="!!selected" :title="selected ? 'Message to ' + selected.recipient : ''" size="xl" data-test="message-drawer" @update:model-value="(v) => { if (!v) close() }">
      <div v-if="selected" class="flex flex-col gap-4">
        <UiAlert v-if="detailError" kind="error">{{ detailError }}</UiAlert>
        <UiKeyValueTable :items="facts" :columns="2" />
        <UiSection title="Text">
          <pre class="whitespace-pre-wrap break-words rounded bg-base-200 p-3 font-mono text-sm" data-test="message-text">{{ selected.text }}</pre>
        </UiSection>
        <UiSection title="Delivery receipts" data-test="message-receipts">
          <UiAlert v-if="receiptError" kind="error">{{ receiptError }}</UiAlert>
          <UiDataTable v-else :items="receipts" :columns="receiptColumns" row-key="id" caption="Delivery receipts, oldest first" empty-title="No delivery receipts yet" />
        </UiSection>
        <UiSection v-if="selected.data && Object.keys(selected.data).length" title="Send options and properties">
          <pre class="overflow-x-auto rounded bg-base-200 p-3 font-mono text-xs">{{ json(selected.data) }}</pre>
        </UiSection>
        <UiSection title="Carrier exchange">
          <template v-if="selected.evidence && (selected.evidence.request || selected.evidence.response)">
            <p class="mb-1 text-xs text-base-content/70">Credentials are removed by the gateway.</p>
            <h4 class="text-sm font-medium">Request</h4>
            <pre class="mb-2 overflow-x-auto rounded bg-base-200 p-3 font-mono text-xs" data-test="message-evidence-request">{{ selected.evidence.request }}</pre>
            <h4 class="text-sm font-medium">Response</h4>
            <pre class="overflow-x-auto rounded bg-base-200 p-3 font-mono text-xs">{{ selected.evidence.response }}</pre>
          </template>
          <UiEmptyState v-else title="No carrier exchange recorded" />
        </UiSection>
      </div>
    </UiDrawer>
    <SendDrawer v-if="ability.can('send', 'SmsMessage')" v-model="sending" @sent="sent" />
  </UiPage>
</template>
