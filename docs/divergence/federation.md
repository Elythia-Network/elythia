# 純正 Misskey との差分: ActivityPub (連合)

[差分カタログの目次](../divergence.md) にある分類と基準 (Elythia と追従している本家の版) は、このファイルにも当てはまる。

## 3. ActivityPub

### 3-3a. リモート actor の `suspended` を読む (Elythia 独自)

**upstream は読まない。** `ApPersonService` / `type.ts` に `suspended` の参照は無く、
`ApRendererService` も出力しない。Elythia は**読むだけ**で、こちらからは出さない
(`RenderPerson` は設定しないので wire shape は変わらない。`TestRenderPerson_SuspendedIsNeverEmitted`
で固定)。

**理由は、凍結したときの合図がファミリーで違うこと。**

| 発信元 | 凍結したときの合図 | 読まないと起きること |
|---|---|---|
| Misskey 系 | `Delete` を送る (`UserSuspendService.postSuspend` が `renderDelete` を全 sharedInbox へ) | `isDeleted = true` になる (対処済み) |
| Mastodon 系 | actor に `toot:suspended` を立てて `Update(Actor)` を push + collections が 403 | **何も起きない** |

**結末は Misskey 系と同じにはならない。** 立つ列も残るデータも違う:

| | Misskey 系 (`Delete` 受信) | Mastodon 系 (この機能) |
|---|---|---|
| 立つ列 | `isDeleted` | `isSuspended` |
| ノート・ドライブ・following 行 | **削除される** | 残る (読み取り時に隠れるだけ) |
| 保留中のフォローリクエスト | 残る | **残る** (`cleanupSuspendedUserRelations` は admin 経路だけが呼ぶ) |
| その人宛のローカル利用者の返信・リノート | 表示されたまま | **timeline から消える** |

最後の行は方向が逆になる。`applySuspendedAuthorExclusion` が見るのは `isSuspended` だけで
`isDeleted` を見ないため、**この経路のほうが巻き添えが大きい**。つまり
「リモートの moderation 判断が、こちらの利用者が書いたノートの表示可否を決める」経路が
新しくできる。データは消えないので凍結を解けば戻るが、下記の制約がある。

**自己申告に限られる。** actor document を出したホストと `actor.id` のホストの一致は
`fetchActor` が必須にしているので、第三者が他人に `suspended` を付けることはできない。

**凍結の由来を持つので、モデレーターの判断はリモートに巻き戻らない** (#2973)。
`user_suspension_origin` に `local` / `remote` を記録する。**Mastodon を出発点にしつつ、
解除側も記録する点だけ厳しくしてある** — Mastodon の `unsuspend!` は
`suspension_origin` を `nil` に戻すので、モデレーターが解除しても発信元が立て直せば
再凍結される。Elythia は解除も `local` として刻むので、その巻き戻しが起きない
(この乖離の目的がまさにそこなので、原典より厳しい側に倒している)。判定の骨格は
`ProcessAccountService#set_suspension!` と同じ:

- モデレーターが凍結・解除した行 (`local`) には**触らない**
- こちらが actor を見て凍結した行 (`remote`) は、発信元の解除にも**追従する** (two-way)
- **記録が無い行は `local` 扱い** — この表より前から凍結されている行をリモートに解除
  させないため。誤って `local` にしてもモデレーターが手で戻せるが、逆は回復しにくい
- **由来を記録できないなら凍結もしない** (未配線 / 書き込み失敗)。記録の無い凍結は
  次の refresh で「由来不明 = local」になり、解除できるかどうかが運任せになる
- **由来の読み取りが失敗したら触らない** — 「記録が無い」と取り違えると、接続断の
  瞬間にモデレーターの判断を上書きしうる

**`user` に列を足さず別テーブルにしてある** (`relay_observed_user` / `signup_application`
と同じ判断)。TS は未知の列も無視するので列追加でも復路は壊れないが、別テーブルなら
TS 側から一切見えない。自動での変更は moderation log に載らないので `slog.Info` を出す。

**drop-in で往復すると、`remote` の行だけ古くなる。** TS は由来を知らないので、TS 稼働中に
モデレーターが「発信元がまだ凍結を主張しているリモート利用者」を解除し、その後 Elythia へ
戻すと、行は `remote` のままなので**次の refresh で再凍結される**。`local` が付いている行は
守られるので、影響は「こちらが actor を見て凍結した行」に限られる。

**読めない形は `false` に倒す** (`APLenientBool`)。`APTruthyBool` だと `[]` / `{"a":1}` /
`"maybe"` のような壊れた値が `true` になり、**誤って凍結する** (差が出る入力を実測で確認)。
真と読むのは PostgreSQL の boolean 入力構文 (`"true"` / `"yes"` / `1` など) なので、
実際に `suspended` を出す Mastodon が送る素の JSON `true` より広い。片方向で巻き戻せない
操作としては広い側だが、コードベースの他の AP boolean (`isCat` / `discoverable`) と
判定を揃えてある。

**キー名だけで読み、`@context` は見ない。** 他の AP boolean も同じで、upstream も同じ。
意味の衝突が無いことを Mastodon (凍結時のみ出力) / Akkoma (出力なし) / Misskey (参照なし)
で確認した。Pleroma / GoToSocial は未確認。

### 3-4. リモート note と antenna

**upstream は載せる。** `ApInboxService` の Announce 経路は `fromRelay` 分岐で
renote を作らずに publish するが、その手前で `apNoteService.resolveNote(target)`
を呼ぶため `ApNoteService.createNote` -> `noteCreateService.create` ->
`addNoteToAntennas` (無条件) を通る。Create 転送型のリレーも同じく
`NoteCreateService` を通る。ただし `fetchNote(uri)` で既知なら早期 return する
ので、載るのは初回観測時だけ。

**Elythia はリレー経由だけ外す** (#2743)。直接配送の inbound Create / Announce は
載せる。判定は 2 系統あり、どちらか一方でもリレーなら発火させない。

| 形態 | 判定 |
|---|---|
| Misskey 系リレー (リレー actor 自身が Announce) | `handleAnnounce` の `viaRelay` (announcer が relay actor か) |
| Mastodon 系リレー (元の Create / Announce を転送、署名だけリレー) | `isRelayDelivery(signer)` |

外す理由は 2 つ。

1. **`enableEphemeralRelayNotes` が有効なとき、リレー投稿は DB に行を持たない**
   (Redis に TTL 付きで置く、#2332)。antenna service は ephemeral note を
   **あえて materialize しない**方針なので DB が膨らむことはないが、
   `pushNote` は先に走るため **DB から引けない ID が antenna の ZSET (上限
   200) を埋める**。読み取り側は DB しか見ないので、幽霊 ID のぶんだけ本来
   載る note が押し出される (#2719 と同じ構造)。**この設定は既定 `false`**
2. 量が最も多いのがこの経路。`ListAllActive` は #2752 でキャッシュしたので DB
   クエリは消えたが、`matchNote` はアクティブ antenna 全件に対して評価され、
   ヒットすれば Redis への push が走る。リレーの firehose 全量に載せると
   コストが読めない

したがって既定構成では 2 番目だけが理由になる。**リレー投稿を antenna で拾いたい
運用がある場合は、この判断を見直す余地がある。**

inbound Announce では **renote 行に加えてブースト対象 (target) も** antenna に
渡す (#2751)。renote は text を持たないので、target を渡さないとキーワード指定
または `withFile` の antenna は 1 件もマッチしない。upstream も
`ApInboxService.ts:329` が `resolveNote(target)` 経由で target を
`NoteCreateService` に通すので同じ。

target 側には**新規に取り込んだときだけ**という gate がある (upstream も既知の
note では `createNote` を通らない)。無条件だと再ブーストのたびに古い note が
antenna に湧く。あわせてローカル note は対象外にする (作成時に既に通っている)。

**この gate は「他の経路で先に DB へ入った note」を全部落とす。** upstream では
それらの経路も `resolveNote` → `createNote` → `NoteCreateService.create` を通り、
`addNoteToAntennas` は `silent` の外にあるので**その時点で antenna に載っている**。
Elythia で載らないまま残るのは、次の経路で先に取り込まれた note がブーストされた
場合:

- リレー配送 (上記の方針で antenna に載せていない)
- featured collection の取り込み
- 他 note の reply / quote target としての解決
- `Add` (pin) の対象解決 — upstream も `resolveNote` なので同型
- `/api/ap/show` などの手動解決
- **`Like` / `Undo(Like)` / `Undo(Announce)` の対象解決** — ここは Elythia 固有。
  upstream は Like で未知 note を DB に作らない (`fetchNote`) ので、後続の
  Announce が `resolveNote` → `createNote` → antenna を通る。Elythia は Like の
  時点で行ができるため、後続の Announce では `created=false` になり載らない。
  Like は Announce より高頻度なので、実効の取りこぼしは他の入口より大きい

数え方は「`ResolveNote` が既存行を返しうる入口」= `grep 'ResolveNote(' internal/`
の非テスト call site。リレー以外は upstream でも antenna に載る経路なので、
**差は「その入口で載せていない」ことと掛け算になる**。

なお inbound Create / Announce **以外**の取り込み経路 (featured collection、
reply / quote target の解決、`/api/ap/show`) でも antenna に載せない。これは
fanout hook と同じ扱いで、過去の note が突然 antenna に湧くのを避けるため。

Mastodon 系リレーが転送した Announce は、**従来どおり renote を作って timeline
には流れる**。antenna に載せないだけで、renote 抑止の判定は変えていない。

### 3-1. reversi / chat の連合まわり

cherrypick 由来の拡張が中心。比較のため関連する upstream 標準機能も併記する。

| 項目 | 実装 | 内容 |
|---|---|---|
| **reversi 連合対戦** | `core/reversi/federation.go` | 固定 `GameTypeUUID = 1c086295-...` を持つ独自 AP object `{type:"Game", game_type_uuid, extent_flags, game_state{...}}`。`game_state.type` は settings / ready_states / putstone |
| reversi 盤面 CRC32 | `core/reversi/game.go` / `service.go` / `api/reversi/handler.go` | DB カラムと `reversi/verify` は **upstream 標準** (`MiReversiGame.crc32`)。ただし **packed game に `crc32` を載せるのは Elythia (cherrypick 系統) 側の拡張** — upstream の `ReversiGameEntityService.packDetail` / json-schema には無い |
| reversi の pack 粒度 | `api/reversi/handler.go` | upstream の `reversi/games` は Lite (`packLiteMany`、`form1`/`form2`/`logs`/`map` を含まない) を返すが、Elythia は cherrypick 系統 + 連合拡張を持つため**全 endpoint で Detailed 相当の `packGame` を共有**する (#2106 L15)。上記 crc32 と併せて reversi は vanilla golden gate の対象外 ([shape-drift.md](../shape-drift.md)) |
| reversi 受信 dispatch | `core/federation/reversi_inbox.go` | `invite` / `join` / `leave` を受信。Invite 受信時にローカル game 行を自動作成、session→gameID は Redis mapping。**招待を受ける側は純正 frontend でも表示できる**ので、fork 側の変更は招待を出す側 (対戦相手選択) のみ (#2270) |
| `reversiVersion` (nodeinfo) | `api/nodeinfo/handler.go` | CherryPick 側がメジャーバージョン一致で連合可否を判定するため 1.1.x を維持 |
| **1-on-1 chat 連合** | `activitypub/renderer.go` / `core/chat/service.go` | DM を `Create + Note(_misskey_talk: true)` で配送。未対応実装では単なる Note として黙殺される設計。**純正は `core/ChatService.ts:381-384` で remote 配送がコメントアウトされており federation しない**ため、純正 frontend は remote を一律ブロックしていた (fork 側で解禁、#2270) |
| **group chat room 連合** | `activitypub/renderer.go` / `core/federation/chat_room_inbox.go` | chat room を AP `Group` object (`https://host/chat/rooms/{id}`) として Invite / Accept / Reject / Remove。room owner が local の場合のみ Invite を署名配送する (remote owner の room は秘密鍵が無い)。**room の身元は URI (#2994)** — `chat_room` に `host` / `uri` を足し、リモート room の行の `id` はこちらで採番する。upstream の `MiChatRoom` は `@PrimaryColumn() id` なので PK は変えられず、`(id, host)` の複合キーにもできない (`host` が NULL のままでは PK に入れられず、複合 FK は `MATCH SIMPLE` で NULL を素通りするのでローカル room だけ整合性が消える)。`note.uri` / `chat_message.uri` と同じ列の足し方なので drop-in は壊れない |
| `_misskey_canChat` | `activitypub/types.go` / `core/federation/resolver.go` | chat 連合の capability flag。欠落時は everyone 扱い (chat 非対応実装を "none" に倒すと送信前 reject で UX が悪化するため、安全側ではなく寛容側に倒す判断)。`false` は `user.chatScope = "none"` に落とし、fork frontend はこれを見て「相手が受け付けない」表示にする |

### 3-2. Ed25519 / FEP-521a (Elythia 独自)

| 項目 | 実装 | 内容 |
|---|---|---|
| Multikey encode/decode | `activitypub/multikey.go` | `z` + base58btc(`0xed 0x01` ‖ 32 byte) の FEP-521a / W3C VC Data Integrity 形式 |
| `assertionMethod[]` 出力 | `activitypub/renderer.go` | Ed25519 鍵を持つ local user に `#ed25519-key` fragment の Multikey を expose |
| 受信側 capability 判定 | `core/federation/deliver_service.go` | `user_publickey_extra` に Ed25519 行があれば Ed25519 署名。未配線 / DB error / 行なしは**すべて RSA へ安全側 fallback**。TTL 5min cache + singleflight |

e2e は `make dropin-fedibird-test` (Fedibird-like mock との双方向 Ed25519 verify)。

### 3-3. その他

| 項目 | 実装 | 分類 |
|---|---|---|
| `RemoteStatsFetcher` | `core/federation/remote_stats.go` | **Elythia 独自**。remote user の notesCount / followersCount / followingCount を origin の `/api/users/show` から取得。LRU 10000 / positive TTL 1h / negative 5min、SSRF guard 経由、失敗時 silent fallback。差し替えるのは単体の `users/show` と、`users/followers` / `users/following` に埋め込む利用者 (#1146)。差し替えた値にもカウントの公開範囲のゲート (`GateCountVisibility`) を掛け直す |
| inbox admission | `activitypub/inbox_admission.go` | **upstream と同等** (`ActivityPubServerService.inbox` も 4 header 要求 + Host 一致 + SHA-256 照合を実施)。Elythia 固有なのは body 照合を定数時間比較にしている点のみ |
| 転送 activity の LD-Signature 検証と compact | `core/federation/ld_signature_verifier.go` / `queue/processors/inbox.go` (`authorizeActor`) | **upstream と同等** (`InboxProcessorService.process`)。HTTP 署名者と `actor` が食い違う転送経路では、`signature` を外して upstream と同じ `CONTEXT` (`ld.InboxCompactContext`) へ compact し、**compact 後の文書で検証して、その文書を handler へ渡す**。以前の Elythia は compact せず生 body を検証・処理していたため、**署名が覆っていないキーを転送者が足せた** — AS2 context は `"@vocab": "_:"` なので、context で定義されていない語 (Mastodon の文書に足した `_misskey_content` / `_misskey_summary` / `quoteUrl` など) は URDNA2015 で blank node 述語になって捨てられ、署名に含まれない。handler は生の JSON キーを読むので、Mastodon 利用者の署名付き Create / Update に `_misskey_content` を足して自分の HTTP 署名で転送するだけで、**その利用者名義のノート本文を偽造・書き換えできた**。compact 後の文書では署名外の述語は `_:<name>` というキーで残り、handler からは見えない。compact は RDF として同値な変形なので、検証を compact 後の文書に掛けることで「渡す文書 = 署名された内容」を検証そのものが保証する。**upstream と同じく、CONTEXT に無い語は完全 IRI で残って読めなくなる** (Mastodon の `toot:blurhash` / `toot:focalPoint` など。Elythia の送信用 context が `misskey:quoteUrl` に割り当てている `quoteUrl` も、upstream の `as:quoteUrl` とは別 IRI なので残らない — 引用は `_misskey_quote` でも届く)。**通常経路 (署名者 == actor) は生 body のまま** — HTTP 署名の digest が body 全体を覆うので不要で、upstream もこの経路では compact しない。**forbidden directive の検査は compact 後の文書にも掛ける** — 生の文書の検査はキー名しか見ないので、inline context で `"g": "@graph"` / `"inc": "@included"` と別名を付けると素通りするが、compact がそれを `@graph` / `@included` に戻す (upstream も compact 後に検査する)。**HTTP 署名ヘッダの無い legacy 経路も、LD-Signature を検証したら compact 後の文書を渡す** (Elythia 独自の経路で upstream に対応物は無い。本番の inbox handler は Signature ヘッダを必須にして Headers を詰めるので到達しないが、検証済みとして扱う以上は生 body を渡さない)。**残る差 (#2106 L49)**: Elythia の loader は preload 済みの 3 context (AS2 / security v1 / identity v1) しか解決せず fetch しないので、それ以外の remote context を参照する転送 activity は compact 段で `ErrCacheFrozen` になり拒否される (upstream は fetch して受理する)。あわせて freeze を compact の**前**に置いている (upstream は compact の後。fetch しない以上結果は同じで、将来 fetch を足したときに compact が無防備に fetch しないようにするため) |
| 軽量 JSON-LD 正規化 | `activitypub/jsonld.go` | Elythia 独自実装。json-gold のフルパイプラインを避け、Mastodon 系 prefix / IRI 直記述 / type 配列 / 言語マップを canonical 短形式に揃える。CherryPick group chat 用 `@context` は破棄せず保持 |
| Collection unroll 制限 | `core/federation/processor.go` | 安全側。深さ 1、item の host 一致を要求 (spoofing 防止)、URI 文字列 item は fetch 増幅回避で skip |
| 受信したノートの DB に無いメンション先・宛先の actor | `core/federation/resolver.go` (`fetchUnknownMentionActors`) | **upstream と同じく取りに行く (上限付き)** (#3330)。upstream は Mention tag の href (`ApMentionService.extractApMentions`) と `to` / `cc` から public と投稿者の followers を除いた残り (`ApAudienceService.parseAudience`) を、全件 `resolvePerson` で取り込み (2 並列、失敗は無視)、取り込みでは featured も取る。Elythia の違い: (1) **直列に取る** (resolveChain は 1 つの木を 1 つの goroutine が触る前提なので、同じ木の中で並列にすると待ちの循環を検出できない)。(2) **1 ノートで取りに行く未知の actor は、投稿者の `mentionLimit` ポリシーの数まで** (既定 20。取り込みの上限判定と同じ値を 1 回だけ引く。それより多く解決できたらどのみちメンション数の上限で弾かれる) で、**全体の予算は 20 秒** (過ぎたら次を始めない。`notes/create` のメンションの取得と同じ長さ)。**取りに行くノートは inbox の worker 全体で同時に 2 件まで** (`inboundMentionFetchSlots`) で、枠が取れなければ取りに行かず DB の照合だけにする (この経路の以前の挙動) — 応答しないホスト (ワイルドカード DNS の別サブドメインなど) へのメンションを並べたノートを送り続けても、外部の応答待ちで止まる worker は 2 つまで。**取得に失敗した URI と、接続・名前解決・TLS の失敗やタイムアウトが出たホストは 10 分覚えて飛ばす** (ホストを覚えるのは、そのホスト自身へのリクエストが失敗したときだけ。redirect 先での失敗や redirect の上限などは URI だけを覚える。http(s) 以外の href はそもそも取りに行かない) (`inboundMentionFailureTTL`。ノートをまたいで効く。一時的な障害でも 10 分は取り直さない)。(3) Mention tag の href の種類が投稿者の `mentionLimit` を超えるノートは上限で弾かれるので何も取りに行かない (Mention も宛先も無いノートでは policy を引かない)。(4) **新しく取り込んだ actor の featured は取らない** (1 配送からメンション数 × ピン留め数の取得になるため。次に actor を取り直したときに埋まる)。(5) 連合しないホスト・自ホスト・DB にある actor は取りに行く前に飛ばす。(6) **宛先 (`to` / `cc`) は specified のノートのときだけ取る** — upstream は公開ノートの宛先も取り込むが、使うのは specified の `visibleUsers` だけで、公開ノートでは行が増えるだけ。(7) **取るのは入口の取り込み (inbox の Create、`ap/show` などの depth 0) だけ** — 引用先・返信先・featured のピンとして入れ子で取り込むノートと、リレー由来の ephemeral なノート (DB に載せない) では取らない。ノートの `Update` (upstream に対応物が無い) も DB にある actor だけを見る |
| `published` の異常値 fallback | `core/federation/published_time.go` | Elythia 独自 hardening。clock skew 5min / 過去 10 年 floor |
| featured (ピン留め) の取り込み | `core/federation/featured.go` | **upstream と同等** (`ApPersonService.updateFeatured`) で、actor の新規取得時と更新時に取り込み、上限 5 件・既存を全置換。差分は 4 点 (#2552 / #2684)。うち (1)-(3) は安全側、(4) は取り込みが遅れる方向。(1) upstream は items を**全件**解決してから Note に絞るが、Elythia は走査を 50 件で打ち切る (巨大なコレクションを置くだけで取得を増幅させられるため。得られるピン留めは同じ)。(2) 著者が actor 本人であることを要求する (upstream は見ないので、他人の投稿を自分のプロフィールに並べられる)。(3) 個々の item の解決失敗を読み飛ばす (upstream は `Promise.all` なので 1 件でも失敗するとピン留めが 1 件も入らない)。(4) **いま取り込み中の投稿は skip する** (#2684 / #2686)。著者が自分の投稿をピン留めしていると、featured の解決がその投稿自身を要求する形になる。**入口によって壊れ方が違う**: `ResolveNote(A)` 経由だと note の singleflight が自分の in-flight entry を自分で待って**永久に止まり** (#2684)、inbox 直送 (`IngestNoteWithCreated`) 経由だと同じ note をもう一度 fetch して内側の ingest が先に行を作り、外側の `Create` が UNIQUE に当たって `created=false` になる — 呼び出し側がそれで通知とチャートのフックを飛ばすので**言及・返信の通知が黙って消える** (#2686)。upstream も `Resolver.history` で同じ形の再解決を弾くが、throw が `Promise.all` を reject するので `updateFeatured` ごと落ちて既存のピン集合が残る (all-or-nothing)。Elythia はその 1 件だけ落として残りを反映する。判定は**解決チェーンに閉じた台帳** (`resolveChain`) で行う (#2685)。以前は Resolver に `sync.Map` を 2 つ置いていたが、プロセス全体で共有されるため「自分の祖先が握っている」(待つと自分を待つので解けない) と「無関係な goroutine が握っている」(待つのが正しい) を区別できず、後者も諦めていた。その結果、別の worker が同じ引用先を取り込んでいる最中に引用元が来ると **`renoteId` を落としたまま保存**していた (再取り込みは `FindByURI` で早期 return するので恒久的に失われる)。upstream の `Resolver.history` が activity ごとに作られる Set なのはこのため。チェーンは鍵 → document id の写像で、singleflight の鍵 (取得 URI) と正規化後の id の両方を持つ。**チェーンに閉じた判定だけでは cross-goroutine のデッドロックを防げない** — プロセス全体の台帳だった頃は「他の goroutine が握っていたら諦める」ことで意図せずそれも防いでいたので、待つようにすると相互に引用し合う 2 投稿を 2 worker が同時に解決したときに待ちが循環し、待ちを打ち切る手段が無ければ両方が永久に止まる。そこで待つ直前に wait-for グラフ (`core/federation/resolve_waits.go`) を辿り、**循環になる場合だけ**諦める。循環でない待ちは待って引けるので、renoteId 欠落は戻らない。**グラフには note と actor の両方の in-flight を載せる** — actor の解決は他の actor を待つ (`processRemoteMove` が移行先を解決するので、**互いを `movedTo` に指す 2 つの actor** を 2 worker が同時に取得すると actor どうしで循環する) し、note の解決も著者解決で actor を待つので、待ちの辺は 2 つの group をまたぐ。片方だけモデル化すると、もう片方で待っているチェーンが「走っている」と見えて循環を見逃す。note → actor → note の形は featured が待たなくなったので現状は作れないが、**待つ経路が 1 つ増えれば再び成立する**ので、それに依存して actor 側を外さない。**検出には保険を付けてある**: 待ちには上限 (5 分) があり、モデルに載っていない待ちで循環しても永久には止まらない。**上限は join ごとではなく解決木ごとの合計**で、木の全枝が 1 つの予算を共有する (join ごとにすると、著者・返信・引用と待つ回数だけ積み上がる)。**予算は「待ちに費やした時間」で減る** — 根で `now + 上限` の期限を打つ形にすると fetch のような待ち以外の作業でも減り、上限より長くかかる解決の途中で正当な待ちに出会うと 1ms も待たずに諦めることになる。この上限のために `singleflight` ではなく自前の group (`core/federation/resolve_group.go`) を使う (`singleflight.Do` は待ちを打ち切れず、`DoChan` は fn の panic を**意図的に recover 不能な形で**別 goroutine へ飛ばすのでプロセスごと落ちる)。**featured の取り込みは最初から待たない** — best-effort な経路が待ちの辺を張ると、(1) その間 actor の鍵を握り続け、(2) その辺が循環に見えたときに**本命の note の解決**が代わりに弾かれる。待たずに既存行へ落とすので、プロセス全体台帳だった頃と同じ挙動になる。**待たないのは枝ごと**で、その解決から下 (取り込む投稿の著者 actor の解決など) も一切待たない。相乗りする瞬間だけ待たない形にすると、自分が先頭になったときに内側で待ってしまう。**取得 URI と id が食い違う別名 URL** (featured が `/@user/x` を載せていて document の id が `/notes/x`) では、取得 URI で引く手前の判定は空振りする。id は fetch しないと判らないので、`resolveNoteOnce` が**取得したあとに**同じ判定をやり直す (#2695)。したがって**この形では**二重取り込みが起きず、inbox 直送の `created` も落ちない (別名がからむもう 1 つの形については後述)。ただしピン自体はその回落ちる (正規形と同じで、次の actor 更新で拾い直す)。**この取得後の判定は featured の取り込みから入った呼び出しにだけ効かせる** — best-effort の印は枝ごと引き継がれるので chain の印で判定すると featured の内側で走る引用解決にも効いてしまい、ピンが取り込み中の投稿を別名 URL で引用しているだけで `renoteId` が恒久的に落ちる。入口が何だったかは `resolveNoteDepthOpt` の `mayWait` が持っているので、それを引数で渡す。**その代償は `created`** — ピンが取り込み中の投稿を**別名 URL で引用**している形 (featured には正規 URI で載っている) では引用解決がこの判定を素通りして内側で先に行を作るので、外側の `Create` は UNIQUE に当たり `created=false` のままになる。#2686 の通知欠落はその形では残る。`renoteId` の恒久的な欠落のほうが重いので意図してそちらを取っている。skip する前に既存行を引く (`ReplaceByUser` が delete-then-insert なので、落とすと集合ごと書き直して生きたピンが消える) ので、**取得 URI か確定した document id で行が引ける限り、既に取り込み済みのピンは消えない**。これは in-flight で skip する枝だけでなく、**解決がエラーで落ちた枝にも掛ける** (待ちを断った `ErrResolveWouldBlock` はここを通る)。ただし別名 URL でここまで来て引けるのは、`resolveNoteOnce` が probe (fetch と id の確定) まで到達した場合だけ。待ちを断った枝 (best-effort な枝は `onJoin` が必ず `ErrResolveWouldBlock` に上書きするので、この 1 つだけ) や fetch 失敗では id が判らないので取得 URI でしか引けず、**別名 URL の生きたピンは依然として落ちうる** (#2695 で残した穴)。ピンが落ちるのは既存行を引けなかった場合で、通常は行がまだ無い初回。いずれも次の actor 更新で拾い直される (actor TTL 既定 24 時間)。またノート解決は depth 1 から始め、**その内側で作られた actor では featured を引かない** (引用先 → その著者 → その featured と入れ子になると 1 段ごとに 5 分岐する取得の連鎖になる) |
| outbound User-Agent | `config/config.go` | `Elythia/<ver> (<url>)` (#3394 より前は `mk-go/<ver> (<url>)`)。画像プロキシは自身の UA を再帰として弾くので、旧名も見続ける (`api/proxy/handler.go`)。**逆向きは直せない** — 改名より前の版は `Elythia/` を自身の UA と見なさず、nodeinfo の `elythia` を絵文字のメタ情報の取得先とも見なさない |
| AP object id の https スキーム非強制 | `core/federation/resolver.go` | **意図的な未実装** (#2507)。upstream の `checkHttps` は非 https の object id を reject する (テスト環境除く)。Elythia は id/attributedTo の host 一致 + SSRF guard で検証するがスキームは見ない。http ベースの e2e stack (dropin / federation) が前提のため、強制するなら upstream 同様の環境ゲートが要る。ブラウザ / AP クライアントは非 https の Location を追わないため実害は限定的 |
| リモート AP document の単一値 / 配列表現の許容 | `activitypub/types.go` | **upstream 同等 (一部は緩い方向)** (#2662)。対象は `type` (配列 → 先頭。`tag` / `attachment` の**要素**の type も含む)、`attachment` / `tag`、collection の `items` / `orderedItems` (単一 object → 1 件)、`to` / `cc` / `alsoKnownAs` (単一値・`{id}` 要素)、`inReplyTo` / `attributedTo` / `outbox` / `followers` / `following` / `sharedInbox` / `endpoints.sharedInbox` / `featured` / `movedTo` (`{id}` object・配列の先頭)、`url` (`{href}` object・配列の先頭)、`endpoints` / `source` / `icon` / `image` が object でない場合 (空として扱う。`icon` / `image` は 1 つの field が読めなくても読めた分は救う)、`assertionMethod` (単一 object・bare IRI 参照・要素ごとに decode)、`summary` / `name` / `publicKey.publicKeyPem` / `_misskey_*` 拡張 (非 string / 非 bool でも document は通す。JSON-LD の展開形は剥がして値を拾う。`publicKeyPem` は**空になった値で既存の鍵を上書きしない**ようにしてある — 上書きするとその actor からの署名検証が恒久的に失敗する)、Question の選択肢 `type` / `replies` (IRI 参照でもよい) / `replies.totalItems` (`3.0` / `"3"` も整数として読む)、`_misskey_quote` (`{id}` object)、`isCat` / `discoverable` / `manuallyApprovesFollowers` / `sensitive` / `_misskey_*` の bool が bool でない場合 (**PostgreSQL の boolean 入力構文で読む**。upstream が生値を代入する field は TypeORM の丸めが効かず PostgreSQL がキャストするので、`"true"` は true、`"false"` / `"0"` / `"no"` / `"off"` は false。**JS の truthy にはしない** — 「空文字以外は true」にするとこれらが軒並み反転する。数値も同じ扱いで、`node-postgres` は `String(val)` で送るので有効なのは `1` / `0` だけ (`2` は `'2'::boolean` = invalid input syntax なので「読めない」側)。**生値を代入するのは `manuallyApprovesFollowers` (→ `isLocked`) / `discoverable` (→ `isExplorable`) / Note の `sensitive` で、`isCat` は違う** — upstream は `isCat: (person as any).isCat === true` なので `"true"` でも false になる (`requireSigninToViewContents` も同じ形)。Elythia はここを他の bool と揃えて読むので**その分だけ緩い**。JSON-LD の展開形は剥がしてから判定する。**読むのは完全形と PostgreSQL が挙げる 1 文字表記 (`t` / `f` / `y` / `n`) まで** (`'tr'` / `'fals'` のような 2 文字以上の一意な接頭辞も PostgreSQL は受け付けるが、そこまでは追わない。曖昧で PostgreSQL 自身が拒否する `'o'` も読まない)。読めない形は field ごとの既定値に倒し、既定は「読めないと危険な側」で決める: `sensitive` / `manuallyApprovesFollowers` は true (隠す / 承認制)、`_misskey_canChat` は false (DM 拒否)、その他は false)、`sensitive` が bool でない場合 (**読めなければ true に倒す**。これは upstream 追従ではない — upstream は `sensitive` を `attach.sensitive ??= note.sensitive` の1 箇所でしか使わず CW は `summary` からしか作らないが、Elythia は `sensitive` が立った note に空 CW を付ける独自実装なので、false に倒すと**送信側が sensitive と宣言したノートが CW 無しで表示される**。JSON-LD の展開形 `[{"@value": false}]` は剥がしてから判定する)、`quoteUrl` (`{id}` object)。AP はこれらを「単一値でも配列でもよい」と定めており、JSON-LD compaction は `@container: @set` の無い term の単一要素配列を素の値に潰す。`@type` は逆に配列表現が正規で、compaction 後も配列で残る実装がある。upstream は `toArray` / `getApId` / `getOneApHrefNullable` で吸収するが、Elythia は Go の型で決め打ちしていたため **document の unmarshal ごと失敗し、その actor / Note がまったく取り込めなかった**。`APType` / `APObjectList` / `APRawList` / `APIDList` / `APLenientID` / `APLenientHref` で受ける (順に upstream の `getApType` / `toArray` / `toArray` (要素を decode せず `json.RawMessage` のまま持つ版) / `getApIds` / `getOneApId` / `getOneApHrefNullable` に対応)。`APLenientString` / `APLenientBool` / `APTruthyBool` / `APLenientInt` / `APLenientTimestamp` / `MultikeyList` / `Source` / `Endpoints` / `QuestionChoiceReplies` / `Image` / `Note` の寛容な `UnmarshalJSON` には upstream の対応物は無く、**JS が型を検査しないので結果的に通る**ものを Go で同じだけ通すためのもの。**inbox 経由の activity は `activitypub.Normalize` が先に `type` 配列や `{"@id": ...}` を潰す**が、actor / note / featured の生 fetch 経路は Normalize を通らないので、これらの型がその役目を負う。**Note は上の per-field の型に加えて、`Note.UnmarshalJSON` が型不一致を握って「読めた field だけ採用する」。** 後者が担うのは per-field で緩めていない `id` / `content` と `oneOf` / `anyOf` / choice の `name`。 upstream は JS なので型検査をほとんどせず (`content` は `typeof === 'string'` のガードを通って text=null のノートを作り、`oneOf` / `anyOf` が読めなくても `extractPollFromQuestion(...).catch(() => undefined)` で poll 無しのノートができる。choice の `name` が非 string のときは upstream も throw せず `filter(x => x != null)` で残すので、Elythia も空の選択肢を含む poll を作る)、この形なら call site を触らずに同じ挙動になる。**構文エラーは従来どおり弾く。** `published` は `APLenientTimestamp` が単一要素配列 / `{"@value": ...}` / epoch ミリ秒まで読む (`{"@value": ...}` は upstream の `new Date()` では Invalid Date になるので、ここは upstream より緩い)。**upstream は malformed な `published` を `isSafeT(new Date(...).valueOf())` で reject するが、Elythia は落とさず `parseAPPublishedTime` が受信時刻に fallback する** (元からの設計。ここだけ reject に倒すと upstream が受理する形まで巻き込むうえ、`encoding/json` は最初の型エラーしか報告しないので先行 field のエラーで判定が飛ぶ)。**actor 側は catch-all を使わず field ごとに緩める** (どの field を緩めたかが読めなくなるため)。`name` / `summary` は `APLenientString` にしてあるので、**upstream `validateActor` が throw する truthy な非 string (`["Alice"]` / `{"@value": "Alice"}`) も Elythia は受理する**。JSON-LD の展開形を拾うためで、値は `description` 2048 / `user.name` 128 に truncate + NUL 除去して書く。upstream も受理する falsy な非 string (`name: 0`) もこれで通る。**upstream より緩い箇所がいくつかある。** (1) actor の `attachment`: upstream `analyzeAttachments` は `Array.isArray` でない入力に `[]` を返して profile fields を捨てる (upstream 自身が TODO で疑問視している) が、Elythia は 1 件として取り込む。(2) `outbox` / actor の `url`: **Elythia はこれらの値をそもそも読まない**。型を緩めた効果は「document を落とさなくなる」ことだけで、値は捨てる。host の検査だけは upstream の `validateActor` と同じく行い、`outbox` が actor と別ホストなら actor ごと拒否する (#3330)。**`followers` / `following` は読む** — upstream の `isPublicCollection` と同じく、actor の取り込みと更新のたびに collection を取得し (IRI なら通常の署名付き fetch、埋め込みならそのまま)、Collection / OrderedCollection で `first` / `items` / `orderedItems` のどれかを持てば `user_profile` の `followersVisibility` / `followingVisibility` を `public`、無ければ `private` にする (`core/federation/collection_visibility.go`)。値が無ければ `private`。失敗の扱いも upstream と同じで、取り込み時はどの失敗も `private`、更新時は 4xx (429 を除く) なら `private`、それ以外 (ネットワーク / 5xx / 429 / 文書の不備) は保存済みの値を変えない。**取得は actor 1 件の取り込み・更新ごとに 2 回増える** (更新は actor の TTL 切れと `Update` の受信のたびで、upstream と同じ頻度)。**collection の host は actor に縛る** — upstream の `validateActor` と同じく、IRI か埋め込みの `id` が別ホストの collection を持つ actor は、取り込み・更新とも actor ごと拒否する (取得もしない、`core/federation/actor_collections.go`、#3330)。**残る差は埋め込みで `id` の無い collection** で、upstream は `getApId` が投げて actor ごと弾くが、Elythia は actor を残して collection だけ使えないものとして扱い、上の「その他の失敗」と同じ扱いにする。followers と following が同じ IRI のときは両方を取得して判定する (upstream は 1 つの Resolver の履歴を共有するので 2 本目が「解決済み」で失敗し、取り込み時は `private`、更新時は値を変えない)。2 本の取得は並行に行い、取り込み時は user 行を作る前に済ませる。**この対応より前に取り込まれたリモート利用者は、次の更新まで列の既定値 `public` のまま**になる。**実際に配送先になる `inbox` / `sharedInbox` / `endpoints.sharedInbox` は host を actor に縛ってある** (upstream の `punyHost` と同じく punycode と既定ポートだけ正規化し、**`www.` は同一視しない**。Elythia の `normalizeMatchHost` は #1820 の object-host binding 用に upstream の `normalizeSynonymousSubdomain` を取り込んで `www.` を剥がすが、upstream はそれを `assertActivityMatchesUrl` でしか使わない。配送先で同一視すると `www` サブドメインが別管理下にある環境でoutbound をそちらへ向けられる) (前者は `ErrInvalidActor`、後者 2 つは破棄)。`sharedInbox` の選択順も upstream の `x.sharedInbox ?? x.endpoints?.sharedInbox` に揃えた。**検証が無かった頃に取り込まれた既存行は直らない** — `fetchActor` が失敗するので `refreshActor` は `lastFetchedAt` 以外の列を更新せず、`user.inbox` に残った値がそのまま配送先に使われ続ける (profile / 鍵ローテーション / `movedTo` の追従も止まる)。検出は `SELECT id, uri, inbox, "sharedInbox" FROM "user" WHERE host IS NOT NULL AND (inbox IS NULL OR regexp_replace(lower(split_part(split_part(split_part(inbox,'//',2),'/',1),'@',-1)), CASE WHEN inbox LIKE 'https://%' THEN ':443$' ELSE ':80$' END, '') <> lower(host))`。**scheme ごとの既定ポート・userinfo・大文字小文字だけ正規化し `www.` は剥がさない** (剥がすと `sameDeliveryHost` が弾く行を見逃す。`:(443\|80)$` と一括で剥がすと `https://h:80/` のような非既定ポートの行を取り逃す)。実 PostgreSQL で 10 パターンを流して `sameDeliveryHost` と一致することを確認済み。punycode と Unicode IDN が混在する行、`user.host` にポートが入っている行、scheme が大文字の行、fragment 付きの inbox は偽陽性になりうる。**不正な percent escape (`%zz`) は偽陰性** — SQL では正常に見えるが `net/url.Parse` が弾いて `sameDeliveryHost` が false を返す。末尾改行 / 前後の空白 / 途中の tab は `trimWHATWGURL` が upstream の `new URL()` と同じだけ除去して**保存値ごと正規化する**ので、該当行は次の refresh で自動的に直る。**これは proxy でしかない** — 詰まるかどうかは相手が今返している document で決まるので、相手が直していれば自己回復する。**破棄の粒度だけ違う**: upstream は選ばれた 1 つを検証して不正なら両方消すが、Elythia は 2 つを独立に検証して不正な方だけ消す (残る値は host 検証済みなので安全側) (#2662)。(3) `featured` / `sharedInbox` / `endpoints.sharedInbox`: upstream の `getApId` は**配列を見ない** (`value.id` が undefined になって throw) が、Elythia は先頭を採る。`sharedInbox` 側は `validateActor` の中なので upstream では**その actor ごと reject** になる (`ApPersonService.ts:157`。`new URL(sharedInbox)` が throw する形も同じ)。Elythia は先頭を採ったうえで `sameDeliveryHost` に通し、通らなければ**その値だけ**捨てる。配送先の host 検証は同じ値に効くので緩いのは「actor を落とすかどうか」だけ。(4) `movedTo` / `alsoKnownAs`: upstream は生値をそのまま使う (`movedToUri: person.movedTo` / `toArray(person.alsoKnownAs)`) ので `{"id": ...}` 形式は一致判定に通らないが、Elythia は id を剥がすため通る。移行の認可 (`alsoKnownAsContains`) に効くが、値を publish するのは移行先サーバー自身なので権限的な穴にはならない。`to` / `cc` は要素単位で読めないものを落とす (upstream は `getApIds` が throw して Note ごと reject する)。要素を落とすと可視性は**狭い側**に寄る。`to` の `#Public` が読めない形 (`{"type":"Link","href":...}` など `id` を持たない object) で来ると、`cc` に followers があれば `followers`、`cc` も読めなければ `specified` (visibleUserIds が空なので事実上誰にも見えない) まで落ちる。document ごと捨てるより影響が小さいため採った。ただし `attributedTo` / `to` を**読めるようになったこと自体**で結果が変わる入力もある: `Create` activity の `to` が `{"id": #Public}` 形式のとき、修正前は audience の union に載らず specified だった Note が public になる (upstream 一致)。`attributedTo` の object 形式は upstream の inbox 経路 (`ApInboxService` の `actor.uri !== note.attributedTo` 生値比較) より緩いが、抽出後の値が配送 actor と一致する必要があり host 検証も同じ値を使うので偽装耐性は落ちない |
| `ap/show` が Note 化に失敗したとき | `api/ap/handler.go` | **生の AP document を `{"type":"Note","object": <raw>}` として 200 で返す** (upstream は `createNote` が失敗すれば throw / `NO_SUCH_OBJECT` で、生 AP JSON を Misskey の Note として返すことはない)。受け付ける type は upstream の `validPost` 9 種に揃えてあるので、ingest が失敗しやすい `Video` / `Event` でもこの経路に来る。frontend は `user` / `userId` / `createdAt` の無い object を掴む |
| リモートの hashtag が NFKC 展開で長くなる場合 | `misc/hashtag/extract.go` | **正規化後に 128 code point を超えたら落とす** (#2662)。upstream は note-tag 経路が正規化**前**に `filter(<=128)` するだけ、user-tag 経路には長さ判定が無いので、`㍿` x100 (100 rune) が NFKC で 400 rune に膨らんで `tags varchar(128)[]` への INSERT ごと落ちる。Elythia は落として actor / Note は取り込む |
| リモート actor の icon / banner URL の長さ | `core/federation/resolver.go` | **列に収まらなければ落とす** (#2662)。列長は upstream と同じ (`user.avatarUrl` は varchar(1024)、`user.bannerUrl` は varchar(512))。upstream も `getPublicUrl(avatar, 'avatar')` の戻り (リモート非キャッシュなら元 URL を query に埋めたプロキシ URL) を入れるので**同じ 22001 で失敗しうる**が、**upstream はそれを user 行を作った後の `update` でやり try/catch で握る**ので actor は残って画像だけ落ちる (`ApPersonService.createPerson` の avatar/banner ブロック)。Elythia は同じ INSERT に載せているため、落とさないと**その actor が 1 行も作られない**。URL は truncate すると壊れるだけなので、画像を諦めて actor は取り込む = upstream の最終状態と同じにする |
| リモート actor の `preferredUsername` の検証 | `core/federation/resolver.go` | **upstream 同等** (#2662)。`validateActor` と同じ条件 (`typeof string`、`1..128`、`^\w([\w-.]*\w)?$`) を満たさなければ `ErrInvalidActor`。素通しすると `user.username` / `usernameLower` (varchar(128) NOT NULL) への書き込みが落ち、原因の分かりにくい DB エラーになる。**この検証は refresh 経路にも効く。** 検証が無かった頃に取り込まれた「条件を満たさない既存行」は、以後 `refreshActor` が更新に失敗し続ける。取得の増幅は抑えてある (`ErrInvalidActor` なら `lastFetchedAt` を進め、鍵の取り直しは `keyFetchBackoff` で 5 分に 1 回まで) が、**その actor の profile は更新されず、鍵ローテーションにも追従できない**。既存行は `SELECT id, uri, username FROM "user" WHERE host IS NOT NULL AND username !~ '^[A-Za-z0-9_]([A-Za-z0-9_.-]*[A-Za-z0-9_])?$'` で特定できる (実 PostgreSQL で検証済み)。**Go の正規表現をそのまま貼らないこと。** PostgreSQL の ARE は bracket 内の `\w` が外側の括弧を失うので `[\w-.]` が `_`(0x5F)→`.`(0x2E) の逆順レンジになり `invalid character range` で実行自体が失敗する。並べ替えて `[\w.-]` にしても、PostgreSQL の `\w` は UTF8 DB ではUnicode 文字を含むため `日本` のような**非 ASCII username を「正常」と報告する** (Go の `\w` は ASCII なので `validRemoteUsername` は false)。文字クラスを明示するのが唯一安全。`length(username) > 128` は列が varchar(128) なので死節。`user` 行の削除はノート・フォロー関係まで巻き込むので、消すなら影響を確認してからにすること |
| リモート actor の `vcard:bday` / `vcard:Address` が string でないとき | `activitypub/types.go` | **upstream より緩い** (#2662)。upstream は TS の型が `string` なだけで実行時検証が無く、`vcard:bday` は `.match()` が TypeError になり、`vcard:Address` は非 string がそのまま `location` に代入される。Elythia は document を通す。**JSON-LD の展開形 (`{"@value": ...}` / `["x"]` / `[{"@value": "x"}]`) は剥がして値を拾い**、それでも読めない形は捨てる (表示用の付加情報でしかないため) |
| AP dereference route の一部欠落 | `server/router.go` | **保留** (#2507)。`/follows/<follower>/<id>` (Follow activity id)・`/users/<id>/likes/<id>` (Like id)・`/emojis/<name>` (emoji tag id) は外向きに広告するが dereference route が無く 404。Follow / Like の id は Accept / Undo の相関にしか使われず他実装が dereference する事例は稀、emoji は tag に inline embed 済みで dereference 不要のため。`<note URI>/activity` は #2507 で実装済み。signature の keyId (`/users/<id>#main-key`) は actor 本体の fragment なので actor route で解決され、upstream の `/users/:user/publickey` 相当は不要 |
| 通報 (Flag) の comment 書式 | `core/federation/processor.go` | **意図的**。upstream は `` `${content}\n${JSON.stringify(uris, null, 2)}` `` (2 space の pretty print、`ApInboxService.ts:576`) だが Elythia は compact。`abuse_user_report.comment` の本文だけの差で、既存の通報との一貫性を優先して揃えていない (#2665) |
| 連合のルール | `core/fedrule` / `core/federation/rules.go` | **Elythia 独自** (#3090)。詳細は §3-5 |
| メディアサイレンスのホストの添付 | `core/federation/resolver.go` (`markMediaSilencedFiles`) | **upstream と結果を揃えた安全側** (#3218)。upstream の `DriveService.addFile` と同じく、`mediaSilencedHosts` に当たるホストのリモート投稿 (取り込みと編集) の添付を `isSensitive` にする (`maybeSensitive` は検出の結果なので触らない)。**添付の行の持ち方が違うので、手当てが要る** — upstream は投稿者ごとに行を作る (`addFile` の dedup は md5 + userId) ので必ず投稿者の行に当たるが、Elythia は URL で行を再利用するので他人の行を指しうる。書き換えるのは投稿者自身の行と、持ち主のホストもメディアサイレンス対象の行だけで、**それ以外の他人の行が残れば投稿ごと空の CW で畳む** (他人の行を書き換えると URL を指すだけで他人のファイルを変えられ、書き換えずに放置すると他人の URL を指すだけですり抜けられる)。upstream に無い挙動として、再利用する投稿者自身の行 (設定する前に取り込んだ画像) と、持ち主のホストが後から対象になった行にも、再利用された時点で当てる (upstream は行を作るときにしか当てない)。**ホストの照合は後方一致** (`blockedHosts` と同じ `MatchesBlockList`) で、upstream の `isMediaSilencedHost` は完全一致なので、サブドメインにも効く (リアクションの制限と同じ、元からある差) |
| メディアサイレンスのホストの投稿の絵文字 | `core/federation/resolver.go` (`noteEmojisFor`) | **upstream と結果を揃えた (照合は後方一致)** (#3220)。`NoteCreateService` と同じく、`mediaSilencedHosts` に当たるホストのリモート投稿 (取り込みと編集) の `emojis` を空にする。絵文字の行は upstream も `ApNoteService.extractEmojis` で作るので作る。本文の `:name:` はそのまま残る。**編集の取り込みは Elythia 独自** (upstream は投稿の編集を取り込まない) で、そこでも同じく落とす。ホストの照合が後方一致なのは添付と同じ |
| 1:1 配送の inbox の選び方 | `core/federation/deliver_service.go` | **経路で使い分ける**。upstream は「フォロワーの inbox 集合を先に作り、direct recipient の `sharedInbox` が既にその中にあれば skip、無ければ個別 inbox」という 1 本の手順 (`ApDeliverManagerService.execute`)。Elythia は direct を先に送ってフォロワー側から exclude する構成なので、**フォロワー配信と重ねる経路 (`reaction` / `note delete` の hook) は `sharedInbox` を使う** — 個別 inbox にすると exclude (URL の完全一致) が効かず同じ activity が 2 通届き、逆に exclude 側へ `sharedInbox` を足すと**そのインスタンスの他のフォロワー全員に届かなくなる** (sharedInbox は 1 エントリで全員を表すため)。**1:1 だけで完結する経路 (`DeliverToUser`、specified なアンケートの Update) は個別 inbox**を使う (upstream の direct recipe と同じ)。個別 inbox を持たない行は `sharedInbox` へ倒す — upstream は `if (recipe.to.inbox === null) continue;` で skip するが、送れるなら送る方が利用者の意図に近い。そのときだけ `IsSharedInbox` が立つので、410 Gone が host 単位の gone 判定 (`MarkGoneSuspended`) へ届きうる |
| 受信した note / Announce の followers 判定 | `core/federation/audience.go` (`deriveVisibility`) | **判定は upstream と同じ** (#3330)。`ApAudienceService.isFollowers` と同じく、投稿者 (Announce は announcer) の `followersUri` (無ければ `uri + '/followers'`) との完全一致で見る。`followersUri` は actor の取得・更新のたびに保存する (`createPerson` / `updatePerson` と同じく、actor が `followers` を出さない更新では既存値を残す)。**差は保存済みの行にだけ残る**: 以前の Elythia は `followersUri` を保存していなかったので、Elythia で取り込んだ既存のリモート利用者は、次に actor を取り直すまで `uri + '/followers'` で判定する (TS 版から引き継いだ行は TS が保存した値を使う)。followers collection が `uri + '/followers'` でない実装 (Mastodon / Misskey は一致する。一致しない例は WordPress の ActivityPub プラグインや Friendica) のフォロワー限定 note は、その間 specified になり宛先のローカル利用者にしか見えない。**倒れる向きは見える範囲が狭い側**で、本来見えるべきフォロワーに届かないだけで、見えてはいけない利用者に見えることはない。actor を取り直せば (TTL 切れか `Update` の受信) 保存した `followersUri` で判定するようになる。`outbox` / `followers` / `following` が actor と別ホストの actor は、upstream の `validateActor` と同じく作成・更新とも actor ごと拒否する (`core/federation/actor_collections.go`、#3330) ので、別ホストの値が `followersUri` に入ることはない |
| ローカルの利用者の URI の解釈 | `core/federation/resolver.go` (`ExtractLocalUserID` / `flagTargetUserID` / `localUserIDFromAPID` / `LocalUserIDFromURI`)、`core/federation/processor.go` (`flagTargetUser` / `userFromAPID`) | **upstream と同じ読み方を経路ごとに使い分ける** (#3330)。(a) メンション・specified の宛先・アカウントの移行先は `ApPersonService.fetchPerson` と同じく、`{url}/` で始まる URI の最後の段を ID とする (`/users/{id}/followers` は誰にも当たらない)。**自ホストかどうかを host ではなく文字列の接頭辞で見るのも本家と同じ** — `http://` を名乗るもの・`:443` を付けたもの・host が大文字のものは `{url}/` で始まらず、本家では続く `createPerson` が host で自ホストと判定して `cannot resolve local user` で失敗するので、誰にも当たらない。Elythia も同じ結果になる。アカウントの移行先を `{url}/notes/{id}` のような `/users/` 以外の URI で指したときも、`movedToUri` に保存して配るのは `{url}/users/{id}` (canonical な actor URI) にする。(b) 通報 (Flag) は `ApInboxService.flag` と同じく、`{url}/users/` で始まる URI の最後の段を ID とする (`/notes/...` のような URI は対象にしない)。複数の利用者に当たるとき、本家は `findBy({ id: In(ids) })` の先頭を対象にし、ORDER BY が無いので順は PostgreSQL の plan で決まる (利用者の表が大きく ID が少数なら主キーの Index Scan で ID の昇順、表が小さいか ID が多いと Seq Scan / Bitmap Heap Scan で物理的な並び)。**Elythia は ID の昇順の先頭に固定する。** host と削除済みかどうかで絞らないのは本家と同じ。(c) Accept / Undo(Accept) / Reject の follower、Follow / Undo(Follow) / Block / Undo(Block) の object と、リモートの acct の WebFinger の `self` が自ホストを指すときは、`ApDbResolverService.getUserFromApId` と同じく `users` の次の段を ID とする (`parseLocalURI`。本家の `parseUri` と同じく、自ホストかどうかはホストで判定し scheme は見ず、パスだけ (`?` と `#` 以降は含めない) をエスケープされたまま割る)。自ホストで `/users/` 以外を指すものは誰にも当てない。これらの activity は、本家と同じく削除済みの利用者を引かない (ローカル・リモートとも)。Follow / Undo(Follow) / Block / Undo(Block) の object が見つからないときは、本家の `skip: followee not found` / `skip: blockee not found` と同じく ack し、DB の障害だけを retry させる (以前は `localBaseURL` の接頭辞で読んでいたので、`?` や `#` を含む URI・`http://` を名乗る URI を誰にも当てず、削除済みの利用者には当て、見つからないときは error を返して inbox の retry を使い切っていた)。(d) chat の宛先 (cherrypick 由来で本家に対応が無い) は、#3330 より前と同じく `users` の次の段を ID とする。reversi の招待と chat room の招待の宛先 (同じく cherrypick 由来) は、従来どおり `localBaseURL` の接頭辞で読む (`resolveTargetUser`)。残る差は、Accept / Undo(Accept) / Reject の follower と Follow / Undo(Follow) / Block / Undo(Block) の object がリモートの URI のとき、本家は `new URL(...).href` (正規化した形) で `uri` 列を引くのに対し、Elythia は受け取った文字列のまま引く点 |


### 3-5. 連合のルール (Elythia 独自)

**ホスト単位の設定より細かく、プラグイン (#3071) より宣言的な中間層** (#3090)。
Akkoma / Pleroma の MRF と同じ考え方で、受信したものに「条件と動作の組」を適用する。
upstream には対応物が無い。

- **2 種類ある。** `note` のルールは投稿の中身まで見て、拒否 / メディアを落とす /
  CW を付ける / 添付をセンシティブにする / タイムラインから外す、ができる。
  `activity` のルールは受信した activity を種別 (`Follow` / `Like` など) で見て、
  拒否だけができる (中身がまだ分からない段階なので書き換えは持てない)
- **条件**: 送信元ホスト (`blockedHosts` と同じ後方一致)、bot か、初めて見てから
  N 時間以内か、本文 / CW / 投票の選択肢のパターン (`prohibitedWords` と同じ書式)、添付の有無、
  タグ (`note.tags` と同じ正規化)。指定した条件はすべて満たす必要があり (AND)、
  配列の中はどれかに当たればよい (OR)。**条件の無い `note` のルールは作れない**
  (全投稿に当たるルールは書きかけの事故で、拒否にすると連合が丸ごと止まるため)
- **どこで効くか。** `activity` のルールは `dispatchActivity` の入口 = inbox の
  **署名検証の後**で評価する (Collection の中身は 1 件ずつ)。`note` のルールは
  投稿を取り込む全経路 (inbox の Create、Announce 先・返信先・引用先の取得、
  リレー) と、リモートの編集 (Update) の取り込みで評価する。編集にも掛けないと、
  当たらない投稿を作ってから差し替えるだけで素通りできる
- **mode が 3 つある。** `disabled` は評価しない。`record` は当たった記録だけを
  残して何もしない。`enforce` で初めて効く。**いきなり効かせると誤爆に気付けない**
  ので、record で当たり具合 (直近 24 時間の件数と、直近 50 件の対象) を見てから
  enforce にする運用を想定している。条件を変えると記録は捨てる (ただし変更の直前に
  評価されて書き込み待ちだった分は後から残りうる。捨てるのは best-effort)
- **合成**: 拒否が最優先。それ以外は当たった `enforce` のルールの動作をすべて
  合わせる。CW の文言は評価順 (`position` → `id`) で最初に当たったルールのもの
- **拒否は ack して捨てる** (禁止語と同じ。retry させない)。inbox の健全性
  (`admin/federation/inbox-health`) では受理として数える — 拒否した件数はルールの
  側の記録で見る
- **CW は送信者のものを上書きしない。** 空の CW (sensitive だけの投稿) には文言を
  補う。ルールの CW から hashtag は拾わない (tag を抜いた後に付ける)
- **リモートの編集では、受け取った Update の `summary` を CW の正とする。** 無ければ
  CW は外れたものとして扱い、ルールが当たれば付け直す。以前は `summary` の無い
  Update を「CW は変えない」として保存済みの値を残していたが、保存済みの CW には
  ルールの文言も入るので、それが送信者の CW として tag に拾われ、パターンや禁止語の
  判定にも混ざった。この変更で、**ルールと関係なく、相手が編集で CW を外したときに
  こちらでも外れる**ようになった (upstream は投稿の編集を取り込まないので、互換性の
  差は生じない)。本文は従来どおり「無ければ変えない」で、CW とは扱いが違う
  (本文の無い Update は投稿として成り立たないので、空の本文で消すと壊れた Update で
  本文が失われる。CW が無いのは普通の状態)
- **「タイムラインから外す」は silence と同じ形** (保存時に public → home)。
  既に配った後の編集で当たった場合も visibility は書き換えるが、配り済みの
  タイムラインからは消えない
- **「センシティブにする」は添付の `drive_file` 行に立てる。** 同じ URL の添付は
  行を再利用するので、その投稿者が同じ画像を添付した他の投稿でもセンシティブに
  なる。**書き換えるのは投稿者自身の行だけ** — 再利用は持ち主を見ないので、他人の
  添付の URL を指す投稿を送るだけで他人のファイルを書き換えられてしまう。
  書き換えられない添付が残ったときは、**投稿ごと空の CW で畳む** (そのままだと
  他人の URL を指すだけで「センシティブにする」をすり抜けられる)
- **タグの条件は `note.tags` の 32 個の上限で打ち切らずに見る** (打ち切ると、tag
  配列を詰め物で埋めて本文のタグを押し出すだけですり抜けられる)
- **編集では「初めて見てから N 時間以内」を投稿した時刻で測る。** 評価した時刻で
  測ると、新規のうちに投稿して時間が経ってから編集するだけで条件から外れ、ルールの
  CW が外れる。ただし投稿の時刻 (AP の `published`) が actor を初めて見た時刻より
  前なら (後から取りに行った古い投稿)、評価した時刻で測る
- **ブーストはルールの対象外。** Announce は投稿ではないので、`note` のルールは
  ブーストされた投稿のほうにだけ効く (ブーストした人には効かない。silence と同じ)。
  特定のサーバーのブーストを止めたいときは `activity` のルールで `Announce` を拒否する
- 投票 (poll への回答として届く Note) も投稿として評価する
- **評価のコスト。** ルールはプロセスごとにスナップショットとして持ち、受信の
  たびに DB を読まない (変更は `internal:federationRulesUpdated` で全プロセスへ
  伝え、届かなくても 5 分で読み直す)。パターンは読み込み時にコンパイルし、Go の
  regexp は線形時間なので壊滅的なバックトラックは起きない。ルールは 100 件、
  パターンは 1 件 1024 文字までに制限している。当たった記録は channel へ積むだけで、
  溢れたら捨てる (件数は下限になる) — 記録のために受信を止めない
- **読み直しに失敗したら前のルールを使い続ける。** 空にすると DB の瞬断で拒否して
  いたものが全部通る
- **TS へ戻したとき**: `federation_rule` は読まれないので、ルールは効かなくなる。
  ホスト単位の設定 (`meta`) は残る。ルールで拒否した投稿は取り込まれていない
  ままで、書き換えた投稿は書き換えた形のまま残る
- 送信側には適用しない

---

### 3-6. 引用の承認 (FEP-044f、Elythia 独自)

Mastodon 4.5 以降は、引用に**引用される側の承認**を求める (FEP-044f)。upstream の Misskey は対応していないので、Mastodon から見た Misskey の投稿は「誰も引用できない」になる (2026-09-30 に mastodon.social で実測: `misskey.io` も `go.k7a.org` も `quote_approval.automatic` が `[]`)。Elythia は**引用される側** (#3234 の段階 1 + 2)、**引用する側で承認を取りに行くこと** (段階 3)、**承認の取り消し** (段階 4) に対応した。

- **引用してよい範囲を配る。** ローカルの投稿に `interactionPolicy.canQuote.automaticApproval` を付ける。public / home は `as:Public`、followers は作者の followers collection、specified は作者だけ (= 誰も引用できない。空配列は JSON-LD で「項目が無い」と同じになるので FEP の指示どおり作者を入れる)。**公開範囲より広く引用させない** (FEP の推奨)。語彙の IRI は Mastodon と同じ (`https://w3id.org/fep/044f#` と GoToSocial の `gts:`)
- **`QuoteRequest` に自動で答える。** 投稿がローカルで、作者が凍結されておらず、公開範囲が許し (followers 限定なら引用者が作者をフォローしている)、どちらの向きにもブロックが無いときに承認して `Accept` を返す。`result` に承認の URI を入れ、`object` には受け取った `QuoteRequest` を id で返す (Mastodon は id で自分の引用を引き当てる)。activity の id が actor と別のホストなら答えない。**手動承認は持たない**
- **引用する投稿 (`instrument`) を確かめる。** actor と同じホストの inline ならそれを、そうでなければ取得して、作者が actor であること・引用先がこちらの投稿であることを見る。**確かめられなければ答えない** (Mastodon も sanity check に落ちたら黙って捨てる。`Reject` にすると、引用する投稿を inline で送らない実装の正当な引用 — 取得が 401 / 404 になるもの — を恒久的に拒否してしまう)。引用先は `quote` / `_misskey_quote` / `quoteUrl` / `quoteUri` のどれでもよい。**取得しても取り込まない** — ここで取り込むと、後から届く `Create` が「既にある」になり、引用の通知と chart のフックが飛ばされる (#2686 と同じ形)
- **inline の instrument は actor と同じホストなら取得せずに信じる** (Mastodon の `import_instrument` と同じ)。そのため、相手が引用する投稿の id を作り出せば、公開の投稿 1 件あたりの承認の行を増やせる。行は投稿を消すと一緒に消え、1 行は数百バイトなので、今は上限を置いていない
- **承認の実体を配る。** `GET /notes/<id>/quote-authorizations/<承認 id>` が `QuoteAuthorization` を返す。**公開範囲では絞らない** — followers 限定の投稿への引用でも第三者が確かめに来るので。出すのは URI だけで、投稿の中身は埋め込まない (FEP の MUST NOT)。承認は `note_quote_authorization` (§2-1) に 1 行ずつ残し、投稿を消すと一緒に消える (作者が相手をブロックしたときも消える。下の「ブロックしたら」)
- **見せていない投稿には答えない。** ダイレクト・ローカル限定の投稿、フォロワーでない相手からのフォロワー限定の投稿、凍結・削除された作者の投稿への `QuoteRequest` には、`Reject` も返さない。`Reject` は作者の署名付きで作者の URI を載せるので、答えるだけで「その id の投稿がある」「誰が書いた」が分かる (aidx の id は時刻と連番なので推測できる)。`Reject` を返すのは、見えている投稿でブロックがあるときだけ。**Mastodon より少し広い** — Mastodon は公開と未収載にしか答えず、フォロワー限定の投稿にはフォロワーからでも答えないが、Elythia はフォロワーには承認する (FEP の範囲内)
- **答えを届けられなければ再試行する。** 相手は `QuoteRequest` を自分からは送り直さないので、`Accept` / `Reject` の送信に失敗したら inbox の再試行に任せる。承認の記録は冪等なので、再試行でも承認は増えない
- **既存の投稿は相手側で古いまま。** Mastodon は引用してよい範囲を投稿を最初に取得したときに保存し、`Update` を受けるまで更新しない。この変更より前に取得された投稿は、Mastodon 側では引き続き引用できない
- **承認があるときだけ `quote` と `quoteAuthorization` を付ける** (段階 3)。`_misskey_quote` / `quoteUrl` / quote-inline span はいつも付ける。**承認の無いまま `quote` を付けてはいけない** — Mastodon は `quote` のある引用を「承認待ち」として表示し、本文の quote-inline span (RE: リンク) を消すので、承認を返さない相手 (本家 Misskey など) への引用が永久にその表示になる。`quote` が無ければ legacy として RE: リンクが残り、承認が届いた後の `Update` で承認済みの引用に切り替わる (Mastodon は legacy の引用も承認を確かめて承認済みにする)。**ただし Mastodon の引用数 (`quotes_count`) には数えられない** — legacy の印は承認済みになっても外れず、Mastodon は legacy の引用を数えない。表示は承認済みの引用と同じ
- **リモートの投稿を引用したら `QuoteRequest` を送る。** 公開・未収載・フォロワー限定の引用だけ (ダイレクトは承認の実体から宛先限定の投稿の URI が漏れるので送らない。ローカル限定はそもそも連合しない)。id は `<引用する投稿の URI>#quote-request` で、`note_quote_request` (§2-1) に記録してから、引用される作者の inbox へ `Create` の後に送る。`instrument` には引用する投稿を埋め込む (Mastodon は届いていない投稿を取りに来ずにそれを取り込める)。**保留のまま残った `QuoteRequest` は、確かめてから送り直す** (#3238)。Mastodon は、その投稿を別の経路 (相手のサーバーにいるフォロワー宛ての `Create`・検索・閲覧) で取り込んでいる最中に `QuoteRequest` が届くと、引用先が結び付く前の記録で照合して黙って捨てる (`QuoteRequest#accept_quote_request!` の sanity check。例外にならないので再試行もされない。e2e で実測し、その引用は相手側で保留のまま残った)。毎分の定期処理 (`maintenance:resendQuoteRequests`) が、送ってから 1 分後と 10 分後に、**まだ保留中なら**同じ id で送り直す (取り込みの後に照合されて承認が返る)。行を「保留中で予定が変わっていない」ときだけ取り分けてから送るので、複数の worker でも二重に送らず、送る前に投稿がまだ引用で公開範囲が対象かも確かめ直す。**保留でなくなったもの (承認・拒否・取り消し) には送らない** (答えが届いた時点で予定も消す) — Mastodon は `QuoteRequest` を受けると引用の状態を見ずに承認し直す (`Quote#accept!`) ので、取り消された引用を承認済みに戻してしまう (配送ジョブの遅延送信で試して、#3237 の敵対的レビューでこの回帰が見つかった)。**配送の queue では再試行しない** — 最初の送信も送り直しも 1 回きりで、届かなければ次の枠を待つ (配送の queue は積んだ後に状態を見ないので、何時間も後の再試行が承認・取り消しの後に届いてしまう)。**積んでから 5 分を過ぎたら送らない** — 相手のホストへの配送を止めている間 (ブレーカー) や流量を絞られている間は、配送の job が試行を消費せずに後へ回され続けるので、回数だけ 1 回にしても何時間も後に届きうる。期限付きの job は後へ回さずに捨てる (管理画面から失敗した job を再実行しても、期限を過ぎていれば送らない)。**配送の queue が 5 分以上詰まっているときも捨てられる** (期限は積んだ時刻から数える)。期限は enqueue した側の時計で決め、配送する側の時計で確かめるので、両者が別のホストで時計が大きくずれていると働かない。そのため**相手がおよそ 10 分 (最後の送り直しの期限まで含めて 15 分ほど) を超えて止まっていると、引用は保留のまま残る** (表示は RE: リンクのまま)。**予定より 3 分を超えて遅れた枠は送らずに使い切る** (Elythia や定期処理が止まっていた間の分。遅れて送っても取り込みとの競合は拾えない)。対応していない相手 (本家 Misskey など) にも最大 3 回届く。**残る窓**: こちらが `Accept` をまだ受けていない (相手の配送待ち・こちらの inbox の滞留や再試行) 間は保留のままなので、送り直しの枠 (最後は約 10 分後) までに相手の作者が取り消していると、送り直しが相手側で承認し直す。同じ間に届いた取り消しの `Delete` は、こちらがまだ承認 URI を持たないので照合できず捨てられる。**相手が FEP-044f に対応しているかは見ない** — 対応していない実装は未知の activity として捨てる
- **承認が返ったら `Update` を配り直す。** `Accept` の actor が引用される投稿の作者で、`result` (承認の URI) が作者と同じホスト (`www.` を同一視しない) のときだけ受け、引用する投稿の `quoteAuthorization` に入れた `Update` を `Create` と同じ宛先 (フォロワー・直接の宛先・公開なら relay) へ送る。`updated` を付けないので、Mastodon は編集ではなく承認の付け直しとして扱い、第三者は承認を取得して確かめてから引用を表示する。**承認を受けるのは保留中のときだけ** (Mastodon と同じ)。一度承認されたら、別の承認 URI の `Accept` が届いても変えず配り直さない — 受けると、引用される作者が URI を変えるたびにこちらの利用者のフォロワー全員へ `Update` を送ることになる。拒否された後の `Accept` でも承認に戻さない。`Update` を届けられなければ inbox の再試行に任せる (承認の記録は消さず、「まだ配っていない」印だけで表す。記録を戻す形にすると、失敗と重なって届いた同じ `Accept` が成功扱いになり、inbox の重複除けが再試行を捨てて承認ごと失われる)。**承認を引けないとき (DB 障害) は `Update` を作らない** — 承認の抜けた `Update` を受けた Mastodon は、承認済みの引用を未承認に戻す (承認 URI が変わったとみなす)。票数の `Update(Question)` も同じで、その回は送らない。取得 (GET) と `Create` は承認なしで描画する。**そのとき受け取った相手では、承認の無い引用のまま残る** — Mastodon は既に持っている投稿を取り直しても引用を確かめ直さないので、RE: リンクの表示に落ちる (承認が変わって Update を送る機会が無い限り戻らない)。`Reject` は保留中の記録を「拒否された」にするだけで、引用する投稿は変えない。**承認の後に届いた `Reject` は取り消しとして扱う** (Mastodon の `Reject#reject_quote!` と同じ。下の「取り消しを受ける」)
- **ローカル同士の引用には自分で承認を発行する。** 第三者から見ると、ローカルの利用者どうしの引用も承認の要る引用なので、引用を作った時点で `QuoteRequest` に答えるときと同じ判定 (公開範囲・フォロー・ブロック) を通るなら承認を 1 つ作り、最初の `Create` から `quoteAuthorization` を付ける。**自分の投稿の引用には付けない** (FEP-044f。Mastodon も承認なしで通す)
- **取り消しを受ける** (段階 4)。引用される作者が承認を取り消すと、承認を object にした `Delete` が届く (Mastodon の `RevokeQuoteService`)。object の id が自分の引用の承認で、actor が引用される作者なら「取り消された」にし、承認を外した `Update` を配り直す (第三者は承認の抜けた `Update` を受けて引用を未承認に戻す)。承認の後に届いた `Reject` も同じ。取り消された後の `Accept` では承認に戻さない。照合は object の型が `QuoteAuthorization` か、型の無い id のときだけ行う (型の付いたノートの `Delete` と、object が actor 自身の `Delete` = アカウント削除では引かない)。**照合できなければ通常の `Delete` として扱う** — 承認の URI は相手のホストの任意の URI でありうるので、たまたま一致した別の `Delete` を飲み込まない。配り直しは承認のときと同じく、届けられなければ inbox の再試行に任せる
- **ブロックしたら出した承認を取り消す** (段階 4、**Elythia 独自**)。ローカルの作者が相手をブロックすると、その相手の引用に出していた承認を消し (承認は 404 になる)、承認を埋め込んだ `Delete` (`<承認 URI>#delete`) を作者のフォロワーと、相手がリモートなら相手の inbox へ送る。相手のサーバーが引用する投稿を配り直す。相手がローカルなら、引用する投稿の `Update` (承認なし) もこちらが配る。**Mastodon はブロックでは取り消さない** — Elythia は段階 2 でブロック中の `QuoteRequest` を `Reject` するので、ブロックする前に出した承認だけが残り続けていた。**相手からのブロックでは取り消さない** (承認を出したのは作者)。解除しても戻さない (相手が承認を取り直す)。配送は best-effort (ブロックの処理は再試行の仕組みを持たない) だが、承認は先に消すので、相手が確かめ直せば失効する。`Delete` には `to` を付けない (フォロワー限定の投稿を指す承認を公開宛てとして出さない。Elythia の他の `Delete` と同じ)。**判定と承認の記録の間にブロックされた場合**に備えて、`QuoteRequest` に答えるときは承認を記録した後でブロックを確かめ直し、ブロックがあれば承認を消して `Reject` する (ブロックは「記録 → 承認を消す」の順なので、記録の後に確かめればそのブロックは見える。**確かめ直した後にブロックされる窓は残る** — そのときは消えた承認の `Accept` を送ることになる。第三者は承認を確かめて (404) 退けるが、**引用した相手のサーバーは `Accept` を確かめずに受ける** (Mastodon の `Accept#accept_quote!`)。取り消しの `Delete` が `Accept` より先に届くと相手はまだ承認 URI を持たないので照合できず、後から届いた `Accept` で承認済みになる (Mastodon が承認を定期的に確かめ直すまで。1 週間以上)。)`Reject` するときに消すのは**送ってきた相手の承認だけ** — 拒否の経路では引用する投稿を確かめていないので、引用 URI で消すと、作者をブロックした相手が他人の引用 URI を並べて他人の承認を消せる。Elythia の `Delete` は LD 署名を持たないので、Mastodon が転送した写しは第三者で検証に通らないが、相手のサーバーが引用する投稿の `Update` を配り直すので結果は同じになる
- **まだやっていないこと。** 利用者が個別の引用の承認を取り消す操作 (Misskey に対応する API が無い)、受け取った引用の承認の検証。受け取った引用は**従来どおり承認を見ずに表示する** (#3234 の論点)。**段階 3 より前に作った引用には承認を取りに行かない** (配り直しもしない)

### 3-7. リモートのアカウントの作成日時 (Elythia 独自)

本家は、リモートの人の `createdAt` を ID の日時から出す。ID はこのサーバーがその人を初めて知ったときに作るので、`createdAt` の意味は「このサーバーが知った日時」で、アカウントを作った日時ではない。Elythia はこれとは別に作成日時を持つ (#3465)。

- **actor の `published` を読む。** Mastodon は actor に `published` (アカウントを作った日。日付の単位) を載せる。リモートの actor を取得・更新するたびに読み、`user.accountCreatedAt` に入れる ([DB の差分](db.md))。RFC 3339 と日付だけの形 (`2017-04-08`) を受け付け、読めない値・2000 年より前 (単位の取り違えを拾う)・未来 (5 分を超える先) は捨てる。送られてくるたびに最新の値で上書きし、無い・読めないときは保存済みの値を残す
- **`published` を送らない Misskey 系は `/api/users/show` を見に行く。** 本家の renderPerson は `published` を送らない。保存しているソフトウェア名 (`instance.softwareName`) が Misskey 系なら、相手の `/api/users/show` (`requireCredential: false`) の `createdAt` を取って列に入れる。見に行くのは、その人からフォローを受け取ったときに列が空の場合だけで、一度取れたら二度と取りに行かない。取得は統計の取得 (`RemoteStatsFetcher`、#943) と同じ仕組み (待ち時間の上限 5 秒、成功 1 時間・失敗 5 分のキャッシュ、SSRF-safe transport、連合を切った相手へは出さない) を使う。時間切れや接続の失敗が起きたホストへは、5 分のあいだ誰の分も取りに行かない (応答しない相手のせいで、フォローを受け取るたびに処理が待たされないように。上限の直前で応答を返す相手は止められないが、actor の取得を遅らせるのと同じ程度で、新しい種類の問題ではない)。止めている間にフォローしてきた人の列は空のままになり、判定ではこのサーバーが知った日時を使う。列は空のときだけ書くので、その間に refresh が `published` を書いていればそちらが残る。失敗してもフォローの処理は続ける
- **既にいるリモートの人は一括で取り直さない。** 次にアカウント情報を取り直したとき (またはフォローを受け取ったとき) に埋まる。連合先への負荷を避けるため
- **相手が偽った日付は防がない。** 作成日時を偽るのは悪意のあるサーバーなので、ドメインのブロックなど別の手段で扱う
- **こちらの actor にも `published` を載せる。** `RenderPerson` がローカルの人の作成日時 (ID の日時、`2006-01-02T15:04:05.000Z` の形) を出す。ID は登録時に作るので、TS から移行した DB のローカルの人でも元の登録日になる。本家は出さないが、受け取る側は無ければ無視するだけのフィールドなので、連合の互換は崩れない
- **API には追加のフィールドで出す。** `createdAt` の値と意味は変えず、詳細なユーザーの応答に `accountCreatedAt` を足す ([API の差分](api.md))
