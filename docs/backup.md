# DBのバックアップ

`elythia backup`で、DBのバックアップを取り、ホストの外の保存先へ送る(#3457)。この文書は、取る手順、取ったものが戻せるかを確かめる手順、保存先に置かれるものを扱う。設定のキーの一覧は[設定リファレンス](configuration.md#バックアップ-backup)にある。

**同じホストのディスクに置いたものは、バックアップとして数えない。** ディスクやホストが壊れたときに、DBと一緒に失われるため。保存先は、S3互換のストレージ(AWS S3、Cloudflare R2、MinIOなど)か、別の機器をmountしたディレクトリ(NASなど)にする。

## 何が取られるか

- `pg_dump -Fc`で取った、DB全体のdump(custom format)
- メタ情報(`meta.json`): 取った日時、Elythiaの版とcommit、PostgreSQLの版、取った`pg_dump`の版、管理表(`schema_migrations` / `schema_migrations_local`)の番号、表ごとの行数、DB単位の設定(`ALTER DATABASE ... SET`)、DBのencodingとlocale、入っている拡張(`pg_extension`の名前と版)、sha256

**入らないもの:**

- ドライブのファイル(ローカルの`drive-files`、オブジェクトストレージ)。DBにはファイルの場所だけが入る
- Redis(タイムラインやキャッシュ)
- PostgreSQLのロール(ユーザー)。`pg_dump`はロールを取らない
- 設定ファイル(`.config/default.yml`)。DBが失われても取り出せるよう、保存先の認証情報はこのファイルにある。ファイル自体は別に控えておく

### 行数とsnapshot

行数は、dumpと同じsnapshotで数える。`REPEATABLE READ`のtransactionで`pg_export_snapshot()`を取り、その中で各表を`count(*)`で数えてから、同じsnapshotを`pg_dump --snapshot`に渡す。**サーバーを動かしたまま取っても、メタ情報の行数はdumpの中身と一致する。** [確かめる](#確かめる)ときは、戻した後の行数を同じ方法で数え、この数と突き合わせる。

数える表は、`pg_dump`がデータを書く表(`pg_catalog` / `information_schema` / `pg_toast*`の外の通常の表で、拡張の持ち物でないもの)。分割表は親ではなく子を数える。キーは`schema.table`の形。

DBを全件数えるので、表が大きいと数える時間がかかる。その間もtransactionを開けたままにするので、`VACUUM`がそれより後に消えた行を片付けられない。このtransactionでは`statement_timeout` / `lock_timeout` / `idle_in_transaction_session_timeout`を0にする(DBやロールに設定してあっても、数える途中や`pg_dump`を待つ間に切られないようにするため。`pg_dump`も自分の接続で同じことをする)。

### 拡張

dumpには`CREATE EXTENSION`が入るので、戻す側のPostgreSQLにも同じ拡張が要る。メタ情報の`extensions`に、取った時点の拡張を記録する。

バックアップ用のimageには、UDSの構成のDB(`deploy/postgres-bigm`)と同じ版の**pg_bigmを入れてある**。[確かめる](#確かめる)ときは、このimageの中で使い捨てのPostgreSQLに戻すため。PGroongaなど、それ以外の拡張を入れたDBは、そのままではimageの中で戻せない。確かめるときは、戻す前に`extensions`の拡張が使い捨てのPostgreSQLにあるかを調べ、無ければその名前を出して止める。

## 保存先に置かれるもの

1回のバックアップを「世代」と呼ぶ。世代は、保存先の下に`generations/<ID>/`として置かれる。IDはUTCの取った時刻(`20261010T040000Z`の形)で、文字列の順が時刻の順になる。

| ファイル | 中身 |
|---|---|
| `dump.pgc` | `pg_dump -Fc`の出力(暗号化しないとき) |
| `dump.pgc.age` | 上をageで暗号化したもの(暗号化するとき) |
| `meta.json` | メタ情報。**dumpを置いて読み直した後に、最後に置く** |
| `verify.json` | 確かめた結果([確かめる](#確かめる))。確かめた後に足される |

S3互換の保存先では、`backup.storage.s3.prefix`の下に`generations/`が置かれる。

**`meta.json`と、それが名指しするdumpの両方がある世代だけを、揃ったものとして扱う。** 取る処理は次の順に進み、途中で失敗したら置いたものを消してから終わる。

1. dumpを保存先へ流す。S3互換の保存先では、64MiBより小さければ1回の`PutObject`、それ以上ならmultipart uploadで送る。multipart uploadは、完了するまでobjectとして見えず、失敗したら中断(abort)する。ディレクトリでは、同じディレクトリの一時ファイル(`.tmp-`で始まる名前)に書いてから置く。**`pg_dump`が途中で失敗しても、書きかけのdumpは`dump.pgc`として見えない**
2. 保存先から読み直し、大きさとsha256が送ったものと一致するかを確かめる
3. `meta.json`を置く。置くのに失敗したと返ってきたときは、実は置けている場合に備えて`meta.json`も消す

`meta.json`の`dumpSha256`は保存したbytes(暗号化したときは暗号化した後)の、`plainSha256`は`pg_dump`の出力そのもののsha256。暗号化しないときは2つが同じ値になる。

### 同じ秒に2つ取ったとき

世代のIDは秒の単位なので、同じ秒に2つ取ると同じ場所に書こうとする。取り始めに同じIDの世代が既にあれば失敗するが、**保存先の側では排他しない**ので、2つがほぼ同時に始まるとファイルが混ざりうる。定期実行(#3460)は同時に1つしか走らせない。**手で`take`を同時に打たないこと。**

保存先で「まだ無ければ置く」形(S3の`If-None-Match: *`など)にしないのは、SDKが500や切断で同じ要求を自動で送り直すため。1回目が保存済みだと送り直しが失敗し、自分が置いたものを他が置いたと取り違える。

### 途中で強制終了したときの残り物

`SIGKILL`やコンテナの強制停止では、後始末が走らない。

- ディレクトリ: 世代のディレクトリに`.tmp-`で始まる一時ファイルが残る。一覧には出ないが容量は使うので、バックアップを取っていないときに消す(例: `find /mnt/nas/elythia-backup/generations -name '.tmp-*' -mmin +1440 -delete`)
- S3互換: 完了しなかったmultipart uploadのpartsが残る。objectとしては見えないが、容量の料金がかかる。bucketのlifecycleに`AbortIncompleteMultipartUpload`(例: 開始から1日)を設定しておく。AWS S3なら次の形

```json
{"Rules": [{"ID": "abort-incomplete", "Status": "Enabled", "Filter": {"Prefix": ""},
  "AbortIncompleteMultipartUpload": {"DaysAfterInitiation": 1}}]}
```

## 保存先

### S3互換

`backup.storage.type: s3`にして、`backup.storage.s3`に接続先を書く。

**`endpoint`は`https://`にする。** `http://`も書けるが、暗号化していないdumpがそのまま流れる。同じ私設網の中のMinIOなど、経路を信頼できるときだけにする。

### ディレクトリ

`backup.storage.type: dir`にして、`backup.storage.dir.path`に別の機器をmountしたディレクトリを書く。

**mountした先の根に、目印のファイル`.elythia-backup`を置く。** 目印が無いディレクトリには書かない。composeのbind mountでは、NASのmountが外れてもホストの空のディレクトリがそのまま見えるので、ディレクトリがあるかだけでは、別の機器に書いているかを判定できないため。目印は、**NASをmountした状態で**一度だけ作る。

```bash
# NAS を /mnt/nas/elythia-backup に mount した状態で
touch /mnt/nas/elythia-backup/.elythia-backup
```

ディレクトリと目印は自動では作らない。作ると、mountが外れたときに同じホストのディスクへ書き始める。

## 暗号化

dumpには、利用者の秘密鍵(`user_keypair`)、token、パスワードのhashが入る。`backup.encryption.enabled: true`にすると、dumpを[age](https://age-encryption.org)で暗号化してから送る。

**取るのに要るのは公開鍵だけ。** 秘密鍵はバックアップを取るホストに置かなくてよい。[確かめる](#確かめる)・戻す(#3461)ときにだけ、`backup.encryption.identityFile`で渡す。

```bash
# 鍵を作る(ageの配布物のage-keygenを使う)。リポジトリの外に置く
age-keygen -o /etc/elythia/backup-identity.txt
# 表示された公開鍵(age1...)をbackup.encryption.recipientsに書く
```

**秘密鍵はリポジトリの中(`.config`を含む)に置かない。** リポジトリの下に置くと、imageを作るときのbuild contextに入りうる(`.dockerignore`は`.config/*.txt`を外しているが、それ以外の場所は外さない)。composeで渡すときは、リポジトリの外のファイルをread-onlyでmountする(`docker-compose.yml`の`backup`サービスのコメントを参照)。

秘密鍵を失うと、暗号化した世代はどれも戻せない。ホストの外に控えておく。

手で戻すときは、`age -d -i backup-identity.txt dump.pgc.age > dump.pgc`で復号してから`pg_restore`に渡す。

## 取る

どの構成でも、設定ファイル(本体と同じもの)に`backup:`の節を書いてから呼ぶ。書き方は`.config/default.yml.example`の末尾にある。

**`pg_dump`のメジャーバージョンは、DBのサーバーより古くてはいけない。** 古い`pg_dump`は新しいサーバーから取れない。バックアップ用のimage(`Dockerfile`のtarget `backup`)は、DBと同じ`postgres:18-alpine`に`elythia`を足したもので、版が揃う。

**imageを作るときは`MKGO_COMMIT`を渡す。** `meta.json`の`elythiaCommit`に入る。Dockerfileの中ではgitを呼べないので、渡さないと空になる。

### compose(TCP)

`docker-compose.yml`の`backup`サービスを使う。`profiles`に入れてあるので、`docker compose up`では起動しない。DBが動いている状態で、`run`で呼ぶ。

```bash
# imageを作る(初回と、Elythiaを更新したとき)
MKGO_COMMIT=$(git rev-parse --short HEAD) docker compose build backup

# 取る
docker compose run --rm --no-deps backup take

# 保存先の世代を一覧する
docker compose run --rm --no-deps backup list
```

- 設定ファイルは`./.config/docker.yml`をmountする。本体(`app`)が設定ファイルをmountしていない(imageに焼き込んだ既定のまま)ときも、`backup`はmountが要る
- 保存先をディレクトリにするときは、`backup`サービスの`volumes`のコメントを外して別の機器のmount先を渡し、`backup.storage.dir.path`に`/backup`を書く。コンテナはUID 70(`postgres`)で動くので、そのユーザーが書けるようにしておく。根に目印`.elythia-backup`が要る(上の「ディレクトリ」)
- `--no-deps`を付ける。`backup`サービスは`depends_on`を持たず、DBが動いている前提で繋ぐ

### UDS

`compose.uds.yaml.example`の`backup`サービスを使う(`compose.uds.yaml`に複製しているなら、同じ節を足す)。`mkgo`と同じ設定ファイルを読み、`pg_sock`のvolumeを通してUNIXドメインソケットでDBへ繋ぐ。

```bash
MKGO_COMMIT=$(git rev-parse --short HEAD) docker compose -f compose.uds.yaml build backup
docker compose -f compose.uds.yaml run --rm --no-deps backup take
docker compose -f compose.uds.yaml run --rm --no-deps backup list
```

### バイナリ直接実行

サーバーと同じ版の`pg_dump`がPATHにあるホストで、`elythia`をそのまま呼ぶ。PATHに無い、または版の違うものが先に見つかるときは、`backup.tools.pgDump`にパスを書く。

```bash
elythia backup take -config .config/default.yml
elythia backup list -config .config/default.yml
elythia backup list -config .config/default.yml -json   # JSONで出す
```

### 接続先とTLS

`pg_dump`の接続先は、本体と同じ`db:`の節から作る(TCPとUDSのどちらも)。パスワードは`PGPASSWORD`で`pg_dump`に渡し、コマンドラインには出さない。

`db.extra`のTLSの設定は、`pg_dump`(libpq)に次の形で渡す。本体(pgx)と挙動が違う点がある。

| `db.extra` | 本体(pgx) | `pg_dump`(libpq) |
|---|---|---|
| `ssl: true`、または`sslmode: verify-full`で`sslrootcert`無し | システムのCAで検証 | `sslrootcert=system`を補い、システムのCAで検証する(PostgreSQL 16以降のlibpq) |
| `sslmode: verify-ca`で`sslrootcert`無し | システムのCAで検証(host名は見ない) | **取れない。** libpqはシステムのCAを`verify-full`でしか使えないので、`sslrootcert`を書くか`verify-full`にする |
| `sslrootcert`あり | そのCAで検証 | 同じ |
| `ssl: no-verify` / `sslmode: require`など | 本体と同じ | 同じ |

`sslrootcert`で指すCAファイルは、`pg_dump`が動く場所から同じパスで読める必要がある。composeでは、`backup`サービスにも同じパスでmountする。

## 確かめる

`elythia backup verify`で、取った世代を使い捨てのPostgreSQLへ実際に戻し、戻せることを確かめる(#3459)。ファイルが読めるだけでは、戻そうとしたときに初めて戻らないと分かることがあるため。**本番のDBには繋がない。** DBを作る権限も要らない。

引数は世代のID(`20261010T040000Z`の形)か`latest`。`latest`は`meta.json`のある世代のうち最も新しいものを選ぶ(`meta.json`の無い、取っている途中か取るのに失敗した世代は選ばない)。全ての段が通れば終了コード0、1つでも通らなければ1を返す。フラグ(`-config`)は世代の指定より前に書く。

**`initdb` / `pg_ctl` / `pg_restore`が要る。** 本体のimageはdistrolessで、これらが無い。バックアップ用のimageで動かす。`initdb`はrootでは動かないので、root以外のユーザーで実行する(バックアップ用のimageは`postgres`で動く)。同梱のmigrationの番号と比べるので、作業ディレクトリに`migration/`が要る(imageでは`/app`)。

### compose(TCP)

```bash
docker compose run --rm --no-deps backup verify latest
docker compose run --rm --no-deps backup verify 20261010T040000Z
```

暗号化した世代を確かめるときは、`backup`サービスの`volumes`のコメントを外して秘密鍵をmountし、`backup.encryption.identityFile`にmount先(`/run/secrets/backup-identity.txt`)を書く。

### UDS

```bash
docker compose -f compose.uds.yaml run --rm --no-deps backup verify latest
```

秘密鍵の渡し方はcompose(TCP)と同じ(`compose.uds.yaml.example`の`backup`サービスのコメント)。

### バイナリ直接実行

DBのサーバーと同じメジャーバージョンの`initdb` / `pg_ctl` / `pg_restore`があるホストで、リポジトリ(または`migration/`のある場所)を作業ディレクトリにして呼ぶ。PATHに無い、または版の違うものが先に見つかるときは、`backup.tools.initdb` / `pgCtl` / `pgRestore`にパスを書く。

```bash
elythia backup verify -config .config/default.yml latest
```

DBに拡張(pg_bigmなど)を入れているときは、このホストのPostgreSQLにも同じ拡張が要る。`meta.json`のlocale(`databaseLocale`)で`initdb`するので、そのlocaleもこのホストに要る。

### 3つの段

段は順に進み、ある段が通らなければ後の段は`skipped`になる。

| 段 | `verify.json`の`stage` | 確かめること |
|---|---|---|
| 読めるか | `readable` | 保存されたバイト列の大きさとsha256が`meta.json`と一致する。暗号化した世代は`backup.encryption.identityFile`で復号でき、復号後のsha256(`plainSha256`)も一致する。`pg_restore --list`が通る |
| 戻せるか | `restorable` | 一時ディレクトリに`initdb`と`pg_ctl`で使い捨てのPostgreSQLを立て、`pg_restore --no-owner --no-privileges --exit-on-error`で戻す。戻した後に、取るときと同じ方法(上の「行数とsnapshot」)で表ごとの行数を数え、`meta.json`の`rowCounts`と突き合わせる |
| Elythiaとして読めるか | `usable` | 戻したDBの管理表(`schema_migrations`、あれば`schema_migrations_local`)がdirtyでなく、このバイナリの同梱のmigrationの最大番号より進んでいない。管理表の値が`meta.json`の`migrations`と一致する。`elythia fsck`と同じ検査が流れる |

- 使い捨てのPostgreSQLは、`meta.json`の`databaseLocale`と同じencodingとlocaleで`initdb`する。一時ディレクトリのunix socketだけで待ち受け、TCPを開かない
- 使い捨てのPostgreSQLとそのデータは、通っても通らなくても止めて消す。復号したdumpを置く一時ファイルも消す
- 一時ディレクトリには、復号したdumpと戻したDBが同時に置かれる。dumpと戻したDBを合わせた大きさの空きが要る。場所は`TMPDIR`(未設定なら`/tmp`)で、composeではコンテナの中
- `rowCounts`が空の`meta.json`は、突き合わせが何も検査せずに通ってしまうので、`restorable`を失敗にする
- `fsck`が見つけたカウンタのずれと孤児行は、段を失敗にせず`warnings`に残す。snapshotの時点で元のDBにあったずれで、バックアップの欠陥ではないため。`fsck`のクエリ自体が通らない(表や列が無い)ときは失敗にする

### 結果(`verify.json`)

結果は`meta.json`の隣に`verify.json`として置く。保存先の既存のファイル(dumpと`meta.json`)は読むだけで、書き換えない。もう一度確かめると、`verify.json`を上書きする。

```json
{
  "id": "20261010T040000Z",
  "verifiedAt": "2026-10-10T05:00:00Z",
  "ok": false,
  "stages": [
    {"stage": "readable", "ok": true},
    {"stage": "restorable", "ok": false, "error": "row counts of 1 table(s) do not match meta.json"},
    {"stage": "usable", "ok": false, "skipped": true}
  ],
  "mismatches": [
    {"table": "public.note", "expected": 120345, "actual": 120344}
  ],
  "elythiaVersion": "2.0.0"
}
```

- `mismatches`の`actual`が`-1`のときは、戻したDBにその表が無い。`expected`が`-1`のときは、戻したDBにあるが`meta.json`に無い
- **確かめること自体ができなかったときは、`verify.json`を書かない。** 世代に`meta.json`が無い、保存先に繋がらない、暗号化した世代なのに`identityFile`が無い、`meta.json`の`extensions`の拡張が使い捨てのPostgreSQLに無い、`initdb`や`pg_ctl start`が失敗した、など。バックアップの良し悪しではなく環境の問題なので、終了コード1で理由を表示するだけにする。使い捨てのPostgreSQLを消せなかったときも、結果を表示したうえで`verify.json`を書かずに1を返す
- **終了コード0のときは、`verify.json`が置かれている。** 1のときは、置かれていることも置かれていないこともある

### 確かめていないこと

- 戻した後に、Elythiaのサーバーとして起動できるか(Redisやメディアの保存先は扱わない)。戻す手順は#3461で扱う
- `pg_dump`に入らないDB単位の設定(`meta.json`の`databaseSettings`)。戻すときに入れ直す(#3461)

## 一覧の見方

```
ID                STATUS    SIZE  ENCRYPTED  VERIFIED  ELYTHIA  MIGRATION
20261010T040000Z  complete  2100  false      -         2.0.0    121
```

| 列 | 意味 |
|---|---|
| `STATUS` | `complete`(`meta.json`と、それが名指しするdumpがある)、`incomplete`(`meta.json`が無い。途中で止まったもの)、`no-dump`(`meta.json`はあるがdumpが無い。戻せない)、`unreadable`(`meta.json`が読めない。知らない`formatVersion`など) |
| `SIZE` | 世代のファイルの合計(bytes) |
| `VERIFIED` | 確かめた結果(`verify.json`の`ok`)。通れば`ok`、通らなければ`failed`、まだ確かめていなければ`-` |
| `MIGRATION` | `schema_migrations`の番号。表が無ければ`missing`、行が無ければ`empty`、dirtyなら`(dirty)`が付く |
