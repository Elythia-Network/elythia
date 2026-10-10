# DBのバックアップ

`elythia backup`で、DBのバックアップを取り、ホストの外の保存先へ送る(#3457)。この文書は、取る手順と、保存先に置かれるものを扱う。設定のキーの一覧は[設定リファレンス](configuration.md#バックアップ-backup)にある。

**同じホストのディスクに置いたものは、バックアップとして数えない。** ディスクやホストが壊れたときに、DBと一緒に失われるため。保存先は、S3互換のストレージ(AWS S3、Cloudflare R2、MinIOなど)か、別の機器をmountしたディレクトリ(NASなど)にする。

## 何が取られるか

- `pg_dump -Fc`で取った、DB全体のdump(custom format)
- メタ情報(`meta.json`): 取った日時、Elythiaの版とcommit、PostgreSQLの版、管理表(`schema_migrations` / `schema_migrations_local`)の番号、表ごとの行数、DB単位の設定(`ALTER DATABASE ... SET`)、sha256

**入らないもの:**

- ドライブのファイル(ローカルの`drive-files`、オブジェクトストレージ)。DBにはファイルの場所だけが入る
- Redis(タイムラインやキャッシュ)
- PostgreSQLのロール(ユーザー)。`pg_dump`はロールを取らない
- 設定ファイル(`.config/default.yml`)。DBが失われても取り出せるよう、保存先の認証情報はこのファイルにある。ファイル自体は別に控えておく

### 行数とsnapshot

行数は、dumpと同じsnapshotで数える。`REPEATABLE READ`のtransactionで`pg_export_snapshot()`を取り、その中で各表を`count(*)`で数えてから、同じsnapshotを`pg_dump --snapshot`に渡す。**サーバーを動かしたまま取っても、メタ情報の行数はdumpの中身と一致する。** 戻せるかを確かめる段階(#3459)は、戻した後の行数をこの数と突き合わせる。

数える表は、`pg_dump`がデータを書く表(`pg_catalog` / `information_schema` / `pg_toast*`の外の通常の表で、拡張の持ち物でないもの)。分割表は親ではなく子を数える。キーは`schema.table`の形。

DBを全件数えるので、表が大きいと数える時間がかかる。その間もtransactionを開けたままにするので、`VACUUM`がそれより後に消えた行を片付けられない。

## 保存先に置かれるもの

1回のバックアップを「世代」と呼ぶ。世代は、保存先の下に`generations/<ID>/`として置かれる。IDはUTCの取った時刻(`20261010T040000Z`の形)で、文字列の順が時刻の順になる。

| ファイル | 中身 |
|---|---|
| `dump.pgc` | `pg_dump -Fc`の出力(暗号化しないとき) |
| `dump.pgc.age` | 上をageで暗号化したもの(暗号化するとき) |
| `meta.json` | メタ情報。**dumpを置いて読み直した後に、最後に置く** |
| `verify.json` | 検証の結果(#3459で足す) |

S3互換の保存先では、`backup.storage.s3.prefix`の下に`generations/`が置かれる。

**`meta.json`の無い世代は、途中で止まったものとして扱う。** 取る処理は次の順に進み、途中で失敗したら置いたdumpを消してから終わる。

1. dumpを保存先へ流す。S3互換の保存先では、64MiBより小さければ1回の`PutObject`、大きければmultipart uploadで送る。multipart uploadは、完了するまでobjectとして見えず、失敗したら中断(abort)する。ディレクトリでは、同じディレクトリの一時ファイル(`.tmp-`で始まる名前)に書いてから名前を変える。**`pg_dump`が途中で失敗しても、書きかけのdumpは`dump.pgc`として見えない**
2. 保存先から読み直し、大きさとsha256が送ったものと一致するかを確かめる
3. `meta.json`を置く

`meta.json`の`dumpSha256`は保存した bytes(暗号化したときは暗号化した後)の、`plainSha256`は`pg_dump`の出力そのもののsha256。暗号化しないときは2つが同じ値になる。

## 暗号化

dumpには、利用者の秘密鍵(`user_keypair`)、token、パスワードのhashが入る。`backup.encryption.enabled: true`にすると、dumpを[age](https://age-encryption.org)で暗号化してから送る。

**取るのに要るのは公開鍵だけ。** 秘密鍵はバックアップを取るホストに置かなくてよい。確かめる・戻すとき(#3459 / #3461)にだけ、`backup.encryption.identityFile`で渡す。

```bash
# 鍵を作る (age の配布物の age-keygen を使う)
age-keygen -o backup-identity.txt
# 表示された公開鍵 (age1...) を backup.encryption.recipients に書く
```

秘密鍵を失うと、暗号化した世代はどれも戻せない。ホストの外に控えておく。

手で戻すときは、`age -d -i backup-identity.txt dump.pgc.age > dump.pgc`で復号してから`pg_restore`に渡す。

## 取る

どの構成でも、設定ファイル(本体と同じもの)に`backup:`の節を書いてから呼ぶ。書き方は`.config/default.yml.example`の末尾にある。

**`pg_dump`のメジャーバージョンは、DBのサーバーより古くてはいけない。** 古い`pg_dump`は新しいサーバーから取れない。バックアップ用のimage(`Dockerfile`のtarget `backup`)は、DBと同じ`postgres:18-alpine`に`elythia`を足したもので、版が揃う。

### compose(TCP)

`docker-compose.yml`の`backup`サービスを使う。`profiles`に入れてあるので、`docker compose up`では起動しない。DBが動いている状態で、`run`で呼ぶ。

```bash
# image を作る (初回と、Elythia を更新したとき)
docker compose build backup

# 取る
docker compose run --rm --no-deps backup take

# 保存先の世代を一覧する
docker compose run --rm --no-deps backup list
```

- 設定ファイルは`./.config/docker.yml`をmountする。本体(`app`)が設定ファイルをmountしていない(imageに焼き込んだ既定のまま)ときも、`backup`はmountが要る
- 保存先をディレクトリにするときは、`backup`サービスの`volumes`のコメントを外して別の機器のmount先を渡し、`backup.storage.dir.path`に`/backup`を書く。コンテナはUID 70(`postgres`)で動くので、そのユーザーが書けるようにしておく
- `--no-deps`を付ける。`backup`サービスは`depends_on`を持たず、DBが動いている前提で繋ぐ

### UDS

`compose.uds.yaml.example`の`backup`サービスを使う(`compose.uds.yaml`に複製しているなら、同じ節を足す)。`mkgo`と同じ設定ファイルを読み、`pg_sock`のvolumeを通してUNIXドメインソケットでDBへ繋ぐ。

```bash
docker compose -f compose.uds.yaml build backup
docker compose -f compose.uds.yaml run --rm --no-deps backup take
docker compose -f compose.uds.yaml run --rm --no-deps backup list
```

### バイナリ直接実行

サーバーと同じ版の`pg_dump`がPATHにあるホストで、`elythia`をそのまま呼ぶ。PATHに無い、または版の違うものが先に見つかるときは、`backup.tools.pgDump`にパスを書く。

```bash
elythia backup take -config .config/default.yml
elythia backup list -config .config/default.yml
elythia backup list -config .config/default.yml -json   # JSON で出す
```

接続先は`db:`の節から決まる(TCP・UDS・`db.extra`のTLSの設定も本体と同じ)。パスワードは`PGPASSWORD`で`pg_dump`に渡し、コマンドラインには出さない。

## 一覧の見方

```
ID                STATUS    SIZE  ENCRYPTED  VERIFIED  ELYTHIA  MIGRATION
20261010T040000Z  complete  2100  false      -         2.0.0    121
```

| 列 | 意味 |
|---|---|
| `STATUS` | `complete`(`meta.json`がある)、`incomplete`(`meta.json`が無い。途中で止まったもの)、`unreadable`(`meta.json`が読めない。知らない`formatVersion`など) |
| `SIZE` | 世代のファイルの合計(bytes) |
| `VERIFIED` | 検証の結果(#3459で足す)。まだ確かめていなければ`-` |
| `MIGRATION` | `schema_migrations`の番号。表が無ければ`missing`、行が無ければ`empty`、dirtyなら`(dirty)`が付く |
