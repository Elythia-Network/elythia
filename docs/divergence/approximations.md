# 純正 Misskey との差分: 逆方向の差分と近似

[差分カタログの目次](../divergence.md) にある分類と基準 (Elythia と追従している本家の版) は、このファイルにも当てはまる。

## 8. 逆方向 divergence (Elythia 独自 error を upstream に合わせて廃止したもの)

`admin/emoji/import-zip` の `NO_SUCH_FILE`、clip 削除の `NOT_CLIPPED`、`notes/translate` の `CANNOT_TRANSLATE` はいずれも Elythia 独自 error だったため upstream に合わせて廃止済み。myReaction fetch の「作成 2 秒以内は skip」guard は Elythia では機能劣化になるため意図的に不採用。

### `notes/drafts/update` は `scheduledNoteLimit` を見ない

upstream の `NoteDraftService.update` は「**未予約 → 予約** に切り替える更新」のときだけ
`scheduledNoteLimit` を数え直し、超過なら `TOO_MANY_SCHEDULED_NOTES`
(`02f5df79-08ae-4a33-8524-f1503c8f6212`) を返す。Elythia の `DraftsUpdate` にはこの検査が
**元から無い**。`notes/drafts/create` 側 (`22ae69eb-...`) は upstream と同じく実装済み。

抜け道は「上限まで予約を作る → 予約でない下書きを作る → update で予約へ切り替える」で、
**上限を 1 件ずつ超えられる**。予約投稿そのものは動くので実害は quota の緩さだけ。

`asynq` driver を消すまで (#2985) は、この id を **driver capability gate** が使っていた
(mkq 以外では予約投稿を丸ごと拒否していた、#1045 Phase 2-C)。既定が mkq になってからは
一度も発火していなかったが、コードが残っていたぶん「update 側にも出口がある」ように
見えていた。gate を消したことで id の出口がゼロになったので、乖離としてここに記録する。
**`errorid-check` では捕まらない** — あれは emission 側の id / code / status の一致しか
見ないので、「emission が無いこと」は検査対象外。

## 9. 実装が近似になっているもの (意図も結末も同じ・数値や内部の形がわずかに違う)

upstream の挙動を再現しているが、依存ライブラリや判定の実装が違うため、**画素値や内部の形までは一致しない**もの。乖離として残す判断ではなく「ここまで揃っていて、残差はこれだけ」という記録。

### プッシュ通知のバッジ画像 (`mediaproxy.processBadge`)

upstream は sharp (libvips) で処理する (#2920 で Elythia も同じ形にした)。**JS の記述を読むだけでは 3 つ取り違える。**

**(1) sharp は呼び出し順ではなく固定の pipeline 順で実行する。** `FileServerProxyHandler.ts` の記述は `resize().greyscale().normalise().linear().flatten()` だが、`sharp/src/pipeline.cc` の実行順は **flatten → greyscale → resize/embed → linear → normalise**。記述順どおりに実装すると輝度レンジの狭い絵文字で平均絶対差 28.7/255・43.8% の画素が 32 以上ずれる。**呼び出し順を入れ替えても sharp の出力が 1 バイトも変わらない**ことで確認できる。

**(2) `normalise()` は min-max ではなく 1/99 パーセンタイル。** sharp の既定は `{lower: 1, upper: 99}` で、`operations.cc` は `luminance.percent()` を使う。min-max で実装すると upstream 自身のテスト画像 `test/resources/192.png` で平均絶対差 23.9/255・最大 42・32 を超える画素 16.7% になる。

**(3) `stats()` は pipeline を無視する。** `sharp/src/stats.cc` は入力を開き直すので、entropy は**元画像**の greyscale ヒストグラムの**標準的な Shannon エントロピー**。`linear(0, 100)` で定数化しても `threshold(128)` で 2 値化しても値が変わらず、96 段のグラデーションで `log2(96) = 6.584963` に一致する。判定は「**元画像**が実質単色か」であって、処理後の mask ではない。

揃っているもの:

- contain なので横長・縦長の絵文字でも内容が切り落とされず、余白は黒帯になる (拡大もする。貼り付け位置は切り捨て)
- 透明部分は黒に落ちる
- greyscale は線形光を経由する (係数を sRGB 値へ直接掛けると純赤が 127 ではなく 54 になり、赤/緑の明暗比が 1.73 → 3.37 に変わる)
- linear (1.75x、0-255 でクリップ) の後に 1/99 パーセンタイルの normalise
- 出力は R=G=B=A なので、**暗いところが透明な silhouette** になる
- 元画像がほぼ単色なら 404 を返し、Service Worker (`create-notification.ts`) が `iconUrl('plus')` へ落ちる

**実測 (sharp の出力と画素単位で突き合わせ)**:

| 入力 | 平均絶対差 | 最大差 | 差が 32 を超える画素 | 完全一致 |
|---|---|---|---|---|
| webp 128x128 (実絵文字) | 0.18 / 255 | 4 | 0.0% | 86.9% |
| png 96x96 (実絵文字) | 4.28 / 255 | 10 | 0.0% | 45.1% |
| gif 100x100 (実絵文字) | 0.23 / 255 | 8 | 0.0% | 84.9% |
| upstream の `test/resources/192.png` | 4.43 / 255 | 15 | 0.0% | 17.7% |
| 同 `192.jpg` | 4.77 / 255 | 15 | 0.0% | 16.9% |

entropy も sharp と一致する (gif は完全一致、他は差 0.03 以下)。**404 になるかどうかの判定は揃う。**

残差の出どころ:

| | upstream (sharp / libvips) | Elythia |
|---|---|---|
| リサンプリング | libvips の lanczos3 | `kovidgoyal/imaging` の Lanczos |
| パーセンタイルの量子化 | LAB の L (0-100 の int) | 0-255 のヒストグラム |
| normalise の色空間 | LAB の L を伸ばして chroma と再結合 | greyscale 値を直接伸ばす |

**これ以上は追わない。** 32 を超える画素がゼロで、通知に出る 96x96 のモノクロ silhouette でこの差は判別できない。

### 画像デコーダ由来の差 (badge に限らない)

`decodeImage` は `kovidgoyal/imaging` を使う。#2920 のレビューで挙がった 3 件を #2925 で実測し、2 件は直した。**全モード (emoji / avatar / preview / static / badge) に効く。**

**インターレース (Adam7) の truecolor PNG — 解消 (#2925)。** imaging は colorType=2 / bitDepth=8 / tRNS 無しの PNG を独自の `*nrgb.Image` に読むが、その Adam7 の pass 合成に `*nrgb.Image` の case が無く、**エラーを返さず全画素 0** を返していた (34 通りの組み合わせで実測し、壊れるのはこの 3 条件が揃ったときだけ)。`internal/misc/imagedecode` に判定を置き、該当するものだけ stdlib の `png.Decode` へ回す。

- **条件を広げてはいけない。** インターレースだけで判定すると、壊れていない種別まで stdlib へ回して **ICC→sRGB 変換を落とす** (Display P3 の PNG でチャンネル差が最大 31/255)。upstream の sharp は `icc_transform("srgb")` を通すので、広げると乖離が増える
- **この経路では EXIF の向きと ICC 変換が失われる。** 代わりに得られるのが「真っ黒な画像」なので、そちらの方がましという判断
- **decode は 2 箇所にある。** media proxy と drive の image processor で、片方だけ直すと**ローカルにアップロードされた画像が真っ黒なサムネイルを storage に焼く** (proxy は variant を優先して返すので原本を読み直さない)。規則を `internal/misc/imagedecode` に 1 つだけ置いてある

**`*image.NYCbCrA` (lossy WebP + alpha) の alpha — 解消 (#2925)。** imaging の scanner は、サブサンプル (4:2:0 / 4:2:2 / 4:4:0 = lossy WebP の通常形) の分岐で alpha の index を内側ループで進めないため、**行の全画素がその行の先頭画素の alpha**になる。行頭が透明な画像は**丸ごと透明**になり、badge は見えないまま 200 で配られる (実測: 128x128 で 50% の画素の alpha が誤り、最大差 255)。

`draw.Draw` / `At()` は逆に alpha は正しいが premultiplied なので**完全に透明な画素の RGB が 0 に潰れる**。`normalizeForResize` が Y / Cb / Cr / A の plane を自分で読んで両方とも正しく取る。**resize 経路だけでなく `processBadge` もここを通す**必要がある。

### MFM の HTML 変換 (`mfm.ToHTML`)

ノートの `content`、プロフィールの `summary`、chat の `content`、RSS / Atom の本文に使う。#3329 で本家 `MfmService.toHtml` にバイト単位で揃えた (改行は CRLF / CR / LF のどれも `<br />`、plain は `<span>`、数式ブロックは `<pre><code>`、ハッシュタグの href は `encodeURIComponent`、ruby / unixtime の出し方も本家どおり)。link / url / メンションの href は、本家の `new URL(x).href` を `internal/activitypub/mfm/whatwg_url.go` で書き写して正規化する (ホストの小文字化と IDN の punycode 化、IPv4 / IPv6 の正規化、既定のポートの削除、`.` / `..` の解決、パス・クエリ・フラグメントの % エスケープ)。合わせた先は本家が動く **Node 26 (ada 3.4)** で、Node 22 とはパスの `^` の扱いが違う。

**実測**: mfm-js 0.26.0 の parse と toHtml の写しを Node 26.4.0 で動かした結果と、構文木と HTML を突き合わせた。数え方は「本家が例外を投げる入力 (下の ruby) を除いた件数」で、違いの件数は HTML の文字列が一致しなかった入力の数。

- 構文の断片を 1〜7 個つないだ乱数入力 15 万件 (3 回): 構文木は全件一致。HTML は 4 件だけ違い、全てリモートのメンションの href の差だった。この計測の時点の Elythia は href を `https://<host>/@<username>` で作っていたので、本家側にも同じ形の url を持つ mentionedRemoteUsers を渡して比べた (違った 4 件は、同じユーザーを大文字小文字違いで 2 回メンションした入力で、本家は最初に見つかったユーザーの url を使う)。その後、メンションの href を本家と同じ作り方にした (下の段落)
- http(s) の URL の各部分をランダムに組んだ入力 36 万件 (4 回): 3 件だけ違い、全てホストに ZWNJ (U+200C) を含むもの (下の表)
- Bidi の種別が違う文字を 4 文字まで並べたホスト 10.8 万件: 全件一致
- メンションの href (`ToHTMLWithMentions`): 一致する要素・大文字小文字違い・url が空や無いとき・host が null や空の要素・同じ利用者が 2 つあるとき・`ſ` (U+017F) を含む username / host・正規化とエスケープ・読めない url / uri の 21 件 (`mention_href_test.go` と `html_upstream_test.go` の該当行): 全件一致

**メンションの href (#3329)。** 本家と同じく、ノートの `mentionedRemoteUsers` 列から username と host を toLowerCase して一致する最初の利用者を探し、`url` (空なら `uri`) へリンクする。見つからないメンションは `<config.url>/<書かれたままの acct>` (`https://<host>/@user@host`) にする。列を渡すのは本家と同じくノートの `content` (`ApMfmService.getNoteHtml`) と RSS / Atom の本文 (`FeedService`) で、プロフィールの `summary` は本家も渡さない。列はローカルのノートの作成時に、本家 `insertNote` と同じ形 (`uri` / `url` / `username` / `host`、メンションの順) で書く。chat の `content` は列を持たないので渡さない。

残っている差:

| 項目 | 本家 | Elythia | 理由 |
|---|---|---|---|
| メンション先の利用者の `url` / `uri` が http / https 以外 | `new URL()` が読めればリンクにする (`javascript:` も) | リンクにせず acct の文字にする | XSS 防止。値はリモートの actor が送ってきたもの。本家も actor を取り込むときに `url` を http(s) に限る (ApPersonService の checkHttps) ので、通常は差が出ない |
| `mentionedRemoteUsers` 列が JSON として読めない | `JSON.parse` が投げ、ノートの HTML を作れない | 空の配列として扱う (全て自サーバーのリンクになる) | 例外で配送やフィードを止めない。列は Elythia か TS 版が書いたものしか入らない |
| ノートの作成時にメンション先の利用者を引けない | ノートの作成ごと失敗する | 列を `[]` にして作成を続ける。プロフィールだけ引けないときは `url` を省く (`uri` へのリンクになる) | メンションの href が変わるだけで、宛先や通知は `mentions` 列で決まる |
| リモートのノートの `mentionedRemoteUsers` 列 | 取り込むときにも書く | 書かない (既定の `[]`) | 列を読むのはローカルのノートの描画 (連合の `content` とフィード) だけ |
| 子が 1 つで半角空白を含まない ruby (`$[ruby abc]`) と、1 つの子が文字でない ruby | `escapeHtml(undefined)` で TypeError を投げ、ノートの HTML を作れない | 斜体 (`<i>…</i>`) にする | 例外で配送や描画を止めない。本家の不明な fn と同じ形 |
| http / https 以外の scheme の link | `new URL()` が読めればリンクにする (`javascript:` も) | リンクにせず `[文字](url)` の文字にする | XSS 防止。mfm-js の link は http(s) しか作らないので、Parse の結果では起きない |
| ホストの ZWNJ (U+200C) の前後の検査 (CheckJoiners) | ada は「ZWNJ より前のどこかに Joining_Type が L / D の文字、後ろのどこかに R / D の文字」があれば通す | x/net/idna の判定 (RFC 5892 の正規表現に近く、間に非結合の文字を挟むと落とす。逆に ZWNJ の直後の非結合の文字で判定を打ち切って通すこともある) | Joining_Type の表は x/net/idna の中にあり外から呼べない。ペルシア語のように結合する文字の間に ZWNJ を置く普通の綴りはどちらも通る。違うのは `ب_` + ZWNJ + `ب` (Node だけ通す) や `ت` + ZWNJ + `,$0.` (Elythia だけ通す) のような形だけ |
| 非 ASCII のホストのその他の細部 | ada の UTS #46 | x/net/idna (Go 1.27 では Unicode 17 の表)。Bidi の検査は ada の実測に合わせた自前の判定 (`adaBidiLabelOK`)。mapping 後に `xn--` で始まるラベルの検査は、mapping のうち句点・NFKC・小文字化と主な無視される文字だけを近似する。区切りが先頭にしかない punycode (`xn---abc`) は x/net/idna が読まないので、区切りを外して検査する。非常に長い非 ASCII のラベル (`例a` を 1000 回繰り返した 2000 文字など) は、x/net/idna の punycode の符号化が桁あふれの検査で失敗するのでリンクにしない (Node はリンクにする) | ada の Bidi の検査は RFC 5893 と違い (先頭が L のラベルは最後の文字を見ないなど)、x/net/idna の mapping は外から呼べない。長いラベルは DNS のラベルの上限 (63 文字) を大きく超えるので実害は無い。上の乱数入力 (ホストは数文字) では ZWNJ 以外の差は出ていない |

### API のリクエスト body の読み方 (#3330)

本家は body を JS の object にしてから、各 endpoint の paramDef (常に `type: 'object'`) で ajv に検査させる (`server/api/endpoint-base.ts`)。検査は認証・権限の確認の後で、handler の手前にある (`ApiCallService.ts` の `call`)。Elythia は次の 2 点を揃えている。実装は `internal/server/json_serializer.go` と `json_binder.go` にある。

- **キーは完全一致で読む。** `encoding/json` (v1) は大文字小文字を無視した一致も採るので、`{"reportid": ...}` が reportId に入り、`{"reportId": "r1", "REPORTID": null}` では後の null が勝っていた。Go 1.27 で既定有効の `encoding/json/v2` に v1 の option 一式を渡し、`MatchCaseInsensitiveNames` だけを外して decode する。**`GOEXPERIMENT=nojsonv2` ではビルドできない。** 認証 middleware が body から読む `i` も完全一致にした
- **token の読み方も本家に揃えた。** body の `i` は `application/json` の body からだけ読み、`?i=` の query は GET のときだけ読む (本家は GET では query を、それ以外では body を params にする。`ApiCallService.ts` の `handleRequest`)。GET の body は読まない。**form-urlencoded や text/plain の Content-Type で JSON を送る緩いクライアントと、POST の URL に `?i=` を付けるクライアントは匿名扱いになる** (認証が要る endpoint では CREDENTIAL_REQUIRED)。本家では text/plain と POST の `?i=` は同じく匿名で、form-urlencoded は Fastify が 415 で弾く (Elythia は 415 にしない。下の「未知の Content-Type」)
- **body が object でなければ INVALID_PARAM (`info: {param: "#/type", reason: "must be object"}`) にする。** 対象は `/api/` の endpoint (本家が endpoint の外で受ける `signup` / `signup-pending` / `signin-flow` / `signin-with-passkey` / `miauth/:session/check` / `clear-browser-cache` を除く) で、body が `null` / 配列 / 文字列 / 数値 / 真偽値のとき、body が無いとき (Content-Type 無し、または text/plain。長さの分からない chunked の空の body も含む)、text/plain の body のとき。空の `application/json` は従来どおり Fastify の `FST_ERR_CTP_EMPTY_JSON_BODY` を返す。`api.POST` / `api.Match` で登録した route には、route の middleware (認証・権限) の後ろに `requireObjectBody` が付くので、bind しない handler (引数の無い endpoint) にも効き、認証の確認が先に応答する順序も本家と同じになる。bind のエラーは、全ての handler が INVALID_PARAM (`apierr.JSONInvalidParam`) で返す (以前は捨てるか 204 / 200 にしていた handler があった)
- **`drive/files/create` に multipart の file が無ければ、本文の無い 400 を返す。** 本家は requireFile の endpoint を別の経路 (`ApiCallService.ts` の `handleMultipartRequest`) で受け、最初の `request.file()` が失敗するか file の part が無ければ `reply.code(400); reply.send()` で返す。これは token を読む前なので、token が無くても無効でも同じ 400 になる。Elythia は `middleware.RequireMultipartFile` を認証 middleware より前の global middleware に置き、Content-Type が無い・text/plain・object でも何でも JSON・multipart だが file の part が無い・multipart が読めない、のどれでも 400 (本文も Content-Type も無し。`Cache-Control: private, max-age=0, must-revalidate` は付ける) を返す。空や壊れた JSON は従来どおり Fastify の `FST_ERR_CTP_*` を返す (Fastify は handler の前に body を parse する)。file の part は field 名を問わない (本家の `request.file()` は最初の file の part を返す)
- **multipart の真偽値の field (`isSensitive` / `force`) と、GET の query の数値・真偽値は、本家と同じく JSON として変換する。** 本家は `ApiCallService.ts` の `call` で、GET の query と requireFile の field のうち paramDef が boolean / number / integer のものを `JSON.parse` し、失敗すると ajv より先に INVALID_PARAM (id `0b5f1631-7c1a-41a6-b399-cce335f34d85`、`info: {param: "<名前>", reason: "cannot cast to <型>"}`) を返す。GET は `apiBinder` が bind の前に `query` タグの付いた field を見て変換できない値を印し、handler の `apierr.JSONInvalidParam` (と `JSONInvalidParamClient`) がこの形で返す。multipart は `drive/files/create` の handler が見る。変換できても真偽値でない値 (`"1"` や `"null"`) は ajv の `#/properties/<名前>/type` で落とす (以前は `"true"` 以外を全て false と読んでいた)。multipart の field は本文の値だけを読み、URL の query は見ない
- **INVALID_PARAM の id は、本家の 2 つ (ajv の `3d81ceae-…` と、変換の `0b5f1631-…`) だけにした。** 以前は `auth/session/*`、`auth/accept`、`miauth/gen-token`、`ap/get` / `ap/show`、`i/2fa/*`、`i/claim-achievement`、`i/webhooks/*`、`sw/*`、`reversi/*`、`bubble-game/*`、`chat/*`、`fetch-external-resources`、`admin/emoji/import-zip` の handler が独自の id (`ed1d7571-…` など) で返していた。`apierr` の `TestInvalidParamIDLint` が、本家に対応物の無い Elythia 独自の endpoint、本家の endpoint 自身が定義している id (`users/lists/list` の `ab36de0e-…`)、下の `reset-password` 以外を落とす
- **`signin-flow` と `signin-with-passkey` の body は、本家と同じく型を検査せずに読む。** 本家はこの 2 つを endpoint の外 (`SigninApiService` / `SigninWithPasskeyApiService`) で受け、ajv を通さずに `body['credential']` などを読む。以前の Elythia は `signin-with-passkey` の body を struct に bind し、配列や文字列の body・文字列でない `context` を本家に無い独自の id (`ed1d7571-…`、`code` も無い) の 400 で返していた。今は、object でない body (配列・文字列・数値・真偽値・text/plain) は credential が無いものとして challenge を返し、credential があって `context` が文字列でなければ本家と同じ `1658cc2e-…` の 400 を返す。credential の有無は両 endpoint とも JS の truthy で見る (`null` / `false` / `0` / `""` は無いものとして扱う。`signin-flow` では鍵の検証に進まず challenge を返す)。`signin-flow` で credential を request に包めないときの同じ id の 400 は、包む処理を失敗しない形 (`http.Request.Clone`) にして経路ごと無くした (この処理は `i/2fa/key-done` と共通の `twofactor.CredentialRequest` にまとめた)。`signin-flow` の `username` が文字列でないとき (body が object でない場合も含む) と、`token` / `password` が null でも文字列でもないときは、本家と同じく本文も Content-Type も無い 400 を返す (本家の `reply.code(400); return;`)。`token` はユーザーを引く前、`password` はユーザーを引いて凍結を確かめた後に見る順も本家と同じ。空文字列の `username` は文字列なので 404 (`6cc579cc-…`)。captcha の応答が文字列でないときは、本家と同じく captcha の検証に落ちる (`CAPTCHA_FAILED`。以前は body を読めないものとして `6cc579cc-…` の 400 にしていた)。3 つ (`signin` を含む) とも、キーは本家と同じく完全一致で読む (`{"CREDENTIAL": ...}` は credential にならない)。本番の JSONSerializer も完全一致だが、handler が c.Bind を通すと読み方が serializer の配線に依存するので、handler 自身で `application/json` の body を完全一致で decode する。`apierr` の `TestRetiredCustomIDIsGone` が、`internal/api` にこの id の文字列が残っていれば落とす

残っている差は次のとおり。

- **`drive/files/create` に未知の Content-Type (`application/x-www-form-urlencoded` など) の body を送ると、本家は Fastify の 415、Elythia は上の本文の無い 400 になる。** 下の「未知の Content-Type」と同じ理由で 415 は作っていない
- **`drive/files/create` の file が `file` 以外の field 名で来ると、本家は受け付けるが Elythia は INVALID_PARAM になる。** handler が `file` の field しか読まない
- **GET の変換は Go の field の型から param の型を決める。** 本家は paramDef を見る。整数の field は integer、浮動小数の field は number、bool の field は boolean として扱い、`query` タグの無い field と配列は見ない。複数の値が変換に失敗したときは、本家は paramDef の順、Elythia は struct の field の順で最初のものを返す。`JSON.parse` は通るが echo が読めない値 (整数の field への `1e3` など) は、従来どおり bind のエラー (`3d81ceae-…`) になる。逆に真偽値の field への `1` / `0` は、本家は ajv の型の違反だが、echo の binder は真偽値として読む
- **`info` が付くのは `apierr.JSONInvalidParam` / `JSONInvalidParamClient` / `InvalidParamClient` を通した応答で、binder が印を付けた場合と、handler が ajv の違反を名指しする場合だけ。** 独自の文言で INVALID_PARAM を組む handler (`c.JSON(400, apierr.Error("INVALID_PARAM", "userId is required.", apierr.UUIDInvalidParam))` など) は、id は一致するが message が本家の `Invalid param.` と違い、`info` も付かない
- **ajv の外で失敗する経路の一部を、Elythia は INVALID_PARAM (400) で返す。** `i/2fa/done` で 2 段階認証の設定を始めていないとき、本家は素の `Error` を投げて 500 INTERNAL_ERROR になる (`endpoints/i/2fa/done.ts`)。Elythia は INVALID_PARAM (id は `3d81ceae-…`) を返す。`fetch-external-resources` の url が http(s) でないときも、本家に同じ検査は無く、Elythia は同じ INVALID_PARAM を返す。`reset-password` の token が無い・期限切れのときは、本家は `findOneByOrFail` と素の `Error` で 500 になり (`endpoints/reset-password.ts`)、Elythia は INVALID_PARAM (400、Elythia 独自の id `6382759e-…`) を返す
- **プラグインの `Request.Bind` (`internal/server/plugin_wiring.go`) と、独自の `UnmarshalJSON` を持つ型の中身 (`optional.Nullable[T]`、`role.CondFormula`) は、従来どおり大文字小文字を無視する。** プラグインは本家に無い経路で、`Nullable` の中身は今は全て scalar。プラグインの route には `requireObjectBody` も付けない
- **`application/x-www-form-urlencoded` などの未知の Content-Type** は、本家では Fastify が 415 (`FST_ERR_CTP_INVALID_MEDIA_TYPE`。`/api` に登録された parser は JSON・text/plain・`@fastify/multipart` の multipart/form-data だけ) で弾くが、Elythia は 415 にしない。endpoint は echo の binder に任せたまま (この節の対象外)。**endpoint の外の `signin-flow` / `signin-with-passkey` (と Elythia 独自の互換 shim の `signin`) は binder を使わず、`application/json` の body だけを読む** (#3330)。そのため form-urlencoded などの body には、本家の 415 に対して、`signin-with-passkey` は credential が無いものとして challenge の 200 を、`signin-flow` は username が無いものとして本文の無い 400 を、`signin` は `6cc579cc-…` の 400 を返す。text/plain の body は、本家では Fastify が文字列にするので credential や username が undefined になり、`signin-with-passkey` は本家も Elythia も challenge を、`signin-flow` は本家も Elythia も本文の無い 400 を返す。multipart/form-data の body は、本家では `@fastify/multipart` の parser が body を設定しないので `body['credential']` が TypeError になって 500、Elythia は `signin-with-passkey` では challenge の 200、`signin-flow` では本文の無い 400 を返す
- **`signin-with-passkey` に `null` の body か body 無しで送ると、本家は `body['credential']` が TypeError になって Fastify の 500 を返すが、Elythia は credential が無いものとして challenge を返す。** 本家の例外を再現する意味が無いため。本家の `signin-flow` も同じ body で 500 になるが、Elythia は username が無いものとして本文の無い 400 を返す
- **`signin-with-passkey` の credential に `id` が無いとき (credential が object でない場合も含む)、Elythia は鍵の有無に関わらず `b18c89a7` を返す。** 本家は `findOneBy({ id: undefined })` の条件を TypeORM が落として任意の鍵を引き、`@simplewebauthn` が `Missing credential ID` で throw するので `b18c89a7` になるが、鍵が 1 件も無い instance だけは `36b96a7d` になる。文字列でない `id` (数値など) は、どの鍵とも一致しないものとして `36b96a7d` にする (本家の TypeORM は値の型で引き方が変わる)。鍵の持ち主が引けないとき (行が無いか remote) は、本家と同じく検証を通したあとで `652f899f` を返す
