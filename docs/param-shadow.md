# 引数を本家のparamDefで検証する(paramshadow)

本家かElythiaの`paramDef`のあるendpointへのリクエストを、その`paramDef`(`internal/api/paramschema`、#3673)で検証し、宣言から外れるリクエストを本家と同じ400で拒否する(#3742)。対象は、テストのモードでだけ登録する`reset-db`を除く、宣言のあるendpointのすべて(`internal/server/param_shadow.go`の`paramEnforcedEndpoints`)。#3742から#3888まで、パッケージ単位で対象を広げ、#3890で、手で並べた一覧をやめて宣言から決める形にした。`reset-db`は検証しない(`paramUnvalidatedEndpoints`)。宣言(`properties: {}`)はどのobjectも通すので載せても応答は変わらないが、照合のテスト(`TestEnforcedEndpointsMatchTheCorpus`)で流すとテストのサーバーのDBを実際に消すので、載せない。一覧が宣言と合っていることは`TestEnforcedEndpointsAreDeclared`が確かめる。詳細は下の「[本家に揃えたendpoint](#本家に揃えたendpoint3742)」。

**この仕組みは、検証だけを走らせて件数を数えるところから始まった(#3677)。** 手書きの検証を本家の`paramDef`に置き換える前に、どの差が実際の利用で踏まれているかを、応答を変えずに本番で数えた。数えた結果は#3677の本文にまとめてある。`reset-db`を除く本家のendpointがすべて拒否に載った後、数える経路と、その設定(`disableParamShadow`・`paramShadowLogSeconds`)は#4172で消した。パッケージ名(`paramshadow`)と関数名(`withParamShadow`など)は、その頃のままにしてある。

実装は`internal/server/paramshadow`、配線は`internal/server/param_shadow.go`にある。

## どこで走るか

`/api`のリクエストは次の順に通る。検証は、本家がajvを走らせる位置(認証・rate limit・権限の後、handlerの前)に置いてある。

1. `RequireMultipartFile`(global): 本家の`requireFile`の検査。fileの無い`drive/files/create`は、ここで本文の無い400になる。本家でもajvより先
2. `auth.Authenticate`(global)
3. `JSONBodyParse`(`/api`のgroup): 壊れたJSON、空のJSON、`__proto__`や`constructor.prototype`を持つJSON(本家のsecure-json-parseが拒否するもの)は、ここでFastifyの形の400になる
4. rate limit、IPの記録、`HideAppUserWithoutRead`(group)
5. routeの認証・権限のmiddleware(`RequireAuth`、`RequireScope`など)
6. **検証**(`withParamShadow`)。宣言から外れるリクエストは、ここで拒否する
7. `requireObjectBody`(bodyがobjectでなければ`#/type`の400)
8. handler

routeのmiddlewareで401や403になったリクエストは、6に届かないので検証しない。本家でもajvに届かないリクエストだからである。handlerの中で権限を確かめるendpointでは、権限の誤りと検証の拒否が重なると、検証の400が先に返る。

## 対象

- **プラグインがAPIを呼ぶ経路も検証する。** プラグインの呼び出し(`pluginCaller.Call`)は`echo`のrouterを通るので、利用者の通信と区別しない
- **本家の`paramDef`のgolden(`internal/api/paramschema/upstream_paramdefs.json`)にあるendpointと、Elythia独自のendpointのうち`paramDef`を宣言したもの(`elythia_paramdefs.json`、#3869)だけ。** どちらもバイナリに埋め込まれていて、endpoint名は`/api/`の後のroute(`notes/create`)で引く。宣言の無いElythia独自のendpointは検証しない(下の「[Elythia独自のendpointの宣言](#elythia独自のendpointの宣言3869)」)
- **本家のendpointに足したElythia独自の引数**(`frontend/packages/elythia-js/src/extensions.ts`の`ElythiaRequestExtensions`)は、本家の`paramDef`に重ねて宣言する(#3867、下の「[Elythia独自の値を重ねる](#elythia独自の値を重ねる3865)」)。本家の`paramDef`は`additionalProperties`を`false`にしていない(使われているのは`true`だけ)ので、宣言していない項目は、ajvと同じく判定に効かない
- **POSTは、`application/json`のbodyだけ。** bodyは読み切ってから同じバイト列を戻し、自分用の写しを`UseNumber`でdecodeして検証する(`1e400`は本家と同じく有限でない数として型の誤りになり、`10.0`は整数として通る)。検証器がdefaultを入れるのも写しのほうで、handlerには届かない
- **GETは、`meta.allowGet`を持つendpointだけ。** 本家と同じく、queryに型の変換(`ApiCallService.ts`の「Cast non JSON input」)を掛けてから検証する。同じキーが2回来たら配列にし、変換はboolean / number / integerの項目の文字列の値だけに掛ける。変換できない値があれば、本家はajvより先に`INVALID_PARAM`(`info.param`は項目名)を返すので、その項目名を`schemaPath`として扱う

### 検証しないもの

次のリクエストは検証せず、handlerへ渡す。handlerの手書きの検査は残してあるので、そこで今までと同じ判定を受ける。理由の名前は`internal/server/paramshadow`の`Skip*`の定数で、ログには出ない(#4172までは、理由ごとに数えて要約に出していた)。

| 理由 | 中身 | 検証しない訳 |
|---|---|---|
| `not_json` | `application/json`でないPOST(本文が無い、`text/plain`など) | 本家ではfastifyのparserが決める。本家の答えは、Elythiaの`requireObjectBody`が再現する: 本文が無い・`text/plain`・requireFileでないendpointへの`multipart`は`#/type`の400、それ以外のContent-Type(`application/x-www-form-urlencoded`・XML・`application/octet-stream`など)は本家と同じ415 `FST_ERR_CTP_INVALID_MEDIA_TYPE`(2026-10-31の監査まではそれらを素通ししてechoのbinderが束縛していたので、宣言の検証を迂回してhandlerに届いた)。`paramDef`が`{}`の2件(`admin/captcha/current`、`reversi/invitations`)では本家は本文が無くても通し、Elythiaも`requireObjectBody`の対象から外して通す(#3843) |
| `multipart` | `meta.requireFile`のendpoint(本家の`drive/files/create`と、Elythiaの`drive/files/create-chunked/append`) | 本家は、`@fastify/multipart`がfileより前に読んだfieldだけを引数にし、同じfieldが2回あると`undefined`にする。Goの解析済みのformはfieldとfileの順を失っているので、本家の入力を作り直せない |
| `method` | `meta.allowGet`の無いendpointへのGET、GETとPOST以外 | 本家は引数を見る前に答える |
| `unparsable_query` | `url.ParseQuery`が拒否するquery(`;`、壊れた`%`のescape) | 本家のfast-querystringとは読み方が違う |
| `too_large` | 16KiB(16,384バイト、`DefaultMaxBody`)を超えるJSONのbody、またはquery | Goの値へのdecodeは、bodyの大きさの数十倍のメモリを確保する。検証には予算も同時実行の枠も無いので、1件の費用を大きさで抑える(下の「[本家に揃えたendpoint](#本家に揃えたendpoint3742)」)。Content-Lengthが上限を超えるbodyは読まない。Content-Lengthが分からないbodyは、上限+1バイトまでしか読まない |
| `too_many_values` | トークン(キー、値、括弧をそれぞれ1つと数える)が256(`DefaultMaxValues`)を超えるJSONのbody、または項目が256を超えるquery | decodeの確保は値の数に比例する。小さな値を並べたbodyは、16KiBに収まっていても検証の費用が大きい |
| `body_error` | bodyの読み取りかdecodeに失敗した | `JSONBodyParse`が検査済みなので、通常は起きない |

対になっていないサロゲートのescape(`"\ud800"`)と、UTF-8として正しくないbodyやqueryは、2026-10-31の監査まで`lone_surrogate`・`invalid_utf8`として検証しなかった。飛ばすとbodyに1バイト混ぜるだけで拒否の検証を迂回できたので、今はU+FFFDに置き換えた写しで判定する(本家もbodyを文字列にするときに不正なバイト列をU+FFFDに置き換える)。置き換えの数(Nodeは最大の部分列ごとに1つ、Goは連続した不正なバイト列ごとに1つなので、Goの方が短くなり`maxLength`は受理側にずれうる)と、lone surrogateの同一視(`JSON.parse`はそのまま残し、Goは`"\ud800"`と`"\ud801"`を同じU+FFFDにするので、enum・`uniqueItems`は拒否側にずれうる)で、本家と判定が分かれうる。正当なクライアントは送らない入力なので受け入れる。

上限まで埋めたbodyの大きさとトークンの数は次のとおり(#3677で、上限まで埋めたbodyをJSONにして数えた)。上限(16KiB・256)を超えるものは、検証せずにhandlerへ渡す。

| リクエスト | 大きさ | トークン | 検証するか |
|---|---|---|---|
| `notes/create`: 本文3000文字をすべて4バイトの絵文字にし、cw・投票10択・ファイル16個・宛先20人を付ける | 14,750バイト | 78 | する |
| `i/update`: 名前と自己紹介1500文字を絵文字で、項目16組を上限まで埋める | 22,950バイト | 115 | しない(`too_large`) |
| `i/update`: ワードミュートとハードミュートを200件ずつ | 14,074バイト | 1,210 | しない(`too_many_values`) |
| `admin/update-meta`: `blockedHosts`を1000件 | 27,057バイト | 1,007 | しない(`too_large`) |

## 本家に揃えたendpoint(#3742)

宣言のあるendpoint(`paramEnforcedEndpoints`。`reset-db`を除く宣言の全部)は、検証の結果で拒否する。手書きの検証を本家の`paramDef`へ揃える段階(#3667の(3)と(4))で、パッケージ単位で対象を広げてきた。#3888で宣言のあるendpointがすべて載ったので、#3890で一覧を宣言から決める形にした。宣言を足せば、そのendpointは拒否の対象になる。

- **拒否したときは、本家と同じ400を返す。** `INVALID_PARAM`(id `3d81ceae-…`)、message `Invalid param.`、`info`は`{param: <schemaPath>, reason: <ajvのmessage>}`(`paramschema.Error.Response`)。handlerは呼ばない
- **受理したときは、handlerがそのまま答える。** 検証器がdefaultを入れるのは写しのほうなので、handlerが読むbodyは変わらない。handlerの手書きの検査は残してあり、二段目になる。**手書きの検査が本家より厳しいと、本家が通す入力を400にしてしまう**ので、対象に足すときは、#3673で作ったajvのcorpusを本物のrouteに流して確かめる(`internal/server`の`TestEnforcedEndpointsMatchTheCorpus`)
- **検証できない入力は拒否せず、handlerに任せる。** 上の「検証しないもの」のすべてと、GETの型の変換の失敗が当たる(`not_json`は`requireObjectBody`が本家と同じ400か415で止めるので、handlerに届くのはrequireFileのmultipartと、`paramDef`が`{}`の2件へのJSON以外の本文と、Content-Type無しで本文のあるPOST(本家は415、以前からの差)だけ)。本家の判定なしに拒否すると、本家が通すリクエストを落としうるからである。これらはhandlerの検査で、今までと同じ判定を受ける
- **予算や同時実行の枠を持たない。** 持つと、それに当たったリクエストが本家の検証を受けずにhandlerへ届く。代わりに、1件の費用を小さい上限で抑える。bodyかqueryが16KiB(16,384バイト、`DefaultMaxBody`)かトークン256(`DefaultMaxValues`)を超える入力は、検証せずにhandlerへ渡す(上の項目と同じ扱い)。`clips/*`の正当なbodyは、UTF-8のまま送れば`description`の2048文字を4バイトの文字で埋めても約8KiBで、トークンは全項目と`i`を送る`clips/notes`でも18である。非ASCIIを`\u`のescapeで送るクライアント(Pythonの`json.dumps`の既定など)が`description`を絵文字で埋めると約24KiBになり、上限を超えてhandlerの検査に回る。`antennas/create` / `antennas/update`は、本家の`paramDef`が`keywords`・`excludeKeywords`・`users`の数に上限を持たないので、キーワードや利用者を多く並べたbodyは上限を超えうる(キーワード1行が括弧2つと語の数のトークンになる)。同梱のfrontendの送る更新のbodyでは、1語のキーワードを74行か、利用者を218人並べると超える(全項目と`i`で、トークンは37+3×行数か39+人数。作成は`antennaId`が無いので35+3×行数か37+人数で、キーワードは同じ74行、利用者は220人で超える)。超えたbodyも拒否はせず、handlerの検査に回る。`flash/create` / `flash/update`も、`script`が約16KiBを超える大きなPlayは同じくhandlerの検査に回る(同梱のプリセットは224〜2,343バイト)。`gallery/posts/create` / `gallery/posts/update`も、本家が`description`の長さに上限を持たないので、長い説明は同じくhandlerの検査に回る。`channels/update`も、本家が`pinnedNoteIds`の数に上限を持たないので、ピン留めしたノートが多いと(同梱のfrontendが`i`と一緒に送るbodyでは236件から)同じくhandlerの検査に回る。`pages/create` / `pages/update`も、本家が`content`の数にも文字列の長さにも上限を持たないので、ブロックが多いPage(同梱のエディタのbodyでは、textのブロック29個から)や長い本文は、同じくhandlerの検査に回る。`users/show`の`userIds`と`users/relation`の`userId`も、本家が配列の数に上限を持たないので、`i`と一緒に送るbodyでは250件から同じくhandlerの検査に回る。`admin/roles/create` / `admin/roles/update`も、本家の`paramDef`が`policies`を中身を決めない`object`にしているので、ポリシーを多く並べると超える。同梱のロールのエディタは全部のポリシーを`{useDefault, priority, value}`で埋めて送るので、普段の保存のbodyが常にトークンの上限256を超える (全項目と`i`を送るbodyで、`admin/roles/update`はポリシー24件から、`roleId`の無い`admin/roles/create`は25件から超える。#3859)。`i/update`も、本家の`paramDef`が`mutedWords`・`hardMutedWords`の数に上限を持たないので、同梱のfrontendのワードミュートの保存は、`i`と一緒に送るbodyで1語の行を84行並べると超える(トークンは7+3×行数。#3886)。`admin/update-meta`も同じで、同梱のfrontendのモデレーションの画面は`blockedHosts`などの一覧を1つずつ`i`と一緒に送るので、250行から超える(トークンは7+行数。#3888)。名前・自己紹介・項目16組を絵文字で上限まで埋めたプロフィールも、下の表のとおり16KiBを超える。antennasのhandlerは名前の長さを本家と同じ1〜100文字で見るが、それ以外は以前の判定になる([docs/divergence/approximations.md](divergence/approximations.md#本家のparamdefで引数を検証するendpoint-3742))。上限を大きくすると、資格情報さえあれば重い検証を走らせ続けられるので、大きくはしない。`clips/show`・`clips/notes`、`charts/*`の12件、`hashtags/*`の5件、`flash/show`・`flash/featured`・`flash/search`、`gallery/featured`・`gallery/popular`・`gallery/posts`・`gallery/posts/show`、`channels/show`・`channels/featured`・`channels/search`・`channels/timeline`、`roles/show`・`roles/users`、`app/show`・`auth/session/show`・`auth/session/userkey`、`announcements`・`announcements/show`、`bubble-game/ranking`、`federation/followers`・`federation/following`・`federation/users`・`federation/show-instance`・`federation/stats`、`pages/show`・`pages/featured`、`emoji`・`emojis`・`endpoint`・`endpoints`・`get-avatar-decorations`・`get-online-users-count`・`meta`・`ping`・`pinned-users`・`retention`・`server-info`・`stats`・`test`、`users`・`users/achievements`・`users/clips`・`users/featured-notes`・`users/flashs`・`users/gallery/posts`・`users/get-frequently-replied-users`・`users/notes`・`users/pages`・`users/reactions`・`users/search`・`users/search-by-username-and-host`・`users/show`・`users/lists/get-memberships`・`users/lists/list`・`users/lists/show`、`reversi/games`・`reversi/show-game`・`reversi/verify`、`verify-email`、`notes`・`notes/children`・`notes/clips`・`notes/conversation`・`notes/featured`・`notes/global-timeline`・`notes/local-timeline`・`notes/reactions`・`notes/renotes`・`notes/replies`・`notes/search`・`notes/search-by-tag`・`notes/show`・`notes/show-partial-bulk`は資格情報なしで呼べてrate limitも無い(`fetch-rss`も資格情報なしで呼べるが、1分300回のrate limitがある。`users/followers`・`users/following`も資格情報なしで呼べるが、Elythiaだけが持つ1分30回のrate limitがある。`charts/*`と`hashtags/trend`、`bubble-game/ranking`、`federation/stats`、`emoji`・`emojis`・`fetch-rss`・`get-online-users-count`・`retention`・`server-info`、`users/featured-notes`、`notes/featured`はGETでも呼べる。`email-address/available`・`username/available`も資格情報なしで呼べるが、Elythiaだけが持つIPごとに1分120回のrate limitがある。`request-reset-password`も資格情報なしで呼べるが、本家と同じ1時間3回のrate limitがあり、`reset-password`も資格情報なしで呼べるが、Elythiaだけが持つ1時間30回のrate limitがある。Elythia独自の`signup-application/status`も資格情報なしで呼べるが、1時間30回のrate limitがある)ので、この上限が無いと、重い検証を走らせ続けられる(#3742のレビュー1周目M-1)。1件の費用は下の表のとおり
- **検証器の不具合では止めない。** 検証器がpanicしたときは、stackをプロセスで最初の1回だけ警告のログに出し(値は出さない)、リクエストはhandlerへ渡す。compileできない宣言は、起動のときに警告を出し、そのendpointのリクエストはhandlerへ渡す。compileできない宣言は`TestEveryUpstreamParamDefCompiles`と`TestEnforcedEndpointsAreDeclared`が落とすので、通常は起きない
- **宣言の無いルートを足すと、テストが落ちる(#3890)。** `internal/server`の`TestAPIRoutesHaveParamDefs`が、本番と同じく(テストのモードでなく、ルートを持つプラグインを1つ入れて)組み立てたサーバーの`echo.Routes()`から`/api/*`のルートを列挙し、本家かElythiaの宣言の無いルートを落とす。ソースを静的に読む方法(`internal/entitycompat`の`parseRouteRegistrations`)は、`api.Match`で登録する`charts/*`の12件を取りこぼすので使わない。新しいルートには`elythia_paramdefs.json`に宣言を書く(宣言すれば拒否の対象に載る)。宣言しない例外は、どこで登録するかと一緒に`undeclaredAPIRoutes`に書く。今の例外は、catchallの`*`と、本家の旧いpathの互換の`signin`の2件と、本家がfastifyへ直に登録するroute(goldenの`directRoutes`から読む。登録しているのは`signup`・`signup-pending`・`signin-flow`・`signin-with-passkey`・`miauth/:session/check`・`v1/instance/peers`の6件)と、組み立てに渡したプラグインの名前空間(`/api/plugin/<名前>/...`。本体がそこに張るpeerの受信と秘密の値の入力口も含む)。名前で絞るので、本体のルートを`/api/plugin/`の下に登録しても落ちる。逆向きに、宣言があるのにPOSTのルートが無いものも落とす(例外はテストのモードでだけ登録する`reset-db`)。`meta.allowGet`の無いendpointのGETのルートも、検証器が飛ばすので落とす(例外は`notes/reactions`。[divergence/approximations.md](divergence/approximations.md#本家のparamdefで引数を検証するendpoint-3742))。例外の一覧に使われていない項目があると落ち、件数の下限(500)と、実在するルート(`api.Match`で登録する`charts/notes`など)を名指しで要求する。組み立てにはPostgreSQLが要る。`internal/server`の`TestMain`は、PostgreSQLに繋がらないとDBを使うテストをskipするが、`MK_SERVER_TESTS_REQUIRE_DB`があれば落とす。CIの`test-shards`はこれを渡す。2026-10-29に数えた本番の組み立てのpathは529件で、本家の宣言があるものが437件、Elythiaの宣言があるものが83件、宣言の無いものが9件
- **metaを書くendpointは、照合のテストで入力ごとに行とキャッシュを戻す。** `admin/update-meta`・`admin/roles/update-default-policies`・`admin/captcha/save`(`metaWritingEndpoints`)の入力の後に、metaの行を流す前の値へ戻し、meta系のキャッシュ(`cachedMeta`・`metaResponse`・`nodeinfo`・`captcha`・`reactionBuffering`)を捨てる。行だけを戻すと、後の入力が書き換えた後の値を読む。戻した後の`admin/meta`が流す前と同じことを確かめる。`admin/update-meta`の受理の入力は204になること、行を書き換えた入力があることも確かめる(#3888)。
- **GETで呼べるendpoint(`charts/*`・`hashtags/trend`・`bubble-game/ranking`・`federation/stats`・`emoji`・`emojis`・`fetch-rss`・`get-online-users-count`・`retention`・`server-info`・`users/featured-notes`・`notes/featured`)は、queryの型の変換も本家と同じに扱う。** 本家はinteger / number / booleanの項目の文字列の値を`JSON.parse`してからajvに渡す。変換に失敗した値はajvより先に`INVALID_PARAM`(id `0b5f1631-…`、`info: {param: <項目名>, reason: "cannot cast to <型>"}`)になる。この失敗は検証器では拒否せず、binderの印を見たhandlerが同じ形で返す(上の「検証できない入力」)。変換を通った値(`10.0`、`null`)と、同じキーを2回並べたquery(配列になる)は検証器が判定する。GETのqueryの確かめ方は、corpusがbodyだけなので、`internal/server`の`TestEnforcedGETMatchesUpstream`に代表的な入力を表で持つ

`BenchmarkEnforcedWorstCase`(`clips/notes`のroute、handlerはbodyを読んで200を返すだけ。Intel Xeon E-2274G、2026-10-21の実測。値は3回の幅)。

| body | 大きさ | 検証なし | 検証あり | 確保(検証あり) |
|---|---|---|---|---|
| トークン上限ちょうどの`{}`の配列 | 約0.4KB | 2.3µs | 45〜46µs | 24KB / 546回 |
| トークン上限ちょうどのキーと値 | 約1.4KB | 2.3〜2.4µs | 66〜68µs | 37KB / 430回 |
| 大きさ上限ちょうどの`\u`のescapeの文字列 | 16,383バイト | 2.3〜2.4µs | 138〜139µs | 83KB / 49回 |
| トークン上限+1(handlerへ渡す) | 約0.4KB | 2.3µs | 3.5µs | 6.9KB / 23回 |
| 64KiBの`\u`のescapeの文字列(handlerへ渡す) | 65,535バイト | 2.3µs | 2.7µs | 5.8KB / 19回 |

大きさの上限を置く前は、64KiBの`\u`のescapeの文字列が1件0.52ms・311KBだった(上限だけを64KiBにして同じベンチで測った)。上限ちょうどの`\u`のescapeの文字列が最も重く、1件約0.14msになる。

本家と違う挙動として残すものは[docs/divergence/approximations.md](divergence/approximations.md#本家のparamdefで引数を検証するendpoint-3742)にある。

## Elythia独自の値を重ねる(#3865)

Elythiaは、本家の`paramDef`の外の値をいくつか受ける(独自のキューの名前など)。そのままでは本家の`paramDef`がこれを拒否するので、`internal/api/paramschema/elythia_overlays.json`に宣言して、本家の`paramDef`に重ねる。検証は、重ねた後のもの(`paramschema.Effective`)で行う。`paramschema.Upstream`は本家の`paramDef`のままで、ajvのcorpusとの照合に使う。

- **宣言は本家の`paramDef`と同じ形で書く。** endpointの名前ごとに、`properties`・`items`で重ねる項目までたどり、そこで操作を書く。操作は4つだけ
  - `enum`: 本家のenumに値を足す。既にある値、項目の型に合わない値、`x-elythia-enumPattern`が既に通す値を足す宣言は拒否する
  - `x-elythia-enumPattern`: 本家のenumに、patternに合う文字列を足す(プラグインのキュー`plugin:<名前>`)。既にある値に合うpatternは拒否する
  - `x-elythia-formatAlso`: 本家の形式(`misskey:id`)に、別の値(`values`)かpatternに合う値(`pattern`)を許す。形式の名前は変えないので、どちらにも合わない値には本家と同じエラー(`must match format "misskey:id"`)を返す。既に形式に合う値を足す宣言は拒否する
  - `x-elythia-properties`: 本家の`paramDef`に無い項目(Elythia独自の引数)を、項目ごとに本家と同じ形の完全なスキーマで足す(#3867)。本家にある項目を書く宣言、`default`を持つ宣言は拒否する(検証器が写しに入れる値が変わると、本家が受理する入力のdefaultの照合が崩れる)。足した項目は本家の項目の後に検証するので、本家が拒否する入力の最初のエラーは変わらない。本家は知らない項目として何でも通していたので、足した項目に宣言から外れる値を持つ入力だけが、受理から拒否に変わる
- **足すことしかできない。** 本家に無い項目・enum・形式に重ねる宣言や、知らないキーワード(`required`を外すなど)は、読み込みで拒否する。読み込めないと、本家の`paramDef`での検証ごと止まる(起動時に警告を出す)
- **テストで守ること**(`internal/api/paramschema`): ajvのcorpusの全件を重ねた後のもので流し、本家が受理する入力は同じdefaultで受理され、本家が拒否する入力は同じ最初のエラーを返すこと(重ねたキーワードで落ちた入力が通るようになるものだけ例外)。宣言した値が受理され、外の値に本家と同じエラーを返すこと
- **値の出どころと突き合わせる**(`internal/server`の`TestElythiaOverlaysMatchTheImplementation`): キューは`queue.AllQueueNames()`から本家のenumを除いたものと、プラグインのキューは`queue.PluginQueueNames`がキューを作る名前とちょうど同じ名前を受けること(振る舞いで比べる)、通知の型は`notification.MkGoTypeNames()`と、`federation/instances`の並べ替えは`repository`の定数と一致すること。同梱のfrontendが送るキューの名前(`misskey-js`の`queueTypes`)が、本家のenumか重ねた値に入っていること

今重ねている値:

| endpoint | 項目 | 重ねた値 |
|---|---|---|
| `admin/queue/*`のうち`queue`を受ける10件 | `queue` | `push`・`export`・`webhook`・`maintenance`と、プラグインのキュー`plugin:<名前>`(`x-elythia-enumPattern`。名前は`internal/queue`の`pluginNamePattern`と同じ形(`[a-z0-9][a-z0-9-]*`)。`admin/queue/queues`の一覧に載り、同梱のジョブキューの画面がその名前のまま各endpointを呼ぶ) |
| `i/notifications`・`i/notifications-grouped` | `includeTypes`・`excludeTypes`の要素 | `importCompleted`・`abuseReport`・`emojiApplicationProcessed`・`emojiApplicationReceived`・`signupApplicationReceived` |
| `federation/instances` | `sort` | `+host`・`-host` |
| `reversi/match` | `userId` | acct(`@user`・`@user@host`。`x-elythia-formatAlso`のpattern) |
| `admin/drive/files` | `userId` | `@system`(`x-elythia-formatAlso`のvalues) |
| `i/update` | `avatarDecorations`の要素の`id` | `""`(`x-elythia-formatAlso`のvalues。カスタム絵文字から新しく装着するデコレーションは、保存するまでidを持たず、同梱のfrontendが`emojiName`と一緒に空のidを送る(#2975)。#3886) |

必須を外した別の経路を足す操作は、要る段(Elythia独自のエンドポイントの宣言、#3667の(4))で足す。

足した独自の引数(`x-elythia-properties`)の一覧と意味は、[divergence/api.md](divergence/api.md#1-1c-リクエストパラメータの-additive)の表にある(`i/update`の分は#3886で、`admin/update-meta`の分は#3888で宣言した。`i/regenerate-token`の`token`は[divergence/security.md](divergence/security.md))。`internal/server`の`TestAddedPropertiesMatchElythiaJS`が、足した項目の名前と`elythia-js`の`extensions.ts`の`ElythiaRequestExtensions`の項目の名前が一致することを確かめる。比べるのは最上位に足した項目だけで、`i/update`の`avatarDecorations`の要素の中に足した`scale`・`emojiName`は対象外。この2つの値の範囲は`TestIUpdateOverlay`が確かめる。`admin/update-meta`に足した項目の値の範囲は、`TestUpdateMetaOverlay`がhandlerとサービスの定数(`core/drive`の分割アップロード、`core/signup`のユーザー名の長さ、`core/signupapplication`の申請フォーム、`queue/processors`のリレーの猶予)と突き合わせる。

## Elythia独自のendpointの宣言(#3869)

Elythiaにしかないendpointにも、本家と同じ形の`paramDef`を`internal/api/paramschema/elythia_paramdefs.json`に書き、宣言したものを本家のendpointと同じく検証して拒否する(#3667の(4))。形は`upstream_paramdefs.json`の`endpoints`と同じ(`{name: {paramDef, meta}}`)で、`paramschema.Effective`が本家の宣言(重ねた後のもの)に足して返す(印は`Endpoint.Elythia`)。`paramschema.Upstream`には入らない。

- **宣言したものは、すべて拒否の対象になる。** 拒否の対象(`paramEnforcedEndpoints`)は宣言から決まる(#3890。それまでは段ごとに、宣言と一覧への登録を同時に行っていた)。宣言の名前のルートが無いと、`internal/server`の`TestAPIRoutesHaveParamDefs`が落とす
- **本家と同じ名前は書けない。** 本家のendpointの引数を変えるのは重ねる宣言(上の節)の役目で、そちらは足すことしかできない。読み込みで拒否する
- **`default`は書けない。** 検証器はdefaultを写しにしか入れず、handlerが既定値を持つので、書いた既定値はhandlerに届かず、handlerと食い違っても気付けない。本家の`paramDef`を写すとき(`i/flashs`は`flash/my`、`i/flashs/likes`は`flash/my-likes`、`following/silent/list`は`following/requests/list`)も、`default`は落とす
- **handlerの方が厳しいものは、理由を付けて照合のテストに載せる。** `paramDef`で書けない検査(IPアドレスとして読めるか、利用者がリモートか)は、handlerが`INVALID_PARAM`で拒否したままにする。`TestEnforcedEndpointsMatchTheCorpus`の`handlerStricterThanUpstream`に、どのcorpusの入力が当たるかと一緒に載せる
- **ajvのcorpusは別のファイルに作る。** `make elythia-paramdef-gen`が、`tools/paramdefspec/corpus.mjs --elythia`で`testdata/ajv_corpus_elythia.jsonl`を作る。ajvの版・設定・形式は本家のgoldenから読むので、本家のgoldenを作り直したら作り直す(`make paramdef-gen`が続けて回す)。headerに本家のgoldenと宣言の両方のhashを持ち、`internal/api/paramschema`の`TestElythiaConformsToAJVCorpus`が、作り直し忘れと判定の食い違いを落とす。`TestCorpusRejectsAtEveryReachableKeyword`と`internal/server`の`TestEnforcedEndpointsMatchTheCorpus`も、こちらのcorpusを読む
- **宣言の名前と項目を、ルートと`elythia-js`の型に突き合わせる。** 名前は`internal/server`の`TestAPIRoutesHaveParamDefs`が、組み立てたサーバーのPOSTのルートと突き合わせる(#3890より前は、`internal/entitycompat`のテストが生成物の`docs/api-compat.md`と突き合わせていた)。項目名と必須の項目は、型の名前をTypeScriptのコンパイラでしか解けないので、宣言から型の検査を生成して`tsc`に判定させる。`make elythia-js-paramdefs`が`frontend/packages/elythia-js/src/paramdefs.generated.ts`を作り、elythia-jsの型検査(`frontend`のcheck)が、`ElythiaEndpoints[name]['req']`の項目名と必須の項目が宣言と同じであることを確かめる。生成物が宣言に追いついているかは、`internal/api/paramschema/elythiajs`のテストが見る。項目の型は突き合わせない
- **どれか1つを必須にするときは、最上位の`anyOf`で必須を分ける。** 本家の`drive/files/show`と同じく最上位の`anyOf`を使うが、本家は分岐ごとに`properties`を持つのに対し、こちらは最上位に`properties`を置いて分岐には`required`だけを書く。`chat/messages/create`(`toUserId`か`toRoomId`)と`admin/emoji/fetch-remote-meta`(`emojiId`か、`name`と`host`の両方、#3877)が使う(#3872)。分岐が項目の値を絞るときは、分岐に`properties`を書く。`admin/backup/restore/prepare`は、`id`の分岐で`kind`を`restore`に、`database`の分岐で`rollback`に絞る(#3878)。ajvは`anyOf`を最上位の`properties`より先に見るので、絞る値は最上位に書かず、最上位の`kind`は名前だけ(`{}`)にする。型の検査は、最上位の項目に分岐の項目を足したものを比べる。elythia-jsの型はunionになるので、型の検査は、分岐ごとにその必須を要求する型をunionから選び、その型の項目名と必須の項目を比べる(項目は最上位の項目にその分岐の項目を足したもの、必須は最上位の必須にその分岐の必須を足したもの)。分岐で選んだ型を合わせるとunionの全員になることも確かめる。`chat/messages/create`では、どちらの型も4つの項目すべてを持つ(両方を送ると`toRoomId`が勝つ)。型の検査が読まない語(`oneOf`、分岐の中の`anyOf`など)を持つ宣言は、生成を拒む
- **ある項目の値で別の項目を必須にするときは、最上位の`if`/`then`で書く。** 本家の`notes/create`と同じ書き方で、`emoji-application/create`(`kind`が`remote`のときの`remoteHost`・`remoteName`)が使う(#3875)。条件つきの必須は型に書けないので、elythia-jsでは省略できる項目のままにし、型の検査は足さない。代わりに、`if`と`then`が揃っていることと、どちらにも`paramDef`の項目しか出てこないことを、型の検査の生成が確かめる
- **宣言を変えたら`make elythia-paramdef-gen`を回す。** corpusと型の検査の両方を作り直す(`frontend/node_modules`が要る)
- **認証・権限・応答の型は、別のファイル(`elythia_apidoc.json`)に書く(#4084)。** `/api.json`に載せるためのもので、拒否の検証は読まない。名前の集合はこのファイルと一致させる。詳しくは[divergence/api.md](divergence/api.md#elythia独自のエンドポイントの宣言-4084)

- **意味の検査は、宣言の拒否と別のcodeで返す。** 連合のルールの`create`・`update`は、正規化した後に数える件数と長さや、条件と動作が1つ以上あることなどを`fedrule.Normalize`で確かめる。宣言には形(型・null・必須・`ruleId`の`misskey:id`・`mode`と`target`のenum)だけを書き、意味の誤りはhandlerが`INVALID_RULE`で返す(#3877)。同じ`INVALID_PARAM`のままだと、宣言の拒否と見分けられず、照合のテストでも受理の入力がhandlerの奥まで届いたかを確かめられない
- **再認証を課すendpointでは、引数の検証を再認証より先に走らせる。** `admin/backup/*`の6件(`list`・`take`・`verify`・`delete`・`download`・`restore/candidates`)は、再認証をrouteのmiddlewareでなく、handlerの中(`internal/api/adminbackup`の`guarded`)で行う(#3878)。routeのmiddlewareは引数の検証(`withParamShadow`)より先に走るので、そこに置くと、形の誤った本文でも再認証を通し、TOTPのコードを使い切ってから400になる。handlerを関数呼び出しで包む形は、routeのhandlerを`x.Method`の形で求めるゲートから外れるので使わない。本家の`i/2fa/*`と同じ順で、`internal/entitycompat`の`TestBackupAdminRoutesAreGuarded`が、routeに再認証を付けないことと、routeが渡すhandlerのメソッドが`guarded`を呼ぶことを確かめる。照合のテストの管理者は`user_profile`を持たないので、受理の入力は再認証がprofileを読む段で`AUTHENTICATION_UNAVAILABLE`になる。`restore/*`の4件は、daemonが無いので転送の手前で止まる。`TestEnforcedEndpointsMatchTheCorpus`は、受理の入力がこの段まで届くことを確かめる
- **外へ取りに行くendpointは、照合のテストで数えるだけの偽物に差し替える。** `admin/self-check`と`admin/federation/check-host`の診断は、`Server`の`selfCheckRunner`・`remoteChecker`が実行元で、nilなら組み立てで本物を作る。`TestEnforcedEndpointsMatchTheCorpus`は`beforeSetup`で偽物を入れ、200を返した受理の入力の数と、偽物が呼ばれた回数が一致することを確かめる。`self-check`の本物は検査の宛先を`url`に固定した、SSRFのガードを通さない素の`http.Client`で取りに行くので、外向きの通信を数えるproxyでは拾えない(#3877)

今宣言しているのは、外向きの通信の無い参照だけの28件(#3869)と、chatの15件・分割アップロードの`start`・`finish`・`abort`の3件(#3872)と、DBとRedisの中で閉じる書き込みの18件(バブルゲームの対戦・絵文字の申請・登録の申請・`admin/federation/close-delivery-breaker`・`clean-gone-instance`、#3875)と、連合のルールの`admin/federation/rules/create`・`update`・`delete`と外向きの通信のある`admin/federation/check-host`・`admin/self-check`・`admin/emoji/fetch-remote-meta`の6件(#3877)と、`admin/backup/*`の11件(#3878)と、`i/federation`・`i/update-federation`の2件(#3852)と、リバーシの対局の操作の`reversi/ready`・`put-stone`・`claim-time-is-up`の3件(#3942。足したときから宣言している)と、分割アップロードの`drive/files/create-chunked/append`(#4090)の計87件。変わった挙動は[divergence/api.md](divergence/api.md#1-1-elythia-にしかない-113)にある。`append`はmultipartなので、本家の`drive/files/create`と同じく`meta.requireFile`を付け、ファイルの欄の名前を`meta.fileField`で`chunk`にする(本家は`file`に決まっている)。検証器はrequireFileのendpointを検証しない(上の`multipart`)ので、宣言を載せても拒否は変わらず、`uploadId`・`index`・`chunk`は以前どおりhandlerが`info`無しの400で拒否する。宣言は`/api.json`の本文と、elythia-jsの型の照合のために載せる(以前は宣言せず、#3877)。宣言しないものは1件ある(`TestAPIRoutesHaveParamDefs`の`undeclaredAPIRoutes`)。`signin`は本家の旧いpathの互換なので宣言しない。

## 本番で数えた結果(#3677)

#4172より前は、拒否に載せる前の本家のendpointで、検証だけを走らせ、検証器とhandlerの判定が分かれた件数を、endpoint名と`schemaPath`だけでログに出していた(値・本文・利用者・IPは記録しない)。保存できた2つの期間(10/18〜21、10/25〜29)で、判定が分かれたのは`federation/instances`の`sort`の716件だけで、#3865で解消した。まとめは#3677の本文にある。数え方・ログの形・取り出し方は、#4172より前のこのファイル(`git log -p -- docs/param-shadow.md`)にある。

## かかる時間

`go test -bench`で、handlerがbodyを読んで200を返すだけのrouteに、検証を付けた場合と付けない場合を比べた(`internal/server/paramshadow`の`BenchmarkShadow`)。bodyは本家のfrontendが送る形に寄せた、本家の検証を通るもの(`benchRequests`。上限の内側にあることを`TestBenchRequestsAreAccepted`が確かめる)。

```bash
go test -run '^$' -bench BenchmarkShadow -benchtime 2s -count 3 ./internal/server/paramshadow/
```

Intel Xeon E-2274G(4.00GHz、8スレッド)、2026-10-18の実測(数える経路があった頃で、検証そのものは今と同じ)。値は3回の幅。

| endpoint | body | 検証なし | 検証あり | 増えた分 |
|---|---|---|---|---|
| `notes/create` | 712バイト | 2.7〜3.3µs | 18.7〜19.1µs | 約16µs |
| `notes/timeline` | 137バイト | 2.5〜2.7µs | 9.0〜9.3µs | 約6.5µs |
| `i/update` | 2,044バイト | 2.5〜2.8µs | 51.7〜54.7µs | 約50µs |

`i/update`のprofileでは、約半分が自分用の写しのdecode(`encoding/json`)で、検証そのものは2割弱だった。

### 最悪の場合

1バイトあたりのdecodeの費用が最も大きいのは、未知のキーに小さな値を並べたbody(`{"x":[{},{},…]}`や`{"x":{"k00001":0,…}}`)である。handlerの`c.Bind`は未知のキーをほぼ確保せずに読み飛ばすが、検証のための写しは全要素をGoの値にする。そのため、bodyの大きさ(16KiB)に加えて、トークンの数(256)にも上限を置いている。上限以内の最悪のbodyの1件の費用は、上の「[本家に揃えたendpoint](#本家に揃えたendpoint3742)」の表にある。

トークンは、bodyのバイト列を確保せずに走査して数える(`tooManyValues`)。`json.Decoder.Token`と同じ数になることは、ajvの照合のcorpusの全入力で確かめている(`TestTooManyValuesMatchesDecoderToken`)。#3677で、64KiBのbodyで比べた費用は次のとおり(このときの上限は64KiB・4096トークン)。

| body(64KiB) | 走査 | `json.Decoder.Token` |
|---|---|---|
| `{}`の配列(4097個目で打ち切る) | 7〜9µs、確保なし | 58〜100µs、8.8KB / 21回 |
| トークン4093個 + 空白で64KiBまで | 89〜105µs、確保なし | 135〜156µs、263KB / 26回 |

Content-Lengthが上限を超えるbodyは読まずに`too_large`にするので、1MiBのbodyを送り続けられても、検証の分の費用はほぼ無い。JSONBodyParseが剥がしたBOMの3バイトはContent-Lengthに残るので、BOM付きで16,385〜16,387バイトのbodyも`too_large`になる(数え方だけの差で、handlerの検査に回るだけである)。

上限を超えて読まないbody(1MiBなど)では、本番の組み立てで`/api`の前に`JSONBodyParse`がbodyを読み(`io.ReadAll`)、JSONとして正しいかを確かめる(`json.Valid`)費用の方がずっと大きく、検証が無くても、同じ送り方で同じくらいのCPUを使わせられる(#3677のレビュー3周目で測った)。上限以内の最悪のbodyでは、検証の方が重い(上の「本家に揃えたendpoint」の表)。

## 設定

設定は無い。検証は常に動く。#4172より前の`disableParamShadow`・`paramShadowLogSeconds`(と環境変数`MK_DISABLEPARAMSHADOW`・`MK_PARAMSHADOWLOGSECONDS`)は消した。前の版の設定ファイルや環境変数に残っていても、知らないキーとして読み飛ばして起動する(`internal/config`の`TestLoad_RemovedParamShadowKeysAreIgnored`)。
