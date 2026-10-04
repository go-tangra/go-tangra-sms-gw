<script setup lang="ts">
import { computed, ref } from 'vue'
import { useAbility } from '@casl/vue'
import { UiPage, UiAlert, UiCard, UiButton, UiDataTable, UiBadge, UiStatusChip, UiDrawer, UiDialog, UiForm, UiInput, UiSelect, UiSecretField, UiSwitch, UiSection, useConfirm, useToast, type Column, type SelectOption } from '@go-tangra/ui'
import { useZodForm } from '@go-tangra/ui/forms'
import { call, errorField, explain } from '@/api/client'
import { SET_MARKER, type ApiClient, type ApiClientCreated, type PasswordResetResult } from '@/api/types'
import { debounce, usePagedList } from '@/components/lists'
import { useRowActivation } from '@/components/rows'
import { when } from '@/components/format'
import { apiClientSchema, passwordResetSchema } from '@/components/schemas'
import OneTimeSecret from '@/components/OneTimeSecret.vue'

const ability = useAbility()
const confirm = useConfirm()
const toast = useToast()
const q = ref('')
const list = usePagedList<ApiClient>('api-clients', '/api/sms-gw/v1/api-clients', ['username', 'authority', 'status', 'last_login_at', 'created_at'], 'username', 'asc', () => ({ q: q.value.trim() }))
const search = debounce(list.refilter)

const drawer = ref(false)
const selected = ref<ApiClient | null>(null)
const formError = ref('')
const canEdit = computed(() => (selected.value ? ability.can('update', 'SmsApiClient') : ability.can('create', 'SmsApiClient')))
const legacyAdmin = computed(() => selected.value?.authority === 'API_ADMIN')
const authorityOptions: SelectOption[] = [
  { title: 'API_CLIENT — send and read own messages', value: 'API_CLIENT' },
  { title: 'API_VIEWER — read own messages', value: 'API_VIEWER' },
]
const shown = ref<{ title: string; value: string } | null>(null)

function fail(form: { setFieldError: (p: string, t: string) => void }, e: unknown): void {
  const f = errorField(e)
  if (f) form.setFieldError(f.field, f.message)
  formError.value = explain(e)
}

const form = useZodForm(apiClientSchema, {
  onSubmit: async (v) => {
    formError.value = ''
    try {
      if (!selected.value) {
        const body = { username: v.username, email: v.email, authority: v.authority || 'API_CLIENT', enabled: v.enabled, callback_url: v.callback_url, ...(v.password ? { password: v.password } : {}), ...(v.callback_secret ? { callback_secret: v.callback_secret } : {}) }
        const res = await call<ApiClientCreated>('POST', '/api/sms-gw/v1/api-clients', { body })
        if (res.password) shown.value = { title: 'Password for ' + res.client.username, value: res.password }
        toast.success('API client created')
      } else {
        // The secret travels as shown: the marker keeps it, "" clears it.
        const body = { email: v.email, enabled: v.enabled, callback_url: v.callback_url, callback_secret: v.callback_secret, ...(v.authority ? { authority: v.authority } : {}) }
        await call<ApiClient>('PATCH', '/api/sms-gw/v1/api-clients/{client_id}', { params: { client_id: selected.value.id }, body })
        toast.success('API client saved')
      }
      drawer.value = false
      void list.load()
    } catch (e) {
      fail(form, e)
    }
  },
})

function open(c: ApiClient | null): void {
  selected.value = c
  formError.value = ''
  form.reset({
    username: c?.username ?? '',
    password: '',
    email: c?.email ?? '',
    authority: c ? (c.authority === 'API_ADMIN' ? '' : c.authority) : 'API_CLIENT',
    enabled: c?.enabled ?? true,
    callback_url: c?.callback_url ?? '',
    callback_secret: c?.callback_secret_set ? SET_MARKER : '',
  })
  drawer.value = true
}

async function remove(): Promise<void> {
  const c = selected.value
  if (!c || !(await confirm.ask({ title: `Delete ${c.username}?`, text: 'Clients that have sent messages cannot be deleted; disable them instead.', danger: true, confirmLabel: 'Delete' }))) return
  try {
    await call('DELETE', '/api/sms-gw/v1/api-clients/{client_id}', { params: { client_id: c.id } })
    drawer.value = false
    void list.load()
  } catch (e) {
    formError.value = explain(e)
  }
}

const resetOpen = ref(false)
const resetError = ref('')
const resetForm = useZodForm(passwordResetSchema, {
  onSubmit: async (v) => {
    resetError.value = ''
    const c = selected.value
    if (!c) return
    try {
      const res = await call<PasswordResetResult>('POST', '/api/sms-gw/v1/api-clients/{client_id}/reset-password', { params: { client_id: c.id }, body: v.password ? { password: v.password } : {} })
      resetOpen.value = false
      resetForm.reset({ password: '' })
      if (res.password) shown.value = { title: 'New password for ' + c.username, value: res.password }
      else toast.success('Password replaced')
    } catch (e) {
      resetError.value = explain(e)
    }
  },
})
function openReset(): void {
  resetError.value = ''
  resetForm.reset({ password: '' })
  resetOpen.value = true
}

const rows = useRowActivation<ApiClient>((c) => String(c.id), (c) => 'Open API client ' + c.username, () => list.items.value, open, 'client')
const columns: Column<ApiClient>[] = [
  { key: 'username', label: 'Username', sortable: true },
  { key: 'email', label: 'Email', hideOnStack: true },
  { key: 'authority', label: 'Authority', sortable: true },
  { key: 'status', label: 'Status', width: 'sm', sortable: true },
  { key: 'callback_url', label: 'Callback', hideOnStack: true },
  { key: 'last_login_at', label: 'Last sign-in', sortable: true, defaultDir: 'desc', format: (c) => when(c.last_login_at) || 'never' },
  { key: 'created_at', label: 'Created', sortable: true, defaultDir: 'desc', hideOnStack: true, format: (c) => when(c.created_at) },
]
</script>

<template>
  <UiPage title="SMS API clients" subtitle="Hermes clients that send through the public API">
    <template #actions>
      <UiButton v-if="ability.can('create', 'SmsApiClient')" icon="mdi-plus" data-test="client-new" @click="open(null)">New API client</UiButton>
    </template>
    <template #filters>
      <UiInput id="client-search" v-model="q" type="search" label="Search" sr-only-label placeholder="Search username or email" size="sm" data-test="client-search" @update:model-value="search()" />
    </template>
    <UiAlert v-if="list.error.value" kind="error" class="mb-3">{{ list.error.value }}</UiAlert>
    <UiCard :padded="false">
      <div @keydown="rows.onKeydown">
        <UiDataTable :items="list.items.value" :columns="columns" row-key="id" :row-attrs="rows.rowAttrs" :loading="list.loading.value" :total="list.total.value" :page="list.lq.page.value" :page-size="list.lq.pageSize.value" :sort="list.lq.sort.value" caption="SMS API clients" empty-title="No API clients yet" clickable data-test="clients-table" @row-click="open" @update:page="list.lq.setPage" @update:page-size="list.lq.setPageSize" @update:sort="list.lq.setSort">
          <template #cell-authority="{ row }">
            <UiBadge :color="row.authority === 'API_ADMIN' ? 'warning' : 'neutral'">{{ row.authority }}</UiBadge>
            <span v-if="row.authority === 'API_ADMIN'" class="ms-1 text-xs text-base-content/70">legacy</span>
          </template>
          <template #cell-status="{ row }"><UiStatusChip :status="row.enabled ? 'enabled' : 'disabled'" /></template>
          <template #cell-callback_url="{ row }">
            <span class="break-all">{{ row.callback_url || '—' }}</span>
            <UiBadge v-if="row.callback_secret_set" size="xs" class="ms-1" color="info">signed</UiBadge>
          </template>
        </UiDataTable>
      </div>
    </UiCard>

    <UiDrawer v-model="drawer" :title="selected ? selected.username : 'New API client'" size="lg" data-test="client-drawer">
      <UiAlert v-if="formError" kind="error" class="mb-3" data-test="client-error">{{ formError }}</UiAlert>
      <UiAlert v-if="legacyAdmin" kind="warning" class="mb-3" data-test="client-legacy">This client has the legacy API_ADMIN authority, which grants no public privilege. Choose an authority to change it.</UiAlert>
      <UiForm :form="form">
        <div class="flex flex-col gap-3">
          <UiInput v-bind="form.field('username')" label="Username" required :disabled="!!selected" :hint="selected ? 'Usernames cannot change.' : 'Globally unique; cannot change later.'" autocomplete="off" data-test="client-username" />
          <UiSecretField v-if="!selected" v-bind="form.field('password')" label="Password" hint="Leave blank to generate one; it is shown once." autocomplete="new-password" data-test="client-password" />
          <UiInput v-bind="form.field('email')" label="Email" type="email" :disabled="!canEdit" />
          <UiSelect v-bind="form.field('authority')" label="Authority" :options="authorityOptions" :clearable="false" :disabled="!canEdit" :placeholder="legacyAdmin ? 'API_ADMIN (legacy)' : undefined" data-test="client-authority" />
          <UiSwitch v-bind="form.field('enabled')" label="Enabled" :disabled="!canEdit" />
          <UiInput v-bind="form.field('callback_url')" label="Delivery callback URL" type="url" :disabled="!canEdit" hint="https only; public destinations only. Blank: no callbacks." data-test="client-callback" />
          <UiSecretField v-bind="form.field('callback_secret')" label="Callback signing secret" :disabled="!canEdit" autocomplete="new-password" :hint="form.values.callback_secret === SET_MARKER ? 'Stored. Keep the masked value to keep it, clear the field to remove it.' : 'Optional: signs callbacks.'" data-test="client-secret" />
        </div>
      </UiForm>
      <UiSection v-if="selected" title="Activity" class="mt-4">
        <p class="text-sm">Last sign-in {{ when(selected.last_login_at) || 'never' }}<span v-if="selected.last_login_ip"> from {{ selected.last_login_ip }}</span></p>
        <p class="text-xs text-base-content/70">Created {{ when(selected.created_at) }} · updated {{ when(selected.updated_at) }}</p>
      </UiSection>
      <template #actions>
        <UiButton v-if="selected && ability.can('delete', 'SmsApiClient')" variant="text" color="error" data-test="client-delete" @click="remove">Delete</UiButton>
        <UiButton v-if="selected && ability.can('update', 'SmsApiClient')" variant="soft" icon="mdi-lock-reset" data-test="client-reset" @click="openReset">Reset password</UiButton>
        <UiButton variant="text" @click="drawer = false">{{ canEdit ? 'Cancel' : 'Close' }}</UiButton>
        <UiButton v-if="canEdit" :loading="form.submitting.value" data-test="client-save" @click="form.submit()">Save</UiButton>
      </template>
    </UiDrawer>

    <UiDialog v-model="resetOpen" :title="'Reset password' + (selected ? ' for ' + selected.username : '')" data-test="client-reset-dialog">
      <UiAlert v-if="resetError" kind="error" class="mb-3">{{ resetError }}</UiAlert>
      <p class="mb-3 text-sm">The current password stops working for new sign-ins immediately; tokens already issued stay valid until they expire.</p>
      <UiForm :form="resetForm">
        <UiSecretField v-bind="resetForm.field('password')" label="New password" hint="Leave blank to generate one; it is shown once." autocomplete="new-password" data-test="client-reset-password" />
      </UiForm>
      <template #actions>
        <UiButton variant="text" @click="resetOpen = false">Cancel</UiButton>
        <UiButton color="warning" :loading="resetForm.submitting.value" data-test="client-reset-confirm" @click="resetForm.submit()">Reset password</UiButton>
      </template>
    </UiDialog>
    <OneTimeSecret :model-value="!!shown" :title="shown?.title ?? ''" label="Password" :value="shown?.value ?? ''" @update:model-value="(v) => { if (!v) shown = null }" />
  </UiPage>
</template>
