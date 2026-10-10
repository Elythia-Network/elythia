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
FROM golang:1.27.2-alpine@sha256:85dc1069ac644ea3c527b177303a406eb3358192816cd7f9e5848eb658851673 AS builder

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

# frontend/ (#3379 で本体へ取り込んだ) の静的アセットが runtime stage の COPY 対象に
# なるので、build context に入っているか先に確かめる。.dockerignore を広げすぎた
# ときに、COPY の not found より分かりやすい形で落とす。
RUN test -f frontend/assets/favicon.ico || \
    (echo "ERROR: frontend/assets is missing (incomplete checkout or .dockerignore)." && exit 1)

# twemojiは本家frontendがUnicode絵文字描画に使うSVG set。pnpm installで
# node_modulesに hoistされる前提 (make e2e-frontend-build等で install済み)。
# upstream 2026.5.2 #17381 で `@discordapp/twemoji/dist/svg` から
# `@misskey-dev/emoji-assets/built/twemoji` に asset path が移行。
RUN test -f frontend/node_modules/@misskey-dev/emoji-assets/built/twemoji/1f004.svg || \
    (echo "ERROR: twemoji assets not found (pnpm install not run?)." && \
     echo "Run: make e2e-frontend-build (installs frontend/ node_modules)" && exit 1)

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
# ビルドした revision を埋め込む (#2700)。**Dockerfile の
# 中では git を呼べない** — `.dockerignore` が `.git` を落とすのでコンテキストに
# リポジトリが入らない。渡し忘れたときは空のまま埋まり、/about-elythia 側が
# 「不明」として表示を省く。
#
# plugins/ に置かれたプラグインをビルドに取り込む (#2480)。生成物は gitignore
# されているので、ここで生成しないと **image だけプラグイン無しになる** (手元で
# make plugins を実行済みなら COPY で入るが、それに依存すると再現性が無い)。
# プラグインが 1 つも無ければ何も生成せず、素の go build と同じになる。
ARG MKGO_COMMIT=
ENV REVISION_LDFLAGS="-X github.com/elythia-network/elythia/internal/config.MkGoCommit=${MKGO_COMMIT}"

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOWORK=off go run ./tools/pluginbuild && \
    CGO_ENABLED=0 go build -tags nodynamic -trimpath -ldflags="-s -w $REVISION_LDFLAGS" -o /out/bin/elythia ./cmd/elythia && \
    ln -s elythia /out/bin/migrate

# Stage: backup (#3458)
#
# DB のバックアップを取る image。`docker build --target backup` で作る。本体の image
# (下の runtime) は distroless で pg_dump を持たないので、postgres の公式 image に
# elythia を足す。**pg_dump の版を DB のサーバーと揃えるため**で、DB と同じ
# postgres:18-alpine を使う (古い pg_dump は新しいサーバーから取れない)。
#
# builder とは別の stage でビルドする。builder は frontend/ の node_modules が無いと
# 落ちるが、バックアップの image に frontend は要らない。命令は builder と同じ並びに
# してあるので、BuildKit は go mod download までの layer を共有する。
FROM golang:1.27.2-alpine@sha256:85dc1069ac644ea3c527b177303a406eb3358192816cd7f9e5848eb658851673 AS backup-builder
RUN apk add --no-cache git
WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
ARG MKGO_COMMIT=
ENV REVISION_LDFLAGS="-X github.com/elythia-network/elythia/internal/config.MkGoCommit=${MKGO_COMMIT}"
# プラグインも本体と同じく組み込む。設定ファイルの plugins: の節を本体と同じ
# 意味で読むため (pluginembed-check も全ての builder に求めている)。
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOWORK=off go run ./tools/pluginbuild && \
    CGO_ENABLED=0 go build -tags nodynamic -trimpath -ldflags="-s -w $REVISION_LDFLAGS" -o /out/bin/elythia ./cmd/elythia

# digest で固定する (理由は builder と同じ)。DB のサーバーの版を上げたら、ここも
# 同じメジャーバージョンへ上げる。
FROM postgres:18-alpine@sha256:4da1a4828be12604092fa55311276f08f9224a74a62dcb4708bd7439e2a03911 AS backup
# pg_bigm を入れておく。UDS の構成の DB (deploy/postgres-bigm) は pg_bigm 入りで、
# CREATE EXTENSION した DB の dump を戻すには、戻す側にも拡張が要る。verify (#3459) は
# この image の中で使い捨ての PostgreSQL に戻すので、無いと CREATE EXTENSION pg_bigm で
# 落ちる。入れるだけなら、使わない DB の dump にも戻す処理にも影響しない。
# **版は deploy/postgres-bigm/Dockerfile と揃える** (同じ commit SHA)。
ARG PG_BIGM_COMMIT=735dceba0ecdd8ac1aaaaa207226a7102b6bbd71
RUN apk add --no-cache --virtual .build-deps build-base curl icu-dev \
 && curl -fsSL "https://github.com/pgbigm/pg_bigm/archive/${PG_BIGM_COMMIT}.tar.gz" -o /tmp/pg_bigm.tar.gz \
 && tar xzf /tmp/pg_bigm.tar.gz -C /tmp \
 && make -C "/tmp/pg_bigm-${PG_BIGM_COMMIT}" USE_PGXS=1 with_llvm=no install \
 && rm -rf /tmp/pg_bigm* \
 && apk del .build-deps
COPY --from=backup-builder /out/bin/elythia /usr/local/bin/elythia
# verify (#3459) と restore (#3461) が、バイナリの同梱の migration の番号と比べる。
COPY --from=backup-builder /app/migration /app/migration
WORKDIR /app
# postgres の公式 image の entrypoint (initdb を走らせる docker-entrypoint.sh) は
# 使わない。サーバーを立てる image ではないため。root では動かさず、image に
# 元からある postgres (UID 70) で動かす。
USER postgres
ENTRYPOINT ["/usr/local/bin/elythia", "backup"]
CMD ["list", "-config", ".config/default.yml"]

# Stage 2: Runtime
#
# distroless/static-debian13 (#621) を採用。Step 3 で binary が完全 static に
# なったので、shell / pkg manager / wget / coreutils 等を持たない最小 image
# でも起動できる。ca-certificates / tzdata は distroless に同梱されている
# ので apk add は不要。
#
# 注意: distroless は shell も wget も持たないので、healthcheck は
# `/app/elythia healthcheck` で binary 自身に叩かせる (internal/cli/diag)。
# tests/dropin*/compose.mk.yml / tests/federation/compose.misskey.yml で使用。
#
# tag を省くと `latest` になり、いつ build したかで中身が変わる。builder と
# 同じく digest で固定する (distroless の更新は dependabot が digest ごと上げる)。
FROM gcr.io/distroless/static-debian13:latest@sha256:58133991db06659feaabe0f4e97a35cebf15ef4ea08f8a4c6d2ee5f75e4aa6a0

WORKDIR /app

# 実行バイナリは elythia 1 つ (#3394)。サーバー・migration・後始末バッチを
# サブコマンドで呼び分ける。runtime は distroless で shell が無いので、バッチは
# command を差し替えた使い捨てコンテナで流す (#2706)。
#   docker compose run --rm --no-deps app backfill remote-host -dry-run
# `/app/migrate` は `elythia` への symlink。古い compose の migrate サービス
# (`entrypoint: ["/app/migrate"]`) のまま新しい image を pull した運営者の migration を
# 止めないための、D11 の期限付きの例外 (2.x の間だけ。3.0 で撤去)。elythia は
# この名前で起動されると、以前の flag のまま migrate として動く (internal/cli)。
# **ディレクトリごと COPY する。** 単独のファイルとして COPY すると symlink が辿られ、
# バイナリの実体がもう 1 つ image に入る (実測)。
COPY --from=builder /out/bin/ /app/
# `docker exec <container> elythia backfill <名前>` のように、パス無しで呼べるように
# する (設計 R7)。distroless は ENV で既存の PATH を参照できる shell を持たないので、
# 既定の PATH を書き下して先頭に /app を足す。
ENV PATH=/app:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
COPY --from=builder /app/migration /app/migration

# 本家のpackages/backend/assets (favicon / icons等) をimageに焼き込む。
# bind-mountなしでも /favicon.ico / /static-assets/* 等が serve できる
# (issue #346)。本家では packages/backend/assets にあり、本体では frontend/assets
# に置いている (#3379、設計 D3)。
COPY --from=builder /app/frontend/assets /app/static-assets
ENV MISSKEY_STATIC_DIR=/app/static-assets

# repo-level assets (ai.png等)。frontendが /assets/ai.png で参照する
# (mascotImageUrl のデフォルト)。本家の直下の assets/ を frontend/repo-assets に
# 置いている (issue #360、#3379)。
COPY --from=builder /app/frontend/repo-assets /app/repo-assets
ENV MISSKEY_REPO_ASSETS_DIR=/app/repo-assets

# twemoji SVG set (Unicode絵文字描画)。frontendが /twemoji/<codepoint>.svg
# で参照する。約18MB (issue #359)。upstream 2026.5.2 #17381 で
# `@misskey-dev/emoji-assets/built/twemoji` に asset path が移行した。
COPY --from=builder /app/frontend/node_modules/@misskey-dev/emoji-assets/built/twemoji /app/twemoji
ENV MISSKEY_TWEMOJI_DIR=/app/twemoji

# fluent-emoji PNG set。frontend が実績バッジ / notification icon を
# /fluent-emoji/<hex>.png で参照する。twemoji と同じ @misskey-dev/emoji-assets
# パッケージに含まれる (upstream 2026.5.2 #17381)。これが無いと実績バッジ等が
# 404 になる (deploy/uds/Dockerfile.mkgo では焼き込み済みだが main は欠落していた)。
COPY --from=builder /app/frontend/node_modules/@misskey-dev/emoji-assets/built/fluent-emoji /app/fluent-emoji
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

# ENTRYPOINT はバイナリだけにして、サブコマンドは CMD に置く。compose の
# command を差し替えるだけで migrate や backfill を同じ image で流せる。
ENTRYPOINT ["/app/elythia"]
CMD ["serve", "-config", ".config/default.yml"]
