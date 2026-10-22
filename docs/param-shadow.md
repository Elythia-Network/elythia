# 引数の検証を本番で走らせて、本家と判定が分かれる件数を数える(paramshadow)

本家と同じendpointへのリクエストを、本家の`paramDef`(`internal/api/paramschema`、#3673)で検証し、その判定とhandlerの答えが分かれた件数をログに出す(#3677)。**応答もhandlerの読むものも変えない。** 検証の結果で拒否はしない。

**例外は、本家に揃えたendpointである(#3742)。** `internal/server/param_shadow.go`の`paramEnforcedEndpoints`(今は`clips/*`の11件(#3742)、`charts/*`の12件(#3744)、`hashtags/*`の5件(#3746)、`antennas/*`の7件(#3748)、`flash/*`の10件(#3750)、`gallery/*`の9件(#3754)、`following/*`の11件(#3756)、`channels/*`の16件(#3758)、`blocking/*`・`mute/*`・`renote-mute/*`の9件(#3760)、`roles/*`・`invite/*`・`notifications/*`の12件(#3762)、`app/*`・`auth/*`・`miauth/gen-token`・`my/apps`の8件(#3764)、`sw/*`・`announcements`の6件(#3766)、`promo/read`・`email-address/available`・`username/available`・`ap/*`・`bubble-game/ranking`・`bubble-game/register`の7件(#3768)、`federation/instances`を除く`federation/*`の6件(#3770)、`pages/*`・`page-push`の8件(#3772)、どのパッケージにも属さない参照だけの`emoji`・`emojis`・`endpoint`・`endpoints`・`fetch-rss`・`get-avatar-decorations`・`get-online-users-count`・`meta`・`ping`・`pinned-users`・`retention`・`server-info`・`stats`・`test`の14件(#3774))は、同じ位置で検証し、拒否すれば本家と同じ400を返す。数える対象からは外れる。詳細は下の「[本家に揃えたendpoint](#本家に揃えたendpoint3742)」。

目的は、手書きの検証を本家の`paramDef`に置き換える前に、どの差が実際の利用で踏まれているかを数えることである。本家より緩い所(省略できる項目へのnull、`misskey:id`の形式、`limit`の丸めなど)を一度に400へ変えると、今は動いているクライアントが落ちうる。数えた結果は、揃える順番と、差として残すものを決める材料にする(親issue #3667)。

実装は`internal/server/paramshadow`、配線は`internal/server/param_shadow.go`にある。

## どこで走るか

`/api`のリクエストは次の順に通る。検証だけの実行は、本家がajvを走らせる位置(認証・rate limit・権限の後、handlerの前)に置いてある。

1. `RequireMultipartFile`(global): 本家の`requireFile`の検査。fileの無い`drive/files/create`は、ここで本文の無い400になる。本家でもajvより先
2. `auth.Authenticate`(global)
3. `JSONBodyParse`(`/api`のgroup): 壊れたJSON、空のJSON、`__proto__`や`constructor.prototype`を持つJSON(本家のsecure-json-parseが拒否するもの)は、ここでFastifyの形の400になる
4. rate limit、IPの記録、`HideAppUserWithoutRead`(group)
5. routeの認証・権限のmiddleware(`RequireAuth`、`RequireScope`など)
6. **検証**(`withParamShadow`)。本家に揃えたendpointでは、ここで拒否する
7. `requireObjectBody`(bodyがobjectでなければ`#/type`の400)
8. handler

routeのmiddlewareで401や403になったリクエストは、6に届かないので数えない。本家でもajvに届かないリクエストだからである。handlerの中で権限を確かめるendpointでは、権限の誤りと検証の拒否が重なると`rejected_handler_other_error`(後述)になる。

## 対象

- **プラグインがAPIを呼ぶ経路も数に入る。** プラグインの呼び出し(`pluginCaller.Call`)は`echo`のrouterを通るので、利用者の通信と区別せずに数える
- **本家の`paramDef`のgolden(`internal/api/paramschema/upstream_paramdefs.json`)にあるendpointだけ。** goldenはバイナリに埋め込まれていて、endpoint名は`/api/`の後のroute(`notes/create`)で引く。goldenに無いElythia独自のendpointは検証しない
- **本家のendpointに足したElythia独自の引数**(`frontend/packages/elythia-js/src/extensions.ts`の`ElythiaRequestExtensions`)は、検証器にとっては知らない項目である。本家の`paramDef`は`additionalProperties`を`false`にしていない(使われているのは`true`だけ)ので、ajvと同じく知らない項目は判定に効かない。独自の引数の値をhandlerが`INVALID_PARAM`で拒否すると、`accepted_handler_invalid_param`として数える。`handlerParam`を見れば、独自の引数によるものか分かる
- **POSTは、`application/json`のbodyだけ。** bodyは読み切ってから同じバイト列を戻し、自分用の写しを`UseNumber`でdecodeして検証する(`1e400`は本家と同じく有限でない数として型の誤りになり、`10.0`は整数として通る)。検証器がdefaultを入れるのも写しのほうで、handlerには届かない
- **GETは、`meta.allowGet`を持つendpointだけ。** 本家と同じく、queryに型の変換(`ApiCallService.ts`の「Cast non JSON input」)を掛けてから検証する。同じキーが2回来たら配列にし、変換はboolean / number / integerの項目の文字列の値だけに掛ける。変換できない値があれば、本家はajvより先に`INVALID_PARAM`(`info.param`は項目名)を返すので、その項目名を`schemaPath`として扱う

### 検証しないもの

検証しなかったリクエストは、理由ごとに数えて要約の`skippedBy`に出す。

| 理由 | 中身 | 検証しない訳 |
|---|---|---|
| `not_json` | `application/json`でないPOST(本文が無い、`text/plain`など) | 本家ではfastifyのparserが決める。本家の答えの大半(`#/type`の400)は、Elythiaの`requireObjectBody`が再現済みである。`paramDef`が`{}`の2件(`admin/captcha/current`、`reversi/invitations`)では本家は本文が無くても通すが、ここは数えない |
| `multipart` | `meta.requireFile`のendpoint(`drive/files/create`) | 本家は、`@fastify/multipart`がfileより前に読んだfieldだけを引数にし、同じfieldが2回あると`undefined`にする。Goの解析済みのformはfieldとfileの順を失っているので、本家の入力を作り直せない |
| `method` | `meta.allowGet`の無いendpointへのGET、GETとPOST以外 | 本家は引数を見る前に答える |
| `lone_surrogate` | 対になっていないサロゲートのescape(`"\ud800"`)を含むbody | `JSON.parse`はそのまま残すが、Goの`encoding/json`はU+FFFDに置き換える。enumや`uniqueItems`(`"\ud800"`と`"\ud801"`が同じ値になる)の判定が本家と分かれうる。decodeで情報が失われるので、検証器の側では直せない |
| `invalid_utf8` | UTF-8として正しくないbodyやquery | 正しくないバイト列をU+FFFDに置き換える数が、NodeとGoで違う(Nodeは最大の部分列ごとに1つ、Goは1バイトごとに1つ)。ajvの数える長さが変わる |
| `unparsable_query` | `url.ParseQuery`が拒否するquery(`;`、壊れた`%`のescape) | 本家のfast-querystringとは読み方が違う |
| `too_large` | 64KiB(65,536バイト)を超えるJSONのbody、またはquery(本家に揃えたendpointでは16KiB。[後述](#本家に揃えたendpoint3742)) | Goの値へのdecodeは、bodyの大きさの数十倍のメモリを確保する。1MiBに近いbodyで検証すると、資格情報の要らないendpointでもCPUとメモリを大きく増やせる(後述の「最悪の場合」)。本家のクライアントが普通に送るbodyは収まる(下の表)。Content-Lengthが上限を超えるbodyは読まない。Content-Lengthが分からないbodyは、上限+1バイトまでしか読まない |
| `too_many_values` | トークン(キー、値、括弧をそれぞれ1つと数える)が4096を超えるJSONのbody、または項目が4096を超えるquery(本家に揃えたendpointでは256) | decodeの確保は値の数に比例する。小さな値を並べたbodyは、64KiBに収まっていても検証の費用が大きい。本家のクライアントが普通に送るbodyは収まる(下の表) |
| `busy` | 同時に走っている検証が上限(`GOMAXPROCS`の4分の1、最低1)に達していた | 検証のためにリクエストを待たせない。同時に検証へ使うCPUを、枠の数までに抑える |
| `budget` | 最近の検証に使った時間が予算(`GOMAXPROCS`÷16コアの速さで減り、100msまで溜まる)に達していた | 時間あたりに検証へ使うCPUを抑える。値と根拠は「最悪の場合」 |
| `body_error` | bodyの読み取りかdecodeに失敗した | `JSONBodyParse`が検査済みなので、通常は起きない |

`too_large`と`too_many_values`の上限は、上限まで埋めたbodyをJSONにして数えて決めた(`internal/server/paramshadow/input.go`の`DefaultMaxBody`と`DefaultMaxValues`)。

| リクエスト | 大きさ | トークン |
|---|---|---|
| `notes/create`: 本文3000文字をすべて4バイトの絵文字にし、cw・投票10択・ファイル16個・宛先20人を付ける | 14,750バイト | 78 |
| `i/update`: 名前と自己紹介1500文字を絵文字で、項目16組を上限まで埋める | 22,950バイト | 115 |
| `i/update`: ワードミュートとハードミュートを200件ずつ | 14,074バイト | 1,210 |
| `admin/update-meta`: `blockedHosts`を1000件 | 27,057バイト | 1,007 |
| `admin/update-meta`: `blockedHosts`を2000件 | 54,057バイト | 2,007 |
| `admin/update-meta`: `blockedHosts`を3000件 | 81,018バイト(`too_large`) | 3,007 |

## 本家に揃えたendpoint(#3742)

`paramEnforcedEndpoints`に載せたendpointは、検証の結果で拒否する。手書きの検証を本家の`paramDef`へ揃える段階(#3667の(3))で、パッケージ単位で対象を広げる。

- **拒否したときは、本家と同じ400を返す。** `INVALID_PARAM`(id `3d81ceae-…`)、message `Invalid param.`、`info`は`{param: <schemaPath>, reason: <ajvのmessage>}`(`paramschema.Error.Response`)。handlerは呼ばない
- **受理したときは、handlerがそのまま答える。** 検証器がdefaultを入れるのは写しのほうなので、handlerが読むbodyは変わらない。handlerの手書きの検査は残してあり、二段目になる。**手書きの検査が本家より厳しいと、本家が通す入力を400にしてしまう**ので、対象に足すときは、#3673で作ったajvのcorpusを本物のrouteに流して確かめる(`internal/server`の`TestEnforcedEndpointsMatchTheCorpus`)
- **検証できない入力は拒否せず、handlerに任せる。** 上の「検証しないもの」のうち、`not_json`・`multipart`・`method`・`lone_surrogate`・`invalid_utf8`・`unparsable_query`・`too_large`・`too_many_values`・`body_error`と、GETの型の変換の失敗が当たる。本家の判定なしに拒否すると、本家が通すリクエストを落としうるからである。これらはhandlerの検査で、今までと同じ判定を受ける
- **予算(`budget`)と同時実行の枠(`busy`)では飛ばさない。** 飛ばすと、本家の検証を受けずにhandlerへ届く。代わりに、1件の費用を小さい上限で抑える。bodyかqueryが16KiB(16,384バイト、`DefaultEnforcedMaxBody`)かトークン256(`DefaultEnforcedMaxValues`)を超える入力は、検証せずにhandlerへ渡す(上の項目と同じ扱い)。`clips/*`の正当なbodyは、UTF-8のまま送れば`description`の2048文字を4バイトの文字で埋めても約8KiBで、トークンは全項目と`i`を送る`clips/notes`でも18である。非ASCIIを`\u`のescapeで送るクライアント(Pythonの`json.dumps`の既定など)が`description`を絵文字で埋めると約24KiBになり、上限を超えてhandlerの検査に回る。`antennas/create` / `antennas/update`は、本家の`paramDef`が`keywords`・`excludeKeywords`・`users`の数に上限を持たないので、キーワードや利用者を多く並べたbodyは上限を超えうる(キーワード1行が括弧2つと語の数のトークンになる)。同梱のfrontendの送る更新のbodyでは、1語のキーワードを74行か、利用者を218人並べると超える(全項目と`i`で、トークンは37+3×行数か39+人数。作成は`antennaId`が無いので35+3×行数か37+人数で、キーワードは同じ74行、利用者は220人で超える)。超えたbodyも拒否はせず、handlerの検査に回る。`flash/create` / `flash/update`も、`script`が約16KiBを超える大きなPlayは同じくhandlerの検査に回る(同梱のプリセットは224〜2,343バイト)。`gallery/posts/create` / `gallery/posts/update`も、本家が`description`の長さに上限を持たないので、長い説明は同じくhandlerの検査に回る。`channels/update`も、本家が`pinnedNoteIds`の数に上限を持たないので、ピン留めしたノートが多いと(同梱のfrontendが`i`と一緒に送るbodyでは236件から)同じくhandlerの検査に回る。`pages/create` / `pages/update`も、本家が`content`の数にも文字列の長さにも上限を持たないので、ブロックが多いPage(同梱のエディタのbodyでは、textのブロック29個から)や長い本文は、同じくhandlerの検査に回る。antennasのhandlerは名前の長さを本家と同じ1〜100文字で見るが、それ以外は以前の判定になる([docs/divergence/approximations.md](divergence/approximations.md#本家のparamdefで引数を検証するendpoint-3742))。上限を大きくすると、資格情報さえあれば予算の外で重い検証を走らせられるので、大きくはしない。`clips/show`・`clips/notes`、`charts/*`の12件、`hashtags/*`の5件、`flash/show`・`flash/featured`・`flash/search`、`gallery/featured`・`gallery/popular`・`gallery/posts`・`gallery/posts/show`、`channels/show`・`channels/featured`・`channels/search`・`channels/timeline`、`roles/show`・`roles/users`、`app/show`・`auth/session/show`・`auth/session/userkey`、`announcements`・`announcements/show`、`bubble-game/ranking`、`federation/followers`・`federation/following`・`federation/users`・`federation/show-instance`・`federation/stats`、`pages/show`・`pages/featured`、`emoji`・`emojis`・`endpoint`・`endpoints`・`get-avatar-decorations`・`get-online-users-count`・`meta`・`ping`・`pinned-users`・`retention`・`server-info`・`stats`・`test`は資格情報なしで呼べてrate limitも無い(`fetch-rss`も資格情報なしで呼べるが、1分300回のrate limitがある。`charts/*`と`hashtags/trend`、`bubble-game/ranking`、`federation/stats`、`emoji`・`emojis`・`fetch-rss`・`get-online-users-count`・`retention`・`server-info`はGETでも呼べる。`email-address/available`・`username/available`も資格情報なしで呼べるが、Elythiaだけが持つIPごとに1分120回のrate limitがある)ので、この上限が無いと、予算の外で重い検証を走らせ続けられる(#3742のレビュー1周目M-1)。1件の費用は下の表のとおり
- **検証にかかった時間は予算に積まない。** 積むと、資格情報の要らないendpointへ送り続けるだけで予算を使い切らせ、他のendpointの計測を止められる
- **数えない。** 判定は常に一致するので、要約の`agreed`にも`diverged`にも入らない。記録するのは`validator_panic`と`compile_error`だけで、どちらのときもリクエストはhandlerへ渡す
- **`disableParamShadow`では止まらない。** 止まるのは数えるだけの検証で、本家に揃えたendpointは検証を続ける
- **対象はパッケージ単位で足す。** `TestEnforcedEndpointsAreUpstreamEndpoints`が、対象に載せたパッケージ(`clips`・`charts`・`hashtags`・`antennas`・`flash`・`gallery`・`following`・`channels`・`blocking`・`mute`・`renote-mute`・`roles`・`invite`・`notifications`・`app`・`auth`・`miauth`・`my`・`sw`・`announcements`・`promo`・`email-address`・`username`・`ap`・`bubble-game`・`federation`・`pages`・`page-push`)の本家のendpointが全部載っていることを確かめる(どのパッケージにも属さないトップレベルのendpointは、1件で1つのパッケージとして数えるので、この検査の対象にならない)。一部だけ載せると、残りが手書きの検査のままになる。例外は、本家の`paramDef`が独自の拡張を拒否してしまうendpointで、理由と一緒に`enforcementPending`に載せる。今は`federation/instances`だけで、独自の`sort`の値(`+host` / `-host`)が本家のenumに無い(#3770)
- **GETで呼べるendpoint(`charts/*`・`hashtags/trend`・`bubble-game/ranking`・`federation/stats`・`emoji`・`emojis`・`fetch-rss`・`get-online-users-count`・`retention`・`server-info`)は、queryの型の変換も本家と同じに扱う。** 本家はinteger / number / booleanの項目の文字列の値を`JSON.parse`してからajvに渡す。変換に失敗した値はajvより先に`INVALID_PARAM`(id `0b5f1631-…`、`info: {param: <項目名>, reason: "cannot cast to <型>"}`)になる。この失敗は検証器では拒否せず、binderの印を見たhandlerが同じ形で返す(上の「検証できない入力」)。変換を通った値(`10.0`、`null`)と、同じキーを2回並べたquery(配列になる)は検証器が判定する。GETのqueryの確かめ方は、corpusがbodyだけなので、`internal/server`の`TestEnforcedGETMatchesUpstream`に代表的な入力を表で持つ

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

## 数える種類

| `kind` | 検証器 | handler | 揃えたときに変わること |
|---|---|---|---|
| `rejected_handler_ok` | 拒否 | 2xx | 今は通っているリクエストが400になる |
| `accepted_handler_invalid_param` | 受理 | `INVALID_PARAM` | 今は400のリクエストが通るようになる(handlerの側のdecodeも直す必要がある) |
| `param_mismatch` | 拒否 | `INVALID_PARAM` | `info.param`が変わる。handlerが`info`を付けていない(`handlerParam`が空)ものもここに入る |
| `rejected_handler_other_error` | 拒否 | `INVALID_PARAM`以外の誤り | errorのcodeが変わる(`misskey:id`でないIDへの`NO_SUCH_NOTE`など) |
| `validator_panic` | panicした | (そのまま流す) | 検証器の不具合。リクエストは止めない。stackは、プロセスで最初の1回だけ警告のログに出す(値は出さない) |
| `compile_error` | compileできなかった | (そのまま流す) | goldenと検証器が合っていない。テスト(`TestEveryUpstreamParamDefCompiles`)が落ちるので、通常は起きない |

両方が受理したもの、両方が拒否して`info.param`が一致したものは、要約の`agreed`に数える。

handlerの答えの読み方は次のとおり。

- statusは、handlerが書いたもの。書かずにerrorを返したときは、echoのerror handlerが書くstatus
- statusは、echoの`DefaultHTTPErrorHandler`と同じ規則で決める。errorを包んでいるときは中を見ない(`fmt.Errorf`で包んだ`*echo.HTTPError`は500になる)。`Internal`に`*echo.HTTPError`を持つときは、そのcodeを使う
- statusが400のときだけ、bodyの先頭8KiBを手元に写し、`error.code`と`error.info.param`を読む。`error.code`が`INVALID_PARAM`なら`INVALID_PARAM`とみなす(idは見ない。型の変換の失敗の`0b5f1631-…`もここに入る)。**`handlerCode`が埋まるのは400のときだけ**で、404(`users/show`の`NO_SUCH_USER`など)・403・401では空になる。そのときはstatusで見分ける

## 記録するもの・しないもの

記録するのは、endpoint名、検証器の`schemaPath`、handlerの`info.param`(`handlerParam`)、handlerのstatus(`handlerStatus`)とerrorのcode(`handlerCode`)、種類、件数だけである。**値、本文、利用者、IP、token、handlerの`reason`は記録しない。**

- `schemaPath`は、本家の`paramDef`の中の位置(`#/properties/limit/maximum`)で、リクエストの値を含まない
- `handlerParam`と`handlerCode`は、handlerのソースのリテラルのはずだが、念のため形を見る。`handlerParam`は`#/a/b`の形か項目名の形、`handlerCode`は英大文字・数字・`_`だけのときに写し、それ以外は`(other)`にする
- 確かめているテストは`internal/server/paramshadow`の`TestEventsCarryNoRequestValue`と`TestLogsCarryNoRequestValue`、`internal/server`の`TestParamShadowChangesNoResponse`

## ログの見方

件数はプロセスのメモリで数え、`paramShadowLogSeconds`(既定600秒)を過ぎた後の最初のリクエストと、停止のときに、slogへ出す。定期の要約は、そのリクエストの処理の中ではなく、serverの世代の背景処理(`bgwork`)で書く。**各行は、前の要約からの差分である。** 期間全体や複数のプロセスの合計は、行のcountの和になる。プロセスが落ちると、最後の要約からの分は失われる。停止のときに、処理中のリクエストを待つ上限を超えて終わったリクエストの分も、どの要約にも入らない。

判定の分かれた組ごとに1行。

```
level=INFO msg="paramshadow divergence" endpoint=notes/create kind=rejected_handler_ok schemaPath=#/properties/fileIds/minItems handlerParam="" handlerStatus=200 handlerCode="" count=3
```

要約の1行。`since`は期間の始まり(UTC)、`seconds`は期間の長さ。

```
level=INFO msg="paramshadow summary" since=2026-10-18T03:00:00Z seconds=600 agreed=1520 diverged=7 skipped=2 skippedBy.not_json=2
```

**飛ばした分は数えていない。** `skippedBy`の`budget`・`busy`・`too_large`・`too_many_values`などは、検証しなかったリクエストの件数で、その間に判定が分かれたかどうかは分からない。分かれた件数を割合で読むときは、分母を`agreed`+`diverged`(検証した件数)にする。`budget`と`busy`は負荷の高い時間に偏って出るので、`budget`や`busy`が多い期間は、その時間帯のリクエストが少なめに数えられている。この2つが多いときは、その期間の数を他の期間と同じ重みで読まない。

組の数には上限(1つの期間で4096)を置いてある。超えた分は、endpointと種類ごとに、`handlerParam`と`handlerCode`を`(overflow)`にした1行へまとめる。

docker composeで動かしている場合、期間の合計は次のように出せる(`<container>`はmk-goのコンテナ名)。

```bash
docker logs <container> 2>&1 \
  | grep 'msg="paramshadow divergence"' \
  | sed 's/^.*endpoint=//' \
  | awk '{c = $NF; sub(/ count=[0-9]+$/, ""); n[$0] += substr(c, 7)} END {for (k in n) print n[k], k}' \
  | sort -rn | head -50
```

## かかる時間

`go test -bench`で、handlerがbodyを読んで200を返すだけのrouteに、検証を付けた場合と付けない場合を比べた(`internal/server/paramshadow`の`BenchmarkShadow`)。bodyは本家のfrontendが送る形に寄せた、本家の検証を通るもの(`benchRequests`)。

```bash
go test -run '^$' -bench BenchmarkShadow -benchtime 2s -count 3 ./internal/server/paramshadow/
```

Intel Xeon E-2274G(4.00GHz、8スレッド)、2026-10-18の実測。値は3回の幅。

| endpoint | body | 検証なし | 検証あり | 増えた分 |
|---|---|---|---|---|
| `notes/create` | 712バイト | 2.7〜3.3µs | 18.7〜19.1µs | 約16µs |
| `notes/timeline` | 137バイト | 2.5〜2.7µs | 9.0〜9.3µs | 約6.5µs |
| `i/update` | 2,044バイト | 2.5〜2.8µs | 51.7〜54.7µs | 約50µs |

`i/update`のprofileでは、約半分が自分用の写しのdecode(`encoding/json`)で、検証そのものは2割弱だった。

### 最悪の場合

1バイトあたりのdecodeの費用が最も大きいのは、未知のキーに小さな値を並べたbody(`{"x":[{},{},…]}`や`{"x":{"k00001":0,…}}`)である。handlerの`c.Bind`は未知のキーをほぼ確保せずに読み飛ばすが、検証のための写しは全要素をGoの値にする。そのため、bodyの大きさ(64KiB)に加えて、トークンの数(4096)にも上限を置いている。トークンは、bodyのバイト列を確保せずに走査して数える(`tooManyValues`)。`json.Decoder.Token`と同じ数になることは、ajvの照合のcorpusの全入力で確かめている(`TestTooManyValuesMatchesDecoderToken`)。

数える処理そのものの費用(`BenchmarkCountTokens`)。

| body(64KiB) | 走査 | `json.Decoder.Token` |
|---|---|---|
| `{}`の配列(4097個目で打ち切る) | 7〜9µs、確保なし | 58〜100µs、8.8KB / 21回 |
| トークン4093個 + 空白で64KiBまで | 89〜105µs、確保なし | 135〜156µs、263KB / 26回 |

`BenchmarkShadowWorstCase`(`notes/create`のroute、同じ機械、load averageは1〜2のとき。1件の費用を測るので、予算は外して流した)。

| body | 大きさ | 検証なし | 検証あり | 確保(検証あり) |
|---|---|---|---|---|
| トークン上限ちょうどの`{}`の配列 | 約8KB | 2.2µs | 0.60ms | 314KB / 8,236回 |
| トークン上限ちょうどのキーと値 | 22,502バイト | 2.1µs | 0.92〜0.94ms | 508KB / 6,214回 |
| 同じ`{}`の配列を空白で64KiBまで | 65,536バイト | 2.1〜2.2µs | 0.70〜0.71ms | 381KB / 8,237回 |
| トークン上限+1(`too_many_values`) | 約8KB | 2.1〜2.2µs | 9.6〜9.8µs | 12.7KB / 23回 |
| 64KiBいっぱいの`{}`の配列(`too_many_values`) | 65,536バイト | 2.1〜2.2µs | 19.8〜20.0µs | 80KB / 23回 |
| 大きさ上限+1(`too_large`。Content-Lengthで判定して読まない) | 65,537バイト | 2.1〜2.2µs | 2.7〜2.8µs | 5.8KB / 19回 |
| 1MiB(`too_large`。同上) | 約1MiB | 2.1〜2.2µs | 2.7〜2.8µs | 5.8KB / 19回 |

トークンの上限を置く前は、64KiBいっぱいの`{}`の配列が検証され、1件あたり6.8〜7.2ms、3.8MB / 87,452回を使った。大きさの上限を置く前は、1MiBのbodyで、本物のserverでの1件あたりが155〜173ms、確保が77.9MBだった(#3677のレビュー1周目)。

費用は、同時に走る検証の数と、時間あたりの予算の2つで抑える。

- **同時に走る検証は`GOMAXPROCS`の4分の1(最低1)まで。** 空きが無ければ待たずに`busy`とする。これで抑えられるのは「同時に何件か」だけで、1件が軽ければ1つの枠が1コアを使い続けられる(予算を入れる前は、2コアの機械で約0.85コア、8コアで約2.2コアを足せた)
- **時間あたりの予算(`budget`)。** 枠の中で使った時間を積み、一定の速さで減らす。積んだ量が上限に達している間は、検証せずに`budget`として数え、待たせない。積むのは、bodyの読み取り・トークンの走査・decode・検証の時間(壁時計)。走査も、64KiBで約0.1msかかり、上限を超えるbodyを送り続けられたときに数えないと予算の外になるので積む。CPUが混んでいると壁時計は実際のCPUより長くなるので、混んでいるほど早く予算に当たる(安全な側)

| 値 | 中身 | 根拠 |
|---|---|---|
| 減る速さ | `GOMAXPROCS`÷16コア(最低0.1コア)。1コアで0.1、2コアで0.125、8コアで0.5、16コアで1 | 検証に使うCPUを全体の約6%に抑える。下限の0.1コアは、小さな機械でも普通の負荷が当たらないための値 |
| 溜められる上限 | 100ms | 普通のリクエストの検証は1件で数十µsなので、数千件が一度に来ても当たらない |

普通の負荷で予算に当たらないことの計算: 予算に当たり続けるのは、平均の検証の時間×件数/sが減る速さを超えたとき。最も小さい予算(0.1コア = 毎秒100ms)で、普通のリクエストの費用(上の「かかる時間」)から割ると次の件数になる。本番の件数は手元では確かめていない(本番には触れない)。数十〜数百件/sの規模なら1桁以上の余裕がある。

| リクエスト | 1件の検証 | 予算に当たり続ける件数/s(0.1コア) | 同(8コア、0.5コア) |
|---|---|---|---|
| `notes/timeline` | 約6.5µs | 約15,000 | 約77,000 |
| `notes/create` | 約16µs | 約6,200 | 約31,000 |
| `i/update` | 約50µs | 約2,000 | 約10,000 |

溜められる上限の100msは、`i/update`を一度に2,000件、`notes/create`を6,000件受けても当たらない量である。

#3677のレビュー2周目と同じ組み立てで、予算と、Content-Lengthが上限を超えるbodyを読まない扱いを入れた後に測り直した。上の最悪のbodyを16 goroutineで3秒送り続け、handlerは1ms寝る。約300MBのポインタを含むliveのheapを置き、プロセスのCPUを`getrusage`で、GCのCPUを`runtime/metrics`で読んだ。値は2回の範囲で、本番と同居の機械(load averageは1〜6)で測った。chunkedは、Content-Lengthを付けずに送ったもの。

| body | `GOMAXPROCS` | 予算 | 検証した件数/s | 予算か枠か上限で飛ばした件数/s | GCのCPU | プロセスのCPU(なし → あり) | 増分 |
|---|---|---|---|---|---|---|---|
| トークン上限ちょうどのキーと値 | 2 | 0.125コア | 179〜181 | 12,435〜12,489 | 0.10〜0.11コア | 0.15 → 0.34〜0.35コア | 0.19〜0.20コア |
| 同じ`{}`の配列を空白で64KiBまで | 2 | 0.125コア | 238〜239 | 12,660〜12,774 | 0.05コア | 0.07 → 0.29コア | 0.22コア |
| 64KiBいっぱいの`{}`の配列(`too_many_values`) | 2 | 0.125コア | 0 | 12,187〜12,209 | 0.14〜0.15コア | 0.08 → 0.28〜0.29コア | 0.20〜0.21コア |
| 同上をchunkedで | 2 | 0.125コア | 0 | 12,295〜12,334 | 0.14コア | 0.07〜0.09 → 0.27〜0.28コア | 0.19〜0.20コア |
| 1MiB(`too_large`) | 2 | 0.125コア | 0 | 13,021〜13,724 | 0.00〜0.05コア | 0.11〜0.15 → 0.09〜0.17コア | ほぼ0 |
| トークン上限ちょうどのキーと値 | 8 | 0.5コア | 519〜541 | 11,842〜11,873 | 0.24コア | 0.12〜0.20 → 0.88〜0.90コア | 0.70〜0.76コア |
| 同じ`{}`の配列を空白で64KiBまで | 8 | 0.5コア | 716〜720 | 11,418〜11,550 | 0.23コア | 0.08 → 0.88〜0.89コア | 0.80〜0.81コア |
| 64KiBいっぱいの`{}`の配列(`too_many_values`) | 8 | 0.5コア | 0 | 12,527〜12,588 | 0.46〜0.47コア | 0.10〜0.16 → 0.72〜0.74コア | 0.58〜0.62コア |
| 同上をchunkedで | 8 | 0.5コア | 0 | 12,566〜12,591 | 0.47〜0.55コア | 0.08〜0.16 → 0.75〜0.79コア | 0.59〜0.71コア |
| 1MiB(`too_large`) | 8 | 0.5コア | 0 | 13,458〜13,749 | 0.00〜0.08コア | 0.08〜0.21 → 0.10〜0.23コア | 約0.02コア |

予算を入れる前は、同じ組み立てで2コアが約0.85コア、8コアが約2.2コア増えていた。今の増分は、2コアで0.19〜0.22、8コアで0.58〜0.81で、予算の値の近くに収まる。予算の値を超える分(2コアで0.07〜0.10、8コアで0.08〜0.31)は、主にGCである。

- **予算はGCのCPUを数えない。** 検証のための確保が起こすGCは、予算の外でかかる
- **最も大きく残るのは、上限以内の最悪のbodyを検証する場合。** 8コアで、予算0.5コアに対して0.70〜0.81コアだった。超える分は、主に検証の写しのdecodeの確保(1件で約0.4〜0.5MB)によるGCである
- **次に残るのは、64KiBいっぱいのbodyを`too_many_values`にする場合。** トークンを数えるためにbodyを写すので、1件ごとに約64KiBの確保が残る。8コアで0.58〜0.71コアだった。Content-Lengthの無いbody(chunked、Content-Lengthの無いHTTP/2)も、最初から上限+1バイトで1度だけ確保するので、Content-Length付きと同じ費用になる
- **Content-Lengthが上限(64KiB)を超えるbodyは読まない。** そのまま`too_large`にするので、1MiBのbodyを送り続けられても、増分はほぼ無い。JSONBodyParseが剥がしたBOMの3バイトはContent-Lengthに残るので、BOM付きで65,537〜65,539バイトのbodyも`too_large`になる(数え方だけの差で、応答には関係しない)
- `runtime/metrics`のGCのCPUは推定値で、プロセスのCPUより大きく出ることがある

本番の組み立てでは、`/api`の前で`JSONBodyParse`がbodyを読み(`io.ReadAll`)、JSONとして正しいかを確かめる(`json.Valid`)。同じリクエストのその費用の方がずっと大きく、検証が無くても、同じ送り方で同じくらいのCPUを使わせられる(#3677のレビュー3周目で測った)。

**2コア以下の機械で、資格情報の要らないendpointへの大量のリクエストが心配なら、`disableParamShadow: true`で止める。** 普通のリクエストでの増分はマイクロ秒の桁で、DBを引くhandlerの時間(ミリ秒の桁)に比べて小さい。最悪の場合も上の範囲(予算の値と、それに近いGCの分)で抑えられるので、**既定では有効**にしてある。止まるのは数えるだけの検証で、本家に揃えたendpointの検証は止まらない。そちらの費用は、上の「[本家に揃えたendpoint](#本家に揃えたendpoint3742)」の上限で1件ごとに抑えてある。

## 設定

| キー | 環境変数 | 既定 | 中身 |
|---|---|---|---|
| `disableParamShadow` | `MK_DISABLEPARAMSHADOW` | `false` | `true`で検証だけの実行を止める。止めたendpointでは、検証のための読み取りもしない。本家に揃えたendpoint(`clips/*`・`charts/*`・`hashtags/*`・`antennas/*`・`flash/*`・`gallery/*`・`following/*`・`channels/*`・`blocking/*`・`mute/*`・`renote-mute/*`・`roles/*`・`invite/*`・`notifications/*`・`app/*`・`auth/*`・`miauth/gen-token`・`my/apps`・`sw/*`・`announcements`・`promo/read`・`email-address/available`・`username/available`・`ap/*`・`bubble-game/ranking`・`bubble-game/register`・`federation/instances`を除く`federation/*`・`pages/*`・`page-push`・参照だけのトップレベルの14件)の検証は止めない |
| `paramShadowLogSeconds` | `MK_PARAMSHADOWLOGSECONDS` | `600` | 要約を出す間隔(秒)。0以下は既定に戻す |

どちらも`bindEnvKeys()`に登録してあり、設定ファイルに書かなくても環境変数だけで変えられる。変更には再起動が要る。
