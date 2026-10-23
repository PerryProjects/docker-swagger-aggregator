import type { Service } from './composables/useServices'

export const slugify = (s: string) =>
  s.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '') || 'api'

/** Display order everywhere: group, then explicit order, then name. */
export const compareServices = (a: Service, b: Service) =>
  (a.group ?? '').localeCompare(b.group ?? '') ||
  (a.order ?? 0) - (b.order ?? 0) ||
  a.name.localeCompare(b.name)

/**
 * Unique, stable Scalar document slugs. `docs` must be the services that have a
 * spec, already sorted with compareServices. Computed over all of them (not the
 * visible subset) so a document keeps its slug when the list is filtered.
 */
export function assignSlugs(docs: readonly Service[]): Map<string, string> {
  const used = new Set<string>()
  const slugs = new Map<string, string>()
  for (const s of docs) {
    let slug = slugify(s.name)
    for (let i = 2; used.has(slug); i++) slug = `${slugify(s.name)}-${i}`
    used.add(slug)
    slugs.set(s.name, slug)
  }
  return slugs
}
