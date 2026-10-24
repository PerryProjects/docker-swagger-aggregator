# Swagger Aggregator

A single Go service that discovers Docker containers labeled `swagger-aggregator.enable=true`,
fetches their OpenAPI/Swagger spec, and shows all of them in one [Scalar](https://scalar.com)
UI with a live up/down marker per API. There is no runtime CDN dependency.

Two small Go programs (one image):

- `swagger-aggregator` – the API: discovery, spec fetching, SSE, and a reverse proxy for "Try it" calls.
- `swagger-aggregator-web` – serves the built Vue UI (`web/dist`, **not embedded**) and forwards `/api` to the API,
  so the browser sees one origin.

## Architecture

Two small Go programs from one module (the UI is built separately and served by the second one):

```
cmd/swagger-aggregator       API process: flags -> app.Config -> app.New -> Run
cmd/swagger-aggregator-web   static UI server + /api forwarder
internal/app                 composition root: builds and wires everything, owns lifecycle (the only place that knows concrete types)
internal/discovery           source-agnostic core: Container model, Source/Handler contracts, Manager (fetch/probe loop), target parsing
internal/discovery/docker    Docker source (depends on a small docker.API interface, not on the client type)
internal/discovery/kubernetes Kubernetes source: informers for Services, EndpointSlices, Ingress, IngressRoute, HTTPRoute, Gateway + a level-triggered reconcile
internal/transport           Mux: routes requests by host (authenticated transport for the Kubernetes API server proxy only)
internal/registry            in-memory state: services, name index, routes, change notification (versioned)
internal/fetcher             bounded spec download + validation/normalisation (Prepare)
internal/server              HTTP: JSON/SSE API, Try-it proxies, static web handler, health probe
internal/status              up/down rule
```

Dependencies are injected through constructors and config structs; interfaces are defined where they are consumed
(`discovery.Store`, `discovery.SpecFetcher`, `server.Store`, `docker.API`, `fetcher.Doer`, client-go's `kubernetes.Interface`), and there are no package-level
variables or hidden defaults like `http.DefaultClient` inside the libraries. That is what makes the end-to-end test in
`internal/app` possible without a Docker daemon (fake `docker.API` in, real HTTP out).

**Adding a source**: implement `discovery.Source` (`Run(ctx, handler)`), resolve each workload to a `discovery.Container`
(using `discovery.ParseTarget` on its labels/annotations) and report it with `Apply`/`Remove`/`Retain`; add it to `app.New`.
Docker and Kubernetes are done this way and can run side by side.

## Quick start (WSL2 / Docker)

Keep the repo in the WSL filesystem (e.g. `~/projects`), not under `/mnt/c`.

```bash
docker compose up --build
# open http://localhost:8080
```

The compose file starts a `tecnativa/docker-socket-proxy`, the API, the web server, and two demo services
(`orders`: OpenAPI 3 **with** a HEALTHCHECK; `inventory`: Swagger 2.0 **without** one, using the
path/name/server-url labels). Try `docker compose stop orders` and watch the marker turn red.
Locally the UI port comes from `docker-compose.override.yml` (loaded automatically). With Traefik see [Traefik](#traefik).

More combinations (own login, OIDC, MCP, two API instances, Traefik, a remote Kubernetes cluster) are ready-made in
[`examples/`](examples/README.md).

## Image size

One image holds the API and the web server. Measured (this repository, `docker build`; "pull" is what a registry transfers, gzip):

| Build | On disk | Pull | |
|---|---|---|---|
| default (`docker build .`) | 88 MB | 22 MB | the full API **with Kubernetes support**: what `deploy/kubernetes` uses |
| `--build-arg KUBERNETES=false` | 60 MB | 16 MB | **what `docker compose` builds**: the API without client-go (`-tags nokubernetes`): 10 instead of 31 MB |
| + `--build-arg COMPRESS_BINARIES=best` | 40 MB | 15 MB | UPX-packed binaries: start ~0.08 s slower, ~5 MB more RAM per process |
| + `--build-arg COMPRESS_BINARIES=lzma` | 34 MB | 12.5 MB | smaller still, start ~0.35 s slower |

The big step is dropping Kubernetes support, which a Docker-only setup does not need (client-go and the Kubernetes API types are ~26 MB of
the API binary). `--kubernetes` in such an image fails at startup with a clear message; `examples/kubernetes-remote.yml` builds the full one.
UPX shrinks the files on disk but little of what is pulled (the registry compresses anyway); it also makes some security scanners suspicious and
every process start decompress the binary. It is off by default: use it only if disk space matters more than that. The web server (23 MB) has no
Kubernetes code; its UI is 6.5 MB (1.8 MB gzipped).

## Labels

| Label | Default | Meaning |
|---|---|---|
| `swagger-aggregator.enable=true` | – (required) | opt in |
| `swagger-aggregator.port` | lowest exposed TCP port | port of the spec endpoint |
| `swagger-aggregator.path` | `/openapi.json` | path of the spec |
| `swagger-aggregator.name` | compose service name, else container name | display name, URL key |
| `swagger-aggregator.server-url` | – (or derived from Traefik labels, see below) | public base URL; replaces `servers` (OpenAPI 3) or `host`/`basePath`/`schemes` (Swagger 2.0). Its path is replaced by the container root when "Try it" is proxied (like a Traefik `StripPrefix`), unless `strip-prefix` is `false` |
| `swagger-aggregator.strip-prefix` | `true` | only with a `server-url` that has a path: `true` = the proxy in front strips that path (the workload sees `/items` for `/api/items`); `false` = it does not and the workload itself serves under it (`/api/items` stays `/api/items`) |
| `swagger-aggregator.scheme` | `http` | `https` if the workload itself serves TLS. Its certificate is verified unless `--insecure-upstream-tls` is set (workloads are reached by IP, which certificates rarely cover) |
| `swagger-aggregator.group` | – | shown as `group / name` in the UI, sorts first |
| `swagger-aggregator.order` | `0` | sort key inside a group (integer) |

All label names share a prefix (`swagger-aggregator`, the on-switch is `<prefix>.enable`); change it with `--label-prefix` to run several instances on one
daemon (e.g. staging and production, each with its own prefix).

Containers are reached by **IP on the configured network**, so the app must share a network
with them. A container without exposed ports needs the `port` label (`EXPOSE` / compose
`expose:` both count). Invalid labels are shown as a down service with the reason.

## Configuration

Flags or environment variables (`SWAGGER_AGGREGATOR_` prefix, `-` → `_`).

| Flag / env | Default | |
|---|---|---|
| `--listen` / `SWAGGER_AGGREGATOR_LISTEN` | `:8080` | HTTP listen address |
| `--docker-host` / `SWAGGER_AGGREGATOR_DOCKER_HOST` | `$DOCKER_HOST`, else local socket | e.g. `tcp://socket-proxy:2375` |
| `--network` / `SWAGGER_AGGREGATOR_NETWORK` | first network with an IP | network used to reach containers |
| `--probe-interval` / `SWAGGER_AGGREGATOR_PROBE_INTERVAL` | `15s` | spec re-fetch (= liveness probe) interval; retry backoff cap |
| `--fetch-timeout` / `SWAGGER_AGGREGATOR_FETCH_TIMEOUT` | `5s` | per-request timeout |
| `--max-spec-bytes` / `SWAGGER_AGGREGATOR_MAX_SPEC_BYTES` | `10485760` | spec size limit |
| `--shutdown-grace` / `SWAGGER_AGGREGATOR_SHUTDOWN_GRACE` | `5s` | how long in-flight requests get to finish on shutdown, then connections are closed |
| `--label-prefix` / `SWAGGER_AGGREGATOR_LABEL_PREFIX` | `swagger-aggregator` | label namespace to watch |
| `--base-path` / `SWAGGER_AGGREGATOR_BASE_PATH` | – | public path prefix the UI is served under, e.g. `/docs`; written into the same-origin proxy paths of the specs (`/docs/api/proxy/<id>`). Set the same value on `swagger-aggregator-web` |
| `--insecure-upstream-tls` / `SWAGGER_AGGREGATOR_INSECURE_UPSTREAM_TLS` | `false` | skip certificate verification for workloads labeled `scheme=https` (spec fetch and "Try it") |
| `--prefer-published-ports` / `SWAGGER_AGGREGATOR_PREFER_PUBLISHED_PORTS` | `false` | reach containers via their published host port (`127.0.0.1:<port>`) instead of the network IP |
| `--derive-server-url` / `SWAGGER_AGGREGATOR_DERIVE_SERVER_URL` | `false` | derive a missing `server-url` from Traefik router labels (Docker, see [Traefik](#traefik)) or from Ingress/IngressRoute objects (Kubernetes) |
| `--docker` / `SWAGGER_AGGREGATOR_DOCKER` | `true` | watch Docker containers; set `false` when running in Kubernetes without a Docker socket |
| `--kubernetes` / `SWAGGER_AGGREGATOR_KUBERNETES` | `false` | watch Kubernetes Services, see [Kubernetes](#kubernetes) |
| `--kubeconfig` / `--kube-context` | – | kubeconfig path / context (default: in-cluster ServiceAccount if present, else `KUBECONFIG` / `~/.kube/config`) |
| `--namespace` / `SWAGGER_AGGREGATOR_NAMESPACE` | all | namespace(s) to watch, repeatable or comma separated |
| `--kube-access` / `SWAGGER_AGGREGATOR_KUBE_ACCESS` | `auto` | `service` (in-cluster DNS), `apiserver` (API server service proxy, works from outside), `auto` |
| `--cluster-domain` | `cluster.local` | DNS suffix for `--kube-access=service` |
| `--log-level` / `SWAGGER_AGGREGATOR_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `--log-format` / `SWAGGER_AGGREGATOR_LOG_FORMAT` | `console` | `console` (human readable) or `json` (one object per line, for log collectors; the Kubernetes deployment uses it) |

Logging uses [zap](https://github.com/uber-go/zap); client-go's own (klog) messages are routed through the same logger.

**Validation.** The configuration is checked as a whole at startup and every problem is reported at once, named by flag:

```
Error: invalid configuration (3 problems):
  - --listen: "nope" is not host:port (e.g. :8080 or 127.0.0.1:8080)
  - --probe-interval: 1ns is out of range: must be between 10ms and 24h0m0s
  - --kubeconfig: has no effect because --kubernetes is disabled
```

It checks formats (listen address, label prefix, Docker host and network names, namespaces, cluster domain), ranges (intervals, timeouts, spec size), and
options that would silently do nothing (Kubernetes options while `--kubernetes` is off, Docker options while `--docker` is off). Values from the
environment are parsed strictly: `SWAGGER_AGGREGATOR_PROBE_INTERVAL=15` (no unit) or `=abc` is an error, not a silent `0`.
`--kube-access=service` is refused when no ServiceAccount is present (in-cluster DNS only works inside the cluster).

Both programs have a `healthcheck` subcommand (`swagger-aggregator healthcheck`, `swagger-aggregator-web healthcheck`) that
probes their own `/healthz`; the compose file uses it as the container HEALTHCHECK (the image has no shell or curl).

## How status is decided

A service is **up** only if *all* available signals are positive; the most negative one wins:

1. container not running → **down** (the spec is kept, the service stays listed)
2. container has a HEALTHCHECK → must be `healthy` (`starting`/`unhealthy` = down)
3. the periodic HTTP fetch of the spec URL must succeed and return a valid OpenAPI/Swagger document

Containers without a HEALTHCHECK are judged by 1 and 3 only. After a `start` event the app
retries the fetch with exponential backoff (1s, 2s, 4s … capped at the probe interval).
A destroyed container is removed; a stopped one is only marked down. When a stopped container is
recreated under the same name (compose recreate), the new one takes over the entry and the last
known spec.

## Service status page

`/status` lists **every** discovered service (also those that never delivered a spec, which are not in the docs):
up/down as text and marker, last successful fetch, the current problem (fetch error, wrong network, invalid labels, ...) and
an "Open docs" link. It updates live over the same SSE stream. The docs sidebar links to it. It is a PrimeVue page (light/dark follows the OS). Every page has the same navigation: documentation, API reference, status and, for admins, users.

## HTTP API

| Endpoint | |
|---|---|
| `GET /api/services` | `[{name,status,lastSeen,specHash,error,group,order}]` |
| `GET /api/services/{name}/spec` | last known spec as JSON, ETag = spec hash |
| `GET /api/events` | SSE: event `services` with the full list on connect and on every change, `: heartbeat` comment every 15s |
| `ANY /api/proxy/{containerID}/{path...}` | same-origin reverse proxy to that container (used by "Try it", see below) |
| `ANY /api/scalar-proxy?scalar_url=…` | Scalar's `proxyUrl` endpoint: forwards a public URL below a registered service's `server-url` to that container |
| `GET /healthz` | `ok` |
| `GET /spec/openapi.{json,yaml}` | this interface as OpenAPI 3.1 (REST, the SSE events, MCP), see [Build your own frontend](#build-your-own-frontend); `/auth/*` is the built-in login |

`lastSeen` is the last successful fetch. Changes that only move `lastSeen` do not emit an SSE event.
YAML specs are accepted and served as JSON.

## Build your own frontend

The UI that ships with this program is only one client of an HTTP interface that is documented in the program itself:
the **API** page (`/api-docs`) shows it with Scalar, as one OpenAPI 3.1 document: the REST endpoints, the live event
stream (every Server-Sent Event, with the wire format and a browser example) and the MCP endpoint (JSON-RPC, tools, examples). It is also
served as a plain file for code generators and your own tooling: `/spec/openapi.json` and `/spec/openapi.yaml` (public, like
`/healthz`). The source is [`internal/apidocs/openapi.yaml`](internal/apidocs/openapi.yaml); tests check it against the code (the schemas against
the structs and real responses, every documented operation against the real handlers, the MCP section against the real `/mcp`).

```js
const services = await (await fetch('/api/services')).json()                    // [{name, status, specHash, ...}]
const spec = await (await fetch(`/api/services/${encodeURIComponent(services[0].name)}/spec`)).json()
const es = new EventSource('/api/events')                                       // live: a full snapshot on every change
es.addEventListener('services', (e) => render(JSON.parse(e.data)))
```

With `auth=build_in`, `POST /auth/login` (JSON) sets the session cookie, `GET /auth/session` tells which login to show;
`auth=oidc` redirects the browser to the provider.

**Same origin is the simplest.** Point `--dir` of `swagger-aggregator-web` at your build (it needs an `index.html`; unknown paths fall
back to it, so a single-page app works), or route your frontend and this server's paths (`/api`, `/auth`, `/spec`) to one host in your
reverse proxy. Then none of the following is needed.

**A frontend on another origin** needs the CORS list: `SWAGGER_AGGREGATOR_WEB_CORS_ORIGINS=https://app.example.com` (exact origins,
comma separated; `*` is refused, because the browser only sends cookies to an explicitly named origin). For those origins the server sends
`Access-Control-Allow-Origin: <that origin>` and `Access-Control-Allow-Credentials: true`, answers preflights itself (methods `GET, POST, PUT,
PATCH, DELETE`; headers `Content-Type`, `Accept`, `If-None-Match`) and exposes `ETag` and `Retry-After`. The frontend must ask for credentials:
`fetch(url, {credentials: "include"})` and `new EventSource(url, {withCredentials: true})`. See [`examples/custom-frontend.yml`](examples/custom-frontend.yml).

- **The cookie is `SameSite=Lax` by default.** That works for a frontend on the same *site* (`localhost:5173` talking to `localhost:3000`, or
  `app.example.com` talking to `docs.example.com`). A frontend on another *site* needs `SWAGGER_AGGREGATOR_WEB_COOKIE_SAMESITE=none`; the
  cookie is then `Secure`, i.e. HTTPS (or localhost). `none` is refused without a CORS list.
- **The server checks `Origin` itself once a CORS list is set.** With `SameSite=None` the browser no longer keeps other sites from sending
  the cookie, so requests that change something (`POST`, `PUT`, `PATCH`, `DELETE`) with an `Origin` that is neither listed nor the server's own
  get `403`, and a preflight from an unlisted origin gets `403`. Requests without an `Origin` header (curl, other servers) pass. The server's own
  origin is recognised by the `Host` (or `X-Forwarded-Host`) header: a proxy that rewrites `Host` must send `X-Forwarded-Host`.
- **Without a CORS list nothing changes:** same origin only, `SameSite=Lax`, no extra checks.
- Tested in a real browser: a page on another origin signs in, reads the API and follows the event stream; an unlisted origin is blocked.
  The `SameSite=None` cookie attributes are unit-tested; a truly cross-site setup (two domains over HTTPS) was not run.

For tools and assistants that cannot do a browser login there is the token-protected [MCP server](#mcp-ai-assistants).

## "Try it" (CORS)

Scalar sends test requests from the browser to the spec's `servers` URL, which is normally an internal address
the browser cannot reach (or a service without CORS headers) → "Failed to fetch". Two cases:

- **No `server-url`**: `servers` becomes the same-origin path `/api/proxy/<containerID>` (plus the original
  server's path, e.g. `/v1`; Swagger 2.0: `host`/`schemes` removed, `basePath` set) and the API forwards the call.
- **`server-url` set** (explicitly, or derived from Traefik labels): the spec keeps showing that real public URL, also
  in code samples. The UI sets Scalar's `proxyUrl` to `/api/scalar-proxy`, so the browser calls
  `/api/scalar-proxy?scalar_url=<public URL>` and the API maps it to the container. **No CORS involved, and the public
  host does not even need to resolve from the browser.** (Verified in Chrome against a fake host.)

Both proxies are not open proxies: the target is looked up in the registry (by container ID, or by matching a registered,
*running* service's `server-url` by scheme + host + path prefix, longest prefix wins). Everything else gets 404. Requests
are limited to 10 MB bodies and have dial/response-header timeouts; `Cookie`, `Origin` and `Referer` are not forwarded.
Scalar also sends our own relative `/api/proxy/<id>` URLs through `scalar-proxy` once it has made them absolute; those are
recognized by path only (the host is never contacted).

## Traefik

Route **only the `web` service** (port 3000) through Traefik and keep the API (`api`) unpublished:

```bash
TRAEFIK_NETWORK=proxy AGGREGATOR_HOST=docs.example.com \
  docker compose -f docker-compose.yml -f docker-compose.traefik.yml up -d --build
```

`docker-compose.traefik.yml` joins `web` and `api` to your external Traefik network, sets `SWAGGER_AGGREGATOR_NETWORK` to
it (the API must share a network with your services), enables `--derive-server-url`, and carries the router labels
(optional auth middleware included, commented). Add `swagger-aggregator.enable=true` (+ labels) to your services as usual.

- **`--derive-server-url`** (Docker part) fills a missing `server-url` from `traefik.http.routers.<r>.rule` **only if** exactly one
  public URL results: the rule must be exactly ``Host(`x`)`` and the router must have TLS via labels (`.tls=true` or
  `.tls.*`), giving `https://x`. Rules with `PathPrefix`, `||`, regexps, several different hosts, or TLS configured only
  on the entrypoint are skipped (a path prefix would need to know whether a `StripPrefix` middleware applies; entrypoint
  TLS is invisible in labels). An explicit `server-url` label always wins.
- Authentication middlewares on the router also protect `/api/proxy`, `/api/scalar-proxy` and the event stream (same origin).
- **Traefik only routes healthy containers.** The `web` service has a healthcheck (10 s interval), so after it is
  (re)created Traefik answers 404 until the first check passes. Expected, not a misconfiguration.
- SSE works through Traefik, including with the `compress` middleware on the router (frames are flushed immediately). Keep
  Traefik's idle timeout above the 15 s heartbeat. Use `compress` for the large UI bundle rather than a Go-side implementation.

**Tested against a real Traefik** (v3.7.16, Docker provider behind its own socket proxy, self-signed default certificate):
`demo/traefik-local.yml` is the setup used and can be reused to repeat it:

```bash
docker network create sa-traefik-net
export TRAEFIK_NETWORK=sa-traefik-net AGGREGATOR_HOST=docs.example.test
docker compose -p sa-traefik -f docker-compose.yml -f docker-compose.traefik.yml -f demo/traefik-local.yml up -d --build
curl -k --resolve docs.example.test:18443:127.0.0.1 https://docs.example.test:18443/api/services
```

## Kubernetes

The API can run **inside** the cluster or **outside** it (a laptop, another server) and documents Services, using Ingress
and Traefik IngressRoute objects to find their public URLs. It can run next to the Docker source or instead of it.

**What is documented.** A Service labeled `swagger-aggregator.enable=true`. Everything else is optional:

| Attribute | Where | Default |
|---|---|---|
| `swagger-aggregator.enable=true` | **label** (it is the selector) | – (required) |
| `.port` (number or port name), `.path`, `.name`, `.group`, `.order`, `.server-url` | label **or annotation**; use annotations for URLs (labels cannot contain `:` or `/`) | first TCP port of the Service, `/openapi.json`, `<name>.<namespace>` |

A Service is **up** when the spec fetch works and at least one of its endpoints is ready (EndpointSlices); scaling to zero shows it as down
within seconds. A Service without a spec stays visible on the status page with the reason.

**Public URL.** With `--derive-server-url`, a missing `server-url` is derived from the objects that route to the Service, if exactly one URL results:
a standard `Ingress` rule (host + root path; `https` if `spec.tls` covers the host) or a Traefik `IngressRoute` (route `Host(\`x\`)` exactly,
`https` if `spec.tls` is set; `traefik.io/v1alpha1`, Traefik v3) or a Gateway API `HTTPRoute` (`gateway.networking.k8s.io/v1`: explicit `hostnames`, a rule
that matches the whole host, plain Service `backendRefs`; `https` if a parent Gateway listener for the host uses protocol `HTTPS`). `PathPrefix` rules, wildcard hosts and several different hosts are skipped
(a path prefix needs to know whether the controller strips it), and an explicit annotation always wins. The IngressRoute and Gateway API CRDs are optional:
without them that part is ignored and picked up when the CRD appears.

**Inside the cluster** (`--kube-access=service`, the default when a ServiceAccount is present): Services are reached by their DNS name.

```bash
kubectl apply -k deploy/kubernetes        # namespace, read-only RBAC, one pod with the API and the web server
kubectl apply -f deploy/kubernetes/ingressroute.example.yaml   # optional: expose the UI through Traefik
kubectl apply -f deploy/kubernetes/demo.yaml                   # optional: demo Services with an Ingress and an IngressRoute
kubectl apply -f demo/kubernetes-paths.yaml                    # optional: PathPrefix / StripPrefix routing examples
```

`deploy/kubernetes/deployment.yaml` runs both programs in one pod (they talk over localhost), non-root, read-only filesystem, all capabilities
dropped, no Docker socket (`--docker=false`). The image is `swagger-aggregator:local`; build and push your own and change the tag.
`rbac.yaml` is cluster-wide read access to services, endpointslices, ingresses, ingressroutes, httproutes and gateways; for fewer namespaces use a `Role` per namespace
and pass `--namespace`. If RBAC is incomplete the source logs a warning after 30 s and continues with what it can read (no derived URLs).

**Outside the cluster** (`--kube-access=apiserver`, the default without a ServiceAccount): pod and service IPs are usually not routable, so the
aggregator goes through the API server's service proxy (`/api/v1/namespaces/<ns>/services/<name>:<port>/proxy`) using your kubeconfig credentials.
Only that one host gets the authenticated transport.

```bash
go run ./cmd/swagger-aggregator --docker=false --kubernetes --derive-server-url   # current kubeconfig context
```

Limits of the API server proxy: it needs `get` on `services/proxy`, it speaks HTTP to the Service only, and it consumes the `Authorization` header, so
Try-it calls that need their own `Authorization` header do not work in this mode (use the in-cluster deployment for those).

## Scaling

**The API instances need no coordination.** Everything an API instance holds is *derived* from Docker or Kubernetes (read-only
discovery) plus a fetch of each spec: nothing is authoritative, so a second instance simply builds the same picture on its own.
Run two or more for availability (`examples/scaled.yml`, `replicas:` in the Kubernetes deployment).

- **Server-Sent Events need no routing.** An event stream is served by the instance that accepted the connection, from that instance's
  own state, which is the same as everybody else's (only `lastSeen` timestamps differ by milliseconds). A reconnect to another instance is
  harmless: every message is a full snapshot. There is no "right pod" to send events to, so no sticky sessions are needed.
- **What it costs:** every instance runs its own watchers and fetches every spec itself, so probe traffic grows with the number of instances
  (`--probe-interval` is the knob). In Kubernetes each instance also runs its own informers.
- **Instances must agree.** They do: the spec hash does not depend on the instance (found and fixed while testing two API containers against
  three compose replicas: they had picked different replicas to document the service).
- **Docker discovery is per host.** One API instance sees the containers of one Docker daemon; there is no multi-host or Swarm support.
- **The web server:** stateless with `auth=none` or `oidc` (give every replica the **same** `SESSION_SECRET`; the login throttle is per
  replica) and with MCP (stateless). With `auth=build_in` it is a **single instance**: the users are one SQLite file.

**Leader election and Redis: possible, deliberately not built.** A Kubernetes `Lease` (client-go leader election) could let only the
leader fetch specs, but then the others need the results, which means a shared store plus pub/sub so that each instance can still feed its
own event streams, and a gap or stale data whenever the leader changes. That trades a few small HTTP requests per probe interval for a new
dependency and new failure modes; it pays only with hundreds of services *and* many instances. `redsync` is a Redis *lock* library: it
serialises work, it does not share state, so it would not solve this problem. If the probe load ever matters, first raise
`--probe-interval` or run two instances instead of ten.

Tested: two API containers against one Docker daemon (consistent lists and spec hashes, event stream through the web server). Not tested:
several pods in Kubernetes, several web replicas.

## Development

```bash
# backend (needs a reachable Docker daemon)
go run ./cmd/swagger-aggregator --listen :8080
# Docker Desktop: container IPs are NOT routable from the host, so a host-side API cannot reach them.
# Publish the containers' ports (ports: ["127.0.0.1::8000"]) and add --prefer-published-ports,
# or run the API in a container on the same network (the compose setup does that).

# frontend with hot reload (bun) - no Go web server needed: Vite serves the UI and proxies /api (incl. SSE), /auth and /mcp
# to the backend; /spec/openapi.* (the API page) is read from internal/apidocs/openapi.yaml by a Vite plugin
cd web && bun install && bun run dev      # http://localhost:5173, backend http://localhost:8080 = the API server
# other backend address: SWAGGER_AGGREGATOR_BACKEND=http://host:port bun run dev
# with auth=none the API server alone is enough; to try the login (auth=build_in/oidc) or MCP, point the backend at the
# Go web server (swagger-aggregator-web), which answers /auth and /mcp

# production mode: build the UI, then serve it with the Go web server
cd web && bun install --frozen-lockfile && bun run build && cd ..   # `bun run bundle` = vite only, no typecheck
go run ./cmd/swagger-aggregator-web --dir web/dist --api http://localhost:8080   # http://localhost:3000

# tests and lint (explicit patterns: ./... would also walk web/node_modules)
go test -race ./cmd/... ./internal/...
golangci-lint run ./cmd/... ./internal/...   # needs golangci-lint v2.15+, config in .golangci.yml

# integration test against the REAL local Docker daemon (skips if there is none). It uses a unique label prefix,
# network and container names, so it does not show up in a swagger-aggregator instance already running on that daemon.
go test -tags=integration -count=1 -timeout 180s ./internal/discovery/docker -run Integration

# integration test against a Kubernetes cluster: creates and deletes a namespace, so it only runs when you name the context
SWAGGER_AGGREGATOR_IT_KUBE_CONTEXT=docker-desktop go test -tags=integration -count=1 -timeout 240s ./internal/app -run KubernetesIntegration
cd web && bun run typecheck

# frontend regression tests (Playwright against the production build + a mock API)
cd web && bunx playwright install chromium && bun run test:e2e
#   or with the locally installed Chrome: PW_CHANNEL=chrome bun run test:e2e
```

The Playwright suite also runs the real Go web server with `auth=build_in` against the built UI (`e2e/auth.spec.ts`: setup, login, users); it builds the binary itself, so Go must be installed (or set `E2E_WEB_BIN` to a prebuilt one). It pins the Scalar behaviours listed below (no external requests, status flips keep position,
remount keeps the open document, stale hash cleanup, group/order, hide-offline, Try-it through the proxy).
Note: `vue-tsc` (typecheck) needs Node; the Docker image has none, so it builds with `bun run bundle`.

`swagger-aggregator-web` flags/env: `--listen`/`SWAGGER_AGGREGATOR_WEB_LISTEN` (`:3000`), `--dir`/`SWAGGER_AGGREGATOR_WEB_DIR` (`web/dist`),
`--api`/`SWAGGER_AGGREGATOR_WEB_API` (`http://localhost:8080`), the `--auth`/`--oidc-*`/`--session-*` options ([Authentication](#authentication)), `--cors-origins`/`SWAGGER_AGGREGATOR_WEB_CORS_ORIGINS` and `--cookie-samesite`/`SWAGGER_AGGREGATOR_WEB_COOKIE_SAMESITE` ([a frontend on another origin](#build-your-own-frontend)), `--base-path`/`SWAGGER_AGGREGATOR_WEB_BASE_PATH` (none; e.g. `/docs`,
stripped when present, so it works with or without a `StripPrefix` in front; set the same value on the API).

## CI/CD

`.github/workflows/ci.yml` (GitHub Actions) and `.gitlab-ci.yml` (GitLab CI) run the same checks: `go vet` + `go test -race`, golangci-lint,
`govulncheck`, the web typecheck + build + Playwright e2e tests, rendering of the Kubernetes/compose files and a multi-arch (amd64, arm64)
image build. The image is pushed to `ghcr.io/<owner>/<repo>` (GitHub) or the project's container registry (GitLab) from the default branch and
from `v*` tags; merge/pull requests only prove that it builds. Neither pipeline has run yet: they were checked for valid YAML and by running
their commands locally, not on a runner. Pin the lint version and the base images once you want reproducible builds.

## Authentication

Off by default (`SWAGGER_AGGREGATOR_WEB_AUTH=none`). `swagger-aggregator-web` can protect **everything** it serves, the UI, `/api/*`
(including the event stream) and the Try-it proxies, in one of two ways. The API container stays internal and needs no auth of its own,
because it is not published. API calls without a session get `401` (the UI then reloads and shows the login). `/healthz` stays open for probes.

| `SWAGGER_AGGREGATOR_WEB_AUTH` | What |
|---|---|
| `none` (default) | no login |
| `build_in` | own login form and users stored in SQLite, with a user management page; see [Built-in users](#built-in-users) |
| `oidc` | login at an OpenID Connect provider ([Authelia](https://www.authelia.com), Keycloak, Authentik, Dex, Entra ID, ...); see below |

### OpenID Connect

`auth=oidc`: browser navigations are redirected to the provider.

| Variable (flag `--x-y`) | Default | Meaning |
|---|---|---|
| `SWAGGER_AGGREGATOR_WEB_AUTH` | `none` | `oidc` for this table |
| `SWAGGER_AGGREGATOR_WEB_OIDC_ISSUER` | – | issuer URL of the provider (its `/.well-known/openid-configuration` is used) |
| `SWAGGER_AGGREGATOR_WEB_OIDC_CLIENT_ID` / `..._CLIENT_SECRET` | – | the client registered for this app |
| `SWAGGER_AGGREGATOR_WEB_OIDC_REDIRECT_URL` | – | public callback URL, `<public url><base path>/auth/callback`, registered at the provider |
| `SWAGGER_AGGREGATOR_WEB_SESSION_SECRET` | – | signs and encrypts the cookies, at least 32 characters (`openssl rand -base64 48`) |
| `SWAGGER_AGGREGATOR_WEB_OIDC_ALLOWED_GROUPS` | – | comma separated; only members may sign in |
| `SWAGGER_AGGREGATOR_WEB_OIDC_ALLOWED_EMAILS` | – | comma separated; alternative to groups (the provider must report the address as verified) |
| `SWAGGER_AGGREGATOR_WEB_OIDC_GROUPS_CLAIM` | `groups` | claim with the user's groups (also read from userinfo if the ID token lacks it) |
| `SWAGGER_AGGREGATOR_WEB_OIDC_SCOPES` | `openid,profile,email,groups` | requested scopes |
| `SWAGGER_AGGREGATOR_WEB_SESSION_TTL` | `8h` | login lifetime |

Without allowed groups/emails every user the provider authenticates may sign in. Pass secrets as environment variables (a Kubernetes
`Secret`, a compose `env_file`), not as flags. Details: authorization code flow with PKCE, `state` and `nonce`, `HttpOnly` + `SameSite=Lax`
cookies (`Secure` when the redirect URL is `https`), local-only return paths. `GET /auth/logout` ends the local session only; the provider's
own session may sign you straight back in. The provider may start later than this server: login answers `503` until it is reachable.

Authelia (OIDC client, adapt to your Authelia version; the secret is stored hashed there):

```yaml
identity_providers:
  oidc:
    clients:
      - client_id: swagger-aggregator
        client_name: Swagger Aggregator
        client_secret: '$pbkdf2-sha512$...'   # authelia crypto hash generate pbkdf2 --password '<secret>'
        public: false
        authorization_policy: two_factor
        redirect_uris: [https://docs.example.com/auth/callback]
        scopes: [openid, profile, email, groups]
```

Verified end to end with Dex and a real browser (login, requested page restored, UI + event stream, logout); the flow itself is covered by
unit tests against a fake provider. **Not tested against Authelia itself.** As an alternative without any code in this app you can
keep `auth=none` and let Traefik (or nginx) ask Authelia through a `forwardAuth` middleware on the router of `swagger-aggregator-web`.

### Built-in users

`auth=build_in`: the web server shows its own login form (PrimeVue + Tailwind, Vue) and keeps the users in a SQLite database
(GORM with `modernc.org/sqlite`, SQLite transpiled to Go: **no CGO and no WebAssembly**) in `SWAGGER_AGGREGATOR_WEB_DATA_DIR` (`/data` in the image and in compose, `data` otherwise).
The schema is created by Atlas migrations that are embedded in the binary and applied at startup.

- **First start without users:** every page shows a *setup* form. It needs the **setup token** that is printed in the log at startup
  (`docker compose logs web`) or set with `SWAGGER_AGGREGATOR_WEB_SETUP_TOKEN`; without the token nobody can claim a freshly
  started, reachable instance. Wrong tokens are throttled. The first account is an admin.
- **Or from the environment:** `SWAGGER_AGGREGATOR_WEB_ADMIN_USER` + `SWAGGER_AGGREGATOR_WEB_ADMIN_PASSWORD` (or `..._ADMIN_PASSWORD_HASH`,
  a bcrypt hash) create the admin on the first start instead of the setup page. The environment **never overwrites** an existing account.
- **Users page** (admins, `/users`): create users, set a new password, change the role, disable, delete. Roles: `admin` (manages users)
  and `user` (reads the docs). The last active admin cannot be demoted, disabled or deleted, nobody can delete themselves. Every user can
  change their own password in the account menu.
- **Sessions:** an encrypted, signed `HttpOnly` + `SameSite=Lax` cookie (`Secure` behind HTTPS). A password change, disabling or deleting a
  user ends their sessions at once. The cookie secret is `SWAGGER_AGGREGATOR_WEB_SESSION_SECRET`, or, if unset, a key file `session.key`
  in the data dir (so logins survive restarts).
- **Passwords:** bcrypt, 10-72 characters. Failed logins are delayed per username (1 s, 2 s, ... up to 30 s; a delay, not a lock-out, so
  nobody can keep a user out). Unknown users cost the same time as wrong passwords. State-changing requests must be JSON.
- **Keep the data:** mount a volume on the data dir (compose: the named volume `web-data`, already there; Kubernetes: replace the `emptyDir`
  by a PersistentVolumeClaim). Without it the users are gone when the container is re-created and the setup form opens again.
- **Limits:** one instance only (SQLite file; the deployment has one replica anyway), no two-factor login, no self-service password reset
  (an admin sets a new one). If the last admin password is lost, stop the server and delete `swagger-aggregator.db` in the data dir (this
  removes all users), then start again to register a new admin.

| Variable | Default | Meaning |
|---|---|---|
| `SWAGGER_AGGREGATOR_WEB_AUTH` | `none` | `none`, `build_in` or `oidc` |
| `SWAGGER_AGGREGATOR_WEB_DATA_DIR` | `data` (`/data` in the image) | database and `session.key` |
| `SWAGGER_AGGREGATOR_WEB_ADMIN_USER` / `..._ADMIN_PASSWORD` / `..._ADMIN_PASSWORD_HASH` | – | first admin from the environment |
| `SWAGGER_AGGREGATOR_WEB_SETUP_TOKEN` | random, logged | token for the setup form |
| `SWAGGER_AGGREGATOR_WEB_SESSION_SECRET` | key file in the data dir | at least 32 characters |
| `SWAGGER_AGGREGATOR_WEB_SESSION_TTL` | `8h` | login lifetime |

Schema changes: edit the models in `internal/store/schema`, then `atlas migrate diff <name> --env gorm` (see `atlas.hcl`; needs the
[Atlas CLI](https://atlasgo.io), which the app itself does not). The first migration was generated by `tools/migrate-gen`, a separate Go
module so that the application does not depend on the GORM provider's MySQL/Postgres drivers. The Atlas CLI step itself is untested here.

## MCP (AI assistants)

`SWAGGER_AGGREGATOR_WEB_MCP=true` serves a [Model Context Protocol](https://modelcontextprotocol.io) server at `/mcp` (Streamable HTTP,
stateless, the official Go SDK). It is **read-only**: it cannot call the documented services and has no Try-it. Tools:

| Tool | Does |
|---|---|
| `list_services` | the services with status, group, whether a spec exists and the last problem |
| `list_endpoints` | method, path, summary, operationId, tags of one service |
| `search_endpoints` | search all services by a word in name, path, summary, description, operationId or tags |
| `get_spec` | the complete spec of one service as JSON (can be large) |

Clients send `Authorization: Bearer <token>`; the token is `SWAGGER_AGGREGATOR_WEB_MCP_TOKEN` (at least 32 characters, e.g.
`openssl rand -base64 32`) and is **required**. `/mcp` sits outside the session login on purpose (assistants cannot do a browser login),
so treat the token like a password: whoever has it can read all specs. It is one shared token (no per-user tokens, no rate limit; put
that in the reverse proxy). Specs can be sensitive: they go to whatever model the client uses. Example client entry (Claude Code):
`claude mcp add --transport http swagger-aggregator https://docs.example.com/mcp --header "Authorization: Bearer <token>"`
(untested here; the server was tested with the Go SDK client and with curl).

## Security


- **Do not mount `/var/run/docker.sock` into the app.** The compose file puts a
  `docker-socket-proxy` in front (`CONTAINERS=1`, `EVENTS=1`; `POST` stays off, so it is read-only)
  on an `internal` network that only the API shares. `EVENTS`, `PING` and `VERSION` are on by default in the proxy.
- **Container inspect can expose environment variables, including secrets.** The app inspects
  every labeled container (needed for health, network, ports) and only keeps labels/state/IP, but the
  proxy still lets *the app* read the full inspect output, and `CONTAINERS=1` covers all containers, not just labeled ones.
  Anyone who can compromise the app can read those. Keep the app patched and don't expose the proxy.
- Authentication is **off by default**; see [Authentication](#authentication). Route only `swagger-aggregator-web` through your reverse proxy and do not publish the API port (see
  [Traefik](#traefik)). The spec endpoint serves whatever the documented services publish, and the Try-it proxies let
  any UI user send requests to every registered container, so treat the UI as sensitive as those services.
- Hardening in compose: the two Go containers run non-root, `read_only`, `cap_drop: [ALL]`, `no-new-privileges`, with memory/pid
  limits; `swagger-aggregator-web` sends `nosniff`, `Referrer-Policy: same-origin`, `X-Frame-Options: DENY` and caches hashed assets as
  immutable. No CSP yet (Scalar's inline styles/workers need testing). The socket proxy only gets `no-new-privileges`
  (more would need testing against its HAProxy). Pin its image by digest in production.
- Unauthenticated SSE clients each hold a connection; rate limiting belongs in the proxy.

## UI stack and licences

The documentation is [Scalar](https://github.com/scalar/scalar). The login, setup, status and users pages are Vue + **PrimeVue 5** + Tailwind
CSS 4 (`web/src/shell`, `web/src/views`); Tailwind and PrimeVue are loaded only for those pages, so Scalar keeps its own styles (an e2e test checks
that).

**PrimeVue 5 is not MIT.** It is under the *PrimeUI* licence and needs a **licence key**, which the browser verifies offline (no network, no
telemetry). This project uses the **Community** licence, which PrimeTek grants free of charge to individuals and to organisations below
its limits (fewer than 5 developers and 10 employees, under 1 M USD revenue, under 3 M USD outside funding), renewed yearly by confirming that you
still qualify. Whether *you* qualify is for you to check against [the licence](https://primeui.dev/licenses/community); it is not decided here.

- **The key is not in the repository** (the licence says it must not be published for others to use). It is given at build time:
  `VITE_PRIMEVUE_LICENSE` for `bun run dev` / `build` (put it in `web/.env.local`, which git ignores), and `PRIMEVUE_LICENSE` for the Docker
  build (`docker build --build-arg PRIMEVUE_LICENSE=...`; `docker compose` reads it from `PRIMEVUE_LICENSE` in a git-ignored `.env`). In CI set it
  as a secret. Note that the key necessarily ends up **inside the built JavaScript**; the licence says it may and that it holds nothing sensitive.
- **Without a key everything works**, but PrimeVue logs a console warning and shows a small red *"Invalid PrimeUI License"* badge in the corner of
  the login, setup, status and users pages. So anyone who clones this repository and builds it needs their own key (or lives with the badge).
- **The key expires** (the one in use here: 2027-10-24, plus a 30-day grace period). After that the badge appears until you renew it.
- **What this means for open-sourcing the repository:** the code can be published under a free licence, but PrimeVue 5 is a dependency with its
  own terms that every user of the *built* UI has to meet. If that is unwanted, PrimeVue **4.5.5 (MIT, its last 4.x release, April 2026)** is
  the free alternative: it needs no key and gets no further updates, and the change back is small (`primevue@4.5.5`, `@primeuix/themes@2.0.3`,
  `primeicons@7.0.0`, drop the `license` option in `web/src/main.ts`).

## Frontend behaviour (verified against `@scalar/api-reference` 1.76.0)

Tested in Chrome with a mock backend. Findings:

1. **Reactive config changes** – the component has a deep watcher on `configuration`.
   - Status flips (title change), spec hash changes and `url` changes **keep the selected document and the
     scroll position**; only the changed document is refetched. This works only because each source has an explicit `slug`; otherwise the slug
     is derived from the title and a status marker would change the document identity.
   - Adding/removing/reordering sources **resets the view to the first document** (the component calls
     `changeSelectedDocument(first)` whenever the ordered slug list changes).
   - Mitigation (implemented): remount only when the set of slugs changes (`:key`). The remount restores the open
     document **and section** from the URL hash. The exact pixel offset is not preserved (3000px → 4704px in my test,
     because it scrolls the section into view). If the open service disappears, the hash is cleared and the first document opens.
2. **Empty `sources` at startup** – it does not crash and recovers once sources arrive, but the component then runs with
   its defaults: it **loads fonts from `fonts.scalar.com`** (ignoring `withDefaultFonts: false`) and logs
   `Document  not found in configList`. So `ApiReference` is mounted only after the first non-empty list.
3. **`slug` per source** – supported (`SourceConfiguration.slug`) and used in the URL hash (`#bravo/…`).
4. **`proxyUrl`** – with a *relative* `proxyUrl`, Scalar rewrites every request to an *absolute* URL as
   `<proxyUrl>?scalar_url=<absolute URL>` and leaves relative URLs alone (read from `@scalar/helpers`). Because it resolves our
   relative `servers` to absolute URLs first, those go through the proxy as well (handled, see "Try it").

Also set: `withDefaultFonts: false`, `telemetry: false`, `agent: { disabled: true }` (otherwise "Ask AI" is shown and fonts come from Scalar's CDN).
With these, the page contacts only its own origin (asserted by the e2e suite). Scalar itself is loaded lazily (entry bundle ~74 KB).
The view has a "Hide offline services" checkbox (Scalar's `sidebar-start` slot, remembered in `localStorage`).

## Verification status

Verified on a **real Docker daemon** (Docker Desktop 29.8 + socket proxy), by hand and by `go test -tags=integration`:
both images build, both containers become healthy through the `healthcheck` subcommand while `read_only`/`cap_drop: ALL`/non-root,
the web server serves the UI and security headers, the API discovers containers through the socket proxy, and the integration
test covers start, stop (down, spec kept, no error), restart, destroy and **health-status events** (unhealthy → down,
healthy → up). Bugs found there and fixed: a stopped container was reported as "not attached to network", wrong binary names in
the image, `vue-tsc` under Bun.

Verified against a **real Traefik 3.7.16** (see [Traefik](#traefik)): the derived `server-url` (`https://orders.example.test`) matches a route Traefik
really serves; an explicit `server-url` label wins; the UI, API and event stream work through Traefik over HTTP/2 (snapshot at 0 s, a
container stop arrived 1 s later, heartbeats at 15 s and 30 s, stream open for 40 s); the same with the `compress` middleware active;
in Chrome the page contacted only the docs host and Try-it for the Traefik-derived public URL showed the container's response; a basic-auth
middleware on the router protected every path, including both proxies and the event stream.

Verified on a **real Kubernetes cluster** (Docker Desktop, Kubernetes 1.36, real Traefik 3.7.16 installed with its Helm chart), both ways:
- *Outside* (kubeconfig, API server proxy): Services discovered, readiness from EndpointSlices, specs fetched through the API server proxy;
  the derived public URLs came from a real `Ingress` (`https://orders.k8s.example.test`) and a real `IngressRoute` and match routes Traefik really serves;
  Try-it for those URLs returned the Services' data; scaling a Deployment to zero showed down within 1 s of the command and back up on scale-up;
  removing the label removed the service; `--kube-access=auto` chose the API server proxy; `--namespace` restricted the watch.
- *Inside* (image deployed with `deploy/kubernetes`): non-root, read-only filesystem, no capabilities; `access=service` auto-detected, in-cluster DNS,
  the RBAC is sufficient; in Chrome, through Traefik, the UI contacted only its own host and Try-it worked for both the Ingress- and the IngressRoute-derived URL.
- With incomplete RBAC (no ingresses/ingressroutes) it warns after 30 s and keeps working without derived URLs.
- Bugs found there and fixed: `USER nonroot` (a name) is rejected by Kubernetes' `runAsNonRoot`; the Dockerfile now uses the numeric uid 65532.

Verified on a **second, different cluster**: k3s v1.35.5 created with k3d (a different distribution from Docker Desktop's kind-based one), with its own
bundled Traefik 3.6.13. Same results inside and outside the cluster (derived URLs from an `Ingress` and an `IngressRoute`, Try-it, readiness, JSON logs from the pod),
and the Kubernetes integration test passes there too (`KUBECONFIG=<k3d kubeconfig> SWAGGER_AGGREGATOR_IT_KUBE_CONTEXT=k3d-<name> go test -tags=integration ...`).

Verified with **Traefik and an ACME resolver**: Traefik 3.7.16 with `certificatesresolvers.le.acme` pointed at Pebble (Let's Encrypt's test CA; real ACME protocol with
HTTP-01 validation): the certificate was issued by "Pebble Intermediate CA" (a router without a resolver gets Traefik's default certificate). A router that has only
`tls.certresolver` (no `tls=true`) derives `https://...` correctly and Try-it works. Real Let's Encrypt was not used (it needs public DNS and ports); Traefik's ACME
handling is not our code, only the label/`spec.tls` detection is.

Verified with **PathPrefix and StripPrefix routers** on real Traefik, on both Docker (labels) and Kubernetes (`IngressRoute` + `Middleware`), comparing what the public URL returns with
what the aggregator's Try-it returns for the same URL:
- Traefik strips `/strip` -> matches with the default (`strip-prefix` true).
- Traefik keeps `/api` (the workload serves `/api/items`) -> **did not match** with the default; this was a real gap, now handled by `strip-prefix: "false"`, and matches.
- A route with `PathPrefix` and no `server-url`/annotation is deliberately not derived (the aggregator cannot know whether a middleware strips the prefix); the
  spec then uses the same-origin proxy path and Try-it works.

Not verified: Traefik's file provider, Gateway API (`HTTPRoute`) and Traefik v2 CRDs (`traefik.containo.us`) which are not implemented, a managed cloud cluster
(EKS/GKE/AKS), the Docker integration test on a native Linux daemon (it then uses container IPs; on Docker Desktop it uses published ports).

**Docker Desktop and the Kubernetes cluster.** Docker Desktop's cluster is a kind cluster: its node is a Docker container (`desktop-control-plane`) that Docker Desktop hides from
`docker ps -a` and from the Containers view, and the pods are containerd containers *inside* that node, so they are never Docker containers. Use `kubectl` to see them.
A k3d cluster, by contrast, shows its nodes as ordinary containers in `docker ps` (`k3d-<name>-server-0`).

## Known limitations

- PrimeVue 5 needs a licence key (Community, renewed yearly); without it a badge shows on the PrimeVue pages. See [UI stack and licences](#ui-stack-and-licences).
- The built-in authentication is single-node SQLite without two-factor login or self-service password reset; the MCP token is one shared secret. See [Authentication](#authentication) and [MCP](#mcp-ai-assistants).
- `moby/moby/client` is **v0.x** (v0.6.2 is the latest release, checked 2026-10-24); its API may still change between releases. Pinned in `go.mod`.
- Compose replicas (`scale`) are one service: it is up while any replica is up and is documented by one healthy replica (the spec's "Try it" path points at that replica, and moves with it). Replicas are recognised by the compose project + service labels; unrelated containers that merely share a name get a `-<id>` suffix, which is only stable while the container runs.
- `servers` (also on path items and operations) and top-level `host`+`basePath`+`schemes` are rewritten; callback/webhook `servers` are left alone.
- Spec parsing re-serializes the document (keys are sorted, YAML is converted to JSON). `$ref`s to *external* files are not resolved.
  Unchanged specs (identical bytes) are not re-parsed on later probes.
- A service that never delivered a spec has no document in the reference view (Scalar needs a spec); it is listed with its error in the sidebar and on `/status`.
- The proxies are plain forwards to the container (HTTPS only with `scheme=https`; redirects are passed to the browser as is).
  A `server-url` with a path is treated like a `StripPrefix` (the workload sees the path without it) unless you set `strip-prefix: "false"`. Routes that
  rewrite the path in other ways (e.g. `ReplacePathRegex`) are not understood.
- Kubernetes: services in different namespaces are named `<name>.<namespace>` unless `.name` is set; ingress-derived URLs need exactly one host-based route
  with the root path per Service; `ExternalName` Services and Services without ready endpoints are shown as down.
- Last-known specs are kept only in memory: a stopped container that exists when the API restarts is listed as down *without* a spec.
- `swagger-aggregator-web` must run in front of the API (or any reverse proxy that serves `/` from `web/dist` and `/api` from the API). Under a sub-path (`--base-path`) the proxy must forward that prefix to `swagger-aggregator-web`, which strips it itself.
- `typescript` is aliased to `typescript-native-bridge` (a tsgo-backed TypeScript fork from the `johnsoncodehk/typescript-native-bridge` repo, pinned to an exact version): `vue-tsc` 3.3.13 does not work with TypeScript 7 yet ([vuejs/language-tools#6124](https://github.com/vuejs/language-tools/issues/6124)). Drop the alias once a `vue-tsc` release supports TypeScript 7. It is a third-party package with a native binary, used only for `bun run typecheck` (not part of the Docker image); the typecheck needs Node (it fails under `bun --bun`).
