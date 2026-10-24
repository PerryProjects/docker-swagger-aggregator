import { readFileSync } from 'node:fs'
import type { IncomingMessage, ServerResponse } from 'node:http'
import { fileURLToPath } from 'node:url'
import { defineConfig, type Plugin } from 'vite'
import vue from '@vitejs/plugin-vue'
import tailwindcss from '@tailwindcss/vite'
import { parse } from 'yaml'

// The UI runs without the Go web server: `bun run dev` serves it with Vite and forwards the calls the Go web
// server would answer to a backend. The backend is the API server (`swagger-aggregator`, default :8080) - enough for
// auth=none - or the Go web server itself (then /auth and /mcp work too, e.g. with auth=build_in).
const backend = process.env.SWAGGER_AGGREGATOR_BACKEND ?? 'http://localhost:8080'
// Keys starting with ^ are regular expressions: a plain '/api' would be a prefix match and also swallow the page /api-docs.
const proxy = { '^/api/': backend, '^/auth/': backend, '^/mcp$': backend }

// /spec/{openapi,asyncapi}.{json,yaml}: this program's own API documents, read from the files the Go server embeds,
// so the API page works without the Go server and always shows the current files.
const SPEC_DIR = fileURLToPath(new URL('../internal/apidocs/', import.meta.url))

function ownApiSpec(): Plugin {
  const serve = (req: IncomingMessage, res: ServerResponse, next: () => void) => {
    const path = (req.url ?? '').split('?')[0]
    const match = /^\/spec\/(openapi|asyncapi)\.(json|yaml)$/.exec(path)
    if (!match) return next()
    const yaml = readFileSync(`${SPEC_DIR}${match[1]}.yaml`, 'utf8')
    res.setHeader('Cache-Control', 'no-cache')
    if (path.endsWith('.json')) {
      res.setHeader('Content-Type', 'application/json')
      res.end(JSON.stringify(parse(yaml)))
    } else {
      res.setHeader('Content-Type', 'application/yaml')
      res.end(yaml)
    }
  }
  return {
    name: 'own-api-specs',
    configureServer: (s) => void s.middlewares.use(serve),
    configurePreviewServer: (s) => void s.middlewares.use(serve), // production build, used by the e2e tests
  }
}

export default defineConfig({
  base: './', // relative asset URLs: the same build works under any public path prefix (--base-path)
  plugins: [vue(), tailwindcss(), ownApiSpec()],
  build: { chunkSizeWarningLimit: 2000 }, // Scalar is one big chunk; it is embedded, not downloaded cold
  server: { proxy },
  preview: { proxy },
})
