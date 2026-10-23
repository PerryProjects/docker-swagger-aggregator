import { defineConfig } from '@playwright/test'

// Runs against the production build (vite preview), with /api proxied to the mock API.
// Local Chrome instead of a downloaded Chromium: PW_CHANNEL=chrome bun run test:e2e
export default defineConfig({
  testDir: 'e2e',
  workers: 1, // the tests share one mock API
  timeout: 30_000,
  use: { actionTimeout: 5_000, baseURL: 'http://127.0.0.1:4173', channel: process.env.PW_CHANNEL || undefined },
  webServer: [
    { command: 'bun e2e/mock-api.mjs', url: 'http://127.0.0.1:18081/api/services', reuseExistingServer: false },
    {
      command: 'bun run build && SWAGGER_AGGREGATOR_BACKEND=http://127.0.0.1:18081 bunx vite preview --host 127.0.0.1 --port 4173 --strictPort',
      url: 'http://127.0.0.1:4173',
      reuseExistingServer: false,
      timeout: 120_000,
    },
  ],
})
