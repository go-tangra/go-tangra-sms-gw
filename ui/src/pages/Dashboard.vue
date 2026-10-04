<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { UiPage, UiAlert, UiCard, UiButton, UiTabs, UiStatGrid, UiStatTile, UiBarList, UiEmptyState, type TabItem } from '@go-tangra/ui'
import { call, explain } from '@/api/client'
import { WINDOWS, type InstantResult, type RangeResult, type Window } from '@/api/types'
import { bars, headline, percent, rate, seconds } from '@/components/dashboard'
import Sparkline from '@/components/Sparkline.vue'

// Predefined monitoring queries only: the browser names a window and query
// names, the module builds and bounds the PromQL.
const win = ref<Window>('1h')
const tabs: TabItem[] = WINDOWS.map((w) => ({ key: w, label: w }))
const instant = ref<InstantResult | null>(null)
const range = ref<RangeResult | null>(null)
const loading = ref(false)
const error = ref('')
let seq = 0

async function load(): Promise<void> {
  const mine = ++seq
  loading.value = true
  error.value = ''
  try {
    const body = { window: win.value }
    const [i, r] = await Promise.all([call<InstantResult>('POST', '/api/sms-gw/v1/dashboard/instant', { body }), call<RangeResult>('POST', '/api/sms-gw/v1/dashboard/range', { body })])
    if (mine !== seq) return
    instant.value = i
    range.value = r
  } catch (e) {
    if (mine === seq) error.value = explain(e)
  } finally {
    if (mine === seq) loading.value = false
  }
}
function pick(w: string): void {
  win.value = w as Window
  void load()
}
onMounted(load)

const unavailable = computed(() => {
  const r = [instant.value, range.value].find((x) => x && !x.available)
  if (!r) return ''
  return r.reason === 'not_configured' ? 'Monitoring is not configured for the SMS gateway. Ask a platform administrator to set the metrics source.' : 'The monitoring service is unreachable right now. Try again shortly.'
})
const ir = computed(() => instant.value?.results)
const rr = computed(() => range.value?.results)
</script>

<template>
  <UiPage title="SMS dashboard" subtitle="Deployment-wide totals across all tenants">
    <template #actions>
      <UiButton variant="soft" icon="mdi-refresh" :loading="loading" data-test="dashboard-refresh" @click="load">Refresh</UiButton>
    </template>
    <template #filters>
      <UiTabs :model-value="win" :tabs="tabs" data-test="dashboard-window" @update:model-value="pick" />
    </template>
    <UiAlert v-if="error" kind="error" class="mb-3" data-test="dashboard-error">{{ error }}</UiAlert>
    <UiCard v-if="unavailable" data-test="dashboard-unavailable">
      <UiEmptyState icon="mdi-chart-line-variant" title="Monitoring unavailable" :text="unavailable" />
    </UiCard>
    <div v-else-if="instant && range" class="flex flex-col gap-4" data-test="dashboard-data">
      <UiStatGrid :cols="3">
        <UiStatTile title="Sends" :value="rate(headline(ir, 'sends_per_sec'))" icon="mdi-send" />
        <UiStatTile title="Send success" :value="percent(headline(ir, 'success_rate'))" icon="mdi-check-circle-outline" color="success" />
        <UiStatTile title="Send p95" :value="seconds(headline(ir, 'send_p95_seconds'))" icon="mdi-timer-outline" />
        <UiStatTile title="Receipts" :value="rate(headline(ir, 'dlrs_per_sec'))" icon="mdi-email-check-outline" />
        <UiStatTile title="Delivery p95" :value="seconds(headline(ir, 'delivery_p95_seconds'))" icon="mdi-timer-sand" />
      </UiStatGrid>
      <div class="grid gap-4 lg:grid-cols-3">
        <UiCard class="lg:col-span-3"><Sparkline title="Send rate by outcome" :series="rr?.send_rate_by_outcome ?? []" :format="rate" /></UiCard>
        <UiCard><Sparkline title="Send latency p95" :series="rr?.send_latency_p95 ?? []" :format="seconds" /></UiCard>
        <UiCard class="lg:col-span-2"><Sparkline title="Receipt rate by status" :series="rr?.dlr_rate_by_status ?? []" :format="rate" /></UiCard>
      </div>
      <div class="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
        <UiCard><h3 class="mb-2 text-sm font-medium">Sends by outcome ({{ win }})</h3><UiBarList :items="bars(ir?.send_by_outcome)" empty-title="No sends" /></UiCard>
        <UiCard><h3 class="mb-2 text-sm font-medium">Providers ({{ win }})</h3><UiBarList :items="bars(ir?.top_providers)" empty-title="No sends" /></UiCard>
        <UiCard><h3 class="mb-2 text-sm font-medium">Receipts by status ({{ win }})</h3><UiBarList :items="bars(ir?.dlr_by_status)" empty-title="No receipts" /></UiCard>
        <UiCard><h3 class="mb-2 text-sm font-medium">Callbacks by outcome ({{ win }})</h3><UiBarList :items="bars(ir?.webhook_by_outcome)" empty-title="No callbacks" /></UiCard>
        <UiCard><h3 class="mb-2 text-sm font-medium">API sign-ins by outcome ({{ win }})</h3><UiBarList :items="bars(ir?.login_by_outcome)" empty-title="No sign-ins" /></UiCard>
      </div>
    </div>
  </UiPage>
</template>
