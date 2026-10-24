import { expect, test, type Page } from '@playwright/test'

// The built-in authentication end to end: the real Go web server (auth=build_in, empty database) serves the
// real build in front of the mock API. The tests depend on each other (setup comes first), so they run in order.
test.describe.configure({ mode: 'serial' })
test.use({ baseURL: 'http://127.0.0.1:4174' })

const SETUP_TOKEN = 'e2e-setup-token'
const ADMIN = { name: 'admin', password: 'correct horse battery' }
const THROTTLE_MS = 1200 // a failed attempt delays the next one by 1 s

// PrimeVue messages carry role=alert (the setup page also has an info one): find them by their text
const alert = (page: Page, text: string) => page.getByRole('alert').filter({ hasText: text })

const wait = (ms: number) => new Promise((r) => setTimeout(r, ms))

async function signIn(page: Page, name: string, password: string) {
  await page.goto('/')
  await page.locator('#username').fill(name)
  await page.locator('#password').fill(password)
  await page.getByRole('button', { name: 'Sign in' }).click()
  // the login reloads the page: wait for the signed-in view before the caller navigates on
  await expect(page.getByRole('button', { name: /^Sign out \(/ })).toBeVisible()
}

test.beforeAll(async () => {
  await fetch('http://127.0.0.1:18081/__mock', { method: 'POST', body: JSON.stringify({ services: [{ name: 'alpha' }] }) })
})

test('a new instance asks for the setup token and creates the first admin', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByText('Create the first admin')).toBeVisible()
  expect(await page.locator('style[data-primevue-style-id]').count()).toBeGreaterThan(0)

  await page.locator('#setup-token').fill('not-the-token')
  await page.locator('#setup-password').fill(ADMIN.password)
  await page.locator('#setup-repeat').fill(ADMIN.password)
  await page.getByRole('button', { name: 'Create admin' }).click()
  await expect(alert(page, 'wrong setup token')).toBeVisible()

  await wait(THROTTLE_MS)
  await page.locator('#setup-token').fill(SETUP_TOKEN)
  await page.getByRole('button', { name: 'Create admin' }).click()
  // signed in: the documentation shows, with the account links in its sidebar
  await expect(page.getByRole('button', { name: 'Sign out (admin)' })).toBeVisible()
  await expect(page.getByRole('link', { name: 'Users' })).toBeVisible()
})

test('the API is closed without a session, the UI files are not', async ({ request }) => {
  expect((await request.get('/api/services')).status()).toBe(401)
  expect((await request.get('/api/events')).status()).toBe(401)
  expect((await request.get('/healthz')).status()).toBe(200)
  const info = await (await request.get('/auth/session')).json()
  expect(info).toMatchObject({ mode: 'build_in', setupRequired: false })
  expect(info.user).toBeUndefined()
})

test('signing out shows the login; a wrong password is refused with a message', async ({ page }) => {
  await signIn(page, ADMIN.name, ADMIN.password)
  await page.getByRole('button', { name: 'Sign out (admin)' }).click()
  await expect(page.getByText('Sign in', { exact: true }).first()).toBeVisible()

  await page.locator('#username').fill(ADMIN.name)
  await page.locator('#password').fill('wrong password!')
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(alert(page, 'wrong username or password')).toBeVisible()
  await wait(THROTTLE_MS)
  await page.locator('#password').fill(ADMIN.password)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.getByRole('button', { name: 'Sign out (admin)' })).toBeVisible()
})

test('the status page shows the services and the user menu', async ({ page }) => {
  await signIn(page, ADMIN.name, ADMIN.password)
  await page.goto('/status')
  await expect(page.locator('[data-service="alpha"]')).toBeVisible()
  await expect(page.getByTestId('current-user')).toHaveText('admin')
  await expect(page.getByRole('link', { name: 'Users' })).toBeVisible()
})

test('an admin creates a user; that user can read the docs but not manage users', async ({ page }) => {
  await signIn(page, ADMIN.name, ADMIN.password)
  await page.goto('/users')
  await page.getByRole('button', { name: 'New user' }).click()
  await page.locator('#user-name').fill('dave')
  await page.locator('#user-password').fill('dave-password-1')
  await page.getByRole('button', { name: 'Create', exact: true }).click()
  await expect(page.getByRole('row', { name: /dave/ })).toContainText('user')

  // creating the same name again is refused in the dialog
  await page.getByRole('button', { name: 'New user' }).click()
  await page.locator('#user-name').fill('Dave')
  await page.locator('#user-password').fill('dave-password-1')
  await page.getByRole('button', { name: 'Create', exact: true }).click()
  await expect(alert(page, 'username is taken')).toBeVisible()
  await page.getByRole('button', { name: 'Cancel' }).click()

  await page.goto('/')
  await page.getByRole('button', { name: 'Sign out (admin)' }).click()
  await page.locator('#username').fill('dave')
  await page.locator('#password').fill('dave-password-1')
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.getByRole('button', { name: 'Sign out (dave)' })).toBeVisible()
  await expect(page.getByRole('link', { name: 'Users' })).toHaveCount(0)

  await page.goto('/users') // not for plain users: sent back to the documentation
  await expect(page).not.toHaveURL(/\/users$/)
  expect((await page.request.get('/auth/users')).status()).toBe(403)
  expect((await page.request.get('/api/services')).status()).toBe(200)
})

test('an admin disables and deletes a user, who is signed out at once', async ({ page, browser }) => {
  const dave = await browser.newPage({ baseURL: 'http://127.0.0.1:4174' })
  await signIn(dave, 'dave', 'dave-password-1')
  await expect(dave.getByRole('button', { name: 'Sign out (dave)' })).toBeVisible()

  await signIn(page, ADMIN.name, ADMIN.password)
  await page.goto('/users')
  await expect(page.getByRole('button', { name: 'Delete admin' })).toBeDisabled() // not yourself
  await page.getByRole('button', { name: 'Edit dave' }).click()
  await page.locator('#user-disabled').click({ force: true })
  await page.getByRole('button', { name: 'Save' }).click()
  await expect(page.getByRole('row', { name: /dave/ })).toContainText('disabled')
  expect((await dave.request.get('/api/services')).status()).toBe(401) // dave's session ended

  await page.getByRole('button', { name: 'Delete dave' }).click()
  await page.getByRole('button', { name: 'Delete', exact: true }).click()
  await expect(page.getByRole('row', { name: /dave/ })).toHaveCount(0)
  await dave.close()
})

test('changing the own password keeps this browser signed in and works for the next login', async ({ page }) => {
  await signIn(page, ADMIN.name, ADMIN.password)
  await page.goto('/status')
  await page.getByRole('button', { name: 'Account menu' }).click()
  await page.getByRole('menuitem', { name: 'Change password' }).click()
  await page.locator('#current-password').fill(ADMIN.password)
  await page.locator('#new-password').fill('a brand new password')
  await page.locator('#repeat-password').fill('a brand new password')
  await page.getByRole('button', { name: 'Change password' }).last().click()
  await expect(page.getByText('Password changed')).toBeVisible()
  expect((await page.request.get('/api/services')).status()).toBe(200)

  await page.getByRole('button', { name: 'Account menu' }).click()
  await page.getByRole('menuitem', { name: 'Sign out' }).click()
  await page.locator('#username').fill(ADMIN.name)
  await page.locator('#password').fill('a brand new password')
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.getByTestId('current-user')).toHaveText('admin')
})

test('the navigation reaches every page and the API page documents REST, the event stream and MCP', async ({ page }) => {
  await signIn(page, ADMIN.name, 'a brand new password')
  const nav = page.getByRole('navigation', { name: 'Main' })
  await expect(nav.getByRole('link', { name: 'Users' })).toBeVisible()

  await nav.getByRole('link', { name: 'API' }).click()
  await expect(page).toHaveURL(/\/api-docs(#.*)?$/)
  await expect(page.getByRole('heading', { name: 'Swagger Aggregator HTTP API' })).toBeVisible()
  const sidebar = page.getByRole('complementary')
  await expect(sidebar.getByText('Events', { exact: true })).toBeVisible() // the SSE events
  await expect(sidebar.getByText('MCP', { exact: true })).toBeVisible()

  // the raw documents are public and valid JSON, so a custom frontend can fetch them
  const res = await page.request.get('/spec/openapi.json')
  expect(res.status()).toBe(200)
  expect((await res.json()).openapi).toBe('3.2.0')
  expect((await (await page.request.get('/spec/asyncapi.json')).json()).asyncapi).toBe('3.1.0')

  // the AsyncAPI document describes both event streams
  await page.getByText('REST API (OpenAPI)').first().click()
  await page.getByText('Events (AsyncAPI)').first().click()
  await expect(page.getByRole('heading', { name: 'Swagger Aggregator events' })).toBeVisible()
  await expect(page.getByRole('complementary').getByText('User list').first()).toBeVisible()

  await page.getByRole('navigation', { name: 'Main' }).getByRole('link', { name: 'Status' }).click()
  await expect(page.getByTestId('current-user')).toHaveText('admin') // the PrimeVue menubar of the status page
  await page.getByRole('menubar').getByRole('menuitem', { name: 'API' }).click()
  await expect(page).toHaveURL(/\/api-docs(#.*)?$/)
})

test('the user list follows changes live (SSE): a user created in another browser shows up without a reload', async ({ page, browser }) => {
  await signIn(page, ADMIN.name, 'a brand new password')
  await page.goto('/users')
  await expect(page.getByTestId('live')).toContainText('Live') // the event stream is connected
  await expect(page.getByRole('row', { name: /zed/ })).toHaveCount(0)

  const other = await browser.newPage({ baseURL: 'http://127.0.0.1:4174' })
  await signIn(other, ADMIN.name, 'a brand new password')
  const created = await other.request.post('/auth/users', { data: { username: 'zed', password: 'zed-password-12' } })
  expect(created.status()).toBe(201)
  await expect(page.getByRole('row', { name: /zed/ })).toBeVisible() // no reload, no click

  const id = (await (await other.request.get('/auth/users')).json()).find((u: { username: string }) => u.username === 'zed').id
  expect((await other.request.patch(`/auth/users/${id}`, { data: { disabled: true } })).status()).toBe(200)
  await expect(page.getByRole('row', { name: /zed/ })).toContainText('disabled')
  expect((await other.request.delete(`/auth/users/${id}`)).status()).toBe(204)
  await expect(page.getByRole('row', { name: /zed/ })).toHaveCount(0)
  await other.close()
})

// A separate frontend on another origin (e2e/custom-frontend, port 4175) against the Go server (4174), whose CORS
// list names exactly http://127.0.0.1:4175.
test.describe('a frontend on another origin', () => {
  const api = 'http://127.0.0.1:4174'
  type Result = { login?: number; services?: string[] | number; event?: string[] | string; error?: string }
  const attempt = (page: Page) =>
    page.evaluate(([a, u, p]) => (window as unknown as { attempt: (...x: string[]) => Promise<Result> }).attempt(a, u, p),
      [api, ADMIN.name, 'a brand new password'])

  test('a listed origin can sign in, read the API and follow the event stream with credentials', async ({ page }) => {
    await page.goto('http://127.0.0.1:4175/')
    const result = await attempt(page)
    expect(result).toEqual({ login: 200, services: ['alpha'], event: ['alpha'] })
  })

  test('an origin that is not listed is blocked by the browser, and the server answers its preflight with 403', async ({ page, request }) => {
    await page.goto('http://localhost:4175/') // another origin than the one in the list
    const result = await attempt(page)
    expect(result.error).toMatch(/Failed to fetch|NetworkError|Load failed/)
    expect(result.login).toBeUndefined()

    const preflight = await request.fetch(`${api}/auth/login`, {
      method: 'OPTIONS',
      headers: { Origin: 'http://localhost:4175', 'Access-Control-Request-Method': 'POST', 'Access-Control-Request-Headers': 'content-type' },
    })
    expect(preflight.status()).toBe(403)
    expect(preflight.headers()['access-control-allow-origin']).toBeUndefined()
  })

  test('a changing request from an unlisted origin is refused by the server, one without Origin is not', async ({ request }) => {
    const refused = await request.post(`${api}/auth/logout`, { headers: { Origin: 'http://evil.example' } })
    expect(refused.status()).toBe(403)
    const fine = await request.post(`${api}/auth/logout`) // curl, a server: no Origin header
    expect(fine.status()).toBe(204)
  })
})
