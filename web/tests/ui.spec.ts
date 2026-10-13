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

test('telephony: line with Telekom preset, number, extension and ring group', async ({ page }) => {
  page.on('dialog', (d) => void d.accept())
  await loginAs(page, admin)
  await page.getByRole('link', { name: 'Telephony' }).click()
  await expect(page.getByRole('heading', { name: 'Telephony' })).toBeVisible()

  // Line: the preset fills registrar, transport and SRTP.
  await page.getByRole('button', { name: 'Add line' }).click()
  const dlg = page.getByRole('dialog')
  await dlg.getByRole('button', { name: 'Deutsche Telekom' }).click()
  await expect(dlg.getByLabel(/^Registrar/)).toHaveValue('tel.t-online.de')
  await dlg.getByLabel(/^Name/).fill('telekom2')
  await dlg.getByLabel(/^Username/).fill('someone@t-online.de')
  await dlg.getByLabel(/^Password/).fill('secret')
  await dlg.getByRole('button', { name: 'Save' }).click()
  await expect(dlg).toBeHidden()
  const card = page.locator('section.card', { has: page.getByRole('heading', { name: 'telekom2' }) })
  await expect(card.locator('.badge', { hasText: 'Telekom' })).toBeVisible()

  // Number in national format is normalized; an invalid one shows a field error.
  await card.getByRole('button', { name: 'Number' }).click()
  await dlg.getByLabel(/^Phone number/).fill('abc')
  await dlg.getByRole('button', { name: 'Save' }).click()
  await expect(dlg.locator('.error')).toBeVisible()
  await dlg.getByLabel(/^Phone number/).fill('0201 7654321')
  await dlg.getByLabel(/^Label/).fill('Office')
  await dlg.getByRole('button', { name: 'Save' }).click()
  await expect(dlg).toBeHidden()
  await expect(card.getByText('Office')).toBeVisible()
  const tel = await (await page.request.get('/api/v1/telephony')).json()
  const line = tel.trunks.find((t: { name: string }) => t.name === 'telekom2')
  expect(line.numbers.map((n: { number: string }) => n.number)).toContain('+492017654321')
  expect(line.password_set).toBe(true)
  expect(line.password ?? '').toBe('')

  // Extension: emergency numbers are refused; the username is suggested from the name.
  await page.getByRole('tab', { name: 'Extensions' }).click()
  await page.getByRole('button', { name: 'Add extension' }).click()
  await dlg.getByLabel(/^Extension number/).fill('112')
  await dlg.getByLabel(/^Name/).fill('Büro Süd')
  await expect(dlg.getByLabel(/^SIP username/)).toHaveValue('buro-sud')
  await dlg.getByRole('button', { name: 'Save' }).click()
  await expect(dlg.locator('.error')).toBeVisible()
  await dlg.getByLabel(/^Extension number/).fill('42')
  await dlg.getByRole('button', { name: 'Save' }).click()
  await expect(dlg).toBeHidden()
  const row = page.locator('tbody tr', { hasText: 'Büro Süd' })
  await expect(row).toBeVisible()

  // Generated password is shown in the phone setup dialog.
  await row.getByRole('button', { name: 'Credentials' }).click()
  await expect(dlg.getByText('buro-sud').first()).toBeVisible()
  const pw = await dlg.locator('dd code').last().textContent()
  expect(pw?.length).toBeGreaterThanOrEqual(16)
  await dlg.getByRole('button', { name: 'Done' }).click()

  // Do not disturb toggle.
  await row.getByLabel('Do not disturb for 42').check()
  await expect(row.getByText('on', { exact: true })).toBeVisible()

  // Ring group with the new extension, then route the new number to it.
  await page.getByRole('tab', { name: 'Ring groups' }).click()
  await page.getByRole('button', { name: 'Add ring group' }).click()
  await dlg.getByLabel(/^Name/).fill('Front desk')
  await dlg.getByText('Büro Süd').click()
  await dlg.getByRole('button', { name: 'Save' }).click()
  await expect(dlg).toBeHidden()
  const group = page.locator('section.card', { has: page.getByRole('heading', { name: 'Front desk' }) })
  await expect(group.getByText('42 Büro Süd')).toBeVisible()

  await page.getByRole('tab', { name: 'Lines' }).click()
  await card.getByRole('button', { name: 'Edit +492017654321' }).click()
  await dlg.getByLabel(/^Calls ring/).selectOption({ label: 'Front desk' })
  await dlg.getByRole('button', { name: 'Save' }).click()
  await expect(dlg).toBeHidden()
  await page.getByRole('tab', { name: 'Ring groups' }).click()
  await expect(group.getByText('02017654321')).toBeVisible()

  // A ring group that numbers still use cannot be deleted.
  await group.getByRole('button', { name: 'Delete Front desk' }).click()
  await expect(page.locator('.toast', { hasText: 'numbers still ring this group' })).toBeVisible()

  // Clean up the line (numbers go with it), then the group and extension.
  await page.getByRole('tab', { name: 'Lines' }).click()
  await card.getByRole('button', { name: 'Delete line telekom2' }).click()
  await expect(card).toHaveCount(0)
  await page.getByRole('tab', { name: 'Ring groups' }).click()
  await group.getByRole('button', { name: 'Delete Front desk' }).click()
  await expect(group).toHaveCount(0)
  await page.getByRole('tab', { name: 'Extensions' }).click()
  await row.getByRole('button', { name: 'Delete 42' }).click()
  await expect(row).toHaveCount(0)
})

test('users cannot open telephony settings', async ({ page }) => {
  await loginAs(page, alice)
  await expect(page.getByRole('link', { name: 'Telephony' })).toHaveCount(0)
  expect((await page.request.get('/api/v1/telephony')).status()).toBe(403)
})

test('blocking: status, test a number, add, pause and anonymous switch', async ({ page }) => {
  page.on('dialog', (d) => void d.accept())
  await loginAs(page, admin)
  await page.getByRole('link', { name: 'Blocking' }).click()
  const status = page.locator('section.card.status')
  await expect(status).toContainText('Blocking is active')
  const scr = await (await page.request.get('/api/v1/screening')).json()
  await expect(status).toContainText(`${scr.sizes.block.toLocaleString()} blocked numbers`)
  await expect(page.locator('section.card', { has: page.getByRole('heading', { name: 'PhoneBlock' }) })).toContainText('synced')

  // Test a number from the PhoneBlock range.
  await page.getByLabel('Test a number').fill('030 120045 777')
  await page.getByRole('button', { name: 'Test number' }).click()
  await expect(page.getByRole('status').filter({ hasText: 'Blocked' })).toContainText('PhoneBlock')

  // Add a number, find it, remove it.
  await page.getByRole('tab', { name: 'Numbers' }).click()
  await page.getByRole('button', { name: 'Add number' }).click()
  const dlg = page.getByRole('dialog')
  await dlg.getByLabel(/^Number/).fill('0201 99*')
  await dlg.getByRole('button', { name: 'Add' }).click()
  await expect(dlg.locator('.error')).toBeVisible() // range too short
  await dlg.getByLabel(/^Number/).fill('0176 55554444')
  await dlg.getByLabel(/^Comment/).fill('Fake parcel service')
  await dlg.getByRole('button', { name: 'Add' }).click()
  await expect(dlg).toBeHidden()
  await page.getByPlaceholder('Search number or name').fill('55554444')
  const row = page.locator('tbody tr', { hasText: 'Fake parcel service' })
  await expect(row).toHaveCount(1)
  await expect(row).toContainText('017655554444')
  await row.getByRole('button', { name: /Remove/ }).click()
  await expect(row).toHaveCount(0)

  // Pause and resume.
  await page.getByRole('button', { name: '5 min' }).click()
  await expect(status).toContainText('Blocking is paused')
  await page.getByRole('button', { name: 'Resume blocking' }).click()
  await expect(status).toContainText('Blocking is active')

  // Anonymous switch is saved.
  const anon = page.getByLabel(/Block anonymous callers/)
  const before = await anon.isChecked()
  await anon.click()
  await expect
    .poll(async () => (await (await page.request.get('/api/v1/screening')).json()).settings.anonymous_block)
    .toBe(!before)
  await anon.click()
  await expect
    .poll(async () => (await (await page.request.get('/api/v1/screening')).json()).settings.anonymous_block)
    .toBe(before)

  // PhoneBlock preset in the add-list dialog.
  await page.getByRole('tab', { name: 'Lists' }).click()
  await page.getByRole('button', { name: 'Add list' }).click()
  await dlg.getByRole('button', { name: 'PhoneBlock' }).click()
  await expect(dlg.getByLabel(/^URL/)).toHaveValue('https://phoneblock.net/phoneblock/contacts/')
  await expect(dlg.getByLabel(/^CardDAV token/)).toBeVisible()
  await dlg.getByRole('button', { name: 'Cancel' }).click()
})

test('blocking: import a CSV file as a new list', async ({ page }) => {
  page.on('dialog', (d) => void d.accept())
  await loginAs(page, admin)
  await page.goto('/blocking?tab=import')
  const csv = 'number;comment\n0170 1111111;Spam A\n0170 2222222;Spam B\n0170 2222222;dup\nkeine nummer;x\n040 1234*;Range\n'
  await page.getByLabel('File', { exact: true }).setInputFiles({ name: 'spam-list.csv', mimeType: 'text/csv', buffer: Buffer.from(csv) })
  await expect(page.getByLabel('Name of the new list')).toHaveValue('spam-list')
  await page.getByRole('button', { name: 'Check file' }).click()
  const preview = page.locator('.preview')
  await expect(preview).toContainText('3valid numbers')
  await expect(preview).toContainText('1duplicates in the file')
  await expect(preview).toContainText('1not usable')
  await page.getByRole('button', { name: 'Import 3 numbers' }).click()
  await expect(page.locator('.toast', { hasText: '3 numbers imported' })).toBeVisible()

  await page.getByRole('tab', { name: 'Lists' }).click()
  const card = page.locator('section.card', { has: page.getByRole('heading', { name: 'spam-list' }) })
  await expect(card).toContainText('3 numbers')
  await card.getByRole('button', { name: 'Delete spam-list' }).click()
  await expect(card).toHaveCount(0)
})

test('call details: block or allow the caller', async ({ page }) => {
  await loginAs(page, admin)
  const res = await (await page.request.get('/api/v1/calls?status=answered&direction=inbound&limit=200')).json()
  const call = res.calls.find((c: { caller: string }) => c.caller === '+441314960001')
  expect(call).toBeTruthy()
  await page.goto(`/calls/${call.id}`)
  await page.getByRole('button', { name: 'Block number' }).click()
  await expect(page.locator('.qa .badge', { hasText: 'Blocked (My blocklist)' })).toBeVisible()
  await page.getByRole('button', { name: 'Always allow' }).click()
  await expect(page.locator('.qa .badge', { hasText: 'Allowed' })).toBeVisible()

  // Clean up both entries.
  for (const target of ['block', 'allow']) {
    const list = await (await page.request.get(`/api/v1/lists/entries?target=${target}&q=1314960001`)).json()
    for (const e of list.entries) {
      const me = await (await page.request.get('/api/v1/me')).json()
      await page.request.delete(`/api/v1/lists/entries/${e.id}`, { headers: { 'X-CSRF-Token': me.csrf_token } })
    }
  }
})

test('call log: block a caller from the row menu', async ({ page }) => {
  await loginAs(page, admin)
  await page.goto('/calls?q=1314960001')
  const row = page.locator('tbody tr').first()
  await row.getByRole('button', { name: 'Block or allow caller' }).click()
  await row.getByRole('button', { name: 'Block number' }).click()
  await expect(row.locator('.qa .badge', { hasText: 'Blocked (My blocklist)' })).toBeVisible()
  await expect(page).toHaveURL(/\/calls\?q=/) // the click did not open the call

  const me = await (await page.request.get('/api/v1/me')).json()
  const list = await (await page.request.get('/api/v1/lists/entries?target=block&q=1314960001')).json()
  expect(list.entries.length).toBe(1)
  await page.request.delete(`/api/v1/lists/entries/${list.entries[0].id}`, { headers: { 'X-CSRF-Token': me.csrf_token } })
})

test('contacts: add, search, edit and delete', async ({ page }) => {
  page.on('dialog', (d) => void d.accept())
  await loginAs(page, admin)
  await page.getByRole('link', { name: 'Contacts' }).click()
  await expect(page.locator('tbody tr', { hasText: 'Grandma' })).toBeVisible()

  await page.getByRole('button', { name: 'Add contact' }).click()
  const dlg = page.getByRole('dialog')
  await dlg.getByLabel(/^Name/).fill('Uncle Bob')
  await dlg.getByLabel('Number 1', { exact: true }).fill('0171 2468024')
  await dlg.getByRole('button', { name: '+ Add number' }).click()
  await dlg.getByLabel('Number 2', { exact: true }).fill('not a number')
  await dlg.getByRole('button', { name: 'Save' }).click()
  await expect(dlg.locator('.num-row .error')).toBeVisible()
  await dlg.getByLabel('Number 2', { exact: true }).fill('0201 556677')
  await dlg.getByLabel('Type of number 2').selectOption('home')
  await dlg.getByRole('button', { name: 'Save' }).click()
  await expect(dlg).toBeHidden()

  await page.getByLabel('Search contacts').fill('556677')
  const row = page.locator('tbody tr', { hasText: 'Uncle Bob' })
  await expect(row).toContainText('01712468024')
  await expect(page.locator('tbody tr')).toHaveCount(1)

  const check = await (await page.request.get('/api/v1/screening/check?number=01712468024')).json()
  expect(check.decision.name).toBe('Uncle Bob')

  await row.getByRole('button', { name: 'Delete Uncle Bob' }).click()
  await expect(row).toHaveCount(0)
})
