# プラグイン — 互換性ポリシー

Elythia 本体を変更する人向け。**公開面を広げてよい条件**と、壊してよい範囲を定める。

## 公開面とは

| | 場所 |
|---|---|
| Go | `plugin/` と `plugin/peercache/` と `plugin/plugintest/` |
| TypeScript | `frontend/packages/frontend/src/plugin-api.ts` |
| HTTP | `/api/plugin/<name>/` の名前空間 |
| ページ | `/plugin/<name>/` と `/admin/plugin/<name>/` の名前空間 |
| ナビ | `navbarItemDef` の `plugin:<name>` キー |
| DB | `plugin_<name>` schema と、そこへ渡す `*sql.DB` |
| 設定 | `.config/default.yml` の `plugins:` セクション |

`internal/` の中身は公開面ではない。**プラグインは Go の internal ルールにより import できない**ので、自由に変えてよい。

## 変更の分類

### 追加（マイナー）

新しい型・メソッド・スロットの追加。既存のプラグインは影響を受けない。

**`APIVersion` は上げない。**

`plugin.Definition` はフィールド名を指定したkeyed struct literalだけを互換対象とする。外部プラグインのpositional / unkeyed literalはサポートしない。既存フィールドの意味や型を変えないoptional fieldはv1のまま追加できるため、プラグイン作者は必ず`Name: ...`のようにフィールド名を書くこと。

`Definition.EffectivePolicies`と関連型の追加はこのadditive契約に従い、`Validate`もRoutes、Jobs、EffectivePoliciesのいずれかを要求する形へ緩和するだけなので、`APIVersion`は1のまま維持する。

`EffectivePolicyRequest.ActiveAssignments` と `plugin.ActiveRoleAssignment` の追加も同じ扱い。`EffectivePolicyRequest` はhostが組み立ててproviderへ渡す入力であり、**`RoleIDs` は変更されない**。そのため`RoleIDs`だけを読む既存providerの挙動は変わらず、新しいsliceを無視する実装にも影響はない。`APIVersion`は1のまま。

`EffectivePolicyContribution.ReplaceRoleID`の追加も同じ扱い。**keyed struct literalで組み立てる限り**、**未設定なら追加contributionのまま**なので既存providerの挙動は変わらず、pluginは「対象ロールのネイティブcontributionを置き換える」という新しい契約にだけオプトインする。置換の`Priority`/`Order`制約はprovider作者の誤りを弾くもので、既存providerの出力形式は変えない。`APIVersion`は1のまま。

`EffectivePolicyRequest`をpluginのtestやヘルパーで組み立てる場合は`plugin.Definition`と同じく**keyed struct literalだけ**を互換対象とする（`RoleIDs: ...`のようにフィールド名を書く）。外部プラグインのpositional / unkeyed literalはサポートしない。

`EffectivePolicyContribution`をpluginのコードやtestで組み立てる場合も`plugin.Definition`と同じく**keyed struct literalだけ**を互換対象とする（`Key: ...`のようにフィールド名を書く）。**exported fieldを増やした型はpositional / unkeyed literalがソースで壊れる** — `ActiveAssignments`の追加で`EffectivePolicyRequest`が、`ReplaceRoleID`の追加で`EffectivePolicyContribution`がこれに当たるので、実行時の挙動がadditiveのままであってもコンパイルは通らない。

`Definition.Peer`（#2819）と`Context.Queue()`、`plugin.Queue` / `EnqueueOption`の追加も同じ扱い。`Definition.Peer`は既存プラグインが`Routes`の中でpeerを登録していても壊さない（`RoleBoth`ならそのまま動く）が、**ロールを分割した構成では応答が届かない**ので、移すこと。登録が無いロールでは起動時にwarnが出る。

`Context`はElythiaが実装してプラグインは受け取るだけなので、メソッドが増えてもプラグインは壊れない（プラグイン側が`Context`を自前で実装している場合はこの限りではないが、それはサポート対象外）。

### キューの実装との関係

**`plugin.Queue`はmkqを再公開しない。** 本体のworkerはペイロードを`{type, body}`で包んで`type`でdispatchするので、素のmkqハンドルから積んだジョブは処理者が見つからない。包み方は本体のenqueueとworkerの間の契約であって、プラグインに晒す面ではない（`internal/queue/driver/mkqdriver`）。

したがって**`plugin.APIVersion`はmkqのメジャー版に連動しない**。mkqを差し替えても`plugin.Queue`の形が変わらなければ、プラグインは再ビルドだけで動く。逆に`EnqueueOptions`のフィールドの意味を変えるときは、mkqが同じままでも破壊的変更になる。

### 破壊的変更（メジャー）

既存のシグネチャ変更、削除、意味の変更。

**`plugin.APIVersion` を上げる。** 合わないプラグインは `elythia-plugin.yml` の `apiVersion` 検査でビルド時に落ちる。黙って動かない状態にはならない。

破壊的変更を入れるときは、

1. 非推奨期間を置く（可能なら新旧を並立させる）
2. `APIVersion` を上げる
3. 同梱プラグイン (`plugins/status/` / `plugins/trustlevel/`) を追従させる（サンプルが壊れたまま残らないように）

### Go のモジュールパスの変更 (#3394)

2.0 で、本体の Go のモジュールパスが `github.com/shiroha-a/mk` から `github.com/elythia-network/elythia` に変わった。公開パッケージの import パスも `github.com/elythia-network/elythia/plugin` (`plugintest` / `peercache` / `imagedecode` を含む) に変わる。中身と `plugin.APIVersion` は変えていない。

古いパスのままのプラグインは、`make plugins` が次のように止める (無効化したプラグインは止めない)。

```
pluginbuild: plugins/foo: go.mod が以前のモジュールパス github.com/shiroha-a/mk を参照しています。…
```

プラグインのディレクトリで次を流す。`go.mod` の `require` と `replace` を付け替え、`.go` の import を書き換える。

```bash
go mod edit \
  -droprequire=github.com/shiroha-a/mk -dropreplace=github.com/shiroha-a/mk \
  -require=github.com/elythia-network/elythia@v0.0.0 \
  -replace=github.com/elythia-network/elythia=../..
grep -rlZ --include='*.go' '"github.com/shiroha-a/mk/' . \
  | xargs -0 -r sed -i 's#"github.com/shiroha-a/mk/#"github.com/elythia-network/elythia/#g'
gofmt -w .
```

- 書き換えた後のプラグインは、2.0 より前の本体ではビルドできない。本体と同じ版の組み合わせで上げる
- プラグイン自身のモジュール名 (`module` 行) は変えなくても動く。同梱プラグインは `github.com/elythia-network/elythia-plugin-<名前>` にそろえた

### マニフェストの改名 (#3400)

2.0 で、プラグインのマニフェストの名前が `mk-plugin.yml` から `elythia-plugin.yml` に変わった。中身の書式はそのままで、名前を変えるだけでよい。

```bash
git mv mk-plugin.yml elythia-plugin.yml   # git で管理していなければ mv
```

- 旧名は読まない。**旧名のマニフェストだけがあるディレクトリは、`make plugins` が止める** (`disabled: true` を書いていても止める)。黙って飛ばすと、プラグインが組み込まれていない image が出来上がるため
- 新しい名前があれば、旧名のファイルが残っていても新しい方を読む
- nodeinfo の宣言も `metadata.mkGoPlugins` から `metadata.elythiaPlugins` に変わった。`Peered` を宣言しているプラグインは、相手が 2.0 より前の版のあいだ、相手を対応サーバーと見なさない ([Peer のプロトコル](../plugin-peer-protocol.md#相手が持っているかの判定))
- マニフェストの名前を変えたプラグインも、2.0 より前の本体では検出されない。本体と同じ版の組み合わせで上げる

### 上流追従による破壊

`plugin-api.ts` が再公開している Misskey のコンポーネント（`MkInput` 等）は、upstream が props を変えると壊れる。

これは**受け入れている**。見た目の完全一致と引き換えのコストで、どのプラグイン機構でも追従は必要という判断。CI の `frontend`（`vue-tsc`）と手元の `make frontend-check` で検出できる。

## 公開面を広げてよい条件

**実際のプラグインが要求したときだけ。**

想像で足さない。#2484 で実際にプラグインを 2 本書いたところ、想定していなかった不足が 7 件見つかり、逆に**想定して作った機能のうち使われなかったもの**もあった。

追加するときは以下を満たすこと。

- [ ] 具体的なプラグインの具体的な用途がある（「あると便利そう」では足さない）
- [ ] 内部の型を露出していない（`echo.Context` / `gorm.DB` / `model.User` などを渡さない）
- [ ] 代替手段が無い（既存の組み合わせで書けないか確認した）
- [ ] 同梱プラグインか新しいサンプルで実際に使われる、または doc に用例がある

### 露出してはいけないもの

| | 理由 |
|---|---|
| `echo.Context` | Echo は内部の選択。差し替えたときにプラグインが全滅する |
| `*gorm.DB` | 同上。標準の `*sql.DB` に留める |
| `model.*` | DB モデルが契約になり、migration が打てなくなる |
| ActivityPub 関連 | 不具合の症状が他人のサーバー側に出る。**後から塞げない**。プラグイン同士の通信が要る場合は [`Peer`](authoring.md#他のインスタンスとやりとりする) を使う (Elythia 同士に閉じた経路、#2537) |
| repository / service | 可視性判定などのアプリケーション側のガードを迂回できる |
| ルーターの定義そのもの | プラグインが本体のパスを奪える。名前空間を切った登録だけを許す |

## drift gate

公開面は golden で固定してある。

```
internal/entitycompat/testdata/golden_plugin_surface.txt
```

export が増減すると `TestPluginSurfaceDrift` が落ちる。意図した変更なら再生成する。

```bash
go run ./tools/pluginspec -write
```

**golden の差分は必ずレビューで意図を確認すること。** これが「うっかり公開面が広がる」ことを防ぐ唯一の仕組み。

対象は `plugin/` と `plugin/peercache/` と `plugin/plugintest/` の 3 つ。テスト用だからと外すと、そこだけ黙って育つ。

## サンプルプラグイン

`plugins/status/` と `plugins/trustlevel/` を**リポジトリに同梱**してある。

別リポジトリに置くと、`plugin/` を変えたときに壊れても CI で気付けない。同梱していれば公開面を壊した時点で CI が落ちる。**サンプルの一番の価値は「常に動くこと」**。

ただし**素の `make build` では落ちない** — 既定無効なので取り込まれないため。落とすのは次の 3 つ。

| job | 見るもの | required |
|---|---|---|
| `build` の `Vet bundled plugins` | 各プラグインを `go vet` (テストファイルも含めてコンパイル) | ○ |
| `build` の `Build integrated binary with sample plugins` | `make plugins-all` (`-include-disabled`) → 統合バイナリのビルド。毎回走る | ○ |
| `plugin-tests` | 各プラグインのテストを実行 (`replace` で本体の公開面に対してコンパイルされる) | × |
| `frontend` (`frontend.yml`) | `make plugins-all` (`-include-disabled`) → frontend のビルド・`vue-tsc`・vitest。`plugins/` か `plugin/` を触った PR では必ず走る | ○ |

required なのは `build` と `frontend` (`docs/ci.md` の required check は `build` / `test` / `lint` / `frontend` の 4 つ)。`plugin-tests` だけが落ちる壊れ方はマージをブロックしない。

`plugins/*` は gitignore されているが、`!plugins/status/` と `!plugins/trustlevel/` (#2586) で例外指定してある。**どちらも `elythia-plugin.yml` で既定無効**なので、clone して `make build` してもバイナリにもフロントにも入らない。`status` は #2495 から。`trustlevel` は #2586 で `disabled: true` 付きで同梱したあと、#2585 の実測を採るために一度外し、実測が終わって #2701 で戻している。既定無効であることは `build` job の `Check bundled plugins are disabled by default` が見る。

## 変更時のチェック

- [ ] `go run ./tools/pluginspec -write` で golden を更新した（差分をレビューで説明できる）
- [ ] 破壊的変更なら `plugin.APIVersion` を上げた
- [ ] 同梱プラグインが通る（`make plugin-test`。recipe が `MK_PLUGIN_TESTS_REQUIRE_DB=1 GOWORK=off` で回す。**この変数が無いと DB 不通で skip = 成功扱い**になるので、素の `go test` で代用しない）
- [ ] `plugin-api.ts` を変えたなら `make frontend-check` が通る
- [ ] `docs/plugins/authoring.md` の公開面一覧を更新した (`TestPluginDoc_*` が CI で検査する)
- [ ] `make plugin-doc-check` が通る (スニペットのコンパイル。`plugin-tests` job でも回る)

## 参考

- 設計の経緯と却下した案: #2476
- 実プラグインで確定させた経緯: #2484
