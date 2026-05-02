FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download || true
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X github.com/floholz/ts-server-manager/internal/version.Version=$(git -C /src describe --tags --always 2>/dev/null || echo dev)" \
    -o /out/ts-server-manager .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ts-server-manager /ts-server-manager
USER nonroot:nonroot
EXPOSE 9988
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/ts-server-manager", "healthcheck"]
ENTRYPOINT ["/ts-server-manager"]
