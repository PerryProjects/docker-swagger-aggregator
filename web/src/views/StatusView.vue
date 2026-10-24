<script setup lang="ts">
import { computed, onScopeDispose, ref } from 'vue'
import Column from 'primevue/column'
import DataTable from 'primevue/datatable'
import Message from 'primevue/message'
import Tag from 'primevue/tag'
import { BASE } from '../base'
import { useServices } from '../composables/useServices'
import { assignSlugs, compareServices } from '../slug'

const { services, loaded } = useServices()

const sorted = computed(() => [...services.value].sort(compareServices))
const slugs = computed(() => assignSlugs(sorted.value.filter((s) => s.specHash)))
const up = computed(() => services.value.filter((s) => s.status === 'up').length)
const down = computed(() => services.value.length - up.value)

// "3 minutes ago" needs a clock; tick every 30 s. (An up service is shown as "now": its lastSeen only moves
// when something visible changes, so a relative time would be misleading.)
const now = ref(Date.now())
const timer = setInterval(() => { now.value = Date.now() }, 30_000)
onScopeDispose(() => clearInterval(timer))

const rtf = new Intl.RelativeTimeFormat('en', { numeric: 'auto' })
function ago(iso?: string): string {
  if (!iso) return 'never'
  const seconds = Math.round((new Date(iso).getTime() - now.value) / 1000)
  if (Math.abs(seconds) < 60) return rtf.format(seconds, 'second')
  if (Math.abs(seconds) < 3600) return rtf.format(Math.round(seconds / 60), 'minute')
  if (Math.abs(seconds) < 86_400) return rtf.format(Math.round(seconds / 3600), 'hour')
  return rtf.format(Math.round(seconds / 86_400), 'day')
}
</script>

<template>
  <main class="mx-auto max-w-6xl p-4 sm:p-6">
    <header class="mb-4 flex flex-wrap items-center justify-between gap-3">
      <h1 class="text-2xl font-semibold">Service status</h1>
      <div v-if="loaded && services.length" class="flex gap-2" role="status">
        <Tag severity="success" icon="pi pi-check-circle" :value="`${up} up`" />
        <Tag :severity="down ? 'danger' : 'secondary'" icon="pi pi-times-circle" :value="`${down} down`" />
      </div>
    </header>

    <Message v-if="!loaded" severity="secondary" role="status">Loading…</Message>
    <Message v-else-if="!services.length" severity="info" role="status">No services discovered yet.</Message>
    <DataTable v-else :value="sorted" data-key="name" size="small" striped-rows>
      <Column header="Service">
        <template #body="{ data }">
          <span :data-service="data.name" class="font-medium">{{ data.group ? `${data.group} / ` : '' }}{{ data.name }}</span>
        </template>
      </Column>
      <Column header="Status">
        <template #body="{ data }">
          <Tag
            :severity="data.status === 'up' ? 'success' : 'danger'"
            :icon="data.status === 'up' ? 'pi pi-check-circle' : 'pi pi-times-circle'"
            :value="data.status"
          />
        </template>
      </Column>
      <Column header="Last seen" class="hidden sm:table-cell">
        <template #body="{ data }">{{ data.status === 'up' ? 'now' : ago(data.lastSeen) }}</template>
      </Column>
      <Column header="Problem">
        <template #body="{ data }"><span class="break-words text-muted-color">{{ data.error ?? '' }}</span></template>
      </Column>
      <Column header="">
        <template #body="{ data }">
          <a v-if="data.specHash" :href="`${BASE}#${slugs.get(data.name)}`" class="text-primary hover:underline">Open docs</a>
        </template>
      </Column>
    </DataTable>
  </main>
</template>
