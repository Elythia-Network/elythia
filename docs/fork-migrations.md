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

## テスト

- `testutil.ApplyMigrations`は、本体のmigrationの後に`migration/local/`のup migrationも流す。forkが足した列を使うモデルのテストが、テスト用のDBで動く
- **migrationの中身を見るテストとゲートは、`migration/`の直下だけを読み、`migration/local/`を見ない。** 対象は、up → down → upの往復(`internal/repository/migration_roundtrip_test.go`)、冪等性(`internal/entitycompat/migration_idempotency_test.go`)、index名(`index_naming_test.go`)、seed(`migration_seed_test.go`)、schemaのドリフト(`schema_drift_test.go`)、`make gates`の本数の検査(`migrationdoc-check`)。forkの系列のdownや冪等性は、forkの側で確かめる

## その他

- テストモードの`/api/reset-db`は、`schema_migrations`と同じく`schema_migrations_local`を消さない
- TS版Misskeyはforkの表や列を知らない。TSへ戻すこと(復路)は本体でも保証しておらず([TS版からの移行](migration-from-ts.md))、forkの系列が加わると戻せないものが増える
