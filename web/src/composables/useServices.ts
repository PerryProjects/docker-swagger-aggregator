import { onScopeDispose, readonly, ref } from 'vue'
import { BASE } from '../base'

export interface Service {
  name: string
  status: 'up' | 'down'
  lastSeen?: string
  specHash?: string
  error?: string
  group?: string
  order?: number
}

const POLL_MS = 15_000
const RECONNECT_MS = 5_000

/**
 * Service list: initial fetch + live updates over SSE. If the stream is down
 * (or the browser gave up reconnecting), fall back to polling every 15s and
 * retry the stream.
 */
export function useServices() {
  const services = ref<Service[]>([])
  const loaded = ref(false) // true once the first list has arrived
  const live = ref(false) // true while the SSE stream is connected

  let es: EventSource | undefined
  let reconnect: ReturnType<typeof setTimeout> | undefined
  let last = ''

  function apply(list: Service[]) {
    const json = JSON.stringify(list)
    if (json !== last) { // skip no-op updates so nothing downstream re-renders
      last = json
      services.value = list
    }
    loaded.value = true
  }

  async function poll() {
    try {
      const res = await fetch(`${BASE}api/services`)
      if (res.ok) apply(await res.json())
    } catch { /* backend unreachable; try again next tick */ }
  }

  function connect() {
    es = new EventSource(`${BASE}api/events`)
    es.onopen = () => { live.value = true }
    es.addEventListener('services', (e) => apply(JSON.parse((e as MessageEvent).data)))
    es.onerror = () => {
      live.value = false
      // CONNECTING = the browser retries by itself. CLOSED = it gave up (e.g. HTTP error).
      if (es?.readyState === EventSource.CLOSED) {
        es.close()
        reconnect = setTimeout(connect, RECONNECT_MS)
      }
    }
  }

  poll()
  connect()
  const timer = setInterval(() => { if (!live.value) poll() }, POLL_MS)

  onScopeDispose(() => {
    clearInterval(timer)
    clearTimeout(reconnect)
    es?.close()
  })

  return { services: readonly(services), loaded: readonly(loaded), live: readonly(live) }
}
