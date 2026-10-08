# syntax=docker/dockerfile:1.7
FROM golang:1.27.2-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY tests/bench/queue/blackhole ./tests/bench/queue/blackhole
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/blackhole ./tests/bench/queue/blackhole

FROM alpine:3.21
COPY --from=builder /out/blackhole /usr/local/bin/blackhole
ENTRYPOINT ["/usr/local/bin/blackhole"]
