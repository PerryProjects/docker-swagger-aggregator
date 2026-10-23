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
  page.evaluate(() => ({ hash: location.hash, scrollY: Math.round(window.scrollY), top: document.body.innerText.split('\n')[0] }))

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

test.describe('status page', () => {
  const broken: Svc = { name: 'ghost', status: 'down', noSpec: true, error: 'container is not attached to network "swagger-aggregator"' }

  test('lists every service, including one without a spec, with its problem', async ({ page }) => {
    await set([alpha, { ...bravo, status: 'down' }, broken])
    await page.goto('/status')
    await expect(page.getByRole('status').filter({ hasText: '1 up / 2 down' })).toBeVisible()

    const ghost = page.locator('tr[data-service="ghost"]')
    await expect(ghost).toContainText('down') // text, not only a color
    await expect(ghost).toContainText('not attached to network')
    await expect(ghost).toContainText('never')
    await expect(ghost.getByRole('link', { name: 'Open docs' })).toHaveCount(0) // nothing to open

    await expect(page.locator('tr[data-service="bravo"]')).toContainText('down')
    await expect(page.locator('tr[data-service="alpha"]')).toContainText('up')
    await expect(page.locator('tr[data-service="alpha"]')).toContainText('now') // up services are "now", not a stale time
  })

  test('updates live when a service changes', async ({ page }) => {
    await page.goto('/status')
    await expect(page.getByRole('status').filter({ hasText: '3 up / 0 down' })).toBeVisible()
    await set([alpha, { ...bravo, status: 'down', error: 'unexpected status 503 Service Unavailable' }, charlie])
    await expect(page.locator('tr[data-service="bravo"]')).toContainText('503')
    await expect(page.getByRole('status').filter({ hasText: '2 up / 1 down' })).toBeVisible()
  })

  test('"Open docs" opens that document, and the docs link back to the status page', async ({ page }) => {
    await page.goto('/status')
    await page.locator('tr[data-service="bravo"]').getByRole('link', { name: 'Open docs' }).click()
    await expect(page.getByText('🟢 bravo').first()).toBeVisible()
    await expect.poll(() => page.url()).toContain('#bravo')

    await page.getByRole('link', { name: 'Service status' }).click()
    await expect(page).toHaveURL(/\/status$/)
    await expect(page.getByRole('heading', { name: 'Service status' })).toBeVisible()
  })
})
