<script setup lang="ts">
import { computed, defineAsyncComponent, ref, watch } from 'vue'
import { useServices } from '../composables/useServices'
import { assignSlugs, compareServices } from '../slug'

// Scalar is ~1.5 MB: load it only once there is something to show.
const ApiReference = defineAsyncComponent(async () => {
  const [{ ApiReference }] = await Promise.all([
    import('@scalar/api-reference'),
    import('@scalar/api-reference/style.css'),
  ])
  return ApiReference
})

const { services, loaded } = useServices()

// Per-viewer convenience only; the view works without storage.
const HIDE_KEY = 'swagger-aggregator.hideOffline'
const hideOffline = ref(false)
try {
  hideOffline.value = localStorage.getItem(HIDE_KEY) === '1'
} catch { /* storage blocked */ }
watch(hideOffline, (v) => {
  try {
    localStorage.setItem(HIDE_KEY, v ? '1' : '0')
  } catch { /* storage blocked */ }
})

// One Scalar source per service that has a spec. The slug is derived from the
// name only, so a status flip (title change) does not change the document identity.
const withSpec = computed(() => services.value.filter((s) => s.specHash).sort(compareServices))
const slugs = computed(() => assignSlugs(withSpec.value))
const sources = computed(() =>
  withSpec.value
    .filter((s) => !(hideOffline.value && s.status === 'down'))
    .map((s) => ({
      title: `${s.status === 'up' ? '🟢' : '🔴'} ${s.group ? `${s.group} / ` : ''}${s.name}`,
      slug: slugs.value.get(s.name) ?? s.name,
      // specHash busts the cache and makes Scalar refetch when the spec changes
      url: `/api/services/${encodeURIComponent(s.name)}/spec?v=${s.specHash}`,
    })))

// Scalar jumps back to the first document whenever the ordered list of slugs
// changes, even without a remount. Keying on that list remounts exactly then
// (status/spec changes keep the same key and update in place).
const setKey = computed(() => sources.value.map((s) => s.slug).join('|'))

// A remount restores the open document and section from the URL hash. If that
// document is gone, drop the hash first; otherwise Scalar appends to the stale one.
watch(setKey, () => {
  const doc = decodeURIComponent(location.hash.slice(1)).split('/')[0]
  if (doc && !sources.value.some((s) => s.slug === doc)) {
    history.replaceState(null, '', location.pathname + location.search)
  }
}, { flush: 'pre' })

// Offline-friendly: no web fonts from fonts.scalar.com, no telemetry, no hosted "Ask AI" agent.
// proxyUrl: requests to absolute URLs (a service's server-url) go through our
// /api/scalar-proxy, which only forwards to registered services (no CORS, no open proxy).
const configuration = computed(() => ({
  withDefaultFonts: false,
  telemetry: false,
  agent: { disabled: true },
  proxyUrl: '/api/scalar-proxy',
  sources: sources.value,
}))

const hasServices = computed(() => services.value.some((s) => s.specHash))
</script>

<template>
  <ApiReference v-if="sources.length" :key="setKey" :configuration="configuration">
    <template #sidebar-start>
      <label class="toggle">
        <input v-model="hideOffline" type="checkbox" />
        Hide offline services
      </label>
      <a class="toggle" href="/status">Service status</a>
    </template>
  </ApiReference>
  <div v-else class="empty">
    <p v-if="!loaded">Loading…</p>
    <p v-else-if="hasServices">All services are offline.</p>
    <p v-else>No services with an OpenAPI spec discovered yet.</p>
    <label v-if="hasServices" class="toggle">
      <input v-model="hideOffline" type="checkbox" />
      Hide offline services
    </label>
    <a class="toggle" href="/status">Service status</a>
  </div>
</template>

<style scoped>
.empty { font-family: system-ui, sans-serif; color: #666; text-align: center; margin-top: 20vh; }
.toggle { display: flex; gap: 0.5rem; align-items: center; justify-content: center; padding: 0.5rem 1rem; font: 13px system-ui, sans-serif; cursor: pointer; }
</style>
