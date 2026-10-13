import { createServer, type IncomingHttpHeaders } from 'node:http'
import { createHmac } from 'node:crypto'
import type { AddressInfo } from 'node:net'
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
  await dlg.getByRole('button', { name: 'PhoneBlock', exact: true }).click()
  await expect(dlg.getByLabel(/^API key/)).toBeVisible()
  await expect(dlg.getByLabel(/^URL/)).toHaveCount(0)
  await dlg.getByRole('button', { name: 'Cancel' }).click()
})

test('blocking: PhoneBlock API list', async ({ page }) => {
  page.on('dialog', (d) => void d.accept())
  const pbURL = process.env.PB_URL ?? 'http://127.0.0.1:18097'
  await loginAs(page, admin)
  await page.goto('/blocking?tab=lists')
  // The demo's CardDAV PhoneBlock list suggests switching to the API.
  await expect(page.getByText("PhoneBlock's address book may only contain part")).toBeVisible()

  await page.getByRole('button', { name: 'Add list' }).click()
  const dlg = page.getByRole('dialog')
  await dlg.getByRole('button', { name: 'PhoneBlock', exact: true }).click()
  await expect(dlg.getByText('Create an API key')).toBeVisible()
  await dlg.getByLabel('Name').fill('PhoneBlock API')
  await dlg.getByLabel('Block numbers from').selectOption('10')
  await dlg.getByRole('button', { name: 'Show advanced settings' }).click()
  await dlg.getByLabel('API URL').fill(`${pbURL}/phoneblock/api`)
  await dlg.getByRole('button', { name: 'Add and sync' }).click()
  await expect(dlg.getByLabel('API key')).toHaveJSProperty('validity.valueMissing', true)
  await dlg.getByLabel('API key').fill('pb-key')
  await dlg.getByRole('button', { name: 'Add and sync' }).click()

  const card = page.locator('section.card', { has: page.getByRole('heading', { name: 'PhoneBlock API' }) })
  await expect(card).toContainText('synced')
  await expect(card).toContainText('2,001 numbers in the PhoneBlock list')
  await expect(card).toContainText('blocked from 10 votes')
  // 20 votes: blocked; the generated numbers below 10 votes are not.
  await page.getByLabel('Test a number').fill('0170 99990000')
  await page.getByRole('button', { name: 'Test number' }).click()
  await expect(page.getByRole('status').filter({ hasText: 'Blocked' })).toContainText('PhoneBlock API')

  await page.goto('/blocking?tab=lists')
  await card.getByRole('button', { name: 'Delete PhoneBlock API' }).click()
  await expect(card).toHaveCount(0)
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

test('integrations: e-mail settings with field errors and a kept password', async ({ page }) => {
  await loginAs(page, admin)
  await page.getByRole('link', { name: 'Integrations' }).click()
  await page.getByRole('tab', { name: 'E-mail' }).click()
  const form = page.getByRole('form', { name: 'E-mail settings' })
  await form.getByLabel('Send e-mail notifications').check()
  await form.getByRole('button', { name: 'Save' }).click()
  await expect(form.getByText("enter the mail server's host name")).toBeVisible()
  await expect(form.getByText('enter a sender address')).toBeVisible()

  await form.getByLabel('SMTP server').fill('smtp.example.org')
  await form.getByLabel('Encryption').selectOption('tls')
  await expect(form.getByLabel('Port')).toHaveValue('465')
  await form.getByLabel('Username').fill('pbx')
  await form.getByLabel('Password').fill('mail-secret')
  await form.getByLabel('Sender').fill('Telefon <pbx@example.org>')
  await form.getByLabel('Missed calls also to').fill('family@example.org')
  await form.getByRole('button', { name: 'Save' }).click()
  await expect(page.getByText('E-mail notifications saved')).toBeVisible()
  await expect(page.locator('section.card .badge', { hasText: 'running' })).toBeVisible()

  await page.reload()
  await expect(form.getByLabel('Password')).toHaveAttribute('placeholder', /unchanged/)
  await expect(form.getByLabel('Missed calls also to')).toHaveValue('family@example.org')
  const mods = await (await page.request.get('/api/v1/modules')).json()
  const notify = mods.modules.find((m: { name: string }) => m.name === 'notify')
  expect(notify.config.password).toBeUndefined()
  expect(notify.config.password_set).toBe(true)

  // Status page lists the module; switch it off again.
  await page.getByRole('link', { name: 'Status' }).click()
  await expect(page.locator('section.card', { has: page.getByRole('heading', { name: 'Integrations' }) })).toContainText('E-mail')
  await page.getByRole('link', { name: 'Integrations' }).click()
  await page.getByRole('tab', { name: 'E-mail' }).click()
  await form.getByLabel('Send e-mail notifications').uncheck()
  await form.getByRole('button', { name: 'Save' }).click()
  await expect(page.getByText('E-mail notifications are off')).toBeVisible()
})

test('integrations: webhook with secret, test delivery and history', async ({ page }) => {
  page.on('dialog', (d) => void d.accept())
  const got: { headers: IncomingHttpHeaders; body: string }[] = []
  const rcv = createServer((req, res) => {
    let body = ''
    req.on('data', (c: Buffer) => (body += c.toString()))
    req.on('end', () => {
      got.push({ headers: req.headers, body })
      res.end('ok')
    })
  })
  await new Promise<void>((r) => rcv.listen(0, '127.0.0.1', r))
  const url = `http://127.0.0.1:${(rcv.address() as AddressInfo).port}/hook`
  try {
    await loginAs(page, admin)
    await page.goto('/integrations?tab=webhooks')
    await page.getByLabel('Send events to webhooks').check()
    await expect(page.locator('section.card .badge', { hasText: 'running' })).toBeVisible()

    await page.getByRole('button', { name: 'Add webhook' }).click()
    const dlg = page.getByRole('dialog')
    await dlg.getByLabel('Name').fill('Home Assistant')
    await dlg.getByLabel('URL').fill('not a url')
    await dlg.getByRole('button', { name: 'Add' }).click()
    await expect(dlg.getByLabel('URL')).toHaveJSProperty('validity.valid', false)
    await dlg.getByLabel('URL').fill(url)
    await dlg.getByLabel('All events').uncheck()
    await dlg.getByLabel('Missed call').check()
    await dlg.getByRole('button', { name: 'Add' }).click()

    await expect(page.getByTestId('webhook-secret')).toHaveText(/^whsec_/)
    const secret = (await page.getByTestId('webhook-secret').textContent()) ?? ''
    await page.getByRole('button', { name: 'Done' }).click()
    const row = page.locator('tr', { hasText: 'Home Assistant' })
    await expect(row).toContainText('Missed call')

    await row.getByRole('button', { name: 'Send test to Home Assistant' }).click()
    await expect(page.getByText(/test delivered \(HTTP 200/)).toBeVisible()
    expect(got).toHaveLength(1)
    const h = got[0]!.headers
    const sig = createHmac('sha256', secret).update(`${h['x-voipd-timestamp'] as string}.${got[0]!.body}`).digest('hex')
    expect(h['x-voipd-signature']).toBe(`sha256=${sig}`)
    expect(JSON.parse(got[0]!.body).type).toBe('test.ping')

    await row.getByRole('button', { name: 'Deliveries of Home Assistant' }).click()
    await expect(page.getByRole('dialog').locator('td', { hasText: 'test.ping' })).toBeVisible()
    await page.getByRole('dialog').getByRole('button', { name: 'Close' }).last().click()

    await row.getByRole('button', { name: 'Delete Home Assistant' }).click()
    await expect(page.getByText('No webhooks yet.')).toBeVisible()
    await page.getByLabel('Send events to webhooks').uncheck()
  } finally {
    rcv.close()
  }
})

test('integrations: MQTT settings', async ({ page }) => {
  await loginAs(page, admin)
  await page.goto('/integrations')
  const form = page.getByRole('form', { name: 'MQTT settings' })
  await form.getByLabel('Connect to an MQTT broker').check()
  await form.getByPlaceholder('mqtt://homeassistant.local:1883').fill('http://nope')
  await form.getByRole('button', { name: 'Save' }).click()
  await expect(form.getByText('use mqtt://, mqtts://, ws:// or wss://')).toBeVisible()

  // No broker listens there: saved, the module keeps trying and says why.
  await form.getByPlaceholder('mqtt://homeassistant.local:1883').fill('mqtt://127.0.0.1:1')
  await form.getByLabel('Username').fill('voipd')
  await form.getByLabel('Password').fill('mqtt-secret')
  await form.getByRole('button', { name: 'Show advanced settings' }).click()
  await form.getByLabel('Base topic').fill('pbx')
  await form.getByRole('button', { name: 'Save' }).click()
  await expect(page.getByText('MQTT saved – connecting')).toBeVisible()
  await expect(page.getByText('pbx/blocking/anonymous/set')).toBeVisible()
  await expect
    .poll(async () => (await (await page.request.get('/api/v1/modules')).json()).modules[0].status.state, { timeout: 15_000 })
    .toBe('error')
  await page.reload()
  await expect(form.getByLabel('Password')).toHaveAttribute('placeholder', /unchanged/)

  await form.getByLabel('Connect to an MQTT broker').uncheck()
  await form.getByRole('button', { name: 'Save' }).click()
  await expect(page.getByText('MQTT is off')).toBeVisible()
})

function wav(seconds: number, rate = 8000, channels = 1): Buffer {
  const n = seconds * rate * channels
  const b = Buffer.alloc(44 + n * 2)
  b.write('RIFF', 0)
  b.writeUInt32LE(36 + n * 2, 4)
  b.write('WAVEfmt ', 8)
  b.writeUInt32LE(16, 16)
  b.writeUInt16LE(1, 20)
  b.writeUInt16LE(channels, 22)
  b.writeUInt32LE(rate, 24)
  b.writeUInt32LE(rate * 2 * channels, 28)
  b.writeUInt16LE(2 * channels, 32)
  b.writeUInt16LE(16, 34)
  b.write('data', 36)
  b.writeUInt32LE(n * 2, 40)
  return b
}

test('voicemail: own mailbox, play marks heard, delete, greeting upload', async ({ page }) => {
  page.on('dialog', (d) => void d.accept())
  await loginAs(page, alice)
  await page.getByRole('link', { name: 'Voicemail' }).click()
  const box = page.getByRole('region', { name: 'Mailbox 10' })
  await expect(box).toBeVisible()
  await expect(page.getByRole('region', { name: 'Mailbox 11' })).toHaveCount(0) // not alice's
  await expect(box.locator('header .badge')).toHaveText('2 new')
  await expect(box.locator('li')).toHaveCount(3)

  // The audio is served to the player.
  const res = await page.request.get('/api/v1/voicemails/demo-vm-1/audio')
  expect(res.status()).toBe(200)
  expect(res.headers()['content-type']).toBe('audio/wav')

  // Playing a message marks it as heard.
  await box.locator('audio[aria-label="Message from Mom"]').dispatchEvent('play')
  await expect(box.locator('header .badge')).toHaveText('1 new')
  await box.locator('li', { hasText: 'Jonas' }).getByRole('button', { name: 'Mark as heard' }).click()
  await expect(box.locator('header .badge')).toHaveCount(0)

  await box.getByRole('button', { name: 'Delete message from Dentist Dr. Weber' }).click()
  await expect(box.locator('li')).toHaveCount(2)

  // Greeting: wrong format, then a valid one, then back to standard.
  const upload = box.getByLabel('Upload greeting for 10')
  await upload.setInputFiles({ name: 'greeting.wav', mimeType: 'audio/wav', buffer: wav(1, 44100) })
  await expect(box.getByText('8 kHz or 16 kHz')).toBeVisible()
  await upload.setInputFiles({ name: 'greeting.wav', mimeType: 'audio/wav', buffer: wav(2, 16000) })
  await expect(box.getByText('Greeting: personal')).toBeVisible()
  await expect(box.locator('audio[aria-label="Greeting of 10"]')).toBeVisible()
  await box.getByRole('button', { name: 'Use standard greeting' }).click()
  await expect(box.getByText('Greeting: standard')).toBeVisible()
})

test('voicemail: admins see every mailbox and the dashboard count', async ({ page }) => {
  await loginAs(page, admin)
  const dash = await (await page.request.get('/api/v1/stats/dashboard?range=24h')).json()
  await expect(page.getByRole('link', { name: new RegExp(`${dash.voicemail_new} new voicemail`) })).toBeVisible()
  await page.getByRole('link', { name: 'Voicemail', exact: true }).click()
  await expect(page.getByRole('region', { name: 'Mailbox 11' })).toContainText('Insurance')
  await expect(page.getByRole('region', { name: 'Mailbox 12' })).toContainText('No messages.')
})

test('phone codes page', async ({ page }) => {
  await loginAs(page, alice)
  await page.getByRole('link', { name: 'Phone codes' }).click()
  const row = page.locator('tr', { hasText: '*98' })
  await expect(row).toContainText('Voicemail')
  await expect(row).toContainText('7 delete')
})

test('users cannot open integrations', async ({ page }) => {
  await loginAs(page, alice)
  await expect(page.getByRole('link', { name: 'Integrations' })).toHaveCount(0)
  await page.goto('/integrations')
  await expect(page).toHaveURL(/\/calls$/)
})
