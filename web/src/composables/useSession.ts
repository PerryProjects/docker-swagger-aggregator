import { readonly, ref } from 'vue'
import { loadSession, logout as apiLogout, type SessionInfo } from '../api/auth'

const session = ref<SessionInfo | null>(null)

/** Who is signed in. `session` is null until the first answer arrived. */
export function useSession() {
  async function load() {
    session.value = await loadSession()
    return session.value
  }
  async function logout() {
    try {
      await apiLogout()
    } finally {
      location.reload() // back to a clean state: the app then shows the login form
    }
  }
  return { session: readonly(session), load, logout }
}
