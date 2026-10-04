import { test, expect, type Page } from '@playwright/test'
import AxeBuilder from '@axe-core/playwright'

// Real-portal acceptance of the sms-gw remote through the gateway and shell.
// E2E_BASE is the portal origin and E2E_STORAGE_STATE a signed-in session of
// an operator holding the SMS Gateway administrator role. E2E_MUTATE=1 also
// runs the create/delete flows (template, block, API client); none of them
// reaches a carrier.
const configured = !!process.env.E2E_BASE && !!process.env.E2E_STORAGE_STATE
test.skip(!configured, 'set E2E_BASE and E2E_STORAGE_STATE for authenticated V4 portal acceptance')

const pages = [
  { path: '/sms-gw/providers', nav: 'SMS providers', title: 'SMS providers' },
  { path: '/sms-gw/templates', nav: 'SMS templates', title: 'SMS templates' },
  { path: '/sms-gw/api-clients', nav: 'SMS API clients', title: 'SMS API clients' },
  { path: '/sms-gw/blocks', nav: 'SMS blocks', title: 'SMS blocks' },
  { path: '/sms-gw/messages', nav: 'SMS messages', title: 'SMS messages' },
  { path: '/sms-gw/dashboard', nav: 'SMS dashboard', title: 'SMS dashboard' },
]

// The shell groups module navigation under a collapsible module button.
async function navLink(page: Page, name: string) {
  const link = page.getByRole('link', { name })
  const group = page.getByRole('button', { name: 'SMS Gateway' })
  await expect(link.or(group).first()).toBeVisible({ timeout: 20_000 })
  if (!(await link.isVisible())) await group.click()
  await expect(link).toBeVisible()
  return link
}

async function heading(page: Page, title: string): Promise<void> {
  await expect(page.getByRole('heading', { name: title, level: 1 })).toBeVisible({ timeout: 20_000 })
}

test('the gateway relays the built remote with the manifest exports', async ({ request }) => {
  const res = await request.get('/m/sms-gw/mf-manifest.json')
  expect(res.ok()).toBe(true)
  const doc = (await res.json()) as { name: string; exposes: { path: string }[]; metaData: { publicPath: string } }
  expect(doc.name).toBe('sms-gw')
  expect(doc.metaData.publicPath).toBe('/m/sms-gw/')
  expect(doc.exposes.map((e) => e.path).sort()).toEqual(['./nav', './routes'])
})

test('the management API refuses requests without the portal session', async ({ playwright }) => {
  // An explicit empty state: request contexts otherwise inherit the signed-in storageState from `use`.
  const anon = await playwright.request.newContext({
    baseURL: process.env.E2E_BASE!,
    ignoreHTTPSErrors: process.env.E2E_INSECURE === '1',
    storageState: { cookies: [], origins: [] },
  })
  const res = await anon.get('/api/sms-gw/v1/providers')
  expect(res.status()).toBe(401)
  await anon.dispose()
})

test('navigation reaches every page and each page is accessible', async ({ page }) => {
  const errors: string[] = []
  page.on('pageerror', (e) => errors.push(e.message))
  await page.goto('/')
  for (const p of pages) {
    await (await navLink(page, p.nav)).click()
    await expect(page).toHaveURL(new RegExp(p.path + '(\\?|$)'))
    await heading(page, p.title)
    const axe = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa']).analyze()
    expect(axe.violations.filter((v) => v.impact === 'serious' || v.impact === 'critical').map((v) => v.id), p.path).toEqual([])
  }
  expect(errors).toEqual([])
})

test('tables page on the server and rows open from the keyboard', async ({ page }) => {
  await page.goto('/sms-gw/messages')
  await heading(page, 'SMS messages')
  const listed = page.waitForResponse((r) => r.url().includes('/api/sms-gw/v1/messages?') && r.request().method() === 'GET')
  await page.reload()
  const res = await listed
  expect(res.status()).toBe(200)
  const url = new URL(res.url())
  expect(url.searchParams.get('page')).toBe('1')
  expect(url.searchParams.get('sort')).toBe('created_at')
  expect(url.searchParams.get('order')).toBe('desc')
  const row = page.locator('[data-test^="message-"][data-row-key]').first()
  if (await row.count()) {
    await row.focus()
    await page.keyboard.press('Enter')
    await expect(page.getByRole('dialog')).toBeVisible()
    await expect(page.getByRole('heading', { name: 'Delivery receipts' })).toBeVisible()
  }
})

test('the dashboard shows data or an explicit unavailable state', async ({ page }) => {
  await page.goto('/sms-gw/dashboard')
  await heading(page, 'SMS dashboard')
  await expect(page.getByTestId('dashboard-data').or(page.getByTestId('dashboard-unavailable'))).toBeVisible({ timeout: 20_000 })
  await page.getByRole('tab', { name: '24h' }).click()
  await expect(page.getByTestId('dashboard-data').or(page.getByTestId('dashboard-unavailable'))).toBeVisible()
})

test('provider forms never reveal stored credentials', async ({ page }) => {
  await page.goto('/sms-gw/providers')
  await heading(page, 'SMS providers')
  const row = page.locator('[data-test^="provider-"][data-row-key]').first()
  test.skip(!(await row.count()), 'no provider configured')
  const listed = await page.request.get('/api/sms-gw/v1/providers?page=1&page_size=200')
  const body = await listed.text()
  expect(body).not.toMatch(/"(token|dlr_token)":"(?!__set__)[^"]+"/)
  await row.click()
  for (const input of await page.locator('[data-test^="provider-config-"] input[type="password"]').all()) {
    const v = await input.inputValue()
    expect(v === '' || v === '__set__').toBe(true)
  }
})

test.describe('create and delete flows', () => {
  test.skip(process.env.E2E_MUTATE !== '1', 'set E2E_MUTATE=1 to create and delete records')
  const suffix = Date.now().toString(36)

  test('template: create, preview parts, delete', async ({ page }) => {
    await page.goto('/sms-gw/templates')
    await heading(page, 'SMS templates')
    await page.getByTestId('template-new').click()
    await page.getByTestId('template-name').locator('input').fill('e2e-' + suffix)
    await page.getByTestId('template-body').locator('textarea').fill('Hello {{.name}}, your code is {{.code}}')
    await page.getByTestId('template-save').click()
    await expect(page.getByTestId('template-preview')).toBeVisible()
    await page.getByTestId('template-prop-name').locator('input').fill('Ana')
    await page.getByTestId('template-preview-run').click()
    await expect(page.getByTestId('preview-text')).toContainText('Hello Ana')
    await expect(page.getByTestId('preview-parts')).toContainText('1 part')
    await expect(page.getByTestId('preview-missing')).toContainText('code')
    await page.getByTestId('template-delete').click()
    await page.getByRole('button', { name: 'Delete' }).last().click()
    await expect(page.getByTestId('template-drawer')).toBeHidden()
  })

  test('block: every provider, then delete', async ({ page }) => {
    const recipient = '3590000' + String(Date.now()).slice(-6)
    await page.goto('/sms-gw/blocks')
    await heading(page, 'SMS blocks')
    await page.getByTestId('block-new').click()
    await page.getByTestId('block-recipient').locator('input').fill(recipient)
    await page.getByTestId('block-save').click()
    const row = page.locator('[data-row-key]', { hasText: recipient })
    await expect(row).toContainText('All providers')
    await row.click()
    await page.getByTestId('block-delete').click()
    await page.getByRole('button', { name: 'Delete block' }).click()
    await expect(row).toHaveCount(0)
  })

  test('API client: generated password shown once, reset, delete', async ({ page }) => {
    const username = 'e2e_' + suffix
    await page.goto('/sms-gw/api-clients')
    await heading(page, 'SMS API clients')
    await page.getByTestId('client-new').click()
    await page.getByTestId('client-username').locator('input').fill(username)
    await page.getByTestId('client-save').click()
    const secret = page.getByTestId('one-time-secret-value')
    await expect(secret).not.toBeEmpty()
    const first = await secret.textContent()
    await page.getByTestId('one-time-secret-done').click()
    await expect(page.getByText(first ?? '-')).toHaveCount(0)
    await page.locator('[data-row-key]', { hasText: username }).click()
    await page.getByTestId('client-reset').click()
    await page.getByTestId('client-reset-confirm').click()
    await expect(secret).not.toHaveText(first ?? '')
    await page.getByTestId('one-time-secret-done').click()
    // The reset keeps the client's drawer open.
    await page.getByTestId('client-delete').click()
    await page.getByRole('button', { name: 'Delete' }).last().click()
    await expect(page.locator('[data-row-key]', { hasText: username })).toHaveCount(0)
  })
})
