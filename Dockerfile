FROM golang:1.22-alpine AS build
WORKDIR /src
ARG VERSION=dev
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X github.com/floholz/ts-server-manager/internal/version.Version=${VERSION}" \
    -o /out/ts-server-manager .

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
ARG REVISION=
LABEL org.opencontainers.image.title="ts-server-manager" \
      org.opencontainers.image.description="HTTP API sidecar for TeamSpeak 3 ServerQuery: health, server info, Docker Hub update check." \
      org.opencontainers.image.source="https://github.com/floholz/ts-server-manager" \
      org.opencontainers.image.url="https://github.com/floholz/ts-server-manager" \
      org.opencontainers.image.documentation="https://github.com/floholz/ts-server-manager#readme" \
      org.opencontainers.image.licenses="GPL-3.0-only" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"
COPY --from=build /out/ts-server-manager /ts-server-manager
USER nonroot:nonroot
EXPOSE 9988
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/ts-server-manager", "healthcheck"]
ENTRYPOINT ["/ts-server-manager"]
