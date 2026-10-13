import { defineConfig } from '@playwright/test'

// Run via test/e2e/ui.sh, which starts voipd (demo data) and a fake OIDC
// provider and sets BASE_URL / IDP_URL.
export default defineConfig({
  testDir: './tests',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 30_000,
  reporter: [['list']],
  use: {
    baseURL: process.env.BASE_URL ?? 'http://127.0.0.1:18082',
    channel: process.env.PW_CHANNEL ?? 'chrome',
    viewport: { width: 1400, height: 950 },
    trace: 'retain-on-failure',
  },
})
