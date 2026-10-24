<script setup lang="ts">
import { computed, onMounted, onScopeDispose, onUnmounted, ref, watch } from 'vue'
import Button from 'primevue/button'
import Column from 'primevue/column'
import DataTable from 'primevue/datatable'
import IconField from 'primevue/iconfield'
import InputIcon from 'primevue/inputicon'
import InputText from 'primevue/inputtext'
import Message from 'primevue/message'
import Select from 'primevue/select'
import SelectButton from 'primevue/selectbutton'
import Skeleton from 'primevue/skeleton'
import Tag from 'primevue/tag'
import { BASE } from '../base'
import { useServices, type Service } from '../composables/useServices'
import { assignSlugs, compareServices } from '../slug'

const { services, loaded, live } = useServices()

// --- filters (kept in the URL, so a filtered view can be shared or reloaded) ---------------------------------------
type StatusFilter = 'all' | 'up' | 'down'
const params = new URLSearchParams(location.search)
const query = ref(params.get('q') ?? '')
const statusFilter = ref<StatusFilter>(['up', 'down'].includes(params.get('status') ?? '') ? (params.get('status') as StatusFilter) : 'all')
const groupFilter = ref<string | null>(params.get('group'))

watch([query, statusFilter, groupFilter], () => {
  const p = new URLSearchParams()
  if (query.value.trim()) p.set('q', query.value.trim())
  if (statusFilter.value !== 'all') p.set('status', statusFilter.value)
  if (groupFilter.value) p.set('group', groupFilter.value)
  const qs = p.toString()
  history.replaceState(null, '', location.pathname + (qs ? `?${qs}` : '') + location.hash)
})

const sorted = computed(() => [...services.value].sort(compareServices))
const slugs = computed(() => assignSlugs(sorted.value.filter((s) => s.specHash)))
const groups = computed(() => [...new Set(services.value.map((s) => s.group).filter((g): g is string => !!g))].sort())

/** Everything a person may type a word of. */
const haystack = (s: Service) =>
  [s.name, s.title, s.group, s.error, s.version, s.openapi, s.source, s.health, s.status].filter(Boolean).join(' ').toLowerCase()

const filtered = computed(() => {
  const words = query.value.toLowerCase().split(/\s+/).filter(Boolean)
  return sorted.value.filter(
    (s) =>
      (statusFilter.value === 'all' || s.status === statusFilter.value) &&
      (!groupFilter.value || s.group === groupFilter.value) &&
      words.every((w) => haystack(s).includes(w)),
  )
})
const filtering = computed(() => !!query.value.trim() || statusFilter.value !== 'all' || !!groupFilter.value)
function clearFilters() {
  query.value = ''
  statusFilter.value = 'all'
  groupFilter.value = null
}

// --- numbers ------------------------------------------------------------------------------------------------------
const up = computed(() => services.value.filter((s) => s.status === 'up').length)
const down = computed(() => services.value.length - up.value)
const endpoints = computed(() => services.value.reduce((n, s) => n + (s.endpoints ?? 0), 0))
const statusOptions = computed(() => [
  { label: `All ${services.value.length}`, value: 'all' },
  { label: `Up ${up.value}`, value: 'up' },
  { label: `Down ${down.value}`, value: 'down' },
])

// --- time ---------------------------------------------------------------------------------------------------------
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
const exact = (iso?: string) => (iso ? new Date(iso).toLocaleString() : 'never')

function healthSeverity(h?: string) {
  return h === 'healthy' ? 'success' : h === 'unhealthy' ? 'danger' : 'warn'
}

// --- rows, keyboard -----------------------------------------------------------------------------------------------
const expanded = ref<Record<string, boolean>>({})

// "/" jumps to the search, Escape clears it (as on most dashboards)
function onKey(e: KeyboardEvent) {
  const typing = ['INPUT', 'TEXTAREA', 'SELECT'].includes((e.target as HTMLElement)?.tagName)
  if (e.key === '/' && !typing && !e.metaKey && !e.ctrlKey) {
    e.preventDefault()
    document.getElementById('service-search')?.focus()
  } else if (e.key === 'Escape' && (e.target as HTMLElement)?.id === 'service-search') {
    query.value = ''
  }
}
onMounted(() => window.addEventListener('keydown', onKey))
onUnmounted(() => window.removeEventListener('keydown', onKey))
</script>

<template>
  <main class="mx-auto max-w-7xl p-4 sm:p-6">
    <header class="mb-5 flex flex-wrap items-center justify-between gap-3">
      <div>
        <h1 class="text-2xl font-semibold">Service status</h1>
        <p class="text-sm text-muted-color">Every discovered service, also those that never delivered a spec.</p>
      </div>
      <Tag
        v-if="loaded"
        :severity="live ? 'success' : 'secondary'"
        :icon="live ? 'pi pi-bolt' : 'pi pi-sync'"
        :value="live ? 'Live' : 'Polling'"
        :title="live ? 'Updates arrive as they happen' : 'The live stream is not connected; the list refreshes every 15 s'"
        data-testid="live"
      />
    </header>

    <!-- summary -->
    <section class="mb-5 grid grid-cols-2 gap-3 md:grid-cols-4" role="status" aria-label="Summary">
      <template v-if="!loaded">
        <Skeleton v-for="i in 4" :key="i" height="5rem" border-radius="0.75rem" />
      </template>
      <template v-else>
        <button type="button" class="tile" data-testid="tile-all" :class="{ active: statusFilter === 'all' }" @click="statusFilter = 'all'">
          <span class="tile-label"><i class="pi pi-server" aria-hidden="true" /> Services</span>
          <span class="tile-value">{{ services.length }}</span>
        </button>
        <button type="button" class="tile tile-up" data-testid="tile-up" :class="{ active: statusFilter === 'up' }" @click="statusFilter = 'up'">
          <span class="tile-label"><i class="pi pi-check-circle" aria-hidden="true" /> Up</span>
          <span class="tile-value">{{ up }}</span>
        </button>
        <button type="button" class="tile" data-testid="tile-down" :class="[down ? 'tile-down' : '', { active: statusFilter === 'down' }]" @click="statusFilter = 'down'">
          <span class="tile-label"><i class="pi pi-times-circle" aria-hidden="true" /> Down</span>
          <span class="tile-value">{{ down }}</span>
        </button>
        <div class="tile tile-static">
          <span class="tile-label"><i class="pi pi-list" aria-hidden="true" /> Endpoints</span>
          <span class="tile-value">{{ endpoints }}</span>
        </div>
      </template>
    </section>

    <!-- search and filters -->
    <div v-if="loaded && services.length" class="mb-3 flex flex-wrap items-center gap-3">
      <IconField class="min-w-64 flex-1">
        <InputIcon class="pi pi-search" />
        <InputText
          id="service-search"
          v-model="query"
          type="search"
          placeholder="Search by name, title, group, problem …   ( / )"
          aria-label="Search services"
          autocomplete="off"
          fluid
        />
      </IconField>
      <SelectButton
        v-model="statusFilter"
        :options="statusOptions"
        option-label="label"
        option-value="value"
        :allow-empty="false"
        aria-label="Filter by status"
      />
      <Select
        v-if="groups.length"
        v-model="groupFilter"
        :options="groups"
        show-clear
        placeholder="All groups"
        aria-label="Filter by group"
        class="min-w-40"
      />
    </div>

    <Message v-if="loaded && !services.length" severity="info" icon="pi pi-info-circle">
      No services discovered yet. Label a container with <code>swagger-aggregator.enable=true</code> (or a Kubernetes Service)
      and it shows up here.
    </Message>

    <DataTable
      v-else-if="loaded"
      v-model:expanded-rows="expanded"
      :value="filtered"
      data-key="name"
      size="small"
      row-hover
      removable-sort
      :pt="{ root: { class: 'rounded-xl overflow-hidden border border-surface' } }"
    >
      <template #empty>
        <div class="flex flex-col items-center gap-3 py-8 text-center" data-testid="no-match">
          <i class="pi pi-search text-3xl text-muted-color" />
          <p>No service matches <strong v-if="query.trim()">“{{ query.trim() }}”</strong><span v-else>these filters</span>.</p>
          <Button label="Clear filters" icon="pi pi-filter-slash" severity="secondary" size="small" @click="clearFilters" />
        </div>
      </template>

      <Column expander style="width: 3rem" />
      <Column field="status" header="Status" sortable style="width: 7.5rem">
        <template #body="{ data }">
          <Tag
            :severity="data.status === 'up' ? 'success' : 'danger'"
            :icon="data.status === 'up' ? 'pi pi-check-circle' : 'pi pi-times-circle'"
            :value="data.status"
          />
        </template>
      </Column>
      <Column field="name" header="Service" sortable>
        <template #body="{ data }">
          <div class="flex flex-col">
            <span class="font-medium" :data-service="data.name">{{ data.group ? `${data.group} / ` : '' }}{{ data.name }}</span>
            <span v-if="data.title" class="text-sm text-muted-color">
              {{ data.title }}<template v-if="data.version"> · v{{ data.version }}</template>
            </span>
            <span v-if="data.error" class="mt-0.5 text-sm text-red-600 dark:text-red-400 break-words">{{ data.error }}</span>
          </div>
        </template>
      </Column>
      <Column field="endpoints" header="Endpoints" sortable class="hidden md:table-cell">
        <template #body="{ data }">{{ data.endpoints ?? '–' }}</template>
      </Column>
      <Column field="replicasUp" header="Replicas" sortable class="hidden md:table-cell">
        <template #body="{ data }">
          <span v-if="data.replicas" :class="data.replicasUp < data.replicas ? 'text-amber-600 dark:text-amber-400' : ''">
            {{ data.replicasUp ?? 0 }}/{{ data.replicas }}
          </span>
          <span v-else>–</span>
        </template>
      </Column>
      <Column field="source" header="Source" class="hidden lg:table-cell">
        <template #body="{ data }">
          <Tag v-if="data.source" severity="secondary" :value="data.source === 'kubernetes' ? 'Kubernetes' : 'Docker'" />
        </template>
      </Column>
      <Column field="lastSeen" header="Last seen" sortable class="hidden sm:table-cell">
        <template #body="{ data }">
          <span :title="exact(data.lastSeen)">{{ data.status === 'up' ? 'now' : ago(data.lastSeen) }}</span>
        </template>
      </Column>
      <Column style="width: 8rem">
        <template #body="{ data }">
          <Button
            v-if="data.specHash"
            as="a"
            :href="`${BASE}#${slugs.get(data.name)}`"
            label="Open docs"
            icon="pi pi-book"
            size="small"
            severity="secondary"
            text
          />
        </template>
      </Column>

      <template #expansion="{ data }">
        <dl class="details" :data-details="data.name">
          <div><dt>Title</dt><dd>{{ data.title || '–' }}</dd></div>
          <div><dt>Version</dt><dd>{{ data.version || '–' }}</dd></div>
          <div><dt>Spec format</dt><dd>{{ data.openapi ? (data.openapi.startsWith('2') ? `Swagger ${data.openapi}` : `OpenAPI ${data.openapi}`) : '–' }}</dd></div>
          <div><dt>Endpoints</dt><dd>{{ data.endpoints ?? '–' }}</dd></div>
          <div><dt>Source</dt><dd>{{ data.source === 'kubernetes' ? 'Kubernetes Service' : data.source === 'docker' ? 'Docker container' : '–' }}</dd></div>
          <div>
            <dt>Health check</dt>
            <dd><Tag v-if="data.health" :severity="healthSeverity(data.health)" :value="data.health" /><span v-else>none</span></dd>
          </div>
          <div><dt>Replicas</dt><dd>{{ data.replicas ? `${data.replicasUp ?? 0} of ${data.replicas} up` : '–' }}</dd></div>
          <div><dt>Last seen</dt><dd>{{ exact(data.lastSeen) }}</dd></div>
          <div><dt>Spec hash</dt><dd class="font-mono text-sm">{{ data.specHash || '–' }}</dd></div>
          <div v-if="data.error" class="col-span-full">
            <dt>Problem</dt>
            <dd><Message severity="error" size="small" variant="simple" class="break-words">{{ data.error }}</Message></dd>
          </div>
        </dl>
      </template>
    </DataTable>

    <p v-if="loaded && services.length && filtering && filtered.length" class="mt-3 text-sm text-muted-color" data-testid="count">
      {{ filtered.length }} of {{ services.length }} shown
      <Button label="Clear filters" severity="secondary" text size="small" @click="clearFilters" />
    </p>
  </main>
</template>

<style scoped>
.tile {
  display: flex; flex-direction: column; gap: 0.25rem; align-items: flex-start; padding: 0.9rem 1.1rem; text-align: left;
  border-radius: 0.75rem; border: 1px solid var(--p-content-border-color); background: var(--p-content-background);
  color: inherit; cursor: pointer; transition: border-color 0.15s, box-shadow 0.15s;
}
.tile:hover { border-color: var(--p-primary-color); }
.tile.active { border-color: var(--p-primary-color); box-shadow: 0 0 0 1px var(--p-primary-color); }
.tile-static { cursor: default; }
.tile-static:hover { border-color: var(--p-content-border-color); }
.tile-label { display: flex; gap: 0.4rem; align-items: center; font-size: 0.85rem; color: var(--p-text-muted-color); }
.tile-value { font-size: 1.75rem; font-weight: 600; line-height: 1.1; }
.tile-up .tile-value { color: var(--p-green-600); }
.tile-down .tile-value { color: var(--p-red-600); }
@media (prefers-color-scheme: dark) {
  .tile-up .tile-value { color: var(--p-green-400); }
  .tile-down .tile-value { color: var(--p-red-400); }
}
.details { display: grid; grid-template-columns: repeat(auto-fill, minmax(14rem, 1fr)); gap: 0.75rem 1.5rem; padding: 0.5rem 0.5rem 0.75rem 3rem; margin: 0; }
.details dt { font-size: 0.75rem; text-transform: uppercase; letter-spacing: 0.04em; color: var(--p-text-muted-color); }
.details dd { margin: 0.1rem 0 0; }
</style>
