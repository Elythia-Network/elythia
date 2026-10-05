# mk-go

Misskey互換のGoバックエンド実装 [mk-go](https://github.com/shiroha-a/mk) を、
**ビルド無しで動かすためだけ**のブランチ。

必要なのはこの3ファイルだけで、Goのソースもフロントエンドのソースも含まない。
開発する場合は `develop` ブランチを見ること。

```
docker-compose.yml           起動用の compose
.config/docker.yml.example   設定のひな形
```

## 起動

必要なもの: Docker と Docker Compose v2。

```bash
git clone --depth 1 -b docker https://github.com/shiroha-a/mk.git mk
cd mk

# ドライブの実体を置くディレクトリ。コンテナは UID/GID 991 で動く
mkdir -p files && sudo chown -R 991:991 files

docker compose up -d
```

`http://localhost:3000` を開く。初回はマイグレーションが走るので少し待つ。

`docker compose` は起動したディレクトリ名ではなく、compose ファイル内の
`name:` (= `mk-image`) を project 名に使う。同じホストで別の Misskey を
動かしていても混ざらない。

## 設定

`url` は必須項目で、compose が環境変数 `MK_URL` (既定 `http://localhost:3000/`) で
渡している。**`MK_*` は設定ファイルより優先される**ので、実際のアドレスは
設定ファイルの `url` ではなく `MK_URL` で渡す (同じディレクトリの `.env` に
`MK_URL=https://...` と書けば毎回渡さずに済む)。ローカルで試すだけなら既定のままでよい。

```bash
MK_URL=http://localhost:3000/ docker compose up -d
```

| 環境変数 | 既定値 |
|---|---|
| `MK_URL` | `http://localhost:3000/` |
| `MK_PORT` | `3000` |
| `MK_IMAGE` | `ghcr.io/shiroha-a/mk:bundled` |

本格的に運用するなら設定ファイルを使う。

```bash
cp .config/docker.yml.example .config/docker.yml
# url 以外の設定を .config/docker.yml で変える (url は上の MK_URL で渡す)
```

そのうえで、同じディレクトリに `docker-compose.override.yml` を作り、
**`app` と `migrate` の両方**に設定ファイルを mount する。**片方だけだと
マイグレーションと本体が別の DB を見る**ので必ず両方。

```yaml
services:
  app:
    volumes:
      - ./.config/docker.yml:/app/.config/default.yml:ro
  migrate:
    volumes:
      - ./.config/docker.yml:/app/.config/default.yml:ro
```

`docker compose` は `docker-compose.override.yml` を自動で重ねて読む。
**手元の変更は `docker-compose.yml` を直接書き換えずにこちらへ置く。**
下の「更新」で `docker-compose.yml` は配布元の内容で上書きされる。

## 更新

**以前の手順で `docker-compose.yml` の volumes のコメントを外していた場合は、先に
上の「設定」のとおり `docker-compose.override.yml` へ移す。** 下の `git reset --hard` が
`docker-compose.yml` を配布元の内容に戻すので、移さないと設定ファイルの mount が
黙って外れ、焼き込みのひな形の設定のまま起動する (エラーにならない)。`git diff` で
手元の変更を確かめてから進める。

```bash
# compose も image と一緒に更新する。image だけ新しくすると、compose が
# 古い呼び方のまま残る (2.0.0 で実行バイナリを elythia 1 つにまとめた、#3394)
git fetch --depth 1 origin docker
git reset --hard FETCH_HEAD
docker compose pull
docker compose up -d
```

**`git pull` は使えない。** このブランチは更新のたびに履歴を持たない 1 コミットで
作り直されるので、`git pull` は「履歴が繋がらない」として止まる。

**`git reset --hard` は、このブランチが持つファイル (`docker-compose.yml` /
`.config/docker.yml.example` / `README.md`) への手元の変更を捨てる。** 手元の変更は
`docker-compose.override.yml` に置く (上の「設定」)。`.config/docker.yml`・
`docker-compose.override.yml`・`files/` はこのブランチに無いファイルなので残る。

`bundled` タグは `develop` の最新を指す。バージョンを固定したい場合は
`MK_IMAGE` で明示する。**古い版に固定するときは、その版の compose を使う**
(このブランチの compose は最新の image の呼び方に合わせてある)。その版の
compose はリリースのタグから取れる (タグ名に `v` は付かない)。**固定している間は
上の「更新」の手順を使わない** (`git reset --hard` が compose を最新に戻し、古い image と
食い違って起動しなくなる)。版を変えるときは、その版の compose を取り直す。

```bash
curl -fsSL -o docker-compose.yml \
  https://raw.githubusercontent.com/shiroha-a/mk/1.5.0/docker-compose.image.yml
MK_IMAGE=ghcr.io/shiroha-a/mk:1.5.0-bundled docker compose up -d
```

利用できるタグは [GHCR のページ](https://github.com/shiroha-a/mk/pkgs/container/mk)
で確認できる。

## ログの上限

`docker-compose.yml` は全サービスに **50 MB x 3 世代**の上限を掛けている
(`x-logging` アンカー)。Docker 既定の `json-file` はローテーションしないので、
指定しないとログが増え続けてディスクを埋める。

```bash
docker inspect <コンテナ名> --format '{{.HostConfig.LogConfig.Config}}'
# → map[max-file:3 max-size:50m]   効いている
# → map[]                          効いていない (作り直しが要る)
```

**既存のコンテナには効かない。** ログの設定は生成時に固定されるので、
`docker compose up -d` で作り直すまで反映されない。**この設定を初めて取り込む
`up -d` は 4 サービス全部を作り直す**ので、DB も一度止まる。

## 撤去

```bash
docker compose down      # 停止 (データは残る)
docker compose down -v   # volume ごと削除 (DB と Redis のデータが消える)
```

`./files` は volume ではなくホスト側のディレクトリなので `down -v` でも残る。
消す場合は手で削除する。

## このブランチについて

**手で編集しない。** 内容は
[develop](https://github.com/shiroha-a/mk/tree/develop) 側の生成元から
GitHub Actions が自動生成しており、push のたびに履歴ごと作り直される。

| このブランチ | 生成元 (develop) |
|---|---|
| `docker-compose.yml` | `docker-compose.image.yml` |
| `.config/docker.yml.example` | `.config/docker.yml.example` |
| `README.md` | `deploy/README.md` |

変更したい場合は develop 側に PR を出すこと。
