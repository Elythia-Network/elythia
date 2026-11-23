# CherryPickからの移行

CherryPick 4.18.0-beta.1のDBを、そのままElythiaで引き継ぐ手順です(#4246)。ほかの版のCherryPickには対応していません。先に4.18.0-beta.1へ上げてください。

CherryPickのDBは、Elythiaの`migrate up`をそのまま通って起動します。ただし、CherryPickにしか無い制限は、エラーにならずに外れます。たとえば、承認待ちの利用者がサインインできるようになります。`elythia cherrypick`のサブコマンドで、外れるものを数えて、Elythiaの仕組みへ置き換えてから起動します。

Misskeyからの移行は[migration-from-ts.md](migration-from-ts.md)を見てください。設定の書き方やfrontendの用意は同じです。移行した後にCherryPickへ戻すことは保証しません。後始末(`cleanup`)をした後は、確実に戻せません。

## 流れ

1. CherryPickを止めて、DBのバックアップを取る
2. `elythia cherrypick check`で、外れるものを数える
3. `elythia migrate -allow-cherrypick`で、DBの形をElythiaにする
4. `elythia cherrypick convert`で、外れるものを置き換える(既定は何をするかを出すだけで、`-apply`で書き込む)
5. `elythia migrate`を流してから、Elythiaを起動する(`-allow-cherrypick`は付けない)
6. (任意)Elythiaを止めて、`elythia cherrypick cleanup`で、置き換えたCherryPickの列と表を消す

設定ファイルは、CherryPickの`url`とDB、Redisをそのまま書きます。`url`を変えると、ローカルの利用者のActivityPubのidが変わり、連合が切れます。

```bash
# 2. 数える。CherryPickのDBでなければ何も出さずに0で終わる。要対応があれば3で終わる
elythia cherrypick check -config .config/default.yml

# 3. DBの形をElythiaにする。要対応が残っている間は、フラグが無いと止まる
elythia migrate -config .config/default.yml -allow-cherrypick

# 4. 何をするかを見てから、選んで書き込む
elythia cherrypick convert -config .config/default.yml
elythia cherrypick convert -config .config/default.yml -apply \
  -quarantined block -later-deletes keep \
  -resolvers-out /path/to/resolvers.json -accept-manual-reports
```

Dockerで動かしているときは、同梱の`docker-compose.yml`の`migrate`サービス(入口が`elythia`で、コマンドだけを差し替えられる)で流します。`app`サービスは`migrate`の成功を待つので、`convert`の前には起動できません。

- **`migrate`サービスにも、`app`と同じ設定ファイルをmountしてください**(`docker-compose.yml`の`migrate`の`volumes`のコメントを外す)。mountしないと、imageに入っている例の設定で動きます
- **先にDBとRedisを起動してください。** `migrate`サービスはDBの起動しか待ちません。`convert`は、投稿やアカウントを消すときに本体を組み立てるので、Redisにも繋ぎます
- `docker-compose.image.yml`を使うときは、各コマンドに`-f docker-compose.image.yml`を付けてください。この`migrate`サービスは、DBとRedisの接続先と`url`を環境変数(`MK_DB_HOST`・`MK_REDIS_HOST`・`MK_URL`)で上書きします。環境変数は設定ファイルより優先されるので、CherryPickの値と合っているかを確かめてください

**`-resolvers-out`は、ホストのディレクトリをmountした先に書いてください。** コンテナの中に書くと、`--rm`でコンテナと一緒に消え、規則の写しが残りません。コンテナはUID 991で動くので、そのUIDが書けるディレクトリにします。

```bash
mkdir -p cherrypick-out && sudo chown 991:991 cherrypick-out
docker compose up -d --wait db redis
docker compose run --rm migrate cherrypick check -config .config/default.yml
docker compose run --rm migrate migrate -config .config/default.yml -allow-cherrypick
docker compose run --rm -v "$PWD/cherrypick-out:/out" migrate cherrypick convert -config .config/default.yml -apply \
  -quarantined block -later-deletes keep -resolvers-out /out/resolvers.json -accept-manual-reports
docker compose up -d
```

## 起動が止まる仕組み

`migrate up`は、CherryPickのDBで要対応が残っている間は止まります。起動の前に`migrate up`を流す構成(同梱のcomposeの`migrate`サービス、UDSのentrypoint、手順どおりに`migrate`してから起動する場合)では、これで本体の起動が止まります。`elythia serve`は`migrate`を流さないので、バイナリから直接起動するときは、手順の順番を守ってください。

- Elythiaがまだ流していないDBで止まったときは、`check`で中身を見て、`migrate -allow-cherrypick`で一度だけ流します。**`-allow-cherrypick`を起動のコマンドに入れたままにしないでください。** 付けている間は、`convert`の前でも通ります
- 流した後は、`convert`が済むまで止まり続けます。`convert`の前に本体が起動して、承認待ちの利用者などが通ってしまわないようにするためです。止まったときのログは`convert`を案内します
- `convert`が済むと、`migrations`の表に印の行(`ElythiaCherryPickConverted`)を足し、以後の`migrate up`は数えません。Elythiaが流した後で要対応が無ければ、`migrate up`がその場で印を付けます
- 対応していない版のCherryPickのDBは、`-allow-cherrypick`を付けても止まります。一度流すと版を見分けられなくなるためです

**2.3.0以前の版で、すでにCherryPickのDBにElythiaを流していた環境は、この確認が入った版へ上げると、要対応が残っている間は起動の前の`migrate up`が止まります。** `convert`を流せば通ります。

## 外れるものと、`convert`の選択

| CherryPickの機能 | Elythiaで起きること | `convert`がすること |
|---|---|---|
| 登録の承認待ち(承認制が有効で、申請の理由がある利用者) | サインインできる | `-pending-users`で選ぶ。`suspend`(既定)は凍結し、後で管理画面から解除(承認)か削除(否認)を選ぶ。`delete`はアカウントを削除する。`approve`は承認する |
| 通報の自動処理の規則 | 適用されない | 名前と正規表現を`-resolvers-out`へJSONで書き出し、`-accept-manual-reports`で同意してから行を消す。既にあるファイルは上書きしない |
| インスタンスの隔離 | 制限が外れる | `-quarantined block`でブロック(`meta.blockedHosts`)へ足すか、`lift`で外す |
| ローカルの投稿の予約削除 | 時刻が来ても消えない | 期限を過ぎたものは消す。まだのものは`-later-deletes delete`で今消すか、`keep`で残す(以後は消えない) |
| `meta.repositoryUrl`・`meta.feedbackUrl` | CherryPickを指したまま | CherryPickの既定値のときだけ、Elythiaのものに直す |

`convert`の実行前の一覧には、承認待ちの利用者の申請の理由も出します。Elythiaは申請の理由を読まないので、凍結した後の判断の材料にしてください。

### 承認待ちに数えない利用者

CherryPickは`approved`の列を既定値`false`で足し、それまでの利用者の行を埋めません。承認制が無効なら、サインインしたときに`true`へ書き換えます。そのため、次の利用者は`approved=false`でも承認待ちに数えず、`check`の件数を出すだけにします。

- 承認制を有効にする前からの利用者と、管理者が作ったアカウント(申請の理由が無い)。CherryPickはパスワードでのサインインを断りますが、今のセッションはそのまま使わせています。Elythiaではサインインできます
- システムアカウント(`instance.actor`など)
- 承認制が無効の間にメールの確認待ちになり、有効にした後で確認した申請者は、理由を持たないので、ここに入ります

### 削除するときの注意

- アカウントと投稿の削除は、管理画面からの削除と同じ処理を通します。連合への配送はジョブとして積まれ、Elythiaを起動した後に送られます
- 承認待ちを削除すると、管理画面からの削除と同じく、知っているリモートのサーバー全体へDelete(actor)が積まれます
- 後片付けのジョブを積めなかった利用者は、IDの一覧が出ます。Elythiaを起動した後に、管理画面からもう一度削除してください。流し直しても、削除の印が立った利用者は数えません
- 投稿の削除の配送を積み終わる前に止まったときは、流し直しても積み直されません(投稿の行は消えているため)
- `convert`は、削除のために本体を組み立てます(待ち受けはしません)。有効なプラグインの処理も動き出します

### 読まれなくなるもの

次のデータはDBに残りますが、Elythiaは読みません。`convert`は触りません。`check`は、初めの5つの件数を出します。

- 投稿の編集の履歴(`note_history`)。投稿は編集後の本文で出ます
- イベント(`event`)。投稿は普通の投稿として出ます
- 利用者ごとの古い投稿の自動削除の設定(`user.autoDeleteNotesAfterDays`)。以後は消えません
- 下書きに付いた削除の予約(`note_draft.deleteAt`)。投稿された後に消えません
- リモートの投稿の予約削除。元のサーバーがDeleteを送れば消えます
- 配送先の指定(`note.deliveryTargets`)、`meta.bubbleInstances`、翻訳の設定など、CherryPickだけの設定
- リモートの利用者の`user.canChat`(相手のサーバーが知らせた値の控え)

## 後始末(任意)

`convert`が済んだ後に、置き換えたCherryPickの列と表を消せます。消さなくてもElythiaは動きます。

```bash
elythia cherrypick cleanup -config .config/default.yml                        # 消すものを出す
elythia cherrypick cleanup -config .config/default.yml -apply -backup-taken   # 消す
```

- **Elythiaを止めてから流してください。** `user`と`note`の表に列を消す変更をかけるので、動いている本体の読み書きとロックを取り合います(ロックは30秒まで待って諦めます)

```bash
# Dockerでは
docker compose stop app
docker compose run --rm migrate cherrypick cleanup -config .config/default.yml -apply -backup-taken
docker compose up -d
```
- 消すのは`user.approved`、`instance.quarantineLimited`、`abuse_report_resolver`の表(とその型)、`note.deleteAt`だけです。編集の履歴やイベントなどの表は消しません
- ただし、消した列の値も消えます。リモートの投稿の予約削除の時刻(`note.deleteAt`)と、凍結した承認待ちの見分け(`user.approved`。申請の理由の`signupReason`は残る)です。凍結した承認待ちを後で承認するか削除するかを決めるなら、`convert`の出力を控えておいてください
- 消したものは戻せません。先にバックアップを取ってください(`-backup-taken`はその確認です)
- `convert`の印が無いとき、要対応が残っているときは止まります

## 確かめた範囲

- 確かめたのは、CherryPick 4.18.0-beta.1のimageで新しく作ったDBです(`tests/cherrypick`、`make cherrypick-test`)。長く運用したDBでは試していません
- `make cherrypick-test`は、移行した後にCherryPickのトークンとパスワードでのサインイン、投稿が使えること、置き換えが効いていることを確かめます。フォローとリアクションがそのまま引き継がれることは、手元の検証で確かめました
- 本家のMisskeyは、CherryPickのDBでは`dropGroup1676434944993`のmigrationが落ちて起動しません。CherryPickからMisskeyへの移行の代わりにはなりません
