/**
 * Public path prefix the app is served under, with a trailing slash ("/" or "/docs/").
 * Derived from the URL so one build works at any prefix: the only app routes are `status` and `users`.
 */
export const BASE = location.pathname.replace(/(status|users)\/?$/, '').replace(/\/?$/, '/')

export type Route = 'docs' | 'status' | 'users'

export const route: Route = /\/status\/?$/.test(location.pathname)
  ? 'status'
  : /\/users\/?$/.test(location.pathname)
    ? 'users'
    : 'docs'
