# syntax=docker/dockerfile:1

FROM oven/bun:1.3-alpine AS web
WORKDIR /src/web
COPY web/package.json web/bun.lock ./
RUN bun install --frozen-lockfile
COPY web/ ./
# vite only: vue-tsc cannot patch TypeScript under the Bun runtime (no Node in this image); typecheck runs in dev/CI.
RUN bun run bundle

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/swagger-aggregator ./cmd/swagger-aggregator \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/swagger-aggregator-web ./cmd/swagger-aggregator-web
# Empty data directory for the built-in authentication (SQLite + session key); distroless has no mkdir.
RUN mkdir /out/data

# One image, two binaries: the API (default) and the static web server.
#   API: /swagger-aggregator        Web: /swagger-aggregator-web  (serves /web/dist, forwards /api)
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/swagger-aggregator /swagger-aggregator
COPY --from=build /out/swagger-aggregator-web /swagger-aggregator-web
COPY --from=web /src/web/dist /web/dist
# Numeric owner as below. Mount a volume here to keep the users of auth=build_in across container re-creations.
COPY --from=build --chown=65532:65532 /out/data /data
# Numeric (distroless "nonroot" = 65532): Kubernetes runAsNonRoot cannot verify a named user.
USER 65532:65532
ENTRYPOINT ["/swagger-aggregator"]
