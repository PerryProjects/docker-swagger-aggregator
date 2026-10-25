# Contributing

Thanks for helping. Bug reports, fixes and improvements are welcome.

## Sign your commits (DCO)

Contributions are accepted under the project's licence, [Apache-2.0](LICENSE), and you certify the
[Developer Certificate of Origin](https://developercertificate.org/) by adding a `Signed-off-by` line to every commit:

```bash
git commit -s -m "fix: describe the change"
```

The line carries the name and e-mail address of your Git configuration. Use a private address if you prefer (GitHub:
*Settings → Emails → Keep my email addresses private*). A check on every pull request fails when a commit lacks it. There
is no CLA.

## Before you open a pull request

```bash
gofmt -l . && go vet ./... && go test -race ./...           # Go
go vet -tags nokubernetes ./... && go test -tags nokubernetes ./internal/app/ ./cmd/...   # the small Docker-only API
cd web && bun install && bun run typecheck && bun run test:e2e   # UI (PW_CHANNEL=chrome to use a local Chrome)
```

The README ("Development") explains each of them, the integration tests and running the UI without the Go web server.

- Keep a change focused and explain the *why* in the commit message; the history uses
  [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`, `chore:` ...).
- New behaviour comes with a test. The API documents (`internal/apidocs`) are checked against the code: a change to an
  endpoint or to the service JSON needs the documents updated.
- The event streams, the status page and the built-in login each have end-to-end tests in `web/e2e`.

## The UI and PrimeVue

The UI uses PrimeVue 5, which needs a licence key (free Community licence; see README, "UI stack and licences"). You do not
need one to work on the project: without it everything works and a small "Invalid PrimeUI License" badge shows on the
PrimeVue pages. To get rid of it locally, put your own key into `web/.env.local` as `VITE_PRIMEVUE_LICENSE=...` (git
ignores that file). **Never commit a key.**

## Reporting security problems

Not in a public issue: see [SECURITY.md](SECURITY.md).
