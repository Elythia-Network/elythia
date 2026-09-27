# syntax=docker/dockerfile:1.7
FROM golang:1.27.1-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY tests/queue-bench/blackhole ./tests/queue-bench/blackhole
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/blackhole ./tests/queue-bench/blackhole

FROM alpine:3.21
COPY --from=builder /out/blackhole /usr/local/bin/blackhole
ENTRYPOINT ["/usr/local/bin/blackhole"]
