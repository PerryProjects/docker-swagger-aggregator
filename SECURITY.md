# Security policy

## Reporting a vulnerability

Please **do not open a public issue** for a security problem. Use GitHub's private reporting instead:
**Security → Advisories → Report a vulnerability** on this repository. Say what you found, how to reproduce it and
which version or commit you tested.

This is a small project with one maintainer: reports are answered on a best-effort basis, there is no fixed response
time. A fix is released before the details are published.

## Supported versions

Only the current `main` branch. There are no tagged releases yet.

## What to know when you run it

These are design choices, not bugs, but they decide how safely you can run it (details in the README, "Security"
and "Authentication"):

- **There is no login by default** (`SWAGGER_AGGREGATOR_WEB_AUTH=none`). Anyone who can reach the web server can read all
  specs and use "Try it". Turn on `build_in` or `oidc`, or keep it on a private network behind your own access control.
- **"Try it" is a proxy:** any user of the UI can send requests to every registered container. Treat the UI as sensitive
  as those services.
- **Do not publish the API port** (`swagger-aggregator`, :8080); only the web server is meant to be reachable.
- **Docker access:** the API talks to Docker through a read-only socket proxy; never mount `/var/run/docker.sock` into it.
  Container inspect output can contain environment variables, so a compromised API can read them.
- **The MCP endpoint** (`/mcp`) is guarded by one shared bearer token (at least 32 random characters). Whoever has it can
  read all specs.
- **Secrets** (OIDC client secret, session secret, MCP token, admin password) belong into environment variables or
  secrets, not into files in the repository. The PrimeVue licence key is not secret by design, but must not be published
  by you either; see "UI stack and licences" in the README.
