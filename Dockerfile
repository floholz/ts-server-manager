FROM golang:1.22-alpine AS build
WORKDIR /src
ARG VERSION=dev
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X github.com/floholz/ts-server-manager/internal/version.Version=${VERSION}" \
    -o /out/tsm-sidecar . && \
    mkdir -p /out/backups

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
ARG REVISION=
LABEL org.opencontainers.image.title="tsm-sidecar" \
      org.opencontainers.image.description="Sidecar for TeamSpeak 6 servers: health and update checks, scheduled backups, and backup/restore of channels, groups and permissions via ServerQuery." \
      org.opencontainers.image.source="https://github.com/floholz/ts-server-manager" \
      org.opencontainers.image.url="https://github.com/floholz/ts-server-manager" \
      org.opencontainers.image.documentation="https://github.com/floholz/ts-server-manager#readme" \
      org.opencontainers.image.licenses="GPL-3.0-only" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"
COPY --from=build /out/tsm-sidecar /tsm-sidecar
# Owned by the nonroot user (65532) so a named volume mounted here is
# writable without extra setup. Distroless has no shell to mkdir with.
COPY --from=build --chown=65532:65532 /out/backups /backups
USER nonroot:nonroot
EXPOSE 9988
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/tsm-sidecar", "healthcheck"]
ENTRYPOINT ["/tsm-sidecar"]
