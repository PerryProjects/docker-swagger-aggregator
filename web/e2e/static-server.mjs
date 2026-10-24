// Serves a directory (the custom frontend of the CORS test) on 127.0.0.1: bun e2e/static-server.mjs <dir> <port>
import { readFileSync } from 'node:fs'
import { createServer } from 'node:http'
import { join, normalize } from 'node:path'

const [dir, port] = [process.argv[2], Number(process.argv[3])]
createServer((req, res) => {
  const path = normalize(new URL(req.url, 'http://x').pathname).replace(/^(\.\.[/\\])+/, '')
  try {
    res.setHeader('Content-Type', 'text/html; charset=utf-8')
    res.end(readFileSync(join(dir, path === '/' ? 'index.html' : path)))
  } catch {
    res.statusCode = 404
    res.end('not found')
  }
}).listen(port, '127.0.0.1')
