# status — Elythia プラグインのサンプル

利用者が短い「ひとこと」を設定し、プロフィールに表示するプラグイン。期限を付けると自動で消える。

**外部サービスに依存しない**ので、そのまま組み込んで動かしながら読める。

```
設定 → プロフィール          プロフィール
┌────────────────────┐      ┌──────────────────────────┐
│ ▸ ステータス  作業中 │      │ 💬 作業中     あと 3 時間 │
└────────────────────┘      └──────────────────────────┘
```

## 動かす

```bash
# Elythia のリポジトリルートで
make build     # plugins/ を走査してビルドに取り込む
```

`plugins/` に置いても**既定では無効**。`mk-plugin.yml` に `disabled: true` があり `make build` の対象から外れる (同梱サンプルなので、clone しただけの人のビルドに勝手に入らないようにしてある)。有効にするには `disabled: true` を消す。

**UI を出すにはフロントの再ビルドも要る。** `make build` が呼ぶ `pluginbuild` は
取り込み先の TypeScript を生成するだけで、vite ビルドまでは走らない。手順は環境
ごとに違うので [デプロイ](../../docs/deployment.md) を見ること。

**手元で動かすだけなら `make plugin-dev PLUGIN=plugins/status`。** tracked ファイル (`mk-plugin.yml`) を編集せずに済む。`disabled: true` を消して push すると、同梱サンプルが既定無効であることを見る CI (`build` job の `Check bundled plugins are disabled by default`) が落ちる。

```yaml
# .config/default.yml — 変えたいときだけ
plugins:
  status:
    enabled: true
    maxLength: 30
```

## 何を例示しているか

公開面をひととおり使う。**どれも「例のために足した」ものではなく、この機能に必要だから登場する。**

| 使うもの | この機能での必然性 |
|---|---|
| `Definition.Migrations` | ステータスを保存するテーブル |
| `Routes` | 保存・取得・他人の分の表示 |
| `Jobs` | **期限切れの掃除**。期限を持つなら必ず要る |
| `Storage` | 自分の schema への読み書き |
| `Config` | `maxLength` を運営者が決められるようにする |
| `API` | **凍結された利用者の分を出さない**。判断を Elythia に任せる |
| `StatusError` | 文字数超過を利用者に伝える（利用者が直せるエラー） |
| スロット 2 種 | 設定画面とプロフィール |
| `pages` (宣言) | 一覧ページ (ランチパッドに出る) と管理画面 (コントロールパネルに出る) |
| `API` のまとめ引き | 表示名を `users/show` に **userIds でまとめて**問い合わせる (1 件ずつだとレート制限に掛かる) |
| `Request.IsModerator` | **管理用 API を自分で守る**。画面を隠すだけでは守れない |
| `plugintest` | ルートとジョブのテスト |

**使っていないものもある** — `plugin.Blob`（バイナリ応答）、peer 系（`Definition.Peered` / `Context.Peer()` / `Peer` interface / `PeerHandler` / `PeerReplyHandler`）、`API.AsUser`、`Request.IsAdministrator`、`NewCodedStatusError` / `ExtractStatusError`。**これらは status では壊れても検出できない**ので、[compatibility.md](../../docs/plugins/compatibility.md) の drift gate と `TestPluginDoc_*` に頼る。使い方は [authoring.md](../../docs/plugins/authoring.md) を参照。

## 読みどころ

コード中のコメントに、**踏みやすい罠**を残してある。

- **文字数は rune で数える** — `len()` だと日本語が 3 倍に数えられ、「30文字まで」と言いながら 10 文字で弾く
- **読み取り側でも期限を見る** — 掃除ジョブは 1 時間ごとなので、これが無いと最大 1 時間ずれる
- **設定が壊れていたら起動を止める** — 黙って既定値で動かない
- **migration は `Definition` で宣言する** — `Routes` の中で呼ぶと、ロールを分割したとき queue 側でテーブルが作られない

## 構成

```
plugin.go          backend（ルート・ジョブ・設定）
plugin_test.go     plugintest によるテスト
frontend/
  index.ts         スロットへの登録
  api.ts           バックエンド呼び出し
  StatusCard.vue      プロフィール表示
  StatusSettings.vue  設定画面
  RecentPage.vue      一覧ページ (/plugin/status)
  AdminStats.vue      管理画面 (/admin/plugin/status)
```

## テスト

```bash
make plugin-test   # リポジトリのルートで。GOWORK=off なので go.work に居なくても回る
```

PostgreSQL が要る（`TEST_DB_*` 環境変数、既定は `localhost:5432` の `misskey_test`）。

**DB はフェイクにしない。** SQL の挙動を模した偽物は本物とずれ、通ったのに本番で落ちるテストになる。
