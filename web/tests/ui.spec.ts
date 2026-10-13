import { expect, test, type Page } from '@playwright/test'

const idp = process.env.IDP_URL ?? 'http://127.0.0.1:18090'

async function loginAs(page: Page, user: { subject: string; username: string; name: string; groups: string[] }) {
  const res = await page.request.post(`${idp}/_test/user`, { data: user })
  expect(res.status()).toBe(204)
  await page.goto('/')
  await page.getByRole('link', { name: 'Sign in with authentik' }).click()
}

const admin = { subject: 'root', username: 'root', name: 'Root Admin', groups: ['voipd-admins'] }
const alice = { subject: 'alice', username: 'alice', name: 'Alice', groups: ['voipd-users'] }

test.beforeEach(async ({ page }) => {
  page.on('pageerror', (e) => {
    throw e
  })
})

test('admin dashboard shows the same numbers as the API', async ({ page }) => {
  await loginAs(page, admin)
  await expect(page.getByRole('heading', { name: 'Dashboard' })).toBeVisible()

  for (const [button, range] of [
    ['24 h', '24h'],
    ['30 days', '30d'],
  ] as const) {
    await page.getByRole('button', { name: button, exact: true }).click()
    await expect(page).toHaveURL(new RegExp(`range=${range}`))
    const api = await (await page.request.get(`/api/v1/stats/dashboard?range=${range}`)).json()
    const tile = page.locator('.tile', { hasText: 'Total calls' })
    await expect(tile.locator('.value')).toHaveText(api.summary.total.toLocaleString())
    const blocked = page.locator('.tile', { hasText: 'Calls blocked' })
    await expect(blocked.locator('.value')).toHaveText(api.summary.blocked.toLocaleString())
  }
  // Chart and donut rendered with data.
  await expect(page.locator('svg[aria-label="Calls over time"] rect').first()).toBeVisible()
  await expect(page.getByText('Top blocked callers')).toBeVisible()
  await expect(page.locator('.side-foot')).toContainText('Live')
})

test('call log filters and call details', async ({ page }) => {
  await loginAs(page, admin)
  await page.getByRole('link', { name: 'Calls', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'Call log' })).toBeVisible()

  await page.getByLabel('Outcome').selectOption({ label: 'Blocked' })
  await expect(page).toHaveURL(/status=blocked/)
  const api = await (await page.request.get('/api/v1/calls?status=blocked&limit=50')).json()
  const rows = page.locator('tbody tr')
  await expect(rows).toHaveCount(api.calls.length)
  for (const badge of await rows.locator('.badge').allTextContents()) expect(badge).toBe('Blocked')

  await rows.first().click()
  await expect(page.getByRole('heading', { name: 'Call details' })).toBeVisible()
  await expect(page.getByText('Connections and quality')).toBeVisible()
  await expect(page.getByText('PhoneBlock').or(page.getByText('anonymous')).first()).toBeVisible()
  await page.getByRole('button', { name: 'Back' }).click()
  await expect(page).toHaveURL(/status=blocked/)
})

test('status page lists lines, phones and events', async ({ page }) => {
  await loginAs(page, admin)
  await page.getByRole('link', { name: 'Status' }).click()
  await expect(page.getByRole('heading', { name: 'Phones' })).toBeVisible()
  await expect(page.locator('td', { hasText: 'Kitchen' })).toBeVisible()
  await expect(page.getByText('Line registered').first()).toBeVisible()
  await expect(page.getByText('disconnected')).toBeVisible() // no Asterisk in this test
})

test('users only see their own calls and no admin pages', async ({ page }) => {
  await loginAs(page, alice)
  await expect(page.getByRole('heading', { name: 'My calls' })).toBeVisible()
  await expect(page.getByRole('link', { name: 'Dashboard' })).toHaveCount(0)
  await expect(page.getByRole('link', { name: 'Status' })).toHaveCount(0)

  const api = await (await page.request.get('/api/v1/calls?limit=50')).json()
  expect(api.calls.length).toBeGreaterThan(0)
  await expect(page.locator('tbody tr')).toHaveCount(api.calls.length)
  const adminOnly = await page.request.get('/api/v1/stats/dashboard')
  expect(adminOnly.status()).toBe(403)

  await page.goto('/status')
  await expect(page).toHaveURL(/\/calls$/)
})

test('theme toggle and sign out', async ({ page }) => {
  await loginAs(page, admin)
  await page.getByRole('button', { name: 'Toggle dark mode' }).click()
  const theme = await page.evaluate(() => document.documentElement.dataset['theme'])
  expect(['dark', 'light']).toContain(theme)
  await page.getByRole('button', { name: 'Sign out' }).click()
  await expect(page.getByRole('link', { name: 'Sign in with authentik' })).toBeVisible()
  const me = await page.request.get('/api/v1/me')
  expect(me.status()).toBe(401)
})
