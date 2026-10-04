<script setup lang="ts">
import { computed } from 'vue'
import type { RangeSeries } from '@/api/types'
import { labelText } from './format'
import { last, seriesMax, sparkPath } from './dashboard'

// One range query: a line per label set on a shared scale, with the latest
// value of each in text (the chart is decorative for screen readers).
const props = defineProps<{ title: string; series: RangeSeries[]; format: (v: number | null) => string }>()
const W = 320
const H = 64
const max = computed(() => seriesMax(props.series))
const colors = ['text-primary', 'text-success', 'text-error', 'text-warning', 'text-info', 'text-secondary']
</script>

<template>
  <figure class="flex flex-col gap-2">
    <figcaption class="text-sm font-medium">{{ title }}</figcaption>
    <p v-if="!series.length" class="text-sm text-base-content/70">No data in this window.</p>
    <template v-else>
      <svg :viewBox="`0 0 ${W} ${H}`" class="h-16 w-full" preserveAspectRatio="none" aria-hidden="true">
        <path v-for="(s, i) in series" :key="i" :d="sparkPath(s, W, H, max)" :class="colors[i % colors.length]" fill="none" stroke="currentColor" stroke-width="1.5" vector-effect="non-scaling-stroke" />
      </svg>
      <ul class="flex flex-wrap gap-x-4 gap-y-1 text-xs">
        <li v-for="(s, i) in series" :key="i" class="flex items-center gap-1">
          <span :class="['inline-block size-2 rounded-full bg-current', colors[i % colors.length]]" aria-hidden="true" />
          {{ labelText(s.labels) }}: {{ format(last(s)) }}
        </li>
      </ul>
    </template>
  </figure>
</template>
