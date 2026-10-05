# プラグイン — 開発環境

プラグインを編集しながら動かす手順 (#2477)。

## backend だけを触る場合

```bash
make plugin-dev PLUGIN=plugins/status
```

プラグインのソースを監視し、変わったら**生成 → ビルド → 再起動**する。

```
==> 変更を検出: plugin.go
plugin loaded name=status version=1.0.0 routes=true jobs=true migrations=1 schema=plugin_status
```

- `PLUGIN=` を省くと `plugins/` 全体を監視する
- `-config` で設定ファイルを変えられる（既定は `.config/default.yml`）。**ただし `make plugin-dev` は追加引数を転送しない**ので、渡すなら `go run ./tools/plugindev` を直接叩く（後述）
- 監視対象は**指定したディレクトリだけ**。mk-go 本体を触っている間に再起動し続けることはない
- `MK_DEV=1` が自動で立つので、ビルド済みのフロントが残っていても Vite dev server を見に行く

**開発用の設定ファイルを別に用意することを勧める。** 本番と同じ設定を使うと、本番の DB に接続してしまう。

```bash
cp .config/default.yml .config/dev.yml   # url / port / db を書き換える

# make plugin-dev は追加引数を転送しないので、-config を渡すなら直接叩く
GOWORK=off go run ./tools/plugindev -plugin plugins/status -config .config/dev.yml
```

## frontend も触る場合

別の端末で Vite dev server を立てる。

```bash
cd frontend/packages/frontend && pnpm watch
```

`make plugin-dev` 側が `MK_DEV=1` を立てているので、mk-go は `/vite/*` をここへ流す。プラグインの `.vue` / `.ts` を編集すると HMR が効く。

プラグインのソースは `packages/frontend` の外にあるが、`mk-plugins.generated.json` の `allow` に `plugins/` が入るので dev server が配信できる（生成は `make plugins` か `make plugin-dev` が行う）。同じく生成物の `server-plugins.generated.ts` は git で追跡していないので、dev server を立てる前に一度 `make plugins` か `make plugin-dev` を実行しておく。

### node_modules の所有者に注意

`make uds-frontend-build` / `make e2e-frontend-build` は **Docker の中で root として** `pnpm install` するため、`frontend/node_modules` と `frontend/packages/*/node_modules` が root 所有になる。この状態でローカルの `pnpm watch` を起動すると失敗する。

```
Error: EACCES: permission denied, open '.../node_modules/.vite-temp/vite.config.ts.timestamp-....mjs'
```

どちらかで回避する。

```bash
# A. 所有者を自分に移す (以後 Docker ビルドを使わないなら)
sudo chown -R "$(id -un):$(id -gn)" frontend/node_modules frontend/packages/*/node_modules

# B. dev server も Docker で動かす
docker run --rm -it -v "$(pwd)":/work -w /work/frontend/packages/frontend \
  -p 5173:5173 "node:$(tr -d '[:space:]' < frontend/.node-version)-trixie" npx vite --host
```

## 確認できること

| | 見え方 |
|---|---|
| プラグインが読み込まれたか | 起動ログの `plugin loaded`（名前・版・ルート/ジョブの有無・migration 数・schema） |
| 無効化されているか | `plugin disabled` |
| 消したプラグインのデータ | `使われていないプラグインのデータが残っています` |
| dev モードか | 起動時の警告と `elythia config-dump` の「frontend 配信元」 |

## よくある詰まり

**プラグインを置いたのに読み込まれない**

`mk-plugin.yml` と `go.mod` の両方が要る。片方だけだと検出されない（`go.mod` が無い場合は明示的なエラーになる）。

**`make plugin-dev` では動くのに `make build` に入らない**

`mk-plugin.yml` に `disabled: true` が残っていないかを見る。`PLUGIN=` で名指しした監視対象は disabled でも含めて動かす（既定無効の同梱サンプルを tracked ファイルの編集なしで開発するため）が、本番ビルドは含めない。

**既定無効のプラグインを開発したいのに読み込まれない**

`PLUGIN=` で名指しする（`make plugin-dev PLUGIN=plugins/status`）。disabled を含めるのは名指しした 1 つだけで、`PLUGIN=` 無しの全体監視は本番ビルドと同じく disabled を含めない。壊れた状態で `disabled: true` にして退避してあるプラグインを、無関係な開発が巻き込んで止まらないようにするため。

**フロントを再ビルドしたのに変わらない**

mk-go は起動時に一度だけ manifest を読む。**ビルド後は必ず再起動する。** ブラウザ側の Service Worker も掴んでいることがあるのでハードリロードする。

**プラグインを消したらビルドが落ちる**

生成物が残っている。`make plugins` を実行すれば片付く（`GOWORK=off` が付いているので、壊れた `go.work` があっても動く）。

**`toolchain not available` でビルドが止まる**

`go.work` の Go の版が手元のものと違う。`make plugins` で作り直す（本体の `go.mod` から写す）。

## テスト

```bash
make plugin-test   # 同梱プラグイン全部。MK_PLUGIN_TESTS_REQUIRE_DB=1 GOWORK=off で回す
```

PostgreSQL が要る（`TEST_DB_*` 環境変数、既定は `localhost:5432` の `misskey_test`）。

`plugin/plugintest` の使い方は[作者向け](authoring.md#テスト)を参照。

### ドキュメントのスニペット

```bash
make plugin-doc-check
```

`authoring.md` の Go スニペットを使い捨ての module に展開してビルドする。断片は `lookup(...)` のようなプラグイン側のヘルパを意図的に省くので、**非修飾の `undefined: X` だけ**を正常として捨て、それ以外は種類を問わず落とす (`undefined: plugin.Blob` のような修飾付きは API の改名・削除なので落とす)。

断片の置かれる文脈は 3 通り (top-level 宣言 / `(any, error)` を返すハンドラ / `error` を返す routes・jobs) あるので全部に展開し、どれか 1 つでも通れば良しとする。対象外にした fence は理由つきで出る。

**推論では判別できない。** #2639 では「コンパイルできない例を直す」作業が 4 周にわたって別のエラーを作り続けた (`Call` の第 1 引数 → `no new variables` → `declared and not used`)。CI の `plugin-tests` job でも回している。
