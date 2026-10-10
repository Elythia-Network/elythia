# バックアップの定期実行と世代の管理、失敗の通知 (#3460)

`elythia backup daemon`は、設定した間隔でバックアップを取り(#3458)、確かめ(#3459)、古い世代を消す常駐のプロセスです。失敗したとき、検証で食い違ったとき、間隔を過ぎても新しい世代ができないときに、Webhookで運営者へ知らせます。本体の管理画面(#3462)が「今すぐ取る」「今すぐ確かめる」「状態」を頼むための制御APIも、このプロセスが持ちます。

本体のimageには`pg_dump`が無いので、このプロセスはバックアップ用のimage(`postgres:18-alpine` + `elythia`)の中で動かします。本体の動作は変えません。

## 設定

設定ファイルの`backup:`の節に書きます。保存先(`backup.storage`)と暗号化(`backup.encryption`)は、取る処理(#3458)と同じものを使います。

```yaml
backup:
  schedule:
    interval: 24h        # 間隔 (Go の duration)。空なら定期実行しない
    at: "04:00"          # 取る時刻 (HH:MM、プロセスのタイムゾーン)。省略可
    keep: 7              # 残す世代の数。0 なら消さない
    verify: true         # 取った後に毎回確かめる
    delayAfter: 36h      # 遅れを知らせるまでの時間。省略時は interval の 1.5 倍
    listen: ":3010"      # 制御 API の待ち受け。空なら制御 API を持たない
  notify:
    webhookUrl: "https://discord.com/api/webhooks/..."
    format: discord      # generic / discord / slack。省略時は generic
  server:
    serviceToken: "<長いランダムな文字列>"   # 制御 API の認証。本体も同じ値を読む
```

| キー | 意味 |
|---|---|
| `schedule.interval` | 間隔。1分より短い値は受け付けない(単位の書き忘れで取り続けないため) |
| `schedule.at` | 取る時刻。`interval`が24時間の倍数なら、暦の日数で進むので夏時間の切り替えでも同じ時刻に取る。24時間より短い間隔なら、`at`を起点に`interval`ごとに取る |
| `schedule.keep` | 残す「使える世代」の数(下の「世代の整理」) |
| `schedule.verify` | `true`なら、取った後に毎回`backup verify`と同じ検証を流す。**`true`を推奨する。** `false`のときは、検証していない世代も「使える世代」に数える |
| `schedule.delayAfter` | 最後の使える世代から、この時間を過ぎても新しい世代ができなければ遅れを知らせる。`interval`より短い値は受け付けない |
| `schedule.listen` | 制御APIの待ち受けのアドレス。設定するなら`server.serviceToken`も必須(空なら起動しない) |
| `notify.webhookUrl` | 通知先。空なら通知せず、ログにだけ残す |
| `notify.format` | 送る形。下の「通知」 |
| `server.serviceToken` | 制御APIのtoken。本体(#3462)とdaemonで同じ値を使う |

`interval`も`listen`も空なら、何もすることが無いので起動しません。`interval`を空にして`listen`だけを設定すると、定期実行はせず、管理画面から頼まれたときだけ取る・確かめる形になります。

### 時刻とタイムゾーン

`at`は、プロセスのタイムゾーン(環境変数`TZ`)で解釈します。`postgres:18-alpine`は`TZ`が空でUTCになるので、日本時間で決めたいときはサービスに`TZ: Asia/Tokyo`を渡します(このimageには`/usr/share/zoneinfo`が入っている)。起動時のログ`backup: schedule started`に、解釈したタイムゾーン(`timezone=`)と次に取る時刻(`next=`)が出るので、確かめてください。

### 起動したときに取るか

起動したとき、最新の揃った世代(`meta.json`がある世代)が`interval`より古いか、世代が1つも無ければ、すぐに1回取ります。daemonが止まっていた間の分を、次の枠まで待たずに取り戻すためです。最新の世代が`interval`より新しければ、次の枠まで待ちます。そのため、再起動のたびに取り直すことはありません。

取っている途中に次の枠が来たときは、重ねて取らず、終わるのを待って1回だけ取ります。

## 1回の流れ

1. 取る(#3458)。失敗したら通知して終わる
2. `verify: true`なら確かめる(#3459)。検証が最後まで走れなかったら「失敗」、走って食い違ったら「食い違い」として通知し、**世代の整理はしない**
3. 世代を整理する

1つのプロセスの中では、取る・確かめる・消すを同時に1つしか走らせません。確かめている世代を消したり、書いている途中の世代を消したりしないためです。**同じ保存先に対して、daemonを2つ動かさないでください。**

## 世代の整理

**古い世代は、使える新しい世代が`keep`個そろってから消します。** 壊れたバックアップしか残らない状態を作らないためです。

- 「使える世代」は、`verify: true`なら`verify.json`が`ok: true`の世代。`verify: false`なら、`meta.json`があり、検証に落ちていない世代(`verify.json`が無いか`ok: true`)
- 新しい方から数えて`keep`個目の使える世代より古い世代は、使えるかどうかに関わらず全て消す
- それより新しい世代は、検証に落ちた世代も、`meta.json`の無い半端な世代も残す。落ちた理由を調べられるようにするためと、半端な世代は書いている途中かもしれないため。`keep`個の窓の外に出たら消える
- 使える世代が`keep`個に満たない間は、何も消さない。検証に落ち続けると世代が増え続けるので、通知を見たら原因を直す
- `keep: 0`なら何も消さない
- 1つの世代を消すときは、`meta.json`を最初に消す。途中で失敗しても、その世代は「半端な世代」に見え、ダンプの欠けた「揃った世代」には見えない

`verify.json`は、確かめる処理(#3459)が保存先へ書いたものを読みます。

## 通知

| 種類 | いつ | `event` |
|---|---|---|
| 失敗 | 取る・確かめる・消すのどれかが誤りを返した | `failure` |
| 食い違い | 検証が走り、戻せない・行数が違う・Elythiaとして読めない | `mismatch` |
| 遅れ | 最後の使える世代(無ければdaemonの起動時刻)から`delayAfter`を過ぎた | `delay` |

遅れは、取る処理が戻ってこない(固まった)ときにも知らせます。遅れが続く間は、`delayAfter`ごとに繰り返し知らせます。起動した時点で既に遅れていれば、起動してすぐに知らせます。**daemonそのものが止まっていると、遅れも知らせられません。** daemonの死活は、コンテナの再起動の設定(`restart: unless-stopped`など)と、制御APIの`GET /status`で見てください。

### 送る形

`format: generic`は、次のJSONを送ります(`text`は人が読む1行の文面)。

```json
{
  "event": "mismatch",
  "instance": "https://example.tld",
  "occurredAt": "2026-10-10T04:12:00Z",
  "generationId": "20261009T190000Z",
  "message": "the backup did not pass verification",
  "failedStages": [{"stage": "restorable", "ok": false, "error": "..."}],
  "mismatches": [{"table": "public.note", "expected": 10, "actual": 9}],
  "text": "[Elythia backup] https://example.tld: verification found a broken backup ..."
}
```

`stage`(失敗した段: `take` / `verify` / `prune`)と`lastUsableAt`(遅れのときの最後の使える世代)は、値があるときだけ入ります。

`format: discord`は`{"content": "<text>"}`を、`format: slack`は`{"text": "<text>"}`を送ります(DiscordのWebhook、SlackのIncoming Webhook)。Discordでは、誤りの文面に含まれる`@everyone`などで一斉通知が起きないよう、mentionを全て無効にして送ります。長い文面は、各サービスの上限(Discordは2000文字、Slackは3000文字で切る)に収まるよう切ります。

送信に失敗したら2回まで送り直します。リダイレクトはたどりません。送れなかったことはログと`GET /status`の`lastNotifyError`に残ります。

### 通知先への通信

通知は、本体の外向きの通信と同じ`internal/safehttp`の経路で送ります。`proxy` / `proxyBypassHosts` / `outgoingAddress` / `outgoingAddressFamily`が効き、**プライベートなアドレスへは送りません**。同じLANの通知先(自前のntfyなど)へ送るときは、本体と同じく`allowedPrivateNetworks`にそのアドレスを書いて許します。

## 制御API

本体の管理画面(#3462)が使う口です。`schedule.listen`で待ち受けます。TLSは持たないので、composeの内部のネットワークなど、本体からだけ届く場所で待ち受け、portをホストへ公開しないでください。

全てのリクエストに`Authorization: Bearer <server.serviceToken>`が要ります。tokenは定数時間で比べます。一致しなければ、パスに関わらず`401 {"error":"unauthorized"}`を返します。

| メソッドとパス | 本文 | 応答 |
|---|---|---|
| `POST /take` | 無し | `202 {"job": Job}`。走っている作業があれば`409 {"error":"busy","running": Job}` |
| `POST /verify` | `{"id": "<世代のID>"}` | `202 {"job": Job}`。IDの形が違えば`400 {"error":"invalid_id"}`、その世代の`meta.json`が無ければ`404 {"error":"no_such_generation"}`、保存先に届かなければ`502 {"error":"storage_error"}`、走っている作業があれば`409` |
| `GET /status` | 無し | `200 Status` |

daemonが止まりかけているときは、`POST`に`503 {"error":"not_running"}`を返します。`POST /take`は、定期実行と同じ流れ(取る → 設定なら確かめる → 整理する)を1回走らせます。作業は非同期で、結果は`GET /status`の`lastTake` / `lastVerify`で見ます。

`Job`と`Status`の形は次のとおりです。時刻はRFC 3339(UTC)です。

```json
{
  "now": "2026-10-10T04:00:00Z",
  "schedule": {"interval": "24h0m0s", "at": "04:00", "timezone": "Asia/Tokyo", "keep": 7, "verify": true, "delayAfter": "36h0m0s"},
  "running": {"kind": "take", "trigger": "schedule", "generationId": "20261009T190000Z", "startedAt": "..."},
  "pending": false,
  "nextRunAt": "2026-10-11T19:00:00Z",
  "lastTake": {
    "kind": "take", "trigger": "api", "generationId": "20261009T190000Z",
    "startedAt": "...", "finishedAt": "...",
    "ok": false, "stage": "verify", "error": "verification failed",
    "verify": {"id": "...", "ok": false, "stages": [], "mismatches": []},
    "deleted": ["20261001T190000Z"]
  },
  "lastVerify": null,
  "latestUsable": {"id": "20261008T190000Z", "at": "2026-10-08T19:00:00Z"},
  "overdue": false
}
```

- `schedule`は、定期実行をしないときは`null`。`nextRunAt`も`null`
- `running`は、走っている作業(無ければ`null`)。取る作業の`generationId`は、取り終えた時点で入る
- `pending`は、定期実行の枠が来たが、別の作業が走っているので待っているとき`true`
- `lastTake` / `lastVerify`は、最後に終わった作業の結果。`ok`は全ての段が通り、検証(走らせたなら)にも通ったとき`true`。`stage`は失敗した段(`take` / `verify` / `prune`)、`verify`は検証の結果(`verify.json`と同じ形)、`deleted`は整理で消した世代
- `latestUsable`は、最新の使える世代
- `overdue`は、遅れの状態にあるとき`true`
- `lastNotifyError`は、最後の通知が送れなかったときだけ入る(送れたら消える)
- この状態はプロセスのメモリにだけあり、再起動すると`lastTake` / `lastVerify`は`null`に戻る。世代の一覧は保存先から読む

### 接続元IPのヘッダー

本体は、管理画面を操作した人の接続元IPを`X-Elythia-Client-IP`で渡します。daemonは、**tokenが一致したリクエストのときだけ**このヘッダーを読み、ログ(`backup: control API request`の`client_ip=`)に残します。IPとして読めない値は捨てます。tokenが一致しないリクエストは、ヘッダーを読む前に401で返します。

## 動かし方

### compose

バックアップ用のサービス(#3458で足すもの)を、`backup daemon`のコマンドで常駐させます。composeのサービスに、次を足します(設定ファイルのパスは、そのサービスがmountしている場所に合わせる)。

```yaml
    command: ["backup", "daemon", "-config", "<設定ファイルのパス>"]
    restart: unless-stopped
    environment:
      TZ: Asia/Tokyo
```

制御APIを使うときは、本体と同じネットワークに置き、本体の`backup.server.serviceUrl`を`http://<サービス名>:<listenのport>`にします。portはホストへ公開しません。

### バイナリを直接実行する

`pg_dump`などPostgreSQLのクライアントは、DBのサーバーと同じ版のものがPATHに要ります(版が違うときは`backup.tools`で場所を指定する)。

```bash
TZ=Asia/Tokyo elythia backup daemon -config .config/default.yml
```

systemdで常駐させるときは、`Restart=on-failure`を付けます。`SIGINT` / `SIGTERM`で止まり、走っている作業には中断を頼みます(中断した世代は`meta.json`の無い半端な世代として残り、後の整理で消える)。

## 確かめるとき

- 起動のログの`next=`で、次に取る時刻を見る
- 制御APIを有効にしていれば、`curl -H "Authorization: Bearer $TOKEN" http://<daemon>:3010/status`で状態を見る
- 通知が届くかは、`notify.webhookUrl`を設定した上で、一時的に保存先を読めない値にして`POST /take`を頼むと、`failure`が届く
