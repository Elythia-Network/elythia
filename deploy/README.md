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

`url` は必須項目で、ひな形のままだと `https://example.tld/` になっている。
ローカルで試すだけなら環境変数で足りる。

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
# .config/docker.yml の url を実際のアドレスに変更する
```

そのうえで `docker-compose.yml` の **`app` と `migrate` の両方**にある
volumes のコメントを外す。**片方だけ外すとマイグレーションと本体が別の DB を
見る**ので必ず両方。

```yaml
- ./.config/docker.yml:/app/.config/default.yml:ro
```

## 更新

```bash
docker compose pull
docker compose up -d
```

`bundled` タグは `develop` の最新を指す。バージョンを固定したい場合は
`MK_IMAGE` で明示する。

```bash
MK_IMAGE=ghcr.io/shiroha-a/mk:1.1.1-bundled docker compose up -d
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
