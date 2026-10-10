# DBのバックアップ

`elythia backup`で、DBのバックアップを取り、ホストの外の保存先へ送る(#3457)。この文書は、取る手順、取ったものが戻せるかを確かめる手順、それらを定期的に回す手順、保存先に置かれるものを扱う。戻す手順と、PostgreSQLのメジャーバージョンを上げる手順は[デプロイ](deployment.md#バックアップから戻す-restore)にある([戻す](#戻す))。設定のキーの一覧は[設定リファレンス](configuration.md#バックアップ-backup)にある。

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

[戻す](#戻す)とき(`backup restore`、#3461)は、記録した拡張が戻す先のDBのサーバーに無ければ、何もせずに止まる。戻す先はバックアップ用のimageではなくDBのサーバーなので、pg_bigmを使うDBなら、DBのサーバーにもpg_bigmが要る(UDSの構成なら`deploy/postgres-bigm`)。

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

世代のIDは秒の単位なので、同じ秒に2つ取ると同じ場所に書こうとする。取り始めに同じIDの世代が既にあれば失敗するが、**保存先の側では排他しない**ので、2つがほぼ同時に始まるとファイルが混ざりうる。[定期実行](#定期実行)は同時に1つしか走らせない。**手で`take`を同時に打たないこと。**

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

**管理画面(#3462)からも使うときは、本体とグループを共有する。** バックアップ用のサービスはUID 70(postgres)で、本体は別のUIDで動く(compose(TCP)の`docker-compose.yml`ではUID・GIDとも991、UDSの`compose.uds.yaml.example`では65532)。本体が一覧・ダウンロード・削除をするには、本体のGIDのグループとして読めて、ディレクトリに書ける(消せる)必要がある。

- `elythia backup`は、ディレクトリを`0770`、ファイルを`0640`で作る(作った後でchmodし直すので、umaskに左右されない。ただし下のとおりchmodを受け流したときは、作ったときのmode(ファイルは`0600`、ディレクトリは`0770`からumaskを引いたもの)のまま残る)。グループは、ファイルの中身を変えることはできないが、読むことと、ディレクトリの中にファイルを作る・置き換える・消すことはできる(ディレクトリに書けるため)。他人(other)には一切渡さない
- グループは、根に付けたsetgidで引き継がせる。根の所有者とグループを`70:<本体のGID>`、modeを`2770`にする
- composeの`backup`サービスは、本体のGIDを補助グループに持つ(`group_add`。同梱のcomposeに書いてある)。持たないと、作ったディレクトリからsetgidが落ち、その下のファイルが本体から読めなくなる
- 本体のコンテナにも、同じホストのパスを`backup.storage.dir.path`(または`backup.server.storage.dir.path`)と同じ場所にmountする

```bash
# compose(TCP)。UDSでは991を65532に読み替える
chown 70:991 /mnt/nas/elythia-backup
chmod 2770 /mnt/nas/elythia-backup
# 既に世代がある場合(以前の版は0700 / 0600で作っていた)
chgrp -R 991 /mnt/nas/elythia-backup/generations
find /mnt/nas/elythia-backup/generations -type d -exec chmod 2770 {} +
find /mnt/nas/elythia-backup/generations -type f -exec chmod 0640 {} +
```

**GIDがホストやNASの別のグループと重ならないか確かめる。** ファイルにはGIDの数字だけが記録されるので、ホストやNASで同じ数字のグループに入っている者は、バックアップを読めて、消せる。991はホストの別のグループ(例えば`polkitd`)に割り当てられていることがある。`getent group 991`(UDSでは65532)をホストとNASの両方で見て、使われていれば、そのグループに人やサービスが入っていないことを確かめる。

unix extensionsの無いCIFSなど、chmodを受け付けないファイルシステムでは、`elythia backup`はchmodの失敗のうち`EPERM` / `EINVAL`と、対応していないことを示すもの(`ENOTSUP`(`EOPNOTSUPP`) / `ENOSYS`)を警告に留めて書き続ける。警告は、本体では起動ごと、コマンドでは実行ごとに初回だけ出す(`backup: the storage does not accept chmod`)。それ以外の失敗(`EIO`など)では止める。権限はmountの設定(`file_mode` / `dir_mode` / `gid`)で決まるので、そちらで本体のGIDから読めるようにし、otherに渡さない値にする。

**暗号化しないdumpは、本体のグループに読める。** 本体はDBの接続情報(と、DBの中の秘密鍵・token)を元から持っているので、読めて新たに漏れるものは無い、という判断。本体のGIDに他のプロセスを入れないこと。NASがUID・GIDを書き換える設定(`all_squash`など)だと、この分け方は効かない。

本体とバックアップ用のサービスを同じユーザーで動かす構成(バイナリ直接実行など)では、この手順は要らない。

保存先の中の通常のファイルだけを世代のファイルとして扱う。根の下のsymlinkは、根の中を指すものも辿らない。一覧に出さず、読み出しは`not found`にし、途中にsymlinkがある場所へは書かず、消さない。`backup.storage.dir.path`そのものがsymlinkなのは構わない(起動時に実体へ解決する)。

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
- **確かめること自体ができなかったときは、`verify.json`を書かない。** 世代に`meta.json`が無い、保存先に繋がらない、暗号化した世代なのに`identityFile`が無い、`meta.json`の`extensions`の拡張が使い捨てのPostgreSQLに無い、`initdb`や`pg_ctl start`が失敗した、など。バックアップの良し悪しではなく環境の問題なので、終了コード1で理由を表示するだけにする。使い捨てのPostgreSQLを消せなかったときも、結果を表示したうえで`verify.json`を書かずに1を返す(定期実行のdaemonは、この場合も判定を`verify.json`に残す。下の「1回の流れ」)
- **終了コード0のときは、`verify.json`が置かれている。** 1のときは、置かれていることも置かれていないこともある

### 確かめていないこと

- 戻した後に、Elythiaのサーバーとして起動できるか(Redisやメディアの保存先は扱わない)。戻す手順は#3461で扱う
- `pg_dump`に入らないDB単位の設定(`meta.json`の`databaseSettings`)。戻すときに入れ直す(#3461)

## 定期実行

`elythia backup daemon`は、設定した間隔でバックアップを[取り](#取る)、[確かめ](#確かめる)、古い世代を消す常駐のプロセス(#3460)。失敗したとき、検証で食い違ったとき、間隔を過ぎても新しい世代ができないときに、Webhookで運営者へ知らせる。本体の管理画面(#3462)が「今すぐ取る」「今すぐ確かめる」「状態」を頼むための制御APIも、このプロセスが持つ。

取る処理と確かめる処理は、`backup take` / `backup verify`と同じもの(同じ設定、同じ`pg_dump` / `initdb` / `pg_ctl` / `pg_restore`)を使う。そのためバックアップ用のimageの中か、それらがあるホストで動かす。起動するときに、保存先が開けること、同梱のmigrationの番号が読めること(作業ディレクトリに`migration/`が要る)、`backup.encryption.identityFile`を書いたならそれが読めることを確かめ、どれかが駄目なら起動しない。暗号化していて`schedule.verify: true`なのに`identityFile`が空のときも起動しない(毎回の検証が復号できずに落ち、使える世代が1つもできないため)。

### 設定

設定ファイルの`backup:`の節に書く。保存先(`backup.storage`)と暗号化(`backup.encryption`)は、取る・確かめるときと同じものを使う。キーの一覧は[設定リファレンス](configuration.md#バックアップ-backup)にある。

```yaml
backup:
  schedule:
    interval: 24h        # 間隔 (Go の duration)。空なら定期実行しない
    at: "04:00"          # 取る時刻 (HH:MM、プロセスのタイムゾーン)。省略可
    keep: 7              # 残す世代の数。0 なら消さない
    verify: true         # 取った後に毎回確かめる
    delayAfter: 36h      # 遅れを知らせるまでの時間。省略時は interval の 1.5 倍
    listen: ":3010"      # 制御 API の待ち受け。空なら制御 API を持たない (下の「制御API」)
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

`interval`も`listen`も空なら、何もすることが無いので起動しない。`interval`を空にして`listen`だけを設定すると、定期実行はせず、管理画面から頼まれたときだけ取る・確かめる形になる。

#### 時刻とタイムゾーン

`at`は、プロセスのタイムゾーン(環境変数`TZ`)で解釈する。`postgres:18-alpine`は`TZ`が空だとUTCになり、同梱のcomposeの`backup-daemon`サービスも`TZ`を渡さないので、既定では`at`はUTCの時刻になる。日本時間で決めたいときは、`backup-daemon`の`environment`のコメントを外して`TZ: Asia/Tokyo`を渡す(このimageには`/usr/share/zoneinfo`が入っている)。起動時のログ`backup: schedule started`に、解釈したタイムゾーン(`timezone=`)と次に取る時刻(`next=`)が出るので、確かめる。

#### 起動したときに取るか

起動したとき、最新の揃った世代(`meta.json`と、それが名指しするdumpがある世代)が`interval`より古いか、世代が1つも無ければ、すぐに1回取る。daemonが止まっていた間の分を、次の枠まで待たずに取り戻すため。最新の世代が`interval`より新しければ、次の枠まで待つ。そのため、再起動のたびに取り直すことはない。

`at`があるとき、枠は最新の世代を取った日の`at`から`interval`ごとに数える。再起動した日から数え直さないので、2日以上の間隔でも再起動で枠がずれない(例: `interval: 168h`、`at: "04:00"`で、最新の世代が6日前の04:00なら、次は翌日の04:00)。

取っている途中に次の枠が来たときは、重ねて取らず、終わるのを待って1回だけ取る。

### 1回の流れ

1. 取る。取る前に、DBが接続を受け付けるまで最大5分待つ(DBと同時に起動したときや、DBの再起動中のため)。待ちきれなければそのまま取りにいき、失敗したら通知して終わる
2. `verify: true`なら確かめる。検証が最後まで走れなかったら「失敗」、走って食い違ったら「食い違い」として通知し、**世代の整理はしない**
3. 世代を整理する

1つのプロセスの中では、取る・確かめる・消すを同時に1つしか走らせない。確かめている世代を消したり、書いている途中の世代を消したりしないため。**同じ保存先に対して、daemonを2つ動かさない。** daemonを動かしている間に手で`take`を打つと、daemonの作業とは排他されない(同じ秒に重なると、上の「同じ秒に2つ取ったとき」のとおり混ざりうる)。手で取るときは、制御APIの`POST /take`で頼む。

確かめる処理は、判定が出た後に使い捨てのPostgreSQLの後始末(`pg_ctl stop`や一時ディレクトリの削除)か`verify.json`の保存に失敗すると、3つの段が全て通っていても`ok: false`にし、`verify.json`を書かずに誤りと一緒に返す(上の「結果」)。daemonは、3つの段の判定が揃っていれば、**判定を段の結果から求め直す**(全ての段が通っていれば通った世代)。通らなかったなら「食い違い」を知らせ、通ったなら整理まで進める。どちらでも`verify.json`を自分で書いてから、後始末の失敗を「失敗」として別に知らせる。整理は保存先の`verify.json`を読んで行うため。

確かめている途中でdaemonを止めた(`SIGTERM`など)ときは、中断として扱う。中断で`pg_restore`が切れたことを世代の欠陥と取り違えないよう、`verify.json`は確かめる前の状態(無ければ無い)に戻し、通知もしない。`GET /status`の`lastVerify` / `lastTake`の`error`に`interrupted`と残る。

### 世代の整理

**古い世代は、使える新しい世代が`keep`個そろってから消す。** 壊れたバックアップしか残らない状態を作らないため。

- 「使える世代」は、`verify: true`なら、揃った世代で`verify.json`が`ok: true`のもの。`verify: false`なら、揃った世代で、検証に落ちていないもの(`verify.json`が無いか`ok: true`)
- `meta.json`か`verify.json`が読めない世代は、理由(壊れている、保存先の一時的な誤り)に関わらず使えない世代として扱う。使えない世代が増えても、消す世代は増えない
- 新しい方から数えて`keep`個目の使える世代より古い世代は、使えるかどうかに関わらず全て消す
- それより新しい世代は、検証に落ちた世代も、揃っていない世代も残す。落ちた理由を調べられるようにするためと、揃っていない世代は書いている途中かもしれないため。`keep`個の窓の外に出たら消える
- 使える世代が`keep`個に満たない間は、何も消さない。検証に落ち続けると世代が増え続けるので、通知を見たら原因を直す
- `keep: 0`なら何も消さない
- 1つの世代を消すときは、`meta.json`を最初に消す。途中で失敗しても、その世代は揃っていない世代に見え、dumpの欠けた揃った世代には見えない

### 通知

| 種類 | いつ | `event` |
|---|---|---|
| 失敗 | 取る・確かめる・消すのどれかが誤りを返した。確かめる処理の後始末の失敗も含む | `failure` |
| 食い違い | 検証が走り、戻せない・行数が違う・Elythiaとして読めない | `mismatch` |
| 遅れ | 最後の使える世代(無ければdaemonの起動時刻)から`delayAfter`を過ぎた | `delay` |

遅れは、取る処理が戻ってこない(固まった)ときにも知らせる。遅れが続く間は、`delayAfter`ごとに繰り返し知らせる。起動した時点で既に遅れていれば、起動してすぐに知らせる。**daemonそのものが止まっていると、遅れも知らせられない。** daemonの死活は、コンテナの再起動の設定(`restart: unless-stopped`など)と、制御APIの`GET /status`で見る。

#### 送る形

`format: generic`は、次のJSONを送る(`text`は人が読む1行の文面)。

```json
{
  "event": "mismatch",
  "instance": "https://example.tld",
  "occurredAt": "2026-10-10T04:12:00Z",
  "generationId": "20261009T190000Z",
  "message": "the backup did not pass verification",
  "failedStages": [{"stage": "restorable", "ok": false, "error": "the dump cannot be restored or its row counts differ; see verify.json"}],
  "mismatches": [{"table": "public.note", "expected": 10, "actual": 9}],
  "text": "[Elythia backup] https://example.tld: verification found a broken backup ..."
}
```

`stage`(失敗した段: `take` / `verify` / `prune`)と`lastUsableAt`(遅れのときの最後の使える世代)は、値があるときだけ入る。

**`failedStages`の`error`は、段ごとに決まった短い要約で、`pg_restore`などの誤りの文面は載せない。** `pg_restore`の誤りには戻そうとした行の値(利用者の秘密鍵やtokenの一部)が含まれうるので、外の通知先へは送らない。詳しい誤りは保存先の`verify.json`とdaemonのログで見る。`mismatches`(表の名前と行数)は載せる。

`format: discord`は`{"content": "<text>"}`を、`format: slack`は`{"text": "<text>"}`を送る(DiscordのWebhook、SlackのIncoming Webhook)。Discordでは、誤りの文面に含まれる`@everyone`などで一斉通知が起きないよう、mentionを全て無効にして送る。長い文面は、各サービスの上限(Discordは2000文字、Slackは3000文字で切る)に収まるよう切る。

送信に失敗したら2回まで送り直す。リダイレクトはたどらない。送れなかったことはログと`GET /status`の`lastNotifyError`に残る。

#### 通知先への通信

通知は、本体の外向きの通信と同じ`internal/safehttp`の経路で送る。`proxy` / `proxyBypassHosts` / `outgoingAddress` / `outgoingAddressFamily`が効き、**プライベートなアドレスへは送らない**。同じLANの通知先(自前のntfyなど)へ送るときは、本体と同じく`allowedPrivateNetworks`にそのアドレスを書いて許す。

### 制御API

本体の管理画面(#3462)が使う口。`schedule.listen`で待ち受ける。**TLSを持たないので、本体からだけ届く場所で待ち受ける。** composeでは、同梱の`backup-daemon`サービスが`ports`を持たないので、`listen: ":3010"`でもcomposeの内部のネットワークからしか届かない(`ports`を足さない)。バイナリを直接実行するときは、`:3010`と書くと全てのインターフェースで平文の口が開くので、`127.0.0.1:3010`のように絞る(下の「バイナリ直接実行」)。本体の`backup.server.serviceUrl`には`http://backup-daemon:3010`(`listen`のport)を書く。

全てのリクエストに`Authorization: Bearer <server.serviceToken>`が要る。tokenは定数時間で比べる。一致しなければ、パスに関わらず`401 {"error":"unauthorized"}`を返す。

| メソッドとパス | 本文 | 応答 |
|---|---|---|
| `POST /take` | 無し | `202 {"job": Job}`。走っている作業があれば`409 {"error":"busy","running": Job}` |
| `POST /verify` | `{"id": "<世代のID>"}` | `202 {"job": Job}`。IDの形が違えば`400 {"error":"invalid_id"}`、その世代の`meta.json`が無ければ`404 {"error":"no_such_generation"}`、保存先に届かなければ`502 {"error":"storage_error"}`、走っている作業があれば`409` |
| `GET /status` | 無し | `200 Status` |

daemonが止まりかけているときは、`POST`に`503 {"error":"not_running"}`を返す。`POST /take`は、定期実行と同じ流れ(取る → 設定なら確かめる → 整理する)を1回走らせる。作業は非同期で、結果は`GET /status`の`lastTake` / `lastVerify`で見る。

`Job`と`Status`の形は次のとおり。時刻はRFC 3339(UTC)。

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
- `lastTake` / `lastVerify`は、最後に終わった作業の結果。`ok`は全ての段が通り、検証(走らせたなら)にも通ったとき`true`。検証に通っても後始末に失敗したときは`false`で、`stage`が`verify`、`error`に後始末の誤りが入る。`stage`は失敗した段(`take` / `verify` / `prune`)、`verify`は検証の結果(`verify.json`と同じ形)、`deleted`は整理で消した世代
- `latestUsable`は、最新の使える世代
- `overdue`は、遅れの状態にあるとき`true`
- `lastNotifyError`は、最後の通知が送れなかったときだけ入る(送れたら消える)
- この状態はプロセスのメモリにだけあり、再起動すると`lastTake` / `lastVerify`は`null`に戻る。世代の一覧は保存先から読む

#### 接続元IPのヘッダー

本体は、管理画面を操作した人の接続元IPを`X-Elythia-Client-IP`で渡す。daemonは、**tokenが一致したリクエストのときだけ**このヘッダーを読み、ログ(`backup: control API request`の`client_ip=`)に残す。IPとして読めない値は捨てる。tokenが一致しないリクエストは、ヘッダーを読む前に401で返す。

### 動かし方

#### compose(TCP)

`docker-compose.yml`の`backup-daemon`サービスを使う。`backup`サービスと同じimage・設定ファイル・`volumes`(YAMLのanchorで共有している)で、`elythia backup daemon`を常駐させる。`profiles: ["backup-daemon"]`に入れてあるので、profileを有効にしたときだけ起動する。

```bash
# imageを作る(初回と、Elythiaを更新したとき)
MKGO_COMMIT=$(git rev-parse --short HEAD) docker compose --profile backup-daemon build backup-daemon
# 起動する(--no-deps を付ける)
docker compose --profile backup-daemon up -d --no-deps backup-daemon
# ログ(次に取る時刻など)を見る
docker compose --profile backup-daemon logs backup-daemon
```

- **`backup`サービスを`up`で常駐させず、別のサービスにしている。** `backup`は`run --rm`で呼ぶ1回きりのもので`restart: "no"`だが、常駐には`restart: unless-stopped`が要るため。名前が固定なので、本体の`backup.server.serviceUrl`に`http://backup-daemon:3010`と書ける
- **`--no-deps`を付ける。** `backup-daemon`は`backup`と同じく`depends_on`を持たない。`depends_on`があると、`up`がDBのコンテナの定義の差分を見て、動いているDBを作り直しうるため。DBの起動は、daemonが取る前に待つ(上の「1回の流れ」)
- 普段の`docker compose up -d`にも含めたいときは、`.env`に`COMPOSE_PROFILES=backup-daemon`を書く。書かないと、Elythiaを更新したときの`up -d`で`backup-daemon`が作り直されず、古いimageのまま動き続ける
- 保存先をディレクトリにするときと、暗号化した世代を確かめるとき(`verify: true`)は、`backup`サービスの`volumes`のコメントを外す。`backup-daemon`にも同じものが入る
- `TZ`は渡していない(`at`はUTC)。変えるときは`backup-daemon`の`environment`のコメントを外す(上の「時刻とタイムゾーン」)

#### UDS

`compose.uds.yaml.example`の`backup-daemon`サービスを使う(`compose.uds.yaml`に複製しているなら、同じ節を足す)。`mkgo`と同じ設定ファイルを読み、`pg_sock`のvolumeを通してDBへ繋ぐ。制御APIは`mkgo-uds`のネットワークで待ち受けるので、`mkgo`から`http://backup-daemon:3010`で届く。

```bash
MKGO_COMMIT=$(git rev-parse --short HEAD) docker compose -f compose.uds.yaml --profile backup-daemon build backup-daemon
docker compose -f compose.uds.yaml --profile backup-daemon up -d --no-deps backup-daemon
```

#### バイナリ直接実行

[取る](#バイナリ直接実行)・[確かめる](#バイナリ直接実行-1)ときと同じく、DBのサーバーと同じ版の`pg_dump` / `initdb` / `pg_ctl` / `pg_restore`がPATHにあるホストで、リポジトリ(または`migration/`のある場所)を作業ディレクトリにして呼ぶ。

```bash
TZ=Asia/Tokyo elythia backup daemon -config .config/default.yml
```

**`schedule.listen`はループバック(`127.0.0.1:3010`)に絞る。** 制御APIは平文で、`:3010`のように書くと全てのインターフェースで待ち受ける。tokenが要るとはいえ、tokenが平文で流れる。本体が別のホストにあって絞れないときは、TLSを終端するリバースプロキシかVPNの内側に置く。`.config/default.yml.example`の例は`127.0.0.1:3010`にしてある。

systemdで常駐させるときは、`Restart=on-failure`を付ける。`SIGINT` / `SIGTERM`で止まり、走っている作業には中断を頼む(中断した世代は揃っていない世代として残り、後の整理で消える)。

### 動いているかを確かめる

- 起動のログの`next=`で、次に取る時刻を見る
- 制御APIを有効にしていれば、`GET /status`で状態を見る。portを公開していないのでホストからは届かない。コンテナの中から叩く(バックアップ用のimageには`curl`が無く、busyboxの`wget`がある)

```bash
docker compose --profile backup-daemon exec backup-daemon \
  wget -qO- --header "Authorization: Bearer <serviceToken>" http://127.0.0.1:3010/status
```

- 通知が届くかは、`notify.webhookUrl`を設定した上で、一時的に保存先を読めない値にして`POST /take`を頼むと、`failure`が届く

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

## 管理画面(#3462)

管理画面の「情報 → バックアップ」で、世代の一覧と保存先の使用量を見て、取る・確かめる・消す・ダウンロードができる。本体は設定ファイルの`backup:`を読んで保存先を見る(`backup.server.storage.type`が空なら`backup.storage`)。取る・確かめるは、本体のimageに`pg_dump`が無いので、`backup.server.serviceUrl`の`elythia backup daemon`([定期実行](#定期実行)の[制御API](#制御api))に頼む。

本体が保存先を開けないとき(設定が無い、ディレクトリに目印が無いなど)も、本体は起動する。管理画面は「保存先が設定されていない」と出し、理由は本体のログ(`backup: storage for the admin page is unavailable`)に残る。**ディレクトリの保存先では、本体のコンテナにも同じパスでmountし、グループを共有する**([ディレクトリ](#ディレクトリ))。目印`.elythia-backup`が見えないと開けない。

`backup.server.serviceUrl`を書いても`backup.server.serviceToken`が空なら、本体は依頼を送らず、取る・確かめるを使えないものとして扱う(起動時に警告をログに出す)。制御APIへの依頼は、`HTTP_PROXY`などの環境変数があってもproxyを通さない。

本体に渡す鍵は、`backup.server.storage`でバックアップ用のサービスとは別にできる。本体が保存先に対して行うのは、一覧・読み取り(`meta.json` / `verify.json`と、署名付きURL)・削除だけで、書き込みはしない。

### 認証

バックアップには利用者の秘密鍵(`user_keypair`)・token・パスワードのhashが入るので、**閲覧を含む全ての操作**に次を課す。1つでも欠けると拒否する。

- 管理者であること。モデレーターは使えない
- ブラウザでログインしたtokenであること。アプリやAPIのtokenでは使えない
- 二段階認証(TOTP)かパスキーを登録していること。未登録なら画面が登録を案内する
- 操作のたびに、パスワードと、TOTPのコード(またはバックアップコード)かパスキーで再認証すること

再認証の失敗は、パスワードと2つ目の要素のどちらの失敗も`passwordguard`で数える(アカウントと接続元の範囲の組ごとに1時間10回、アカウント全体で1時間100回。`i/*`でパスワードを確かめる操作と同じ枠で、合わせて数える。サインインはこの枠を使わない)。Redisに繋がらないときは照合できないので拒否する(503)。TOTPのコードは一度使うと記録が残っている間(既定で120秒)使えないので、続けて操作するときは次のコードを待つか、パスキーを使う。パスワードが合っていてコードを使い回しただけのときは、失敗として数えない。

この例外のため、直前に使われたTOTPのコードを知っている者は、応答(`TWO_FACTOR_CODE_ALREADY_USED`か`REAUTHENTICATION_FAILED`か)でパスワードの当否を知れる。ただし、パスワードが違えば失敗として数えるので、上の枠を超えて試すことはできない。

パスキーのchallengeは、サインインのものとは別に置く。サインインのために出したchallengeへの応答は、ここでは通らない。challengeを置いたRedisに繋がらないときは、照合できなかったものとして拒否し(503)、失敗には数えない。

成立した操作は、モデレーションログに残す(`listBackups` / `takeBackup` / `verifyBackup` / `deleteBackup` / `downloadBackup`)。ログは操作の応答と別に書くので、書き込みに失敗しても操作は取り消されず、本体のログに警告(`moderation log: write failed`など)だけが残る。

### 一覧と使用量

- 一覧は世代ごとに、取った日時、大きさ(世代の全てのファイルの合計)、暗号化の有無、検証の結果、Elythiaの版、管理表の番号を出す。揃っていない世代(`meta.json`が無い・読めない、または`meta.json`が名指しするdumpが無いもの)も「未完成」として出す。容量を使っているので、要らなければ消す
- 使用量は、保存先の接頭辞(S3の`prefix`、ディレクトリの`path`)の下にある全てのファイルの合計。世代の外のファイルも数える。ディレクトリの目印`.elythia-backup`と、書きかけの一時ファイル(`.tmp-`で始まる名前)は数えない
- 次の定期実行の予定と、前回の取得・検証の結果は、daemonの`GET /status`から出す。daemonの状態はメモリにだけあるので、daemonを再起動すると消える

### 消す

世代の`meta.json`を最初に消してから、残りのファイルを消す。途中で失敗しても、残った世代は「未完成」として出るので、戻せる世代に見えることはない。

### ダウンロード

- S3互換の保存先: 5分だけ有効な署名付きURLを出す。ブラウザは保存先から直接ダウンロードする
- ディレクトリの保存先: 本体の`/backup-download?token=<token>`から渡す。tokenは5分だけ有効で、期限内なら途切れたダウンロードを再開できる(Range)。tokenは本体のアクセスログでは伏せられ、実際に取られたことは本体のログ(`admin/backup: backup downloaded`)に残る。前に置いたnginxでは、同梱の設定はアクセスログにqueryを出さないが、**エラーになったときの`error_log`の行にはtokenが残る**(nginxに伏せる手段が無い。[逆プロキシ](deployment.md#逆プロキシ-nginx))。数GBのdumpを返すので、nginxでは`/backup-download`の`proxy_buffering`を切る(同梱の`deploy/uds/nginx/mkgo.conf`と、[逆プロキシ](deployment.md#逆プロキシ-nginx)の例に入れてある)

どちらも`<世代ID>-<ファイル名>`(例: `20261010T040000Z-dump.pgc`)の名前で保存される。暗号化した世代は、暗号化したまま渡す。戻すにはageの秘密鍵が要る。

### 上げるときにすること

- **ディレクトリの保存先を管理画面から使うときは、[ディレクトリ](#ディレクトリ)の手順でグループを揃える。** 以前の版の`elythia backup`は`0700` / `0600`で作っていたので、そのままでは本体から読めない(一覧が500になる)
- **composeのファイルを手で直す。** `compose.uds.yaml`(UDS)は`compose.uds.yaml.example`から複製したもので、gitignoreしてあるので`git pull`では変わらない。`docker-compose.yml`も、手元で変えていれば同じ。exampleに入った次の2つを写す
  - `backup`サービスに`group_add`(本体のGID。UDSは`"65532"`、compose(TCP)は`"991"`)
  - 本体(UDSは`mkgo`、compose(TCP)は`app`)の`volumes`に、`backup`サービスと同じ保存先のmount(例: `- /mnt/nas/elythia-backup:/backup`)

  写した後は、本体を作り直す(`docker compose -f compose.uds.yaml up -d mkgo`)。`backup`サービスは`run`のたびに作られるので、作り直しは要らない
- **nginxの設定に`/backup-download`の節を足す。** UDSの`deploy/uds/nginx/mkgo.conf`は取り込めば入るが、設定はnginxの起動時に読むので、nginxのコンテナを再起動する(`docker compose -f compose.uds.yaml restart nginx`)。自分で書いた設定には、[逆プロキシ](deployment.md#逆プロキシ-nginx)の例を写す

## 戻す

`elythia backup restore`で、世代からDBを戻す(#3461)。手順は[デプロイ](deployment.md)の次の節にある。

- [バックアップから戻す](deployment.md#バックアップから戻す-restore): 同じサーバーで今のDBと入れ替える形(`-mode swap`)と、空のDBへ戻す形(`-mode empty`)。戻す前に止まる条件、Redisの後始末(`-mode empty`では既定で行わない)、連合への影響、ドライブのファイルが戻らないこと、元に戻す手順(`-rollback`)
- [PostgreSQL 16 → 18 への移行](deployment.md#postgresql-16--18-への移行-既存環境): 最後の世代を取り、新しい版で確かめ、新しいvolumeのサーバーの空のDBへ戻す

`backup`サービスでは、取るときと同じく`run`で呼ぶ。

```bash
docker compose run --rm --no-deps backup restore -id latest -mode swap -confirm <DB名>
```

戻すときは、メタ情報のうち次のものを使う。

| メタ情報 | 使い方 |
|---|---|
| `dumpSize` / `dumpSha256` / `plainSha256` | 落としたdumpと突き合わせる。食い違えば何も書かずに止まる |
| `migrations` | 同梱のmigrationより新しい番号か、dirtyなら止まる |
| `rowCounts` | 戻した後の行数と突き合わせる |
| `databaseSettings` | 戻した後に`ALTER DATABASE ... SET`で入れ直す |
| `databaseLocale` | `-mode swap`では、この値でDBを作る。`-mode empty`では、戻す先のDBと違えば止まる |
| `extensions` | 戻す先のサーバーに無ければ止まる |
