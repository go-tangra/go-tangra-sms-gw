import { defineConfig } from '@playwright/test'

// Runs through the platform gateway (the remote only exists inside the shell):
// E2E_BASE is the portal origin, E2E_STORAGE_STATE a signed-in operator session.
export default defineConfig({
  testDir: 'tests/e2e',
  timeout: 60_000,
  use: {
    ...(process.env.E2E_BASE ? { baseURL: process.env.E2E_BASE } : {}),
    ...(process.env.E2E_STORAGE_STATE ? { storageState: process.env.E2E_STORAGE_STATE } : {}),
    ignoreHTTPSErrors: process.env.E2E_INSECURE === '1',
    testIdAttribute: 'data-test',
    ...(process.env.PW_CHANNEL ? { channel: process.env.PW_CHANNEL } : {}),
  },
  reporter: [['list'], ['html', { open: 'never' }]],
})
