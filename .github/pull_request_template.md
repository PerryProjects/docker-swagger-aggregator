## What and why

<!-- What does this change, and why? Link the issue if there is one. -->

## Checklist

- [ ] Every commit is signed off (`git commit -s`, see [CONTRIBUTING.md](../CONTRIBUTING.md))
- [ ] `gofmt -l .`, `go vet ./...` and `go test -race ./...` pass (also with `-tags nokubernetes` for `./internal/app/ ./cmd/...`)
- [ ] UI changes: `cd web && bun run typecheck && bun run test:e2e`
- [ ] New behaviour has a test; a change to an endpoint or the service JSON updates `internal/apidocs`
- [ ] No secrets, keys or personal data in the diff (the PrimeVue licence key never goes into the repository)
