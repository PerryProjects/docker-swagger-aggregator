/**
 * Public path prefix the app is served under, with a trailing slash ("/" or "/docs/").
 * Derived from the URL so one build works at any prefix: the only app route is `status`.
 */
export const BASE = location.pathname.replace(/status\/?$/, '').replace(/\/?$/, '/')
