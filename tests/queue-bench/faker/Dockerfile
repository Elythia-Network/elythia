# syntax=docker/dockerfile:1.7
FROM golang:1.27.1-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY tests/queue-bench/faker ./tests/queue-bench/faker
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/faker ./tests/queue-bench/faker

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=builder /out/faker /usr/local/bin/faker
ENTRYPOINT ["/usr/local/bin/faker"]
