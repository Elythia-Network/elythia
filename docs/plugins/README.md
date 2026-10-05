# プラグイン

運営者が自分のインスタンスに独自機能を追加する仕組み (#2476)。バックエンドとフロントエンドの両方に手を入れられる。

## 読む順

| 読み手 | 文書 |
|---|---|
| プラグインを**入れる** | [運営者向け](operating.md) — 導入・無効化・**権限の話** |
| プラグインを**書く** | [作者向け](authoring.md) — 手順とリファレンス |
| プラグインを**開発する** | [開発環境](development.md) — `make plugin-dev` と HMR |
| Elythia 本体を**変える** | [互換性ポリシー](compatibility.md) — 公開面を広げてよい条件 |
| peer の **wire を変える** | [peer プロトコル](../plugin-peer-protocol.md) — エンベロープ / 署名 / 上限 / 適用範囲 |

プラグインができるのは、

- 独自の API エンドポイント (`/api/plugin/<name>/...`)
- 自分の PostgreSQL schema と定期ジョブ
- プロフィール / 設定画面への描画
- **独自ページ** (`/plugin/<name>/...`) とナビゲーション項目
- **管理画面** (`/admin/plugin/<name>/...`)
- **同じプラグインを入れた Elythia 同士の通信** (ActivityPub には出ない)

動く実例は [`plugins/status/`](../../plugins/status/)。外部サービスに依存しないので、組み込んで動かしながら読める。ただし**既定では無効**（`mk-plugin.yml`の`disabled: true`）。読みながら動かすだけなら`make plugin-dev PLUGIN=plugins/status`を使うと`mk-plugin.yml`を触らずに済む。その行を消して再ビルドしてもよいが、消したままpushすると`build` jobの`Check bundled plugins are disabled by default`が落ちる。

## 仕組みの要点

**ビルド時に組み込む。** 実行時に読み込むのではなく、Elythia 本体と一緒にコンパイルする。

```
plugins/<name>/ に置く → make build → 再起動
```

理由は、Elythia が `CGO_ENABLED=0` の完全静的バイナリで distroless 上で動くため。動的ライブラリの読み込みも外部インタプリタの実行もできない。

副次的に、**内部の変更でプラグインが壊れたときビルドが落ちて即座に分かる**。実行時読み込みだと本番で静かに壊れる。

代償は、導入・更新のたびに再ビルドと再デプロイが要ること（再ビルドは数秒〜数分）。ただし**無効化は再ビルド不要**で、設定を変えて再起動すれば止まる。ビルドから完全に外したい場合は`mk-plugin.yml`に`disabled: true`を書く（[運営者向け](operating.md)参照）。

## 触れる範囲について

**2 つの意味があり、混同すると危険。**

| | 内容 |
|---|---|
| **A. 約束の範囲** | `plugin/` と `plugin-api.ts` が提供するもの。Elythia 側が壊さないと約束する |
| **B. 技術的に可能な範囲** | **制限は無い**。プラグインは同一プロセスの Go コードで、`os` も `net/http` も使える |

A だけを読むと「プラグインはこの範囲しか触れない」と受け取れるが、**そうではない**。詳細は[運営者向け](operating.md)を参照。
