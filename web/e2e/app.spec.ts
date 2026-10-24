import { expect, test, type Page } from '@playwright/test'

// These pin down behaviour of @scalar/api-reference that was found by reading its
// source and by manual runs (see README, "Frontend behaviour"). If a Scalar upgrade
// breaks one of them, the view regresses silently without these.

interface Svc {
  name: string; status?: 'up' | 'down'; hash?: string; group?: string; order?: number
  noSpec?: boolean; error?: string
}
const set = (services: Svc[]) =>
  fetch('http://127.0.0.1:18081/__mock', { method: 'POST', body: JSON.stringify({ services }) })

const alpha: Svc = { name: 'alpha' }
const bravo: Svc = { name: 'bravo' }
const charlie: Svc = { name: 'charlie' }

const state = (page: Page) =>
  page.evaluate(() => ({ hash: location.hash, scrollY: Math.round(window.scrollY), top: (() => { const nav = new Set((document.querySelector('nav.appnav') as HTMLElement | null)?.innerText.split('\n') ?? []); return document.body.innerText.split('\n').find((l) => l && !nav.has(l)) })() }))

async function openBravoScrolled(page: Page) {
  await page.goto('/')
  await page.getByText('🟢 alpha').first().click()
  await page.getByText('🟢 bravo').first().click()
  await expect.poll(async () => (await state(page)).hash).toContain('#bravo')
  await page.evaluate(() => window.scrollTo(0, 3000))
  await expect.poll(async () => (await state(page)).hash).toContain('bravo/tag/')
}

test.beforeEach(async () => { await set([alpha, bravo, charlie]) })

test('only same-origin requests: no Scalar CDN, fonts or telemetry', async ({ page }) => {
  const hosts = new Set<string>()
  page.on('request', (r) => hosts.add(new URL(r.url()).host))
  await page.goto('/')
  await expect(page.getByText('🟢 alpha').first()).toBeVisible()
  await page.waitForTimeout(1500)
  expect([...hosts]).toEqual(['127.0.0.1:4173'])
})

test('slug is per source and shows up in the URL hash', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByText('🟢 alpha').first()).toBeVisible()
  // Scalar writes the hash a moment after the first render
  await expect.poll(() => page.url()).toContain('#alpha')
})

test('status flips and spec updates keep the open document and scroll position', async ({ page }) => {
  await openBravoScrolled(page)
  const before = await state(page)

  await set([{ ...alpha, status: 'down' }, bravo, charlie]) // another service (not visible while the picker is closed)
  await page.waitForTimeout(500)
  await set([{ ...alpha, status: 'down' }, { ...bravo, status: 'down' }, charlie]) // the open one
  await expect(page.getByText('🔴 bravo').first()).toBeVisible()
  await set([{ ...alpha, status: 'down' }, { ...bravo, status: 'down', hash: 'b2' }, charlie]) // its spec changes
  await page.waitForTimeout(1000)

  const after = await state(page)
  expect(after.hash).toBe(before.hash)
  expect(after.scrollY).toBe(before.scrollY)
})

test('adding and removing services keeps the open document (remount restores it)', async ({ page }) => {
  await openBravoScrolled(page)

  await set([alpha, bravo, charlie, { name: 'delta' }])
  await page.waitForTimeout(1000) // let the remount settle
  expect((await state(page)).top).toBe('🟢 bravo')
  await expect.poll(async () => (await state(page)).hash).toContain('#bravo')

  await set([{ name: 'aardvark' }, alpha, bravo, charlie, { name: 'delta' }]) // new first document
  await page.waitForTimeout(1000)
  expect((await state(page)).top).toBe('🟢 bravo')
  await expect.poll(async () => (await state(page)).hash).toContain('#bravo')
})

test('removing the open service opens the first one with a clean hash', async ({ page }) => {
  await openBravoScrolled(page)
  await set([{ name: 'aardvark' }, alpha, charlie])
  await expect(page.getByText('🟢 aardvark').first()).toBeVisible()
  await expect.poll(async () => (await state(page)).hash).toMatch(/^#aardvark(?!.*bravo)/)
})

test('group and order control sorting and the title', async ({ page }) => {
  await set([{ name: 'zeta', group: 'a-group', order: 1 }, { name: 'omega', group: 'a-group', order: 0 }, alpha])
  await page.goto('/')
  await expect(page.getByText('🟢 alpha').first()).toBeVisible() // no group sorts first
  await page.getByText('🟢 alpha').first().click()
  const options = await page.getByText(/🟢 .*/).allInnerTexts()
  expect(options.join('|')).toContain('a-group / omega')
  expect(options.join('|').indexOf('omega')).toBeLessThan(options.join('|').indexOf('zeta'))
})

test('"Hide offline services" removes offline documents', async ({ page }) => {
  await set([alpha, { ...bravo, status: 'down' }])
  await page.goto('/')
  await expect(page.getByText('🟢 alpha').first()).toBeVisible()
  await page.getByText('🟢 alpha').first().click()
  await expect(page.getByText('🔴 bravo').first()).toBeVisible()
  await page.getByRole('option', { name: '🟢 alpha' }).click() // closes the dropdown, which would overlay the checkbox
  await page.getByLabel('Hide offline services').click() // the view remounts, so no check()
  await expect(page.getByLabel('Hide offline services')).toBeChecked()
  await expect(page.getByText('🔴 bravo')).toHaveCount(0)
})

test('Try it calls the same-origin proxy path and shows the response', async ({ page }) => {
  const proxied: string[] = []
  // Scalar wraps request URLs in <proxyUrl>?scalar_url=...; our proxy path is inside it.
  page.on('request', (r) => {
    const u = new URL(r.url())
    if (u.pathname === '/api/scalar-proxy') proxied.push(u.searchParams.get('scalar_url') ?? '')
  })
  await page.goto('/')
  await page.getByText('alpha get item 0').first().click()
  await page.getByRole('button', { name: /Test Request/i }).first().click()
  await page.getByRole('button', { name: /^Send/i }).first().click()
  await expect(page.getByText('proxiedFor', { exact: false }).first()).toBeVisible()
  expect(proxied[0]).toBe('http://127.0.0.1:4173/api/proxy/alpha/item0')
  await expect(page.getByText(/Failed to fetch/)).toHaveCount(0)
})

test('a service that never delivered a spec is listed with its problem, not hidden', async ({ page }) => {
  await set([alpha, { name: 'ghost', status: 'down', noSpec: true, error: 'container has no network with an IP address' }])
  await page.goto('/')
  const ghost = page.locator('li[data-service="ghost"]')
  await expect(ghost).toContainText('no spec')
  await expect(ghost).toContainText('no network with an IP address')
  await expect(page.getByRole('complementary', { name: 'Sidebar for alpha API' })).toBeVisible() // the documented service is unaffected
})

// The status page is a PrimeVue DataTable: a row is found through the marker inside its first cell.
const row = (page: Page, name: string) => page.locator('tr', { has: page.locator(`[data-service="${name}"]`) })
const expectSummary = async (page: Page, up: number, down: number) => {
  const summary = page.getByRole('status')
  await expect(summary).toContainText(`${up} up`)
  await expect(summary).toContainText(`${down} down`)
}

test('the documentation has a navigation to the other pages', async ({ page }) => {
  await page.goto('/')
  const nav = page.getByRole('navigation', { name: 'Main' })
  await expect(nav.getByRole('link', { name: 'Documentation' })).toHaveAttribute('aria-current', 'page')
  await expect(nav.getByRole('link', { name: 'API' })).toBeVisible()
  await expect(nav.getByRole('link', { name: 'Status' })).toBeVisible()
  await expect(nav.getByRole('link', { name: 'Users' })).toHaveCount(0) // auth=none has no users
})

// These run against `vite preview` and the mock API, i.e. without the Go web server: the UI does not need it.
test('the API page shows the own API incl. the event stream and MCP, served by Vite alone', async ({ page }) => {
  await page.goto('/api-docs')
  await expect(page.getByRole('heading', { name: 'Swagger Aggregator HTTP API' })).toBeVisible()
  const sidebar = page.getByRole('complementary')
  await expect(sidebar.getByText('Events', { exact: true })).toBeVisible()
  await expect(sidebar.getByText('MCP', { exact: true })).toBeVisible()
  await expect(page.getByText('AsyncAPI')).toHaveCount(0) // one document, no switcher
  const res = await page.request.get('/spec/openapi.json')
  expect(res.status()).toBe(200)
  expect(Object.keys((await res.json()).paths)).toEqual(expect.arrayContaining(['/api/events', '/mcp']))
})

test('the documentation view loads no PrimeVue or Tailwind styles', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByText('🟢 alpha').first()).toBeVisible()
  await expect(page.locator('style[data-primevue-style-id]')).toHaveCount(0)
  expect(await page.evaluate(() => getComputedStyle(document.body).margin)).toBe('0px')
})

test.describe('status page', () => {
  const broken: Svc = { name: 'ghost', status: 'down', noSpec: true, error: 'container is not attached to network "swagger-aggregator"' }

  test('lists every service, including one without a spec, with its problem', async ({ page }) => {
    await set([alpha, { ...bravo, status: 'down' }, broken])
    await page.goto('/status')
    await expectSummary(page, 1, 2)

    const ghost = row(page, 'ghost')
    await expect(ghost).toContainText('down') // text, not only a color
    await expect(ghost).toContainText('not attached to network')
    await expect(ghost).toContainText('never')
    await expect(ghost.getByRole('link', { name: 'Open docs' })).toHaveCount(0) // nothing to open

    await expect(row(page, 'bravo')).toContainText('down')
    await expect(row(page, 'alpha')).toContainText('up')
    await expect(row(page, 'alpha')).toContainText('now') // up services are "now", not a stale time
  })

  test('updates live when a service changes', async ({ page }) => {
    await page.goto('/status')
    await expectSummary(page, 3, 0)
    await set([alpha, { ...bravo, status: 'down', error: 'unexpected status 503 Service Unavailable' }, charlie])
    await expect(row(page, 'bravo')).toContainText('503')
    await expectSummary(page, 2, 1)
  })

  test('"Open docs" opens that document, and the docs link back to the status page', async ({ page }) => {
    await page.goto('/status')
    await row(page, 'bravo').getByRole('link', { name: 'Open docs' }).click()
    await expect(page.getByText('🟢 bravo').first()).toBeVisible()
    await expect.poll(() => page.url()).toContain('#bravo')

    await page.getByRole('navigation', { name: 'Main' }).getByRole('link', { name: 'Status' }).click()
    await expect(page).toHaveURL(/\/status$/)
    await expect(page.getByRole('heading', { name: 'Service status' })).toBeVisible()
  })
})
