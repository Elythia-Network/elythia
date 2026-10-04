#!/bin/bash
set -euo pipefail

echo "=== Go dependencies ==="
cd /workspace
go mod download

# **Node / pnpm の版は frontend/ を唯一の定義にする (#2921、#3379)。** image には
# bootstrap 用の Node しか入っていない。CI (`pnpm/action-setup` + `.node-version`)
# と `make e2e-frontend-build` も同じ 2 ファイルを見るので、3 者が揃う。
#
# **拾えなかったら落とす。** 空のまま進むと不正なパッケージ名になるか、
# 黙って別の版で開発することになる。
echo "=== Node / pnpm (frontend/ の宣言に揃える) ==="
node_ver=$(tr -d '[:space:]' < frontend/.node-version)
pnpm_ver=$(sed -n 's/.*"packageManager"[[:space:]]*:[[:space:]]*"pnpm@\([^"]*\)".*/\1/p' \
    frontend/package.json)
if [ -z "$node_ver" ] || [ -z "$pnpm_ver" ]; then
    echo "frontend/ から Node / pnpm の版を読めない" >&2
    exit 1
fi
# **major だけ合わせても駄目。** nodesource は 26 系の最新を入れるので
# 26.4.0 を宣言していても 26.8.1 が入る (実測)。CI は `actions/setup-node` +
# `node-version-file` で厳密に一致させ、`make e2e-frontend-build` は
# `node:<version>-<distro>` タグを引くので、ここも公式 tarball で厳密に合わせる。
if [ "$(node -v)" != "v$node_ver" ]; then
    echo "node $(node -v) -> v$node_ver"
    arch=$(dpkg --print-architecture)
    case "$arch" in
        amd64) narch=x64 ;;
        arm64) narch=arm64 ;;
        *) echo "未対応のアーキテクチャ: $arch" >&2; exit 1 ;;
    esac
    # **既存の npm を先に消す。** `--strip-components=1` で /usr/local へ重ねると、
    # そこに Node がある配置 (公式 node image 等) では古い npm のファイルが残って
    # 混ざり、`Class extends value undefined` で npm が壊れる (実測)。
    # devcontainer は nodesource が /usr へ入れるので通常は衝突しないが、
    # base image が変わっても壊れないようにしておく。
    sudo rm -rf /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/corepack
    curl -fsSL "https://nodejs.org/dist/v$node_ver/node-v$node_ver-linux-$narch.tar.xz" \
        | sudo tar -xJ -C /usr/local --strip-components=1 \
            --exclude CHANGELOG.md --exclude LICENSE --exclude README.md
    hash -r
fi
sudo npm i -g "pnpm@$pnpm_ver"
echo "node $(node -v) / pnpm $(pnpm --version)"

echo "=== Wait for PostgreSQL ==="
for i in $(seq 1 30); do
    pg_isready -h localhost -p 5432 -U misskey && break
    echo "Waiting for PostgreSQL... ($i/30)"
    sleep 1
done

echo "=== Config file ==="
# cmd/migrate も mk-go 本体も -config (既定 .config/default.yml) を必ず読む。
# .config/* は gitignore なので clone 直後は存在せず、無いと failed to load config
# で落ちる。DB / Redis の向き先は compose の MK_DB_* / MK_REDIS_* が上書きする。
# url は example の https://example.tld/ のまま残る。開発中に絶対 URL を
# 永続化する経路 (drive file / emoji の publicUrl 等) を触るなら手で書き換える。
# **MK_URL で渡さないこと** — MK_* は viper で設定ファイルより優先されるので、
# internal/config のテストが fixture ではなくその値を読んで落ちる。
if [ ! -f .config/default.yml ]; then
    cp .config/default.yml.example .config/default.yml
    echo "created .config/default.yml"
fi

echo "=== Database migration ==="
make migrate-up || echo "Migration failed (may already be applied)"

echo "=== Frontend build ==="
# pluginbuild の生成物は追跡していない (#3379) ので、frontend より先に書き出す。
make plugins
cd /workspace/frontend
pnpm install --frozen-lockfile
pnpm build

echo "=== Done! Run 'make dev' to start the server ==="
