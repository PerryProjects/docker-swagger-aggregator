import { BASE } from '../base'

export type Role = 'admin' | 'user'

export interface User {
  id: number
  username: string
  role: Role
  disabled: boolean
}

export interface SessionInfo {
  mode: 'none' | 'oidc' | 'build_in'
  setupRequired: boolean
  user?: User
}

/** A failed request: the status plus the server's message (or a generic one). */
export class ApiError extends Error {
  constructor(public status: number, message: string, public retryAfter?: number) {
    super(message)
  }
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(`${BASE}auth/${path}`, {
    method,
    headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (res.ok) return (res.status === 204 ? undefined : await res.json()) as T
  let message = `Request failed (${res.status})`
  try {
    message = (await res.json()).error ?? message
  } catch { /* no JSON body */ }
  const retry = Number(res.headers.get('Retry-After'))
  throw new ApiError(res.status, message, retry > 0 ? retry : undefined)
}

/**
 * Which authentication is active and who is signed in. A server without the /auth endpoints (auth=none or
 * oidc, or the e2e mock) answers 404 or something that is not JSON: that means "nothing to log in to".
 */
export async function loadSession(): Promise<SessionInfo> {
  try {
    const res = await fetch(`${BASE}auth/session`)
    if (res.ok && res.headers.get('content-type')?.includes('application/json')) return await res.json()
  } catch { /* backend unreachable: behave as without auth */ }
  return { mode: 'none', setupRequired: false }
}

export const login = (username: string, password: string) =>
  call<{ user: User }>('POST', 'login', { username, password })

export const setup = (token: string, username: string, password: string) =>
  call<{ user: User }>('POST', 'setup', { token, username, password })

export const logout = () => call<void>('POST', 'logout')

export const changePassword = (current: string, next: string) =>
  call<void>('POST', 'password', { current, new: next })

export const listUsers = () => call<User[]>('GET', 'users')

export const createUser = (username: string, password: string, role: Role) =>
  call<User>('POST', 'users', { username, password, role })

export const updateUser = (id: number, patch: { password?: string; role?: Role; disabled?: boolean }) =>
  call<User>('PATCH', `users/${id}`, patch)

export const deleteUser = (id: number) => call<void>('DELETE', `users/${id}`)
