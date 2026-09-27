# プラグイン — 運営者向け

## 最初に読むこと

**プラグインはサーバーと同じ権限で動く。**

`plugin/` が提供する範囲は「mk-go が壊さないと約束する API」であって、プラグインができることの上限ではない。プラグインは mk-go と同一プロセスで動く Go コードなので、

- `os` でファイルを読める（設定ファイルの DB 認証情報を含む）
- `net/http` でどこへでも送れる
- `os/exec` で何でも実行できる

フロントエンド側も同じで、バンドルに入る JS は**全利用者のブラウザでそのセッションのまま**動く。

**信頼できる作者のプラグインだけを組み込むこと。** サンドボックスは無い。

これはビルド時拡張を採る Go のエコシステム（Caddy、Terraform provider など）に共通する性質で、mk-go も同じ立場を採っている。安全性はサンドボックスではなく、**運営者が特定のプラグインの特定バージョンを名指しで含める**という選択の明示性で担保する。

## 導入

```bash
# 1. plugins/ にプラグインを置く
git clone https://example.com/mk-plugin-foo plugins/foo

# 2. ビルド
make build          # または docker build / make uds-build

# 3. 再起動
```

`plugins/` を走査して自動的に取り込む。フラグやマニフェストの編集は不要。

各プラグインは**独立した Go module**（`go.mod` を持つ）である必要がある。持たない場合はビルド時にエラーになる。

### Docker の場合

`Dockerfile` / `Dockerfile.bundled` / `deploy/uds/Dockerfile.mkgo` のいずれも生成ツールを実行するので、`plugins/` に置いた状態でイメージをビルドすれば取り込まれる。

`Dockerfile.bundled` は SPA を同梱するが、その供給元は既定で mk-go 公式のアセットイメージ（`ghcr.io/shiroha-a/misskey-ts-assets`）なので、**プラグインのフロントエンドは入らない**。含めるには SPA を自前でビルドしたうえで `--build-arg ASSETS_SOURCE=local` を渡す。下記の GitHub Actions 経由ならこの判定は自動で行われる。

### フロントエンドを持つプラグイン

`third_party/misskey` (submodule) が取得済みである必要がある。未取得のまま frontend 付きプラグインを置くと、生成の時点でエラーになる。

**フロントエンドのビルド後は mk-go の再起動が必須。** mk-go は起動時に一度だけ manifest を読むので、ビルドしただけでは古いファイルを指したままになる（存在しないファイルを指すと画面が真っ白になる）。

## GitHub Actions でビルドする

**運用は 2GB のVPSに載るが、ビルドは載らない。** 実測では定常運用が 343 MiB（mk-go 91 / PostgreSQL 181 / valkey 69 / nginx 2）なのに対し、Go のビルドは 1200MB 制限・既定の並列度で OOM する（`-p 1` なら通る）。フロントエンドのビルドはさらに重い。

加えて、**本番ホストで `docker build` するとメモリが戻らない**。Docker 23 以降の BuildKit は dockerd に組み込まれているため、ビルドで伸びたヒープが dockerd に残る。実測ではビルドキャッシュを 60.88GB 削除しても RSS は 4,465 → 4,485MB でほぼ変化しなかった（原因はキャッシュではなくビルドの実行そのもので、解放には dockerd の再起動が要る）。

ビルドを GitHub Actions に任せれば、どちらも起きない。自分のリポジトリに次の workflow を1つ置くだけでよい。

```yaml
name: Build mk-go

on:
  workflow_dispatch:

jobs:
  build:
    permissions:
      contents: read
      packages: write
    # reusable workflow もタグかコミット SHA で固定する (@main にしない。下記)
    uses: shiroha-a/mk/.github/workflows/build-with-plugins.yml@<タグ または コミット SHA>
    with:
      mk_ref: <同じタグ または コミット SHA>
      plugins: |
        weather    https://github.com/foo/mk-plugin-weather  v1.2.0
        nowplaying https://github.com/bar/mk-plugin-np       0123456789abcdef0123456789abcdef01234567
    secrets:
      plugin_token: ${{ secrets.PLUGIN_TOKEN }}   # private なプラグインを使う場合のみ
```

出来上がったイメージは**自分の GHCR** に入るので、本番は `docker pull` するだけになる。

- **`permissions` は呼び出す側で宣言する。** publish するなら `packages: write` が要る。reusable workflow 側では宣言していない — あちらで書くと呼び出し元の権限以下にしか設定できず、権限を持たない呼び出し（fork からの PR など）は `push: false` でも run ごと拒否されるため
- **`mk_ref` にこの機能を含む版を指す。** リリース `1.3.0` には `tools/pluginresolve` が無いので、指定するとビルドが「no required module provides package」で落ちる。`1.4.0` 以降のタグか、その先のコミット SHA を指すこと
- **ref は必須だが、それだけでは内容は固定されない。** タグ・ブランチ・コミット SHA のいずれも書けるので、`main` と書けば実質的に既定ブランチを追うことになる。省略を許さないのは「どの版を取るかを毎回書かせる」ためで、**内容まで固定したいならコミット SHA か、動かさない運用のタグを指すこと**。ブランチを指した場合、この文書の冒頭にある「特定のバージョンを名指しで含める」という前提は成立しない（作者のアカウントが侵害されれば、次のビルドで任意のコードが入る）
- **フロントエンドを持つプラグインは自動で判定される。** 1つでもあれば SPA を自前でビルドして同梱し、無ければ公式のアセットイメージを使ってフロントエンドのビルドを丸ごと省く。判定は `mk-plugin.yml` で無効化されているものを除いた実際の組み込み対象に対して行われる
- **指定したプラグインが組み込まれなかったらビルドが落ちる。** `mk-plugin.yml` が `disabled: true` のプラグインは生成ツールが黙って読み飛ばすため、突き合わせないと「指定したのに1つも入っていないイメージ」が成功扱いで publish される
- **private なプラグインは `plugin_token` を渡す。** `https://github.com/` の URL 書き換えで差し込むので、プラグインの指定行に token を書く必要はない（書いた場合はビルドが弾く。指定行は秘密として扱われないので、ログに平文で残るため）。書き換えはプラグインを clone する step の中だけで有効で、抜けるとき (失敗時も) に消すので、後続の `pnpm install` や go のビルドからは token を読めない。**token に持たせる権限はプラグインのリポジトリの読み取りだけにする**
- `push: false` を渡すとビルドだけ行い、publish しない。`image` は `ghcr.io/...` のみ受け付ける（ログイン先が ghcr.io に固定されているため）

`mk_repository` を渡せば mk-go 自体を fork したものにも向けられる。

**`uses:` の `@` の後ろも固定する。** `@main` のようにブランチで参照すると、mk-go の `main` に入った変更がそのまま次回のビルドで**呼び出し側の権限のまま**動く。reusable workflow は呼び出し元が渡した `packages: write` (自分の GHCR へ publish できる) と `plugin_token` (private なプラグインを読める) を受け取るので、`main` が壊れたり侵害されたりすれば、それらがそのまま晒される。`mk_ref` を固定しても workflow 自体は別に解決されるので防げない。タグか、より確実にはコミット SHA を指し、上げるときは差分を読んでから上げること。なお、この workflow はビルド結果のキャッシュを**呼び出し元の** Actions キャッシュに書き出す。プラグインのソースを含むので、公開リポジトリで fork からの PR を許している場合は取り扱いに注意すること。

## 設定

`.config/default.yml` の `plugins:` セクションに、プラグイン名をキーとして書く。

```yaml
plugins:
  status:
    enabled: true
    maxLength: 30
  foo:
    apiKey: "..."
```

`enabled` と `peerMaxBody` が mk-go の予約キーで、残りはプラグインにそのまま渡る。

### `peerMaxBody`

peer の受け口 (`/api/plugin/<名前>/_peer`) が受け付ける本文の上限をバイトで指定する。
省略するとプラグインが宣言した値、宣言も無ければ **64 KiB**（AP inbox と同じ）。
下限は 1 KiB（それより小さい値を書いても 1 KiB になる）。

**プラグインが単独で宣言できるのも 64 KiB まで**で、それを超える宣言はここで許可した
ときだけ有効になる（許可が無ければ 64 KiB に丸めて起動時に warn を出す）。プラグインを
1 つ足しただけでインスタンスの露出が広がる形にしないため。上限は 1 MiB（`/api` 全体の
body limit）で、それより大きい値を書いても効かない。

```yaml
plugins:
  bigdata:
    peerMaxBody: 524288   # 512 KiB を許可する
```

小さくする方向にも使える。プラグインが 64 KiB を宣言していても、ここに小さい値を書けば
そちらが優先される。詳細は[peer プロトコル](../plugin-peer-protocol.md)。

**環境変数によるオーバーライドは無い。** Viper は既知のキーにしか環境変数を適用できず、プラグインのキーは事前に列挙できないため。設定ファイルに書けない値は、プラグイン自身が `os.Getenv` で読む実装になっている必要がある。

### 秘密情報

**`-config-dump` では `enabled` 以外の値をマスクする。** どのキーが秘密かを mk-go は判別できないので、既定で全部隠す。

```
  plugin: status          有効
    status.maxlength      <設定済み (マスク)>   (プラグインの設定は既定でマスクする)
```

キー名と「設定されているか」は残るので、診断には使える。

## 止める

止め方は2つあり、速さと深さが違う。

### 実行時に止める（設定ファイル）

```yaml
plugins:
  foo:
    enabled: false
```

**再ビルドは不要。** 再起動するだけで登録がスキップされる。ビルドには含まれたままだが、ルートもジョブも生えない。

問題のあるプラグインを止めるのに再ビルドと再デプロイを要求すると障害対応に間に合わないため、この経路を用意してある。

### ビルドから外す（`mk-plugin.yml`）

プラグインの`mk-plugin.yml`に書く。

```yaml
disabled: true
```

次のビルドから除外され、**バイナリにもフロントエンドのバンドルにも入らない**。ディレクトリを消すのと同じ効果で、ディレクトリ自体は残せる。

判定は`apiVersion`等の検証より**先**に行われるので、mk-goの更新でビルドが通らなくなったプラグインを一時的に外す用途にも使える。

### 同梱プラグインの既定

同梱しているのは`plugins/status/`と`plugins/trustlevel/`の2つ。

**どちらもこの仕組みで既定無効**。動かしたい場合は該当する`mk-plugin.yml`から`disabled: true`の行を消して再ビルドする。

既定無効なのは、同梱プラグインが**ビルドに含まれているだけで有効になる**ため。`plugin_wiring.go`はRoutes/Jobsの登録より先に専用schemaを開いてmigrationを適用するので、設定していなくても`plugin_<name>` schemaとテーブルができる。schemaを開けない環境では起動そのものが失敗する。cloneしただけで全運営者のバイナリ・フロント・DBに入る状態にしない。

**この既定は`build` jobの`Check bundled plugins are disabled by default`が見ている**（#2701）。検証のために一時的に外して戻し忘れるのを止めるため。手元で動かすだけなら`make plugin-dev PLUGIN=plugins/<name>`を使うと`mk-plugin.yml`を触らずに済む（ビルド生成物である`server-plugins.generated.ts`はsubmodule側でtrackedなので書き換わる）。

## 入っているものを確認する

**コントロールパネル → プラグイン → サーバープラグイン**（`/admin/server-plugins`、モデレーター以上）に一覧が出る。バージョン・有効/無効・機能（API/ジョブ/フロントエンド）・schema・migration数・設定キー（値はマスク。キー名は`-config-dump`と同じく小文字で表示される）・宣言ページへのリンク、および後述の残存データがここで確認できる。

起動ログにも出る。

```
INFO plugin loaded name=status version=1.0.0 routes=true jobs=true migrations=1 schema=plugin_status
INFO plugin disabled name=foo version=0.2.0
```

`-config-dump` にも設定と有効・無効が出る。

## 消したあとのデータ

**プラグインを消しても、そのデータは自動では消えない。**

各プラグインは `plugin_<name>` という PostgreSQL schema を持つ。プラグインを `plugins/` から消して再ビルドすると、生成物（登録コード等）は片付くが、**schema とデータは残る**。

削除した行は復元できないので、一時的に外しただけの運用（更新・切り分け・障害対応）でデータが飛ぶ方が損害が大きいと判断している。

残っている場合はコントロールパネルのサーバープラグイン一覧に警告が出るほか、起動時にも知らせる。

```
WARN 使われていないプラグインのデータが残っています schema=plugin_foo
     対処="不要なら DROP SCHEMA \"plugin_foo\" CASCADE で削除してください (自動では消しません)"
```

不要と判断したら手動で消す。

## プラグインが触れる範囲（実装上の制約）

「約束の範囲」ではなく、mk-go 側が**構造として設けている**制約。

| | |
|---|---|
| DB | `plugin_<name>` schema のみ。`search_path` が固定されており、素直に書けば本体のテーブルに届かない |
| 本体のデータ | mk-go の REST API 経由。可視性・権限・レート制限が自動的に効く |
| ActivityPub | **公開していない**。連合に流れるものをプラグインが変えられると、不具合の症状が他人のサーバー側に出る |
| 独自ページ | `/plugin/<name>/...` の名前空間のみ。本体のパスは取れない |
| 管理画面 | `/admin/plugin/<name>/...`。表示はモデレーター以上に限られる |

ただし DB の制約は `search_path` によるもので、**権限による強制ではない**。プラグインが `public.note` のように明示的に修飾すれば到達できる。前述のとおりプラグインは設定ファイルも読めるので、ここを権限で塞いでも意味は薄い。

重要なのは「素直に書いたら本体のテーブルに触れない」ことで、これは満たしている。ノートの可視性判定はアプリケーション側にあり DB には無いため、`SELECT * FROM note` が通ってしまうと非公開ノートが混ざる。

## TS へ切り戻した場合

プラグインの機能は使えなくなる（mk-go固有の仕組みのため）。`plugin_<name>` schemaは未知のものとしてDBに残り、TypeORMは無視する。

ただし、`EffectivePolicies`を宣言するプラグインがある場合、切り戻しはデータを壊さなくても**利用者の実効権限を変える**。providerの寄与はnative roleやDBへ永続化されず、プラグイン停止・除外・Misskey TSへの切り戻しと同時に消える。特に濫用対策など制限方向の寄与が消えると、切り戻した瞬間に権限が緩む。

切り戻す前に`admin/server-plugins`の`effectivePolicies`を確認し、各providerが消えた後のnative policyだけで安全な状態になるか検証すること。停止後も維持すべき権限はproviderではなくnative roleとして永続化する。
