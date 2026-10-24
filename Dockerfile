# syntax=docker/dockerfile:1

FROM oven/bun:1.3-alpine AS web
WORKDIR /src/web
COPY web/package.json web/bun.lock ./
RUN bun install --frozen-lockfile
COPY web/ ./
# PrimeVue 5 needs a licence key at runtime in the browser; it is not in the repository. Pass it at build time:
#   docker build --build-arg PRIMEVUE_LICENSE=... .   (docker compose reads it from PRIMEVUE_LICENSE in .env)
# Without it the UI works and shows a small "Invalid PrimeUI License" badge. The key ends up in the built JS (the
# licence allows that), so the image is as public as the UI is.
ARG PRIMEVUE_LICENSE=""
ENV VITE_PRIMEVUE_LICENSE=$PRIMEVUE_LICENSE
# vite only: vue-tsc cannot patch TypeScript under the Bun runtime (no Node in this image); typecheck runs in dev/CI.
RUN bun run bundle

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
# KUBERNETES=false builds the API without Kubernetes support (-tags nokubernetes): client-go and the Kubernetes API
# types are about 26 MB of the 31 MB API binary, and a Docker-only setup does not need them. The default (true) is the
# full API, which the Kubernetes manifests use. docker-compose.yml builds the small one.
ARG KUBERNETES=true
RUN TAGS=""; if [ "$KUBERNETES" = "false" ]; then TAGS="-tags nokubernetes"; fi; \
    CGO_ENABLED=0 go build $TAGS -trimpath -ldflags="-s -w" -o /out/swagger-aggregator ./cmd/swagger-aggregator \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/swagger-aggregator-web ./cmd/swagger-aggregator-web
# Optional: COMPRESS_BINARIES=best or =lzma packs both binaries with UPX (about -65 %), at the price of a slower start (~+0.08 s / +0.35 s),
# ~5 MB more RAM per process and binaries that some security scanners flag. Off by default.
# (not called UPX: UPX itself reads its options from an environment variable of that name)
ARG COMPRESS_BINARIES=""
RUN case "$COMPRESS_BINARIES" in \
      "") ;; \
      best) apk add -q --no-cache upx && upx -q --best /out/swagger-aggregator /out/swagger-aggregator-web ;; \
      lzma) apk add -q --no-cache upx && upx -q --best --lzma /out/swagger-aggregator /out/swagger-aggregator-web ;; \
      *) echo "COMPRESS_BINARIES must be empty, best or lzma" >&2; exit 1 ;; \
    esac
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
