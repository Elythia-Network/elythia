FROM mcr.microsoft.com/devcontainers/go:1.27

# /go の所有権を vscode ユーザーに変更 (go mod download の権限エラー回避)
RUN chown -R vscode:vscode /go

# PostgreSQL client (pg_isready 等のユーティリティ)
RUN apt-get update && apt-get install -y postgresql-client && rm -rf /var/lib/apt/lists/*

# Node.js (フロントエンドビルド用)。
#
# **ここで入れるのは bootstrap 用。** 実際に使う版は postCreate.sh が frontend/ の
# `.node-version` / `packageManager` を読んで入れ直す (#2921)。image のビルド時点
# ではリポジトリが mount されていないので、ここでは読めない。
#
# **corepack は使わない。** Node.js 26 の配布物に含まれていないので、
# `.node-version` に追従した瞬間に `command not found` になる。pnpm は
# postCreate.sh が `npm i -g pnpm@<packageManager>` で入れる。
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y nodejs

# golang-migrate CLI
RUN su vscode -c "go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest"
