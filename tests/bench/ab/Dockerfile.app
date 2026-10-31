# syntax=docker/dockerfile:1.7
#
# base / head の比較 (#3544) で、Elythia の本体だけを入れる image。
#
# build context は比べる commit の worktree で、この Dockerfile は比較を流す側の
# checkout から渡す (`docker build -f`)。両方の commit を同じ手順でビルドするため。
# 本体の Dockerfile は frontend の node_modules を要求するので使わない (API の性能に
# frontend は関わらない)。
#
# 実行バイナリが elythia 1 つにまとまった #3394 (2026-10-05) より前の commit は、
# ./cmd/elythia が無いのでビルドできない (run.sh は exit 1 で止まる)。
FROM golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS builder

# 公式 image は GOTOOLCHAIN=local なので、比べる commit の go.mod がこの image より
# 新しい Go を要求するとビルドが落ちる。auto にして必要な toolchain を取りに行かせる
ENV GOTOOLCHAIN=auto CGO_ENABLED=0

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    GOWORK=off go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOWORK=off go build -tags nodynamic -trimpath -ldflags="-s -w" -o /out/elythia ./cmd/elythia

FROM alpine:3.21@sha256:ee16d92f96db08d9d2d07da48555a27f3a69d8f0759021dbc601195ed0d2b5bd

# wget は healthcheck 用
RUN apk add --no-cache ca-certificates tzdata wget
WORKDIR /app
COPY --from=builder /out/elythia /app/elythia
COPY --from=builder /src/migration /app/migration
ENV PATH=/app:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
EXPOSE 3000
CMD ["sh", "-c", "elythia migrate -config /app/.config/default.yml -direction up && exec elythia serve -config /app/.config/default.yml"]
