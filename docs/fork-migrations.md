# forkの独自migration(`migration/local/`)

Elythiaをforkして、本体の表(`user`、`note`など)を変える独自のmigrationを足すときの手順と決まりです(#3428)。本体のmigrationとは別の系列として、別のディレクトリと別の管理表で流します。

Elythia本体は`migration/local/`を同梱しません。ディレクトリが無いか空なら、この仕組みは何もしません。

## プラグインのmigrationとの使い分け

| 足したいもの | 使うもの |
|---|---|
| プラグインだけが使う表 | プラグインのmigration(`plugin.Migrations`。プラグインごとのschemaに入る。[プラグインの書き方](plugins/authoring.md)) |
| 本体の表への列・index・制約、本体のschemaに置く表 | この文書の`migration/local/` |

プラグインから本体の表を触れるようにはしません。プラグインの境界(#2476)を崩すためです。

## 置き場所と形式

- `migration/local/`に置く。形式は本体の`migration/`と同じ`NNNNNN_name.up.sql` / `NNNNNN_name.down.sql`
- **番号は6桁固定にする。** テスト用のDBへ流す`testutil.ApplyMigrations`はファイル名の辞書順で流すので、桁数が揃っていないと順序が狂う
- 番号は本体の番号と重なってよい。系列ごとに別の管理表で数えるため。`000001`から始めても、`900001`のように飛んだ番号から始めてもよい
- **新しいmigrationは、必ずその系列の最大の番号より大きくする。** golang-migrateは管理表に最後のversionを1行だけ持ち、それより小さい番号は同じ系列の中でもエラーを出さずに飛ばす。doctorも最大の番号としか比べないので検出できない
- `make migrate-create-local`が、`migration/local/`の**最大の番号+1**でup / downの空ファイルを作る(本体の`make migrate-create`は本数+1で、`migration/`だけを数える)
- downも必ず書く。データが失われるdownには`-- data loss:`で明記する(本体と同じ規約)
- imageへは、`migration/`をディレクトリごとCOPYする既存の手順がそのまま`migration/local/`を運ぶ

## 流し方

```bash
make migrate-up                                  # 本体 → fork の系列の順に最新まで
go run ./cmd/elythia migrate -direction up        # 同じ
go run ./cmd/elythia migrate -direction up -track local   # fork の系列だけ
```

- `-direction up`は、本体の系列(`migration/`)を流した後に、forkの系列(`migration/local/`)を流す。**本体の系列が失敗したら、forkの系列は流さない**
- `-track core` / `-track local`で片方だけにできる。`-steps`を付けるときは`-track`が必須(段数は1つの系列の中で数える)
- migrationの結果のログには`track=core` / `track=local`が付く。forkの系列が無ければ`no local migrations; skipping`を1行出して終わる

## 管理表を分ける理由

forkの系列は、golang-migrateの`x-migrations-table`で別の管理表`schema_migrations_local`に記録します。本体の`schema_migrations`には触れません。

golang-migrateの管理表は、**最後に当てたversionを1行だけ**持ちます。適用した番号の一覧は持たず、`Up`は今のversionより大きいものだけを当てます。forkのmigrationを本体と同じ`migration/`に置き、同じ管理表で流すと、次の順で本体のmigrationが**エラーも出ずに適用されません**。

1. forkが`migration/900001_fork_xxx`を足して適用する(管理表は`900001`)
2. 本体が`000117_...`を足す
3. forkが本体を取り込んでmigrateを流す。`000117`は`900001`より小さいので飛ばされる

ディレクトリだけを分けて管理表を共有した場合は、本体の系列が自分の知らないversion(`900001`)に当たって`no migration found for version 900001`で止まります。どちらも、ディレクトリと管理表の両方を分けることで起きなくなります(`internal/cli/migrate`の`TestMigrateDB_CoreAddedAfterLocalStillApplies`が、local適用後に足した本体のmigrationが当たることを確かめている)。

既に本体の`migration/`へ混ぜてしまったforkの移し方は、[本体のmigrationに混ぜてしまった場合](#本体のmigrationに混ぜてしまった場合)にあります。

## 名前の決まり

**forkが作るオブジェクトの名前には、fork固有の接頭辞を付ける。** 表・列・index・制約・enum型・関数のどれも対象です。例: forkの名前が`acme`なら、表は`acme_badge`、本体の表に足す列は`"acmeBadgeCount"`、indexは`"IDX_acme_badge_userId"`。

本体が後の版で同じ名前のオブジェクトを足すと、次のどれかが起きます。

- 本体のmigrationが`IF NOT EXISTS`でno-opになり、**forkが作った別の型・制約の列がそのまま残る**。本体のコードはその列を本体の定義だと思って読み書きする
- 本体のmigrationが`already exists`で落ち、本体の版上げが止まる
- 本体のdownが、forkの列を自分のものとして落とす

接頭辞は、本体(と、本体が追従しているMisskey)が使わない語にします。

## 順序の注意

- **forkの系列は、いつも本体の系列の後に流れる。** 新しくDBを作るときは「本体を全部 → forkを全部」の順になり、forkのmigrationを書いた時点の順序(本体の途中の版に対して当てた)とは違う。forkのmigrationは、**本体を最新まで当てた後のschema**に対して書く
- 本体の後の版が、forkのmigrationが触る表や列を変えたり消したりすると、新しく作るDBでforkのmigrationが落ちる。本体を取り込むたびに、空のDBに`make migrate-up`が通ることを確かめる
- 冪等に書く(`IF NOT EXISTS` / `IF EXISTS`、enumは`EXCEPTION WHEN duplicate_object`)。途中で失敗したときに手当てしやすくなる

## 戻し方

`-direction down`には`-track`が**必須**です。省くと、DBに繋ぐ前に終了コード2で止まります。

```bash
make migrate-down-local    # fork の系列を 1 段戻す (-track local -steps 1)
make migrate-down          # 本体の系列を 1 段戻す (-track core -steps 1)
```

- **`-steps`を省くと、その系列を全部戻す。** 本体の系列なら全テーブルが消える
- **本体の系列を戻す前に、それに依存するforkのmigrationを戻す。** 本体のdownは`DROP TABLE ... CASCADE`を使うものがあり、forkが足した列やindexも一緒に消える。そのとき`schema_migrations_local`は進んだままなので、次の`migrate-up`はforkのmigrationを当て直さず、doctorも検出しない
- forkのdownで`schema_migrations_local`を消さない。golang-migrateが最後にこの表を書き換えるので、消すとdownが失敗する(本体の`000001`のdownが`schema_migrations`を残すのと同じ理由)
- forkの系列が無いのに`-direction down -track local`を指定すると、終了コード1で止まる(作業ディレクトリの取り違えに気付けるように)

## doctor

`elythia doctor`の`database`の検査は、`migration/local/`にup migrationがあるときだけ`schema_migrations_local`も見ます。

- 表が無い、dirty、または記録されたversionが同梱の最大の番号より小さいとFAIL
- 本体の系列は本数と比べるが、forkの系列は**最大の番号**と比べる(飛んだ番号から始めてよいため)。最大より小さい番号を後から足して飛ばされたものは検出できない
- 問題が無ければ、結果に`local migration version N`が付く

## 本体のmigrationに混ぜてしまった場合

`migration/local/`ができる前(#3428より前)に、独自のmigrationを本体の`migration/`へ足していたforkは、ファイルを`migration/local/`へ動かすだけでは移れません。管理表`schema_migrations`がforkの番号を指したまま残るので、本体のmigrationが流れなかったり、forkのmigrationがもう一度流れようとしたりします(#3453)。

この節の手順は、forkのファイルを`migration/local/`へ移し、2つの管理表を手で書き換えて、本体とforkのmigrationがどちらも1回ずつ当たった状態にします。管理表は`elythia migrate -force`ではなくSQLで書き換えます。2つの管理表を1つのtransactionで書き換えるためです。`-force`(migrationを流さずに管理表のversionだけを書き換える操作、#3455)は1回に1つの系列しか書き換えないので、2回に分けると、間で止まったときに片方だけが書き換わります。

### 混ぜ方による違い

| 混ぜ方 | 例 | 起きていること |
|---|---|---|
| 1. 番号を飛ばした | 本体が`000116`のときに、forkが`900001`から足した | 管理表が`900001`を指すので、本体が後で足した`000117`以降は**エラーも出ずに適用されていない**。`elythia doctor`もokを返す(本体の系列は、versionが同梱の本数以上ならokとみなすため) |
| 2. 本体の続きの番号を使った | forkが`000117`を足した後に、本体も`000117`を出した | ファイル名が違えばgitでは衝突せず、同じ番号のファイルが2つ並ぶ。`elythia migrate`は`duplicate migration file`で止まる。管理表の上では`000117`が当たっているので、forkのファイルを動かしても本体の`000117`は流れない |
| 3. 本体と衝突する前に気付いた | forkが`000117`を足し、本体はまだ`000117`を出していない | ファイルを移して管理表を直すだけで済む |

どの場合も、以下の共通の手順で直します。場合ごとの違いは、書き換える値の決め方と、手順の前に済ませることだけです。

### 共通の手順

#### 1. 止めて、バックアップを取る

- **`elythia serve`を動かしているプロセスを全て止める。** queueのworkerだけを動かしているプロセスも含む。本体のmigrationが当たっていないschemaに新しい版のコードが書き込むのを防ぐため
- **作業が終わるまで、誰も`elythia migrate`を流さないようにする。** 書き換えの途中で流れると、半端な管理表のまま流れてしまう。同梱の`docker-compose.yml`では`app`が`migrate`サービスに依存しているので、`app`を起動すると先に`migrate`が流れる
- バックアップを取る。この手順は管理表を手で書き換えるので、間違えたときに戻せるのはバックアップだけ

```bash
# ホストから繋ぐとき
pg_dump -h <ホスト> -p <ポート> -U <ユーザー> -Fc -f before-fork-migration-move.dump <DB名>
# 同梱の compose で、DB が db サービスのとき
docker compose exec -T db pg_dump -U <ユーザー> -Fc <DB名> > before-fork-migration-move.dump
```

戻すときは、サーバーを止めたまま、**DBを作り直してから**戻します。`pg_restore --clean`で上書きすると、消すのはバックアップにあるものだけなので、バックアップの後に作った`schema_migrations_local`などが残ります。`--create`を付けずに取ったバックアップは、`ALTER DATABASE ... SET`で入れたDBの設定を含みません。そうした設定があるなら(`psql`の`\drds`で見られる)、作り直した後に入れ直します。

```bash
dropdb -h <ホスト> -p <ポート> -U <ユーザー> <DB名>
createdb -h <ホスト> -p <ポート> -U <ユーザー> -O <DBの所有者> <DB名>
pg_restore -h <ホスト> -p <ポート> -U <ユーザー> -d <DB名> before-fork-migration-move.dump
# 同梱の compose では、それぞれ docker compose exec -T db を前に付ける (pg_restore は < でファイルを渡す)
docker compose exec -T db pg_restore -U <ユーザー> -d <DB名> < before-fork-migration-move.dump
```

#### 2. 今の状態を調べる

管理表の値を見ます。`dirty`が`t`なら、前回のmigrationが途中で止まっています。この手順に入る前に、そちらを手当てしてください。

```sql
SELECT version, dirty FROM schema_migrations;
```

forkが足したファイルを洗い出します。**先に本体を`git fetch`します。** 古い参照と比べると、既に取り込んだ本体のファイルまでforkのものに見え、それを移すと本体の系列が壊れます。`upstream/develop`は、取り込んでいる本体のremoteとブランチに読み替えてください。

```bash
git fetch upstream
base=$(git merge-base HEAD upstream/develop)
git diff --diff-filter=A --name-only "$base" -- migration/ ':!migration/local/'
```

表示されたものがforkのファイルの候補です。**候補が本体に無いことを1つずつ確かめます。** 次のコマンドが何か表示したら、それは本体のファイルなので移しません。

```bash
for f in $(git diff --diff-filter=A --name-only "$base" -- migration/ ':!migration/local/'); do
  git cat-file -e "upstream/develop:$f" 2>/dev/null && echo "本体にある: $f"
done
```

**この節の手順を使えるのは、forkが本体のmigrationのファイルを消したり、名前を変えたり、書き換えたりしたことが無いときだけです。** そうしたことがあると、本体のmigrationが番号の途中で抜けたまま後ろが当たっていることがあり、下の`C`の決め方では見分けられません。次の2つを確かめます。

```bash
# 1. 今の migration/ が、本体と比べて「足しただけ」か。A 以外の行 (D / R / M など) が出たら範囲外
git diff --name-status "$base" -- migration/ ':!migration/local/'

# 2. forkの履歴で、本体のファイルを消したり名前を変えたりしたことが無いか。何か表示したら範囲外
#    (後で戻したファイルでも、消していた間に後ろの番号が当たっていれば抜けている)
git log -m --diff-filter=DR --name-status --format= upstream/develop..HEAD -- migration/ |
  awk '{print $2}' | sort -u |
  while read -r f; do git cat-file -e "upstream/develop:$f" 2>/dev/null && echo "本体のファイルを消したか名前を変えた: $f"; done
```

1.で`A`以外の行が出たとき、2.で何か表示したとき、または履歴を書き換えたせいで2.では見えないが覚えがあるときは、この節の範囲外です。ここで作業を止めて相談してください(まだ何も書き換えていないので、戻すものはありません)。

**どのmigrationが当たっているかは、作られるはずのオブジェクトで確かめます。管理表のversionは信用しません。** 管理表は最後のversionを1行持つだけで、それより小さい番号が当たったかどうかを記録していません。

- 場合1では、versionはforkの番号を指していて、その下にある本体の番号(`000117`など)が当たっているかを何も表さない
- 場合2では、versionの番号のファイルが本体とforkに1つずつあり、どちらが当たったのかを表さない

各migrationのup.sqlを読み、作るはずの表・列・indexがあるかを見ます。`elythia`が使うのと同じユーザー・同じDBで繋ぎ、名前は自分のschemaの中で探します(`to_regclass`に名前だけを渡すと、`search_path`の全てのschemaを探してしまう)。

```sql
-- 表と index (無ければ NULL)
SELECT to_regclass(format('%I.%I', current_schema(), 'acme_badge')),
       to_regclass(format('%I.%I', current_schema(), 'IDX_instance_faviconUrl'));

-- 列
SELECT table_name, column_name FROM information_schema.columns
WHERE table_schema = current_schema() AND table_name = 'user' AND column_name = 'acmeBadgeCount';
```

調べた結果から、書き換える値を2つ決めます。

- **C**: 本体の`000001`から`C`までが**全て**当たっている、最大の番号。`schema_migrations`に書く
- **L**: forkのmigrationのうち、当たっている最後のもの。番号は、次の手順で付け直した後の番号で数える。`schema_migrations_local`に書く

**`C`と`L`は、推測せず正確に決めます。** どちらにずれても壊れます。

- **小さすぎると、当たっているmigrationがもう一度流れる。** 本体のmigrationは、もう一度流してよいようには書かれていない。`internal/entitycompat/migration_idempotency_test.go`が見ているのは、DDLの6つの形(`CREATE TABLE` / `ADD COLUMN` / `CREATE INDEX`に`IF NOT EXISTS`、`DROP TABLE` / `DROP COLUMN` / `DROP INDEX`に`IF EXISTS`)だけで、データを書き換えるmigrationは多い。例えば、全部当たったDBで`C`を`74`にすると`000075`が`column "contactHost" does not exist`で落ちて管理表がdirtyになり、`76`にすると`000077`の`DELETE FROM "signup_application"`がもう一度流れて**未処理の登録申請が全て消える**(doctorはokを返す)
- **大きすぎると、当たっていないmigrationがエラーも出ずに飛ばされる。** doctorでは検出できず、オブジェクトを調べるまで気付けない
- forkのmigrationも冪等とは限らず、当たっているものをもう一度流すと`already exists`で落ちる

`C`は次のように決めます。

1. **本体の`000001`から順に、全ての本体のmigrationの効果を確かめる。途中を飛ばしません。** gitの履歴から「この番号までは当たっているはず」と推して省くことはしません。forkが本体の上へrebase・cherry-pick・squashすると、forkのmigrationを足したcommitが作り直され、実際に`elythia migrate`を流した版と合わなくなるためです。効果は、up.sqlを読んで、作る表・列・index・型があるか、落とすものが無いか、書き換えるデータが書き換えた後の形になっているかで見ます
2. **`000001`から途切れずに効果が続く範囲の、最後の番号が`C`です**
3. **`C`より大きい本体のmigrationも、全て確かめます。** 1つでも効果があれば(飛び地)、この手順では直せません。手順5でそれがもう一度流れるためです。作業を止めて、バックアップから戻して相談してください。飛び地は、例えば同じ番号の衝突をforkの側に寄せて解いた(`--ours`)ときや、重複のエラーを避けるために本体の`000117`を消したり名前を変えたりした後に`000118`以降が当たったときにできます
4. **効果で見分けられないmigrationは、前後で決めます。** データを書き換えるだけで、書き換えた跡が残らないもの(書き換える行がもともと無かったものを含む)が該当します。前後のmigrationに効果があって間に挟まれているなら、当たっているものとして数えます。golang-migrateは番号の順に1つずつ当てるので、間だけが当たっていないのは、3.の飛び地と同じ特別な経緯があったときだけです。ただし、**forkのファイルと同じ番号の本体のmigrationは、挟まれていても当たったものとして数えません**(forkのファイルの方が当たって、本体のものは飛ばされていることがあるため)
5. **境界(`C`の候補のすぐ上)に効果で見分けられないmigrationが来たら、推測で決めません。** 抜け道は1つだけです。そのmigrationが`WHERE`で絞った`UPDATE`だけでできていて、その`WHERE`を今のDBに`SELECT count(*)`で流して0件なら、もう一度流しても何も変わらないので、そのmigration**1つだけ**を当たっていないものとして(`C`を小さい側に)数えてかまいません。手順5で最初に流れるのがそのmigrationなので、流れるときのDBは今と同じです。例えば`000084` / `000085` / `000116`は`meta`の`repositoryUrl` / `feedbackUrl`を、特定の値の行だけ書き換える`UPDATE`です。**DDLを含むもの、`WHERE`の無い`UPDATE` / `DELETE`(`000077`の`DELETE FROM "signup_application"`など)、2つ以上続くもの**には使えません。どの版の`elythia migrate`をいつ流したかの記録(`/api/meta`の`mkGoCommit`など)は裏付けには使えますが、確かめるのを省く理由にはしません。決められなければ、作業を止めてバックアップから戻し、相談してください

   ```sql
   -- 000116 の例: 2 つの UPDATE の WHERE をそのまま数える。どちらも 0 なら、もう一度流しても何も変わらない
   SELECT count(*) FROM "meta" WHERE "repositoryUrl" = 'https://github.com/shiroha-a/mk';
   SELECT count(*) FROM "meta" WHERE "feedbackUrl" = 'https://github.com/shiroha-a/mk/issues/new';
   ```

表・列・index・型の有無は、本体のmigrationだけを最新まで当てた使い捨てのDBと`pg_dump --schema-only`の出力を比べると、まとめて見られます。差に出たオブジェクトから、それを作る本体のmigrationを引きます。データだけを書き換えるmigrationは差に出ないので、1つずつ見ます。

**TS版Misskeyから移したDBでは、この差は手がかりにしか使えません。** 列の並び(`000029`などが`ADD COLUMN IF NOT EXISTS`で足す列)や、TS版だけが持つ表・indexが差に出ます。逆に、本体のmigrationが作るのと同じ名前のものをTS版が既に作っていることもあり(`000083`など)、差に出ないことがそのmigrationが当たった証拠になりません。オブジェクトごとに、上のカタログのSQLで確かめてください。

一度でもdirtyになったことがあるDBでは、`CREATE INDEX CONCURRENTLY`のmigration(`000054`など)が途中で止まり、使えない(INVALIDの)indexが残っていることがあります。`to_regclass`はそれも「ある」と返すので、次のSQLで残っていないかを見ます。

```sql
SELECT c.relname FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
WHERE c.relnamespace = to_regnamespace(current_schema()) AND NOT i.indisvalid;
```

使い捨てのDBは、forkの作業ツリーではなく、本体の`$base`(手順2の`git merge-base`)を取り出したworktreeから作ります。forkの作業ツリーの`migration/`にはforkのファイルが混ざっているので、`-track core`でもそれが当たってしまいます。

worktreeには`.config/`が無いので、`-config`には設定ファイルの絶対パスを渡します。

```bash
# 本体の migration だけを、使い捨ての DB に当てる
createdb -h <ホスト> -p <ポート> -U <ユーザー> <使い捨てのDB名>
git worktree add --detach ../elythia-core-ref "$base"
(cd ../elythia-core-ref && MK_DB_DB=<使い捨てのDB名> elythia migrate -config <設定ファイルの絶対パス> -direction up -track core)
pg_dump -h <ホスト> -p <ポート> -U <ユーザー> --schema-only --no-owner <使い捨てのDB名> > core-only.sql
pg_dump -h <ホスト> -p <ポート> -U <ユーザー> --schema-only --no-owner <DB名> > current.sql
# 同梱の compose では docker compose exec -T db pg_dump -U <ユーザー> --schema-only --no-owner <DB名> > current.sql
# (ホストの pg_dump がサーバーより古いと、pg_dump はダンプを拒む)
diff core-only.sql current.sql    # `\restrict` / `\unrestrict`の行は毎回違う鍵なので必ず差に出る。それ以外の差を全て見る
# データだけを書き換える本体の migration の候補 (本体の worktree の中で探す)
grep -liE '^[[:space:]]*(UPDATE|DELETE|INSERT)' ../elythia-core-ref/migration/*.up.sql
# 終わったら worktree と使い捨ての DB を片付ける
git worktree remove ../elythia-core-ref
dropdb -h <ホスト> -p <ポート> -U <ユーザー> <使い捨てのDB名>
```

`L`も、forkのmigrationを1つずつ、オブジェクトで確かめて決めます。

#### 3. forkのファイルを`migration/local/`へ移す

番号は`000001`から6桁で付け直します。**元の番号の順序を保ちます**(当てた順序と同じにするため)。up / downの両方を移します。

```bash
mkdir -p migration/local
git mv migration/900001_acme_badge.up.sql         migration/local/000001_acme_badge.up.sql
git mv migration/900001_acme_badge.down.sql       migration/local/000001_acme_badge.down.sql
git mv migration/900002_acme_badge_count.up.sql   migration/local/000002_acme_badge_count.up.sql
git mv migration/900002_acme_badge_count.down.sql migration/local/000002_acme_badge_count.down.sql
```

移した後の`migration/`は、本体と同じファイルだけになります。手順2の`git diff --name-status "$base" -- migration/ ':!migration/local/'`(作業ツリーと比べるので、commitする前でも効く)が何も出さなくなったことを確かめます。

#### 4. 管理表を書き換える

先に、`schema_migrations_local`が既にあるかを見ます。

```sql
SELECT to_regclass(format('%I.%I', current_schema(), 'schema_migrations_local'));
```

あれば、ファイルを移した後に誰かが`elythia migrate`を流しています。forkのmigrationがもう一度流れて途中で落ちた(dirtyの行が残る)かもしれないので、`SELECT version, dirty FROM schema_migrations_local;`で中身を見て、手順2のforkのオブジェクトを確かめ直してから進めます。下のSQLは、残っている行を消してから書き直します。

**2つの表を1つのtransactionで書き換えます。** 片方だけが書き換わった状態で止まると、次の`elythia migrate`がずれた位置から流れます。次の内容を`fix.sql`に書きます。`2`と`116`は例で、手順2で決めた`L`と`C`に置き換えます。

```sql
-- golang-migrate が作るのと同じ形 (1 行だけを持つ)
CREATE TABLE IF NOT EXISTS schema_migrations_local (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL);
DELETE FROM schema_migrations_local;
INSERT INTO schema_migrations_local (version, dirty) VALUES (2, false);   -- L
UPDATE schema_migrations SET version = 116, dirty = false;               -- C
```

`--single-transaction`で流すと、途中で失敗したときは全体が取り消されます。終了コードが0でなければ、何も書き換わっていません。

```bash
psql -h <ホスト> -p <ポート> -U <ユーザー> -v ON_ERROR_STOP=1 --single-transaction -f fix.sql <DB名>
# 同梱の compose で、DB が db サービスのとき
docker compose exec -T db psql -U <ユーザー> -v ON_ERROR_STOP=1 --single-transaction <DB名> < fix.sql
```

流し終わったら、書き換わったことを確かめます。

```sql
SELECT (SELECT version FROM schema_migrations) AS core,
       (SELECT version FROM schema_migrations_local) AS local;
```

forkのmigrationが1つも当たっていなければ(`L`が0)、`schema_migrations_local`は作りません(既にあれば消す)。次の`elythia migrate`が作って、全部を流します。

#### 5. 残りを流す

```bash
elythia migrate -direction up    # 開発環境では make migrate-up
```

本体の系列は`C`の次から最新まで、forkの系列は`L`の次から最新までを当てます。当てるものが無い系列は`no migration changes to apply`を出します。

#### 6. 確かめる

```bash
elythia doctor
```

`database`の行が`ok`で、`migration version <本体の最大の番号> / local migration version <forkの最大の番号>`になっていることを確かめます。あわせて、手順2で当たっていなかったmigrationのオブジェクトが、今はあることを確かめます。**doctorだけでは足りません。** 本体の系列は本数と比べるだけなので、場合1の状態でもokを返します。

確かめ終わったら、サーバーを起動します。

### 場合1: 番号を飛ばした

管理表はforkの最後の番号(例: `900002`)を指しています。

- **C**: 手順2の決め方のとおり、`000001`から確かめる。`900001`を当てた後に取り込んだ本体のmigrationは、番号が小さいので当たっていない。rebaseなどで履歴が作り直されていると、gitの上では`900001`より前からあったように見えるので、gitでは決めない
- **L**: 当たっているforkのファイルの数(付け直した後の番号)。forkの番号は管理表より小さいので、全部当たっていることが多い

例: 本体が`000116`のときにforkが`900001`と`900002`を当て、その後に本体の`000117`と`000118`を取り込んだ。`000001`〜`000115`の効果があり、`000117`の表が無く、`000118`の効果も無い。`000116`はデータだけを書き換えるので境界に来る。手順2の5.のSQLで旧URLの行を数え、残っていれば`000116`は当たっていない。0件なら、もう一度流しても何も変わらないので、小さい側に倒してよい。どちらでも`C = 115`、`L = 2`になる。手順5で本体の`000116`〜`000118`が流れ、forkの系列は何も流れない。

### 場合2: 本体の続きの番号を使った

**本体を取り込んだ後なら、先に本体とforkのファイルを分けます。**

- ファイル名が違えば、gitは衝突させずに両方を並べる(例: forkの`000117_badge.up.sql`と本体の`000117_core_badge.up.sql`)。手順2の`git diff`でforkのものを見分け、手順3で移す
- ファイル名まで同じなら、gitは追加同士の衝突(add/add)にする。`migration/`のファイルは本体の内容にし(`git merge`で本体を取り込んでいるなら`git checkout --theirs -- <ファイル>`)、forkの内容は手順3の移し先へ新しいファイルとして書く

forkの`000117`が当たっていて、本体の`000001`〜`000116`の効果があり、本体の`000117`以降の効果が無いなら、`C`は`116`です。管理表が`117`を指していても、それはforkのファイルの番号です。例えば本体が`000117`と`000118`を出した版を取り込んだ後の`elythia doctor`は、`適用済み version 117 / 同梱 119`と、同梱の本数(同じ番号の2つのファイルを別々に数える)より小さいとしてFAILを返します。

**次に、名前の衝突を確かめます。** forkが接頭辞を付けずに作った名前(例: 表`badge`)を、本体の同じ番号以降のmigrationも作ることがあります。表・index・主キーやUNIQUEの制約・sequence・型の名前は、schemaの中で1つしか持てません。関数は、名前と引数の型の組で1つです。本体のDDLは`IF NOT EXISTS`付きで、enumは`duplicate_object`を握りつぶし、関数は`CREATE OR REPLACE`で書くので、**衝突してもエラーにならず**、本体のmigrationは何もせずに(関数は上書きして)通ります。

- 表が衝突すると、forkの形の表が残る。本体の形を前提にした後続のmigration(例: 本体にだけある列へのindex)が`column ... does not exist`で落ちて管理表がdirtyになるか、本体のコードがforkの表を読み書きする
- indexが衝突すると、forkの表に付いたindexが残り、本体の表には**indexが張られない**。doctorはokを返す
- enumが衝突すると、forkの値を持つ型が残る
- 関数が名前と引数の型まで衝突すると、本体の`CREATE OR REPLACE FUNCTION`がforkの関数を黙って上書きする

本体の新しいmigrationのup.sqlと、forkのmigrationが作るものの名前を突き合わせてください。forkのmigrationが作ったものは、up.sqlに書いた名前のほかに、自動で名前が付いたもの(主キーの`badge_pkey`、外部キーの`badge_userId_fkey`、serialの列のsequence)もあります。次のSQLで洗い出します。

```sql
-- 表に付いている index (主キー・UNIQUE 制約の index を含む)
SELECT indexname FROM pg_indexes
WHERE schemaname = current_schema() AND tablename = 'badge';

-- 表の制約 (p: 主キー、u: UNIQUE、f: 外部キー、c: CHECK)
SELECT conname, contype FROM pg_constraint
WHERE conrelid = to_regclass(format('%I.%I', current_schema(), 'badge'));

-- 表に属する sequence (serial / identity の列)
SELECT c.relname FROM pg_class c JOIN pg_depend d ON d.objid = c.oid
WHERE c.relkind = 'S' AND d.refobjid = to_regclass(format('%I.%I', current_schema(), 'badge'));

-- enum などの型と関数 (名前は fork の up.sql から拾う)
SELECT typname FROM pg_type
WHERE typnamespace = to_regnamespace(current_schema()) AND typname IN ('badge_kind_enum');
SELECT proname FROM pg_proc
WHERE pronamespace = to_regnamespace(current_schema()) AND proname IN ('badge_touch');

-- 本体の表に fork が足した列
SELECT table_name, column_name FROM information_schema.columns
WHERE table_schema = current_schema() AND table_name = 'user' AND column_name IN ('badgeCount');
```

衝突していたら、手順3・4で次の4つをまとめて行います。

1. **forkが作ったものの名前を、全て接頭辞付きに変える。** 衝突した表だけでなく、上で洗い出したindex・制約・sequence・型・関数と、本体の表に足した列も変える(後の版の本体と衝突しないように)。手順4の`fix.sql`の先頭に書き、管理表と同じtransactionで流す

   ```sql
   ALTER TABLE "badge" RENAME TO "acme_badge";
   ALTER INDEX "badge_pkey" RENAME TO "acme_badge_pkey";   -- 主キー制約の名前も一緒に変わる
   ALTER INDEX "IDX_badge_userId" RENAME TO "IDX_acme_badge_userId";
   ALTER TABLE "acme_badge" RENAME CONSTRAINT "badge_userId_fkey" TO "acme_badge_userId_fkey";
   ALTER TYPE "badge_kind_enum" RENAME TO "acme_badge_kind_enum";
   ALTER TABLE "user" RENAME COLUMN "badgeCount" TO "acmeBadgeCount";
   -- sequence は ALTER SEQUENCE ... RENAME TO、関数は ALTER FUNCTION ...(引数の型) RENAME TO
   ```

2. **移したforkのmigrationのup.sqlを、変えた後の名前を作るように書き換える。** 新しく作るDBは「本体を全部 → forkを全部」の順に流すので、up.sqlに古い名前が1つでも残ると、本体が先に作ったものとぶつかる。`IF NOT EXISTS`を付けていなければ`relation "IDX_badge_userId" already exists`のように落ちる。付けていれば(上の「順序の注意」は冪等に書くことを勧めている)**黙って何もせずに通り**、forkの表にindexが無いままになる。enumを`duplicate_object`で握りつぶしていれば、forkの表が本体の型を使う。自動で付く名前も、変えた後の名前になるように`CONSTRAINT "acme_badge_pkey" PRIMARY KEY`のように明示する
3. **down.sqlも書き換える。** 古い名前のまま`DROP TABLE "badge"`が残ると、`make migrate-down-local`が**本体の`badge`を消す**
4. forkのコードが参照している名前を変える

書き換えたら、使い捨ての空のDBに`elythia migrate -direction up`を流し、移した後の既存のDBと比べます。**migrateが通ることだけでは足りません。** 上のとおり、古い名前が残っていても黙って通ることがあります。次のSQLを両方のDBで流し、出力が一致すること(forkの側のindex・制約・型・列が全てあり、本体の表に本体のindexが張られていること)を確かめます。

```sql
SELECT 'index' AS kind, tablename AS owner, indexname AS name FROM pg_indexes
WHERE schemaname = current_schema() AND tablename IN ('acme_badge', 'badge')
UNION ALL
SELECT 'constraint', conrelid::regclass::text, conname FROM pg_constraint
WHERE connamespace = to_regnamespace(current_schema())
  AND conrelid IN (to_regclass(format('%I.%I', current_schema(), 'acme_badge')),
                   to_regclass(format('%I.%I', current_schema(), 'badge')))
UNION ALL
SELECT 'type', '', typname FROM pg_type
WHERE typnamespace = to_regnamespace(current_schema()) AND typname IN ('acme_badge_kind_enum', 'badge_kind_enum')
UNION ALL
SELECT 'column', table_name, column_name || ' ' || udt_name FROM information_schema.columns
WHERE table_schema = current_schema()
  AND (table_name IN ('acme_badge', 'badge') OR (table_name = 'user' AND column_name IN ('acmeBadgeCount', 'badgeCount')))
UNION ALL
SELECT 'sequence', '', relname FROM pg_class
WHERE relnamespace = to_regnamespace(current_schema()) AND relkind = 'S'
  AND (relname LIKE 'acme\_badge%' OR relname LIKE 'badge\_%')
UNION ALL
SELECT 'function', '', p.oid::regprocedure::text FROM pg_proc p
WHERE p.pronamespace = to_regnamespace(current_schema()) AND p.proname IN ('acme_badge_touch', 'badge_touch')
ORDER BY 1, 2, 3;
```

forkのmigrationを新しく足して名前を変える方法は使えません。新しく作るDBではforkの系列が本体の後に流れるので、そのmigrationが本体の作った`badge`の名前を変えてしまいます。

forkのmigrationが本体の変更を先取りしたもの(本体の後の版と同じ内容)なら、名前を変えずに、forkのファイルを消すこともできます。その場合は、本体のmigrationが当たったものとして`C`を決め、forkのファイルは`L`に数えません。ただし、当たったものとして数えてよいのは、`000001`から途切れずにつながるときだけです。間に当たっていない本体のmigrationがあれば飛び地なので、手順2のとおり止めて相談します。本体のmigrationと内容が同じかを、列の型や制約、index、データの書き換えまで確かめてから行ってください。

### 場合3: 本体と衝突する前に気付いた

本体の最大の番号より大きい番号に、forkのファイルだけがあります。

- **C**: 本体の最大の番号になるはずだが、手順2のとおり`000001`から確かめる
- **L**: 当たっているforkのファイルの数

手順5では何も流れません。この後に本体が`000117`を出しても、本体の系列は`C`の次から当てるので流れます。

forkのものに接頭辞が無ければ、場合2の名前の付け替え(up.sqlとdown.sqlの書き換えを含む)も、この時点で済ませておくと後で衝突しません。

### やってはいけないこと

- **downで戻してから流し直さない。** forkのdownが想定通りに動く保証はなく、本体のdownには`DROP TABLE ... CASCADE`を使うものがあり、forkが足した列も一緒に消える
- **本体のmigrationのファイルの番号を書き換えない。** 本体の次の版と番号がずれ、次に取り込んだときに同じ問題が起きる。doctorが比べる本数も合わなくなる
- **当たったmigrationのSQLを書き換えない。** 既存のDBでは流れ直さないので、新しく作るDBとschemaが食い違う。例外は場合2の名前の付け替えで、既存のDBも同じtransactionで揃え、up.sqlとdown.sqlの両方を書き換えるときだけ
- **管理表に推測した値を書かない。** 小さすぎると当たったmigrationがもう一度流れてデータが消えることがあり、大きすぎると当たっていないmigrationが黙って飛ばされる(手順2)
- **サーバーやqueueのworkerを動かしたまま行わない**(手順1)

migrationが途中で止まって管理表がdirtyになったときは、SQLではなく`elythia migrate -force <番号> -track core|local`で戻します。`-force`はdirtyになっている管理表しか書き換えません。手順は[デプロイ](deployment.md#マイグレーションが途中で止まったとき-dirty)にあります。

## テスト

- `testutil.ApplyMigrations`は、本体のmigrationの後に`migration/local/`のup migrationも流す。forkが足した列を使うモデルのテストが、テスト用のDBで動く
- **migrationの中身を見るテストとゲートは、`migration/`の直下だけを読み、`migration/local/`を見ない。** 対象は、up → down → upの往復(`internal/repository/migration_roundtrip_test.go`)、冪等性(`internal/entitycompat/migration_idempotency_test.go`)、index名(`index_naming_test.go`)、seed(`migration_seed_test.go`)、schemaのドリフト(`schema_drift_test.go`)、`make gates`の本数の検査(`migrationdoc-check`)。forkの系列のdownや冪等性は、forkの側で確かめる

## その他

- テストモードの`/api/reset-db`は、`schema_migrations`と同じく`schema_migrations_local`を消さない
- TS版Misskeyはforkの表や列を知らない。TSへ戻すこと(復路)は本体でも保証しておらず([TS版からの移行](migration-from-ts.md))、forkの系列が加わると戻せないものが増える
