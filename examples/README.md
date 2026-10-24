# Compose examples

Overlays for the common combinations of settings. Each one is used **on top of** the base file (and the override
file that publishes the UI on `http://localhost:8080`):

```bash
docker compose -f docker-compose.yml -f docker-compose.override.yml -f examples/<name>.yml up -d --build
```

Overlays can be stacked (`-f examples/build-in-admin.yml -f examples/mcp.yml`). `docker compose ... down -v` also
removes the `web-data` volume, i.e. the users of the built-in login.

| Example | What it sets up | Checked |
|---|---|---|
| *(none)* | no login, Docker discovery: the default `docker compose up` | yes |
| [`build-in-setup.yml`](build-in-setup.yml) | own login; the first admin is registered in the browser with the setup token from the log | yes: setup, login, API |
| [`build-in-admin.yml`](build-in-admin.yml) + [`.env.example`](build-in-admin.env.example) | own login; the first admin comes from the environment | yes: login |
| [`oidc-dex.yml`](oidc-dex.yml) | login at an OpenID Connect provider (a local Dex stands in for Authelia, Keycloak, ...) | yes: real browser, sign-in, status page |
| [`mcp.yml`](mcp.yml) | the read-only MCP server at `/mcp` (stackable with any login) | yes: 401 without token, `tools/list` with it, together with `build_in` |
| [`scaled.yml`](scaled.yml) | two API instances | yes: two containers, same spec hashes |
| [`traefik-build-in.yml`](traefik-build-in.yml) | behind Traefik with TLS, own login, optional MCP and base path | config renders; not run (needs your Traefik) |
| [`kubernetes-remote.yml`](kubernetes-remote.yml) | Kubernetes Services of a remote cluster instead of Docker containers | config renders; **not run** (needs a reachable cluster) |

Settings that have no example of their own and are set the same way (environment of `api` or `web`): HTTPS workloads
(`--insecure-upstream-tls`, label `scheme=https`), the base path, probe interval, label prefix: see the
[configuration table](../README.md#configuration).
