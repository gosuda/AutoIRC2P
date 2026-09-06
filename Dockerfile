# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM oven/bun:1.3.14@sha256:e10577f0db68676a7024391c6e5cb4b879ebd17188ab750cf10024a6d700e5c4 AS frontend
WORKDIR /src/web
COPY web/package.json web/bun.lock ./
RUN bun install --frozen-lockfile
COPY web/ ./
COPY tokens.css /src/tokens.css
RUN bun run check && bun run build

FROM --platform=$BUILDPLATFORM golang:1.27.0-bookworm@sha256:ded31c68586d2e49e760acc2e65a884b23d032e9bbbed0ae0c55abd3fcaf4452 AS backend
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/autoirc2p ./cmd/autoirc2p
RUN install -d -m 0700 -o 65532 -g 65532 /runtime-data

FROM gcr.io/distroless/static-debian13:nonroot
WORKDIR /
COPY --from=backend --chmod=0555 /out/autoirc2p /app/autoirc2p
COPY --from=frontend /src/web/build /app/web
COPY --from=backend --chown=65532:65532 /runtime-data /data
COPY LICENSE /app/LICENSE
COPY third_party/translation/LICENSE /app/translation-LICENSE
ENV LISTEN_ADDR=0.0.0.0:8080 \
    DATA_DIR=/data \
    IVNP_CONFIG=/data/ivnp.conf \
    WEB_DIR=/app/web
USER 65532:65532
VOLUME ["/data"]
EXPOSE 8080
STOPSIGNAL SIGTERM
ENTRYPOINT ["/app/autoirc2p"]
CMD ["serve"]
