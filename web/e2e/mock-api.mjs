// Mock of the swagger-aggregator API for the e2e tests. POST /__mock {services:[...]} replaces the state.
import http from 'node:http'

let services = []
const clients = new Set()

const spec = (name, hash) => ({
  openapi: '3.0.3',
  info: { title: `${name} API`, version: hash },
  servers: [{ url: `/api/proxy/${name}` }],
  paths: Object.fromEntries(
    Array.from({ length: 40 }, (_, i) => [
      `/item${i}`,
      { get: { summary: `${name} get item ${i}`, tags: [`${name}-tag-${i % 4}`], responses: { 200: { description: 'ok' } } } },
    ]),
  ),
})

// noSpec: a service that never delivered a spec (no specHash, no lastSeen), optionally with an error.
const list = () =>
  services.map((s) => ({
    name: s.name, status: s.status ?? 'up', specHash: s.noSpec ? undefined : s.hash ?? `${s.name[0]}1`,
    group: s.group, order: s.order, error: s.error,
    title: s.title, version: s.version, openapi: s.openapi, endpoints: s.endpoints, source: s.source, health: s.health,
    replicas: s.replicas, replicasUp: s.replicasUp,
    lastSeen: s.noSpec ? undefined : new Date().toISOString(),
  }))
const push = () => { for (const c of clients) c.write(`event: services\ndata: ${JSON.stringify(list())}\n\n`) }

http.createServer((req, res) => {
  const u = new URL(req.url, 'http://x')
  if (req.method === 'POST' && u.pathname === '/__mock') {
    let b = ''
    req.on('data', (d) => (b += d)).on('end', () => { services = JSON.parse(b).services; push(); res.end('ok') })
    return
  }
  if (u.pathname === '/api/services') {
    res.setHeader('content-type', 'application/json')
    return res.end(JSON.stringify(list()))
  }
  if (u.pathname === '/api/events') {
    res.writeHead(200, { 'content-type': 'text/event-stream', 'cache-control': 'no-cache' })
    res.write(`event: services\ndata: ${JSON.stringify(list())}\n\n`)
    clients.add(res)
    req.on('close', () => clients.delete(res))
    return
  }
  let m = u.pathname.match(/^\/api\/services\/([^/]+)\/spec$/)
  if (m) {
    const s = services.find((x) => x.name === decodeURIComponent(m[1]))
    if (!s || s.noSpec) { res.statusCode = 404; return res.end() }
    res.setHeader('content-type', 'application/json')
    return res.end(JSON.stringify(spec(s.name, s.hash ?? `${s.name[0]}1`)))
  }
  if (u.pathname === '/api/scalar-proxy') { // Scalar sends absolute request URLs here (proxyUrl)
    const target = new URL(u.searchParams.get('scalar_url') ?? '', 'http://x')
    req.url = target.pathname
    u.pathname = target.pathname
  }
  m = u.pathname.match(/^\/api\/proxy\/([^/]+)\/(.*)$/)
  if (m) {
    res.setHeader('content-type', 'application/json')
    return res.end(JSON.stringify({ proxiedFor: m[1], path: `/${m[2]}` }))
  }
  res.statusCode = 404
  res.end()
}).listen(18081, '127.0.0.1', () => console.log('mock api on 18081'))
