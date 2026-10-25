<script setup lang="ts">
// The navigation of the Scalar pages (documentation, API reference). Plain CSS on purpose: these pages must not
// load PrimeVue or Tailwind. The PrimeVue pages have the same entries in their Menubar (shell/AppShell.vue).
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { BASE, route } from '../base'
import { useSession } from '../composables/useSession'

const { session, logout } = useSession()
const isAdmin = computed(() => session.value?.user?.role === 'admin')
const builtIn = computed(() => session.value?.mode === 'build_in' && !!session.value.user)

// The API page has two documents; the hash says which one is open (#events, #events/..., or #rest/...).
const hash = ref(location.hash)
const onHash = () => { hash.value = location.hash }
const eventsOpen = computed(() => route === 'api' && hash.value.startsWith('#events'))
const restOpen = computed(() => route === 'api' && !eventsOpen.value)

// On the API page itself, a link that only changes the hash does not make Scalar switch documents: set the hash and
// reload, so that Scalar opens the document the hash names. Elsewhere the link is a normal navigation.
function openDocument(e: MouseEvent, target: '#rest' | '#events') {
  if (route !== 'api') return
  e.preventDefault()
  history.replaceState(null, '', location.pathname + target)
  location.reload()
}

// Scalar offsets its sticky sidebar and headings by this variable.
const HEIGHT = '40px'
onMounted(() => {
  document.documentElement.style.setProperty('--scalar-custom-header-height', HEIGHT)
  window.addEventListener('hashchange', onHash)
})
onUnmounted(() => {
  document.documentElement.style.removeProperty('--scalar-custom-header-height')
  window.removeEventListener('hashchange', onHash)
})
</script>

<template>
  <nav class="appnav" aria-label="Main">
    <strong class="brand">Swagger Aggregator</strong>
    <a :href="BASE" :aria-current="route === 'docs' ? 'page' : undefined">Documentation</a>
    <a :href="`${BASE}api-docs#rest`" :aria-current="restOpen ? 'page' : undefined" @click="openDocument($event, '#rest')">API</a>
    <a :href="`${BASE}api-docs#events`" :aria-current="eventsOpen ? 'page' : undefined" title="The live event streams (AsyncAPI)" @click="openDocument($event, '#events')">Events</a>
    <a :href="`${BASE}status`">Status</a>
    <a v-if="isAdmin" :href="`${BASE}users`">Users</a>
    <span class="spacer" />
    <button v-if="builtIn" type="button" @click="logout">Sign out ({{ session?.user?.username }})</button>
  </nav>
</template>

<style scoped>
.appnav {
  position: fixed; top: 0; left: 0; right: 0; z-index: 1000; box-sizing: border-box; height: 40px;
  display: flex; align-items: center; gap: 1rem; padding: 0 1rem;
  font: 13px system-ui, sans-serif; background: #ffffff; color: #1b1b1b; border-bottom: 1px solid #0000001a;
}
@media (prefers-color-scheme: dark) {
  .appnav { background: #131313; color: #ffffffe6; border-bottom-color: #ffffff22; }
}
.brand { margin-right: 0.5rem; }
a, button { color: inherit; text-decoration: none; opacity: 0.75; background: none; border: 0; font: inherit; cursor: pointer; padding: 0; }
a:hover, button:hover, a[aria-current='page'] { opacity: 1; }
a[aria-current='page'] { font-weight: 600; text-decoration: underline; text-underline-offset: 6px; }
.spacer { flex: 1; }
</style>
