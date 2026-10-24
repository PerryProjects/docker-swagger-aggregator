import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { defineConfig } from '@playwright/test'

// A fresh data dir per run: the built-in authentication starts without users (the setup page is tested).
const dataDir = mkdtempSync(join(tmpdir(), 'swagger-aggregator-e2e-'))

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
    {
      // The real Go web server with built-in auth in front of the mock API; serves the build made above.
      // E2E_WEB_BIN: a prebuilt binary (CI); by default it is built from the sources
      command: `${process.env.E2E_WEB_BIN ?? 'go build -o .e2e-web ../cmd/swagger-aggregator-web && ./.e2e-web'} --dir dist --api http://127.0.0.1:18081 --listen 127.0.0.1:4174`,
      env: { SWAGGER_AGGREGATOR_WEB_AUTH: 'build_in', SWAGGER_AGGREGATOR_WEB_DATA_DIR: dataDir, SWAGGER_AGGREGATOR_WEB_SETUP_TOKEN: 'e2e-setup-token', SWAGGER_AGGREGATOR_WEB_CORS_ORIGINS: 'http://127.0.0.1:4175' },
      url: 'http://127.0.0.1:4174/healthz',
      reuseExistingServer: false,
      timeout: 120_000,
    },
    {
      // A "custom frontend" on another origin (127.0.0.1:4175) that the CORS list of the server above names.
      command: 'bun e2e/static-server.mjs e2e/custom-frontend 4175',
      url: 'http://127.0.0.1:4175/',
      reuseExistingServer: false,
    },
  ],
})
