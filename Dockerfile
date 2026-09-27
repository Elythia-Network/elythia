# syntax=docker/dockerfile:1.7
#
# `# syntax=` directive は BuildKit の RUN --mount=type=cache を有効に
# するために必須 (#432)。ローカル `docker build` でも CI (GitHub Actions
# runner) でも default frontend が 1.5+ になる現代では `1.7` で問題なし。

# Stage 1: Build Go binary
#
# patch version まで固定する。`golang:1.27-alpine` のような floating tag は
# 「いつ pull したか」でリリースに入る標準ライブラリの patch が変わるため、
# 「この image は stdlib の既知脆弱性を含まない」を再現可能な形で言えない。
# go.mod の `go` directive と揃えること (govulncheck は go.mod 側を見るので、
# ここだけ古いと CI が緑のまま脆弱な binary が出る)。
#
# **base image は tag と digest を併記する。** patch version の tag でも付け
# 替えは起きる (公式 image は同じ tag を alpine の更新などで publish し直す)
# ので、tag だけだと「どの builder で作ったか」を再現できない。digest があると
# BuildKit は digest で pull する。tag は読む人向けと CI の版照合用に残す。
# 更新は dependabot (`.github/dependabot.yml` の `docker`) が digest ごと上げる。
FROM golang:1.27.1-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS builder

# Step 2 (#618) で chai2010/webp → gen2brain/webp (libwebp on wazero/WASM) に
# 切替えたので cgo 依存はゼロ。build-base (gcc + musl libc) は不要になった。
# git は go mod download 時の private module fetch に使うので残す。
RUN apk add --no-cache git

WORKDIR /app

COPY go.mod go.sum ./
# go module cache を BuildKit cache mount に乗せると、再ビルド時の
# `go mod download` が依存に変更が無ければ no-op で済む (#432)。
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# third_party/misskey (submodule) がruntime stageのCOPY対象になるので、
# ビルド前に初期化されているか確認する。CIは docker.yml 側で submodules:
# recursive を指定して取得する。ローカル build 時はユーザに指示を出す。
RUN test -f third_party/misskey/packages/backend/assets/favicon.ico || \
    (echo "ERROR: third_party/misskey submodule not initialized (or partial clone)." && \
     echo "Run: git submodule update --init --recursive" && exit 1)

# twemojiは本家frontendがUnicode絵文字描画に使うSVG set。pnpm installで
# node_modulesに hoistされる前提 (make e2e-frontend-build等で install済み)。
# upstream 2026.5.2 #17381 で `@discordapp/twemoji/dist/svg` から
# `@misskey-dev/emoji-assets/built/twemoji` に asset path が移行。
RUN test -f third_party/misskey/packages/backend/node_modules/@misskey-dev/emoji-assets/built/twemoji/1f004.svg || \
    (echo "ERROR: twemoji assets not found (pnpm install not run?)." && \
     echo "Run: make e2e-frontend-build (installs third_party/misskey node_modules)" && exit 1)

# Go の build cache (`$GOCACHE` = /root/.cache/go-build) と module cache を
# BuildKit cache mount として永続化する。再ビルド時に変更の無いパッケージは
# 再コンパイルされずに layer 完成までが秒単位になる (#432)。
#
# CGO_ENABLED=0 + `-tags nodynamic` で static binary を生成する (#619)。
# - CGO_ENABLED=0: gen2brain/webp に切替えた今、cgo に依存するコードは無い。
# - -tags nodynamic: gen2brain/webp は default で purego (dlopen) 経由の
#   shared lib fallback を試みるため、これを切って WASM (wazero) 一本に
#   固定する。これがないと dlopen を呼ぶ層が残り完全 static にならない。
#
# Video thumbnail 抽出は build tag ではなく外部 service (Misskey TS 互換の
# videoThumbnailGenerator API) への HTTP/UDS 呼び出しで実現するので、ここに
# ffmpeg バイナリを同梱する必要は無い (#637 M2)。
#
# ビルドした revision と同梱 frontend の版を埋め込む (#2700)。**Dockerfile の
# 中では git を呼べない** — `.dockerignore` が `.git` を落とすのでコンテキストに
# リポジトリが入らない。渡し忘れたときは空のまま埋まり、/about-mkgo 側が
# 「不明」として表示を省く。
#
# plugins/ に置かれたプラグインをビルドに取り込む (#2480)。生成物は gitignore
# されているので、ここで生成しないと **image だけプラグイン無しになる** (手元で
# make plugins を実行済みなら COPY で入るが、それに依存すると再現性が無い)。
# プラグインが 1 つも無ければ何も生成せず、素の go build と同じになる。
ARG MKGO_COMMIT=
ARG MKGO_FRONTEND_VERSION=
ENV REVISION_LDFLAGS="-X github.com/shiroha-a/mk/internal/config.MkGoCommit=${MKGO_COMMIT} -X github.com/shiroha-a/mk/internal/config.MkGoFrontendVersion=${MKGO_FRONTEND_VERSION}"

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOWORK=off go run ./tools/pluginbuild && \
    CGO_ENABLED=0 go build -tags nodynamic -trimpath -ldflags="-s -w $REVISION_LDFLAGS" -o /app/built/misskey ./cmd/misskey && \
    CGO_ENABLED=0 go build -tags nodynamic -trimpath -ldflags="-s -w" -o /app/built/migrate ./cmd/migrate && \
    CGO_ENABLED=0 go build -tags nodynamic -trimpath -ldflags="-s -w" -o /app/built/backfill-remote-host ./cmd/backfill-remote-host && \
    CGO_ENABLED=0 go build -tags nodynamic -trimpath -ldflags="-s -w" -o /app/built/backfill-emoji-system-file ./cmd/backfill-emoji-system-file && \
    CGO_ENABLED=0 go build -tags nodynamic -trimpath -ldflags="-s -w" -o /app/built/backfill-avatar-public-url ./cmd/backfill-avatar-public-url

# Stage 2: Runtime
#
# distroless/static-debian13 (#621) を採用。Step 3 で binary が完全 static に
# なったので、shell / pkg manager / wget / coreutils 等を持たない最小 image
# でも起動できる。ca-certificates / tzdata は distroless に同梱されている
# ので apk add は不要。
#
# 注意: distroless は shell も wget も持たないので、healthcheck は
# `/app/misskey -healthcheck` で binary 自身に叩かせる (cmd/misskey/main.go
# の -healthcheck フラグ)。docker-compose.dropin*.mk.yml /
# docker-compose.federation.misskey.yml で使用。
#
# tag を省くと `latest` になり、いつ build したかで中身が変わる。builder と
# 同じく digest で固定する (distroless の更新は dependabot が digest ごと上げる)。
FROM gcr.io/distroless/static-debian13:latest@sha256:58133991db06659feaabe0f4e97a35cebf15ef4ea08f8a4c6d2ee5f75e4aa6a0

WORKDIR /app

COPY --from=builder /app/built/misskey /app/misskey
COPY --from=builder /app/built/migrate /app/migrate
# 後始末バッチ。runtime は distroless で shell が無いので、entrypoint を差し替えた
# 使い捨てコンテナで流す (#2706)。
#   docker compose run --rm --entrypoint /app/backfill-remote-host app -dry-run
COPY --from=builder /app/built/backfill-remote-host /app/backfill-remote-host
COPY --from=builder /app/built/backfill-emoji-system-file /app/backfill-emoji-system-file
COPY --from=builder /app/built/backfill-avatar-public-url /app/backfill-avatar-public-url
COPY --from=builder /app/migration /app/migration

# 本家のpackages/backend/assets (favicon / icons等) をimageに焼き込む。
# bind-mountなしでも /favicon.ico / /static-assets/* 等が serve できる
# (issue #346)。third_party/misskey はsubmoduleなのでビルド前に
# `git submodule update --init --recursive` が必要。
COPY --from=builder /app/third_party/misskey/packages/backend/assets /app/static-assets
ENV MISSKEY_STATIC_DIR=/app/static-assets

# repo-level assets (ai.png等)。frontendが /assets/ai.png で参照する
# (mascotImageUrl のデフォルト)。submodule直下 (issue #360)。
COPY --from=builder /app/third_party/misskey/assets /app/repo-assets
ENV MISSKEY_REPO_ASSETS_DIR=/app/repo-assets

# twemoji SVG set (Unicode絵文字描画)。frontendが /twemoji/<codepoint>.svg
# で参照する。約18MB (issue #359)。upstream 2026.5.2 #17381 で
# `@misskey-dev/emoji-assets/built/twemoji` に asset path が移行した。
COPY --from=builder /app/third_party/misskey/packages/backend/node_modules/@misskey-dev/emoji-assets/built/twemoji /app/twemoji
ENV MISSKEY_TWEMOJI_DIR=/app/twemoji

# fluent-emoji PNG set。frontend が実績バッジ / notification icon を
# /fluent-emoji/<hex>.png で参照する。twemoji と同じ @misskey-dev/emoji-assets
# パッケージに含まれる (upstream 2026.5.2 #17381)。これが無いと実績バッジ等が
# 404 になる (deploy/uds/Dockerfile.mkgo では焼き込み済みだが main は欠落していた)。
COPY --from=builder /app/third_party/misskey/packages/backend/node_modules/@misskey-dev/emoji-assets/built/fluent-emoji /app/fluent-emoji
ENV MISSKEY_FLUENT_EMOJI_DIR=/app/fluent-emoji

# デフォルト設定ファイルをコピー (docker-compose でマウント上書き可能)。
# `.config/docker.yml` は gitignored で operator-local なので、image に
# 焼き込むのは `.example` 側 (placeholder 値を持つテンプレート)。
# 実際の運用では docker-compose の volume mount などで
# `/app/.config/default.yml` を上書きする想定。
COPY .config/docker.yml.example /app/.config/default.yml

EXPOSE 3000

# Misskey TS の Dockerfile が `useradd -u 991 -g 991 misskey` で UID/GID 991
# を採用しているので、drop-in 互換 (host volume `./files` の所有権が両者で
# 一致する) のため mk-go も同じ UID 991 で起動する (#621)。distroless static
# には UID 991 の /etc/passwd エントリは無いが、mk-go は os/user.Current()
# を呼ばないので numeric UID で問題なく動く。`:nonroot` tag (UID 65532) は
# drop-in 互換を壊すので使わない。
USER 991:991

ENTRYPOINT ["/app/misskey"]
CMD ["-config", ".config/default.yml"]
