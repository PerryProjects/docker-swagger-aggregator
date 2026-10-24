/**
 * Public path prefix the app is served under, with a trailing slash ("/" or "/docs/").
 * Derived from the URL so one build works at any prefix: the app routes are `status`, `users` and `api-docs`.
 */
export const BASE = location.pathname.replace(/(status|users|api-docs)\/?$/, '').replace(/\/?$/, '/')

export type Route = 'docs' | 'status' | 'users' | 'api'

export const route: Route = /\/status\/?$/.test(location.pathname)
  ? 'status'
  : /\/users\/?$/.test(location.pathname)
    ? 'users'
    : /\/api-docs\/?$/.test(location.pathname)
      ? 'api'
      : 'docs'
