# Misskey-TSからElythiaへの移行ガイド

本ガイドでは、既存のMisskey-TSインスタンスを、データベースとRedisを引き継いだままElythiaへ移行する手順を説明する。移行した後にMisskey-TSへ戻すことは保証しない (#3191)。

## 前提条件

- Go 1.27+
- PostgreSQL 16+ (既存のMisskey-TSデータベース)。**Elythia の compose 群と CI は 18 に統一している** (#2513) ので、docker で運用するなら 18 に上げてから移行する方が構成が揃う。既存の 16 volume はイメージを上げるだけでは開けず dump→restore が要る (手順: [deployment.md](deployment.md#postgresql-16--18-への移行-既存環境))
- Redis 7+
- git

## 1. クローンとビルド

```bash
git clone https://github.com/Elythia-Network/elythia.git elythia
cd elythia
go build -o built/elythia ./cmd/elythia
```

フロントエンドのソースは本体の `frontend/` (Misskey TS の fork を取り込んだ pnpm workspace) に含まれている。

## 2. フロントエンド資産の準備

フロントエンドは、Elythiaに同梱のもの (Misskeyのフロントエンドを取り込み、独自に手を入れたもの、#3379) を使うか、既存のMisskey-TSのビルド済み資産を使う。2つの方法がある。

### 方法A: 同梱のフロントエンド (`frontend/`) をビルド (推奨)

```bash
make e2e-frontend-build
```

Docker内でフロントエンドがビルドされ、成果物は `frontend/built/` 配下に配置される (ビルドの前に `make plugins` も走る)。Elythiaはデフォルトでこのパスを参照するため、環境変数の設定は不要。

### 方法B: 既存のMisskey-TSのビルド済み資産を使う

既にMisskey-TSが動作しているサーバーでは、そのビルド済み資産を環境変数で指定できる:

```bash
export MISSKEY_FRONTEND_DIR=/path/to/misskey/built/_frontend_vite_
export MISSKEY_FRONTEND_DIST_DIR=/path/to/misskey/built/_frontend_dist_
export MISSKEY_REPO_ASSETS_DIR=/path/to/misskey/assets
export MISSKEY_TWEMOJI_DIR=/path/to/misskey/packages/backend/node_modules/@misskey-dev/emoji-assets/built/twemoji
export MISSKEY_FLUENT_EMOJI_DIR=/path/to/misskey/packages/backend/node_modules/@misskey-dev/emoji-assets/built/fluent-emoji
export MISSKEY_CLIENT_ASSETS_DIR=/path/to/misskey/packages/frontend/assets
export MISSKEY_STATIC_DIR=/path/to/misskey/packages/backend/assets
```

## 3. 設定

`.config/default.yml` を作成する:

```yaml
url: https://your-instance.example.com
port: 3000

db:
  host: localhost
  port: 5432
  db: misskey        # 既存のMisskey-TSデータベース
  user: misskey
  pass: your_password

redis:
  host: localhost
  port: 6379

id: aidx             # Misskey-TS側のID生成方式と一致させること
```

> **重要:** `id` フィールドはMisskey-TSインスタンスで使われているID生成方式と必ず一致させること。Misskey-TS側の `.config/default.yml` を確認して正しい値を設定する。よく使われる値: `aidx`, `aid`, `meid`, `ulid`, `objectid`。

## 4. データベースマイグレーション

Elythiaの追加テーブルを作り、共有テーブルを upstream の形に揃える。**Misskey-TSが書いたデータは原則として保持される** (例外は `000081` / `000084` / `000085` / `000094`、後述)。`000082` も行を DELETE するが、対象は upstream Misskey に無い `transfer-ownership` が作った行だけで TS 由来のものは含まない。

共有テーブルにも触るものが 21 件あるので、内容と、TS へ戻したときの影響 (保証はしない) を[破壊的なマイグレーション](#破壊的なマイグレーション)にまとめてある。**先に読むこと。**

**適用する前に、データベースのバックアップを取ること。** down が何もしない (戻せない) ものがあり、戻す手段はバックアップだけになる。

```bash
# ローカルビルドの場合
go run ./cmd/elythia migrate -direction up

# Docker imageの場合 (migrateバイナリが同梱されている)
docker compose exec app /app/elythia migrate -config .config/default.yml -direction up
```

これによりGo側で必要な追加テーブル (`app`, `auth_session`, `webhook`, `sw_subscription`, `chat_room`, `chat_message`, `bubble_game_record` 等) が作成される。

### 破壊的なマイグレーション

「追加のみ」ではない。共有テーブルに触るものが 21 件ある。**うち 16 件は Elythia 側だけが作るもの (列 / FK / index / seed / 重複行 / 譲渡が残した membership) の除去、その初期化、または upstream 追随で、Misskey-TS が書いた列の値には影響しない。残る 5 件 (`000081` / `000084` / `000085` / `000094` / `000121`) は TS が書いた値にも当たる。ただし `000121` が変えるのは値ではなく TS の列の型で、全行を書き直すが、TS が書く値 (`public`) はそのまま残る。**

| migration | 内容 | 位置づけ |
|---|---|---|
| `000029` | `poll_vote."createdAt"` を DROP | Elythiaが余分に作った列。upstream の `MiPollVote` に元から無く、Elythia も書き込むだけで読んでいなかった |
| `000036` | `note."appId"` / `note."score"` を DROP | **Elythiaが `000033` で追加した列**。TS 側は `1580148575182` / `1696569742153` で DROP 済みなので、既存 TS インスタンスの DB には元から存在しない |
| `000053` | `poll."notifiedAt"` の過去分を backfill (`UPDATE`) | Elythia独自列 (`000044` で追加) の初期化。TS 由来の列には触らない。埋めておかないと `ExpiryWorker` の初回 tick で過去のアンケート全件に `pollEnded` 通知が一斉発火する (#1415) |
| `000056` | `note.uri` の重複行を DELETE (最小 `id` を残す) | **Elythia固有の race で作られた重複コピーの除去** (#1527)。`IngestNote` の `FindByURI` → `Create` が並行すると同一 URI の行が増えていた。`000057` で UNIQUE index を張る前提として要る |
| `000064` | `registration_ticket_pendingUserId_fkey` を DROP | Elythiaが `000026` で余分に張った FK (#2083)。upstream の `pendingUserId` は無制約 `varchar`。この FK があると確認メール再送防止が必ず FK 違反で no-op になっていた |
| `000067` | `migrations` の seed 行を DELETE + 正式名へ `UPDATE` + 未 seed 分を `INSERT` | **`000029` が seed した Elythia 由来の行を直すもの** (#2244)。TypeORM は `name` 列の文字列一致で未実行判定するので、短縮形のままだと TS 復帰時に本家 migration が再実行される |
| `000068` | 冗長な index を DROP | **落とすのは Elythia の migration が作った index だけ**。upstream 由来の index は絶対に触らない (TS 生まれの DB と Elythia 生まれの DB で index の名前を揃え、後の migration (`000083` など) が upstream の名前を前提に書けるようにするため。当初の理由は TS へ戻したときに本家が再作成できないことだったが、そちらは今は保証しない) |
| `000080` | `note` の自己参照 FK (`renoteId` / `replyId`) を DROP | **upstream 追随。** 本家も 2025.8.0 の `1753868431598-remove_note_constraints.js` でこの 2 本を削除しており、現在の `MiNote` は `createForeignKeyConstraints: false` で FK を作らない |
| `000081` | 孤児化した `note` 行を DELETE + 痕跡列を NULL 化 | **TS が書いた行が対象になりうる 1 つ目。** 下記参照 |
| `000082` | owner が持つ `chat_room_membership` / `chat_room_invitation` を DELETE | **`transfer-ownership` だけが作れる行の除去** (#2858)。この endpoint は upstream Misskey に無い (出自は [乖離一覧](divergence/api.md))。upstream は owner に membership 行を作らず (`ChatService.ts` の `concat({userId: room.ownerId, isMuted: false})`)、owner 宛の招待も `createRoomInvitation` が弾くので TS 生まれの DB には存在しない |
| `000083` | `IDX_note_userId` を DROP して `("userId","id" DESC)` の複合 index を作る | **upstream 追随であり、seed の実体が無かった穴を塞ぐもの。** 本家は 2025-04 の `1745378064470-composite-note-index.js` で同じ張り替えをしており、`000067` はその `CompositeNoteIndex1745378064470` を**適用済みとして seed していた**。しかし Elythia 側に index を作る migration が無かったため、TS へ復路で渡すと「適用済み」と誤認したまま index が存在しない状態になっていた。落とすのは Elythia 固有名の `IDX_note_userId` だけで、upstream 由来の index には触らない (`000068` と同じ方針)。作る側は upstream と同名なので TS 生まれの DB では `IF NOT EXISTS` で skip される |
| `000084` | `meta."repositoryUrl"` の未設定行を Elythia のリポジトリで `UPDATE` | **TS が書いた列の値に当たる 2 つ目。** 対象は NULL と upstream の列 DEFAULT (`https://github.com/misskey-dev/misskey`) のままの行だけで、operator が設定した URL には触らない。下記参照 |
| `000085` | `meta."feedbackUrl"` の未設定行を Elythia の issues で `UPDATE` | **TS が書いた列の値に当たる 3 つ目。** `000084` とまったく同じ構造で、`000029` が隣り合う 2 行で設定している列 DEFAULT のもう一方。対象は NULL と upstream の列 DEFAULT (`https://github.com/misskey-dev/misskey/issues/new`) のままの行だけ。下記参照 |
| `000094` | `user_ip.ip` を正規化 (`UPDATE`) + 正規化で衝突した行を統合して DELETE | **TS が書いた値にも当たる。** IPv4-mapped IPv6 (`::ffff:a.b.c.d`) を対応する IPv4 へ畳み、IPv6 の大文字・ゼロ圧縮も正規形にする。畳んだ結果 `(userId, ip)` が衝突する行は 1 行へ統合し (初回 = 最古 / 最終 = 最新 / 回数 = 合算)、元の行は DELETE する。**down では戻らない** — 統合前の行数も個別の観測時刻も残っていない。`inet` が読めない値 (port 付き / zone 付き / IP でない文字列) と CIDR は触らない。あわせて Elythia 独自列 `lastSeenAt` / `observationCount` を 足すが、こちらは追加のみ (#3103) |
| `000095` | `IDX_user_ip_ip_lastSeenAt` を DROP して `("ip","lastSeenAt" DESC,"userId")` の複合 index を作る | **落とすのは Elythia の `000094` が作った index だけ** — upstream の `user_ip` は `userId` と `UNIQUE (userId, ip)` しか持たないので、TS 由来の index には触らない (`000068` / `000083` と同じ方針)。張り替えるのは、関連候補の抽出 (#3105) が 1 つの IP から取る件数を上限で打ち切るため、`userId` まで index に乗っていないと**同じ最終観測が固まっているときに上限が保証されない**から。down は対称 (旧を作り直して新を落とす) で、行は触らない |
| `000106` | `abuse_report_notification_recipient` の FK を張り替え (`SET NULL` の Elythia 名 2 本を DROP し、本家と同じ名前の `CASCADE` 3 本を足す) | **upstream 追随** (#3264)。本家 `1713656541000-abuse-report-notification.js` と同じ 3 本 (`userId` -> `user` / `user_profile`、`systemWebhookId` -> `system_webhook`) にする。**TS 製 DB には元から本家の 3 本があるので何もしない** (名前で有無を見て足す)。行は消さない — 直す前に `SET NULL` で宛先が NULL になった通知先は残る。**外部キーを張る前に、本家では作れない形の値を NULL にする** — `user_profile` の無い利用者を指す `userId` (残すと検証で失敗して migration が止まる) と、method に合わない側の参照 (webhook 方式の行の `userId`、email 方式の行の `systemWebhookId`。残すと CASCADE で無関係な削除に巻き込まれて通知先ごと消える)。どれも TS が書く値には当たらず、down でも戻らない |
| `000107` | `note."pageCount"` をページの content から数え直して backfill (`UPDATE`) | **upstream 追随** (#3293)。Elythia はページの作成・更新・削除で `pageCount` を増減していなかったので、それより前に作ったページが参照するノートを本家と同じ数え方 (`PageService.collectReferencedNotes`) で埋める。**増やす向きにしか直さない** (参照されているノートだけを触る)。TS が維持していれば同じ値になっているので **TS 製 DB では何も変わらない**。down は戻さない (0 に戻すと掃除の保護が外れる) |
| `000116` | `meta."repositoryUrl"` / `meta."feedbackUrl"` のうち、`000084` / `000085` か起動時の `EnsureInitial` が入れた以前の既定値 (`https://github.com/shiroha-a/mk`、`.../issues/new`) と同じ行を新しい URL で `UPDATE` | **Elythia が自分で入れた値の更新。** リポジトリを `Elythia-Network/elythia` へ移した (#3394) ため。TS が書いた値 (`misskey-dev` の既定値や operator の値) には触らない。down は新しい URL の行を以前の既定値に戻す (旧 URL は GitHub が転送するので開ける) |
| `000121` | `page."visibility"` の型 `page_visibility_enum` を (`public`, `followers`, `specified`) から (`public`, `private`) に作り替え、`followers` / `specified` の行を `private` に `UPDATE` | **Elythia の機能 (#3479)。** Page に作者だけが見られる `private` を足し、派生版の一つと同じ 2 値にする。本家は作成時に必ず `public` を書き、公開範囲を選ぶ API も画面も持たないので、**TS が書いた行は `public` のまま変わらない**。`followers` / `specified` の行を作れたのは、#3479 より前の Elythia の API だけ。enum が既に 2 値なら何もしない。down は 3 値に戻し、`private` の行を `specified` にする (`followers` だった行と `specified` だった行の区別は戻らない)。TS へ戻したときの影響は「[戻らなくなったもの](#戻らなくなったもの)」の上の段落にある |
| `000133` | `user_profile."mutedWords"` / `"hardMutedWords"` の要素のうち `/.../` の形でない文字列を、同じ語の組 (`"foo"` → `["foo"]`) に `UPDATE`。空白だけの文字列は消す | **Elythia が自分で受け付けた値の書き換え** (#3369)。本家の `i/update` は `/.../` の形 (JavaScript の `^\/(.+)\/(.*)$`、flags は問わない) でない文字列を `INVALID_REGEXP` で拒否し、本家の frontend も作らない。000133 はこの形の文字列 (`/foo/v` なども) を正規表現として残すので、**TS が書いた行は変わらない**。`hardMutedWords` では、#3369 より前の Elythia のサーバーが読んでいた Go の形 (`^/(.+)/([gimsuy]*)$`) も残す。Go の形にだけ当たる文字列 (パターンの中に `\r` や U+2028 を含むもの) は、残しても以後の判定 (JavaScript の形) では何にも当たらない (語の組にしても当たらない)。`mutedWords` の書き換えは、以前は効いていなかった (frontend は `/.../` でない文字列を当てない) 語を効かせ始める。#3369 より前の Elythia の `i/update` だけが受け付けていた。語の組は本家でも有効な形なので、TS へ戻しても読める。down は戻さない (元が文字列か語の組かを区別できない) |
| `000137` | 従来のローカルユーザーを外部参照済みとして記録し、`user` / `user_pending` の状態制約を検証 | **Elythia が追加した列への backfill と検証。** legacy local account を不可逆な OFF 切替から保護する。down は安全印を保持するため no-op |

#### `000081` について

`000080` 以前の FK は `ON DELETE SET NULL` だったので、元ノートが消えるとリノート側の `renoteId` ごと NULL になり、`renoteUserId` だけが残った。frontend は `renoteId` があれば「削除されたノート」と描画できるが、消えていると本文の無い通常ノート扱いになり**何も表示されない**。

DELETE の対象はこの残骸で、条件は

- `renoteId IS NULL` かつ `renoteUserId IS NOT NULL`
- 本文・CW・返信先・添付ファイル・投票・リアクションがすべて無い
- 返信数・リノート数・クリップ数がすべて 0

を**同時に満たす行だけ**。中身を持つ引用リノートは消さない。

**TS が INSERT した行でも、Elythia 稼働中に `renoteId` を失えばこの条件に合致して消える。** 消えるのは表示されない空殻なので実害は小さいが、「TS のデータには一切触らない」わけではない。

続く 2 つの `UPDATE` は痕跡列の掃除で、`renoteUserId` / `renoteUserHost` / `renoteChannelId` を NULL 化するものと、**`replyId` を失った行の `replyUserId` / `replyUserHost` を NULL 化するもの**。後者は `renoteUserId` の条件を通らない通常の返信行にも当たる。

**down は `SELECT 1;` (no-op) で戻せない。** 削除した行の内容も、NULL 化する前の値も保存していないため。適用前にバックアップを取ること。

#### `000084` について

`meta."repositoryUrl"` が「未設定」のままだと、frontend の `/about-elythia` と
`MkSourceCodeAvailablePopup` がソースコードの案内を出せない。AGPL-3.0 section 13 が
求める案内が既定で存在しない状態なので埋める (#2700)。

**未設定は 2 通りある。**

- `NULL` — Elythia 生まれの DB。`000029` が列 DEFAULT を upstream 互換の
  `https://github.com/misskey-dev/misskey` に設定しているが、meta 行を作るのは GORM の
  `Create(&model.Meta{...})` で、`*string` の nil を **NULL として明示挿入する**ため
  列 DEFAULT が効かない (新規行は `internal/repository/meta.go` の `EnsureInitial` 側で
  入れるようにしたので、この migration の対象は既存行だけ)
- `https://github.com/misskey-dev/misskey` — **Misskey TS 生まれの DB**。TypeORM は
  未指定の列に `DEFAULT` を書くので、TS が作った meta 行は必ずこの値を持つ。operator の
  申告ではなく列 DEFAULT の値であり、動いているのが Elythia である以上「このサーバーの
  コード」として Misskey 本体を案内するのは誤りになる。さらに frontend は
  `repositoryUrl !== 'https://github.com/misskey-dev/misskey'` で改変版の告知ポップアップを
  出すか決めるので、この値のままだと**告知そのものが出ない**

**operator が意図的に空にした行も埋まる。** `admin/update-meta` は妥当な絶対 URL で
なければ NULL を書く (upstream の `URL.canParse` と同じ) ので、「フィールドを空にする」
操作の結果は 1 つ目と区別できない。AGPL 13 条の観点では案内が無い状態のほうが問題なので
埋める側に倒してある。別の URL を出したい operator は admin 画面で設定し直せる。

**TS へ戻す場合 (保証はしない) は operator が設定し直すこと。** Elythia が入れた値が残っていると、
TS 側は「Misskey を改変したバージョン」として Elythia のリポジトリを案内し続ける。
`down` は no-op なので自動では戻らない (up 後に operator が同じ値を明示設定した行と
区別できないため)。

#### `000085` について

`000084` とまったく同じ構造。`000029` は `repositoryUrl` と **隣り合う 2 行**で
`feedbackUrl` の列 DEFAULT も設定しているが、どちらも GORM の NULL 明示挿入に負ける。
#2700 は前者だけを直したので、こちらは NULL のまま残っていた (#2891)。

**影響は `/about` の導線だけではない。** `feedbackUrl` は nodeinfo の metadata にも
載る (`internal/api/nodeinfo/handler.go`) ので、未設定だと他インスタンスや一覧サイトから
見ても欠ける。実際に本番の nodeinfo は `feedbackUrl: null` だった。

既定値は **Elythia の issues** (`https://github.com/Elythia-Network/elythia/issues/new`)。upstream が
列 DEFAULT に Misskey 本体の issues を置いているのと同じ位置づけで、**「ソフトウェアへの
フィードバック先」**にあたる。「このサーバーへのフィードバック」を受けたい operator は
admin 画面 (全般 → 情報) で上書きする。

対象・例外の扱い・`down` が no-op である理由はすべて `000084` と同じ。

#### down が no-op のもの

`000053` / `000056` / `000067` / `000068` / `000074` / `000081` / `000082` / `000084` / `000085` / `000097` / `000107` / `000133` / `000135` / `000137` の 14 本は down が `SELECT 1;` で、up を巻き戻せない。**データを不可逆に変えるのはこのうち 13 本**で、変えないのは index を落とすだけの `000068` だけ。`000135` は `000067` と同じく TypeORM の `migrations` テーブルへ seed した行を残す (TS が自分で書いた同名の行と区別できず、消すと TS で migration が再実行されるため)。`000137` は legacy local account に付けた初回参照印を down で消すと、不可逆な OFF 切替が可能になるため保持する。`000074` は backfill で入れた行とその後の実観測で入った行を区別できないので、消すと連合中に蓄積した観測まで巻き添えになる。`000084` / `000085` は未設定だった `meta.repositoryUrl` / `meta.feedbackUrl` を埋めるが、その後 operator が同じ値を明示設定した行と区別できないため戻せない。`000097` は「2FA を解除済みなのに `usePasswordLessLogin` が立ったまま」という壊れた状態を直すもので、どの行がそうだったかを記録していないので戻せない (落としたフラグは利用者が立て直せるのでデータ損失にはならない)。

#### Elythia 内での切り戻し

上の表は「TS 製 DB へ流したときに何が起きるか」の観点なので、Elythia 専用テーブルは含めていない。

**番号をまたいで `make migrate-down` を繰り返すのは、原則としてデータを失う操作。バックアップを取ってから行うこと。** down の大半は `DROP TABLE` / `DROP COLUMN` で up を打ち消すだけなので、その版で入った値は戻らない。`000022` まで戻せばチャット履歴が、`000025` まで戻せば下書きが、`000001` まで戻せば `user` / `note` / `drive_file` ごと消える。

**個別の migration を見て「これは安全」と判断しないこと。** 判断材料になりそうなものが 2 つあるが、どちらも当てにならない。

- **`-- data loss:` の宣言。** あるのは 30 本だけで、**宣言が無いまま `DROP TABLE` / `DROP COLUMN` / `DELETE` する down が 51 本ある**。運用も一貫していない — `000076` は `meta` の設定列 1 本を落とすだけで宣言しているが、同じく `meta` の設定列を落とす `000070` は「data loss も無い」と書いている
- **up が冪等かどうか。** `000029` の up は大半が `IF NOT EXISTS` 付きだが、**down は無条件に DROP する**。落ちるのは `user_security_key` / `user_ip` / `user_memo` / `promo_note` / `promo_read` といった **upstream 所有のテーブル**と、`meta` / `user_profile` の 63 列、そして **TypeORM の `migrations` テーブル**。`000067` がわざわざ守っているものを、より悪い形で壊す。宣言は無い。同じ形 (up は冪等、down は無条件 DROP、対象は upstream) は `000030` / `000031` / `000032` / `000038` にもある

方向が違うので別に挙げておくもの:

| migration | 内容 |
|---|---|
| `000077` | **up も down も無条件に `DELETE FROM "signup_application";` を実行する。** 戻すときだけでなく進めるときも申請が消える |
| `000078` | up で `signup_application."reason"`、down で `"answers"` と `meta."signupApplicationForm"` を DROP する。申請理由・各申請の回答・管理者が定義した申請フォームが失われる |
| `000072` | `instance_secret` を **テーブルごと** DROP する。`GetOrCreate` はテーブル不在を `ErrRecordNotFound` として扱わないので、**この状態で起動すると `resolveMediaProxySecret` が失敗して Elythia が立ち上がらない**。`mediaProxySecret` を設定ファイルに書いていれば回避できる |
| `000074` | backfill で入れた行と実観測で入った行を区別できないので、`instance_signature_capability` の内容は適用前に戻せない |

Elythia の migration を当てた後のデータベースで、Misskey-TS を動かし直せることは保証しない (#3191)。戻す場合の注意と、今どこまで戻れるかは [Misskey-TSへのロールバック](#misskey-tsへのロールバック) を参照。

## 5. Misskey-TSの停止

```bash
# 既存のMisskey-TSサーバーを停止する
# (停止方法はデプロイ手段による: systemd、pm2、docker 等)
systemctl stop misskey
# または
pm2 stop misskey
# または
docker compose stop web
```

## 5.5. チャット(DM)の既読を引き継ぐ

**Misskey-TS(とCherryPick)は、DMの既読をRedisにだけ持ち、DBの`chat_message.reads`には書かない**(作るときに空の配列を入れるだけ)。Elythiaは既読を`reads`で判定するので、何もしないと、移行の前に受け取ったDMがすべて未読になる。`/api/i`の`hasUnreadChatMessages`がtrueのまま残り、再読込するたびにチャットの未読バッジが出る(#3653)。

Misskey-TSを止めた後、Elythiaを起動する前に、`elythia backfill chat-reads`で移行元の既読を`reads`へ移す。

```bash
# まず件数だけを見る (書き込まない)
./built/elythia backfill chat-reads -config .config/default.yml \
  -before 2026-10-01T00:00:00+09:00 -dry-run

# 実行
./built/elythia backfill chat-reads -config .config/default.yml \
  -before 2026-10-01T00:00:00+09:00
```

Dockerなら`docker compose run --rm --no-deps --entrypoint /app/elythia app backfill chat-reads -config /app/.config/default.yml -before ...`の形で流す。**PostgreSQLとRedisは起動しておく。** `--no-deps`は依存するサービスを起動しないので、stackごと止めているとDBやRedisにつながらずに失敗する(何も書かずに止まる)。先に`docker compose up -d db redis`で起こしておく。composeファイルによって、次のように読み替える。

- 配布のimageで動かす構成(`docker-compose.image.yml`): `docker compose`に`-f docker-compose.image.yml`を付ける(例 `docker compose -f docker-compose.image.yml up -d db redis`、`docker compose -f docker-compose.image.yml run --rm --no-deps --entrypoint /app/elythia app backfill chat-reads ...`)。付けないと、同じ場所の`docker-compose.yml`の`app`(`build: .`)が選ばれる
- UDS構成(`compose.uds.yaml`): `-f compose.uds.yaml`を付け、`app`を`mkgo`に、`db` / `redis`を`postgres` / `valkey`にする(例 `docker compose -f compose.uds.yaml up -d postgres valkey`、`docker compose -f compose.uds.yaml run --rm --no-deps --entrypoint /app/elythia mkgo backfill chat-reads -config /app/.config/default.yml ...`)

- **`-before`は必須で、Misskey-TSを止めた時点を、時差の付いたRFC 3339で渡す**(`2026-10-01T00:00:00+09:00`や`2026-09-30T15:00:00Z`)。指定が無い・読めないときは、何もせずに使い方を出して終了コード2で止まる。対象はこの時点より前に作られたDMだけで、作られた時点はメッセージのidから読む(設定の`id`の方式で読む。読めないidの行は触らずに`unparsable-id`として数える)。それより後のDMはElythiaが`reads`を書いているので触らない。**Elythiaを動かし始めた後の時点を渡さないこと。** Elythiaが受けたDMの未読まで、移行元の記録(Elythiaは書かない)で判定して既読にしてしまう。`reads`から受け手を外す手段は無いので、書いた後では戻せない
- **今より後の`-before`は受け付けない**(終了コード2)
- **遅すぎる`-before`は、分かる範囲で検出して止める。** 本家は新着のたびにmarkerを最新のidで上書きするので、markerが残っている会話に「markerより新しく、`-before`より前」のDMがあれば、それは本家が受けていない(移行の後にElythiaで受けた)DMである。比べるのはidから読んだ時刻だけで、**markerの時刻から`-too-late-margin`(既定5分)を越えて新しいDMだけを証拠にする**。本家はidを作ってからDBに書き、その後でmarkerを書くので、同じ会話にほぼ同時に2通来るとmarkerが古い方を指すことがある。また`objectid`は秒、`meid` / `ulid`は乱数の部分があり、同じ時刻の中のidの並びは作った順にならない。幅はそれを吸収するためのもの。1件でもあれば、何も書かずに終了コード1で止まり、該当の会話を`too-late reader=<id> sender=<id> marker=<id> marker-at=<日時> newer=<件数> newest-at=<日時>`で出す(`-dry-run`では止めずに警告として出す。日時はUTC)。そのために、書く前に全部を書かずに見てから、2周目で書く。**検出できるのは本家のmarkerが残っている会話だけ**で、移行の前に読み終えていた会話や、移行の後に始まった会話にElythiaで受けた未読があっても分からない。出力の`newest targeted message: <日時>`(対象のうち最も新しいDMが作られた日時、UTC。`-before`より後のDMは入らない)を、移行した時刻と見比べること
- **止まったときの確かめ方。** `too-late`の行の`newest-at`が移行した時刻より後なら、`-before`が遅い。移行した時刻に直して流し直す。`newest-at`が移行の前で`marker-at`との差が小さい(移行元を止める直前に続けて届いたDMなど)なら、正しい`-before`での誤検出なので、差より大きい`-too-late-margin`(例 `-too-late-margin 30m`)を付けて流し直す。幅を広げると、移行のすぐ後に届いたDMは検出されにくくなる
- **Redisは本体と同じ設定(`redis`)で読む。** キーには`redis.prefix`(未設定ならURLのhost)と`:`が付く。本家と同じ扱いなので、移行元と同じ設定ファイルを使えば一致する。**移行のときにRedisを新しくした(移行元のRedisが今の設定と別の)場合**は、`redis`の節(と`url`、`redis.prefix`を省いているなら)だけを移行元に合わせた設定ファイルを一時的に作り、`-config`で渡す。DBは今のものを指したままにする。**本番の設定ファイルは書き換えない**(戻し忘れると、次の起動で本体が移行元のRedisを見る)。移行元のRedisが残っているのに`-assume-read`を選ぶと、本当の未読も既読になる
  - **`MK_REDIS_*`の環境変数は設定ファイルより優先される。** 配布の`docker-compose.image.yml`は`app`に`MK_REDIS_HOST: redis`を渡しているので、一時ファイルの`redis.host`は黙って無視され、今のRedisを読む。composeの`environment`に`MK_REDIS_*`(`MK_REDIS_HOST` / `MK_REDIS_PORT` / `MK_REDIS_PASS` / `MK_REDIS_DB` / `MK_REDIS_PREFIX`など)があれば、`-e`で移行元の値を渡して上書きする
  - Dockerでは、ホストに作った一時ファイルは`-v`でコンテナに渡す。例: `docker compose -f docker-compose.image.yml run --rm --no-deps -v ./.config/chat-reads-source.yml:/app/.config/source.yml:ro -e MK_REDIS_HOST=<移行元のRedisのhost> --entrypoint /app/elythia app backfill chat-reads -config /app/.config/source.yml -before ... -dry-run`(UDS構成なら上のとおり`-f compose.uds.yaml`と`mkgo`に読み替える)
- **会話ごとに決める。** 本家は、DMを受け取ると`newUserChatMessageExists:<受け手>:<送り手>`(値は最新の未読のメッセージのid)を置き、`newChatMessagesExists:<受け手>`(set)に`user:<送り手>`を足す。会話を開くと両方を消す。**両方が残っている会話は未読のまま残す**(どこまで読んだかは分からない。開けば既読になる)。それ以外の会話は、対象のDMをすべて既読にする。本家の「全チャットを既読にする」はsetだけを消してmarkerを残すので、markerだけが残る会話も既読にする
- **対象は宛先がローカルの利用者のDMだけ。** リモートの宛先には本家も未読の記録を作らない。**ルーム(`toRoomId`)のメッセージも対象にしない。** `/api/i`の`hasUnreadChatMessages`はルームを数えないので、埋めても未読のバッジは変わらない。チャットのホームの一覧では、移行の前のルームのメッセージが未読の印を付けたまま出ることがあり、ルームを開けば消える
- **Redisに記録が無いときは止まる。** 対象のDMがあるのに、prefixの下に上の2つの形のキーが1つも無ければ、終了コード1で止まる。prefixを取り違えたまま流すと、全部の会話が「記録が無い=既読」になるため。移行の途中でRedisを作り直した・空にしたなど、記録が残っていないと分かっているときは、`-assume-read`を付けて流す。Redisを読まずに、対象のDMをすべて既読にする。**このときは、上の遅すぎる`-before`の検出が働かない**(移行元の記録を読まないため)。`-before`が遅くても止まらないので、`-dry-run`の`newest targeted message`を移行の時刻と必ず見比べてから流す。**Redisにつながらないときも止まる**(黙って全部を既読にはしない)
- **型の違うキー(壊れた記録)の会話は、未読のまま残して`broken-conversations`に数える。** 止まらずに続ける
- **キーが部分的に消えている会話は、既読になる。** 消えたキーと「読んだので消えた」キーは区別できない。Redisがメモリの上限に達したことがあり`maxmemory-policy`が`allkeys-*`だった、AOF / RDBが欠けた、手で消した、などの場合は、未読だった会話の一部が既読になる。記録が1つも無いときだけは上のとおり止まる
- 出力は、会話ごとの`conversation reader=<id> sender=<id> state=<read|unread|broken> marked=<件数> kept=<件数>`、利用者ごとの合計、全体の合計。メッセージの本文は出さない
- **冪等。** 受け手が既に`reads`にある行は対象にならないので、2回流しても受け手を二重に足さない。まとまり(`-batch`、既定1000件)ごとに更新し、間に`-sleep-ms`(既定100)だけ待つ。途中で失敗したら、出力の`-from <id>`から再開できる。書かない1周目の分だけ、DMの表を2回読む

### すでに移行したサーバーで流す

Elythiaへ移行した後でも、同じコマンドで引き継げる。`-before`には、Misskey-TSを止めた(Elythiaを最初に起動した)時点を渡す。Elythiaは本家の未読の記録を書きも消しもしないので、Redisを作り直していなければ、記録は移行した時点のまま残っている。稼働中に流してよい(受け手を足すだけで、利用者がElythiaで開いて既読にした行は対象から外れる)。

Redisを作り直した・空にした後なら、記録は残っていないので`-assume-read`を付ける。どちらの場合も、利用者の側では設定の「その他」の「全チャットを既読にする」(`chat/read-all`)でも未読を消せる(移行の前のものに限らず、受け取ったDMが全部既読になる)。

## 6. Elythiaの起動

```bash
./built/elythia serve -config .config/default.yml
```

方法Bで環境変数を使う場合は、**`source .env` では効かない** (`.env` に `export` が無いのでシェル変数にしかならず、子プロセスへ渡らない)。`set -a; . ./.env; set +a` で読み込むか、各行を `export` して起動する。

## 7. 動作確認

1. ブラウザで `https://your-instance.example.com` を開く
2. エントランスページがスタイル付きで正しく表示されることを確認する
3. 既存アカウントでログインする
4. 以下を確認する:
   - タイムラインに自分のノートが表示される
   - プロフィールページが正しく表示される
   - 通知が動作する
   - ファイルアップロードが動作する
   - リアクションが動作する

## Docker Composeで新規構築する場合

新しいインスタンスをDocker Composeで立ち上げる場合は以下で起動できる:

```bash
# 1. 起動前に必ずフロントエンドをビルドする (frontend/ の node_modules を取得し、
#    frontend/built/ に SPA 成果物を生成する)。
make e2e-frontend-build

# 2. 起動 (初回は image build も走る)
docker compose up -d --build
```

`docker-compose.yml` には one-shot の `migrate` サービスが含まれており、app 起動前に
DB マイグレーションが自動適用される (空 DB でも、TS から swap した既存 DB でも冪等)。
そのため上記の 2 ステップだけで Elythia が PostgreSQL および Redis と共に起動する。
詳細は `docker-compose.yml` を参照。

`make e2e-frontend-build` で生成した `frontend/built`(SPA の vite 成果物、約200MB)は、`docker-compose.yml` が bind-mount でコンテナに渡す（`MISSKEY_FRONTEND_DIR` 等で参照）。static-assets / twemoji / fluent-emoji / repo-assets は image に焼き込まれるためマウント不要。**この frontend ビルドを忘れると SPA の JS/CSS が 404 になりフロントエンドが表示されない**ので注意。

> bare-metal 起動(バイナリを repo root から実行)の場合は、Elythia がデフォルトで `frontend/built/` 等の相対パスを参照するため環境変数の設定は不要。docker では WORKDIR が `/app` で相対パスが効かないため、上記の bind-mount + 環境変数で渡す。

### Docker container の UID

Elythia のコンテナは Misskey-TS と同じ **UID/GID 991** で起動する。`./files` (drive ファイルストレージ) を host volume mount している場合、ホスト側ディレクトリは UID 991 が書き込めるパーミッションでなければならない。

- **TS から swap する場合**: 既に `./files` の中身が UID 991 で書かれているのでそのまま動く (drop-in 互換)
- **mk-go-only から旧 root 構成 (#621 以前) で運用していた場合**: 一度だけ `sudo chown -R 991:991 ./files` で所有権を揃える必要がある

## Misskey-TSへのロールバック

**Elythia の DB を Misskey-TS へ戻すこと (復路) は保証しない (#3191)。** 往路 (Misskey-TS の DB をそのまま引き継ぐこと) は引き続き保証する。今後の変更には、TS へ戻せることを理由にした制約を課さない。戻せる可能性を残したいなら、**移行の前に取った DB のバックアップから戻す**のが確実な方法になる。

**ローカル専用アカウント機能 (#3852) の状態と外部参照は、TS へ戻している間は管理されない。** `000136` 適用済みの DB を Misskey-TS で使うと、TS が作成するローカルアカウントには DB 既定の `federationStatus = 'on'` が入り、TS が扱わない `user."firstExternalReferenceAt"` は `NULL` のままになる。既存の ON かつ未参照のアカウントも、TS 稼働中に外部から参照されても同列は更新されず、Elythia に戻った後、未参照としてローカル専用へ切り替えられるおそれがある。pending / OFF の状態も、TS 稼働中に Elythia と同じ意味で保たれる保証はない。**TS へ戻した後、再び Elythia に移行しても、外部参照済み状態が往復で保たれる保証はない。** 確実に元へ戻す場合は、TS から Elythia へ移行する前のバックアップを復元する (バックアップ以降の更新は失われる)。

それでも戻す場合の手順:

1. Elythiaを停止する
2. 従来通りMisskey-TSを起動する

今の版でどこまで戻れるかは、`make dropin-elythia-born-test` (Elythia 生まれの DB を TS に引き渡す) と `make dropin-swap-test` の復路の段階 (TS → Elythia → TS) で**測っている**。どちらも守る対象ではなく、意図的な変更で通らなくなったら期待値を更新し、何が戻らなくなったかを下の「[戻らなくなったもの](#戻らなくなったもの)」に記録する (手順は [dropin-e2e.md](dropin-e2e.md#復路は測る対象-3191))。Elythia が追加したテーブルは Misskey-TS からは無視される。

[破壊的なマイグレーション](#破壊的なマイグレーション) の 21 件は戻らない。うち 16 件は Elythia が自分で作ったものの除去・初期化か upstream 追随なので**戻す必要が無い**。`000056` / `000081` / `000082` が消した行と `000053` / `000067` / `000084` / `000085` が上書きした値は、down が `SELECT 1;` の no-op なので復元できない。`000137` が追加した初回参照印も安全のため保持する。**`000094` は down を持つが、それでも戻らない** — 消すのは自分で足した列と index だけで、正規化した `ip` の値と統合で消えた行は復元できない (統合前の行数も個別の観測時刻も残っていない)。**`000084` / `000085` が書き換えるのは `meta."repositoryUrl"` と `meta."feedbackUrl"` なので、TS へ戻すときは admin 画面で設定し直すこと** (Elythia のリポジトリと issues を案内したままになる)。この経路を CI で測っているのは `make dropin-swap-test` (TS → Elythia → TS) で、`make dropin-elythia-born-test` は逆に Elythia 生まれの DB を TS に引き渡せるかを見ている。

migration の注記やコードのコメントには、TS へ戻せることを設計の理由にした記述が残っている。**それらは当時の判断の記録で、今は保証ではない。** 往路にも要る制約 (upstream 由来の index を触らない、TS が書いた RSA 鍵のテーブルをそのまま読む、など) は、往路の理由で引き続き守る。

**プラグインが管理するアカウント (#3468) は、TS へ戻すと普通のアカウントになる。** TS は `user."managedByPlugin"` を読まないので、Elythia がしていたログインの拒否が外れ、そのアカウントのネイティブトークンが外からのリクエストでも通るようになる (パスワードは持たないので、パスワードではログインできないまま)。戻す前に該当するアカウントを凍結するか削除すること。詳細は [divergence/db.md](divergence/db.md) の `managedByPlugin` の行と [divergence/security.md](divergence/security.md) §6。復路の検証は落ちない (列が無視されるだけ) ので、下の表には載せていない。

**非公開の Page (#3479) は、TS へ戻すと URL を知っている人に見える。** 本家の `users/pages` と注目の Page は `public` だけを出すので、一覧には現れない。しかし `pages/show` は公開範囲を見ないので、ID か名前で引けば作者以外にも中身が返り、画面でも開ける。`/@<user>/pages/<name>` の HTML の metadata にも題名と要約が出る。過去に like した人の `i/page-likes` にも出る。戻す前に、非公開の Page を削除するか公開してよい内容にすること。`000121` の down を流すと `private` は `specified` になるが、本家の `pages/show` はこれも見ないので結果は同じ。復路の検証は落ちない (TS が書く `public` は新しい型にもある) ので、下の表には載せていない。

**受信を拒否するソフトウェア (#3362) は、TS へ戻すと効かなくなる。** TS は `meta."inboxSuspendedSoftware"` を読まないので、拒否していた相手からの受信が黙って再開する。**配信停止中のソフトウェアに `/.../` (正規表現) で書いた行も、TS では何にも当たらなくなり**、その相手への配送が黙って再開する (TS の semver は `/.../` を範囲として読めない)。戻す前に、必要なら配信停止を semver の範囲で書き直すか、ドメインブロックに置き換えること。

### 戻らなくなったもの

復路の検証 (`elythia-born` / `swap-test` の復路の段階) が、Elythia の意図的な変更で通らなくなったときに 1 行ずつ足す。書式は「版 / 落ちた段階 / TS へ戻したときに失われるもの、または起きること / 変更の PR」。

| 版 | 落ちた段階 | 戻したときに失われるもの・起きること | PR |
|---|---|---|---|
| (まだ無い) | | | |

## drop-in 互換性の現状 (2026-05-09 時点)

Playwright spec (#744) を **299 ファイル / 40 directory** (directory は spec を直接含むもの。`find tests/playwright/specs -name '*.spec.ts' -printf '%h\n' | sort -u | wc -l`) に育て、PR ごとに Elythia backend
へ投げている。Misskey TS backend に対しては upstream 追従のタイミングで回し、spec の
期待値が Elythia の挙動に引きずられていないかを検証する。発見した drop-in 互換 drift は
40+ 件すべて解消済 (詳細: [api-compatibility.md](api-compatibility.md))。

- **API endpoint 互換**: 主要 endpoint (admin / notes / users / i / drive / chat / reactions / timeline / emoji / auth / federation / channels / hashtags / roles 等) は両 backend で同 status / 同 shape を返す
- **WebSocket チャンネル**: upstream の 18 をすべて実装 (#125)。加えて Elythia 独自の `notifications` があり計 19
- **ActivityPub 連合**: 主要 Activity (Create / Delete / Update / Follow / Accept / Reject / Undo / Like / Announce / Block / Flag / Move / Add/Remove) は送受信対応 (詳細: [federation.md](federation.md))

## 既知の制限

### 運用上の注意

- **公開サインアップのメール認証** — `emailRequiredForSignup` 有効時の pending user → 確認メール → promote まで実装済み。**SMTP を配線していないと確認メールが飛ばず、登録が完了できない**。`email` 設定を入れるか、`emailRequiredForSignup` を無効にすること
- **サーバーマシン統計** — `enableServerMachineStats` 有効時に gopsutil で CPU / メモリ / ディスク / ネットワークを 2 秒間隔で収集する。無効のあいだは収集も配信もせず、それまでの履歴も返さない。切り替えは再起動を待たずに次の収集から効く (本家は起動時にだけ見る)。**コンテナで動かしている場合、既定では host の値が返る** (gopsutil は cgroup の制限値ではなくホストを見る)。コンテナに割り当てたリソースを見たい場合は別途 cgroup を読む必要がある
- **search backend** — `notes/search` の provider は `fulltextSearch.provider` で切替。既定 `sqlLike` で **Meilisearch 不要のまま動く** (`lower(text) LIKE` による部分一致。**ILIKE ではない** — pg_bigm の GIN index が効かなくなるため)。upstream TS strict-mode (400 UNAVAILABLE) で揃えたい operator は `provider: "none"` を opt-in で選べる (#877)。Meilisearch / pgroonga は optional
- **promo は作成できても表示されない** — `admin/promo/create` は 204 を返し DB 行も増えるが、**利用者へ提示する経路が upstream にも Elythia にも無い** (#2781)。告知・露出の機能として使えると期待しないこと。詳細は [api-compatibility.md](api-compatibility.md) の「既知の制限」
- **upstream 2026.10.1 まで追従済** — 2026.3.2 → 2026.5.1 → 2026.5.4 → 2026.6.0 → 2026.7.0 → 2026.9.0 → 2026.9.1 → 2026.10.0 → 2026.10.1 と段階的に追従した (**2026.8.0 に stable は無い**)。各 release の差分は [docs/update/](update/) を参照 (`<yyyymm><nn>diff.md`)

差分の網羅的な一覧は [divergence.md](divergence.md) を参照。

### Elythia 独自挙動 (TS にない拡張)

- **リモートユーザー counts**: `users/show` でリモートユーザーの notesCount / followersCount / followingCount を origin instance の `/api/users/show` から取得して上書き (#943)。TS は自インスタンス観測値のみ表示するため数値が小さくなる問題を解消。フォロー一覧 / フォロワー一覧は引き続き local user のみ
- **mediaproxy アニメ pass-through**: GIF / APNG をリアクション / 絵文字ピッカー / プレビューで静止化せずに pass-through (#941)
- **URL preview の charset 自動正規化**: Shift_JIS / EUC-JP / ISO-2022-JP 等のページで title / description が文字化けしない (#942)
- **inbox handler の verify-in-worker 化** (#565): HTTP 受信スループットが TS の 2.6-2.8x

### Misskey-TSとの差異

- **タイムライン** — Redisキャッシュが空の場合 (サーバー再起動直後等) はDBクエリにフォールバックする

## トラブルシューティング

### ページが「Loading...」のまま進まない

- フロントエンド資産のパスが正しいか確認する
- 方法Aの場合: `ls frontend/built/_frontend_vite_/manifest.json`
- 方法Bの場合: `ls $MISSKEY_FRONTEND_DIR/manifest.json`

### スタイル/CSSが崩れる

- プロダクションビルドを使用していることを確認する (Viteのdevモードではない)
- `manifest.json` のエントリにCSSファイルが含まれていることを確認する: `cat frontend/built/_frontend_vite_/manifest.json | grep css` (方法Bは `$MISSKEY_FRONTEND_DIR/manifest.json`)

### 絵文字が表示されない

- twemoji SVGファイルが配置されているか確認する
- 方法A: `ls frontend/node_modules/@misskey-dev/emoji-assets/built/twemoji/1f44d.svg`
- 方法B: `ls $MISSKEY_TWEMOJI_DIR/1f44d.svg`

### favicon/アイコンが表示されない

- static assets のパスを確認する
- 方法A: `ls frontend/assets/icons/192.png`
- 方法B: `ls $MISSKEY_STATIC_DIR/icons/192.png`

### 再起動後にタイムラインが空になる

- これは期待通りの挙動。新規ノートは即時にタイムラインへ反映される
- 既存ノートは初回のDBフォールバッククエリ実行後に表示される
- **`enableFanoutTimelineDbFallback` が off だと表示されない** (global timeline
  を除く。あちらは対象外)**。** セットアップ
  ウィザードで「1 人用」以外を選んだインスタンスは既定で off になっている。
  Redis の timeline list が空の状態 (= 移行直後) で最も影響が大きい。詳細は
  [divergence/operations.md §5.6](divergence/operations.md#56-timeline-の-db-fallback-を止めるつまみ)

### チャットを全部読んだのに、再読込すると未読のバッジが出る

- 移行の前に受け取ったDMが未読のまま残っている。[5.5. チャット(DM)の既読を引き継ぐ](#55-チャットdmの既読を引き継ぐ)の`backfill chat-reads`を流す
- チャットのホームの一覧は新しい10会話から出す。未読の会話がそれより古いときは、一覧の下の「もっと見る」で続きを読む(#3653)

### ファイルアップロードが `CREDENTIAL_REQUIRED` で失敗する

- 認証ミドルウェアが `multipart/form-data` リクエストを正しく処理できているか確認する
- サーバーログで認証エラーを確認する

## 関連ドキュメント

- [docs/playwright.md](./playwright.md) — Playwright による frontend / API の e2e (Elythia backend と Misskey TS backend の両方に対して実行する)
- [docs/upstream-catch-up.md](./upstream-catch-up.md) — Misskey TS upstream の新 release を Elythia に取り込む際の triage / Wave 単位 PR 運用と、取り込んだ版へ上げた後の追従手順
