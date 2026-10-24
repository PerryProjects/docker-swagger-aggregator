import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// Dev mode: `npm run dev` proxies /api (incl. the SSE stream) to the Go backend.
const backend = process.env.SWAGGER_AGGREGATOR_BACKEND ?? 'http://localhost:8080'

export default defineConfig({
  base: './', // relative asset URLs: the same build works under any public path prefix (--base-path)
  plugins: [vue()],
  build: { chunkSizeWarningLimit: 2000 }, // Scalar is one big chunk; it is embedded, not downloaded cold
  server: { proxy: { '/api': backend } },
  preview: { proxy: { '/api': backend } }, // production build, used by the e2e tests
})
