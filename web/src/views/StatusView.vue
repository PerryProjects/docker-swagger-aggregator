<script setup lang="ts">
import { computed, onScopeDispose, ref } from 'vue'
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
  <main class="status">
    <header>
      <h1>Service status</h1>
      <a :href="BASE">Documentation</a>
    </header>

    <p v-if="!loaded" role="status">Loading…</p>
    <p v-else-if="!services.length" role="status">No services discovered yet.</p>
    <template v-else>
      <p class="summary" role="status">{{ up }} up / {{ down }} down</p>
      <table>
        <thead>
          <tr><th>Service</th><th>Status</th><th>Last seen</th><th>Problem</th><th></th></tr>
        </thead>
        <tbody>
          <tr v-for="s in sorted" :key="s.name" :data-service="s.name">
            <td>{{ s.group ? `${s.group} / ` : '' }}{{ s.name }}</td>
            <td :class="s.status"><span aria-hidden="true">{{ s.status === 'up' ? '🟢' : '🔴' }}</span> {{ s.status }}</td>
            <td>{{ s.status === 'up' ? 'now' : ago(s.lastSeen) }}</td>
            <td class="problem">{{ s.error ?? '' }}</td>
            <td><a v-if="s.specHash" :href="`${BASE}#${slugs.get(s.name)}`">Open docs</a></td>
          </tr>
        </tbody>
      </table>
    </template>
  </main>
</template>

<style scoped>
.status {
  --fg: #1b1b1b; --muted: #6d6d6d; --line: #0000001a; --bg: #f9f9f9; --link: #0071bb;
  font: 15px/1.5 system-ui, sans-serif; color: var(--fg); background: var(--bg);
  min-height: 100%; box-sizing: border-box; padding: 1.5rem clamp(1rem, 4vw, 3rem);
}
@media (prefers-color-scheme: dark) {
  .status { --fg: #ffffffe6; --muted: #ffffff9e; --line: #ffffff22; --bg: #131313; --link: #4eb3ec; }
}
header { display: flex; justify-content: space-between; align-items: baseline; gap: 1rem; }
h1 { font-size: 1.4rem; margin: 0 0 1rem; }
a { color: var(--link); }
.summary { color: var(--muted); }
table { width: 100%; border-collapse: collapse; }
th, td { text-align: left; padding: 0.5rem 0.75rem; border-bottom: 1px solid var(--line); vertical-align: top; }
th { color: var(--muted); font-weight: 600; font-size: 0.85rem; }
.problem { color: var(--muted); overflow-wrap: anywhere; }
.down { font-weight: 600; }
@media (max-width: 640px) { th:nth-child(3), td:nth-child(3) { display: none; } }
</style>
