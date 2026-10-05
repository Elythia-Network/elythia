# 静的ゲート (`make gates`)

`make gates` は、サーバー・ブラウザ・Docker を使わずに回る静的な検査の集まりです。
一覧と 1 行の説明は `make help` に出ます。ゲートを新しく書くときの原則は
[CLAUDE.md の Section 4「ゲートを書くとき」](../CLAUDE.md#ゲートを書くとき) にあります。

## 一覧

`Makefile` の `gates:` に並んでいる順です。

| target | 見ているもの |
|---|---|
| `shapecheck` | レスポンス形状の drift を検査 |
| `errorid-check` | error id / HTTP status / kind の drift を検査 |
| `limitspec-check` | ページネーションの default / max の drift を検査 |
| `perm-check` | router middleware の権限が upstream より緩くないか検査 |
| `wiring-check` | router で配線が必要なものが外れていないか検査 |
| `catalog-check` | システムカタログのクエリが schema で絞られているか検査 |
| `notfound-check` | repository の lookup error を種別を見ずに 4xx にしていないか検査 |
| `nulparam-check` | 列に入らない値 (NUL) が SQL の bind parameter に載らないか検査 |
| `compose-check` | 配布する compose のログの上限と、検証用 compose の置き場所・相対パス・name: を検査 |
| `testflags-check` | make test が CI と同じテスト条件で走るか検査 |
| `migrationdoc-check` | migration の本数を述べた doc が実態と合っているか検査 |
| `mdtable-check` | md の表の各行がヘッダと同じ列数か検査 (溢れたセルは描画時に捨てられる) |
| `notiftype-check` | 通知タイプの一覧が 1 箇所から導出されているか検査 |
| `pluginembed-check` | mk-go をビルドする Dockerfile が pluginbuild を go build より前に実行するか検査 |
| `dockerignore-check` | .dockerignore がシークレットと利用者データを除外しているか検査 |
| `secretfield-check` | モデルの秘密フィールドが json:"-" を保っているか検査 |
| `ipshape-check` | レスポンス / 連合の shape に IP が出ていないか検査 |
| `iprecord-check` | 利用者の IP を記録する call site が allowlist の外に増えていないか検査 |
| `sqlbind-check` | 値をクォート内へ差し込まずバインドしているか検査 |
| `gaterun-check` | gates の -run が名指しするテストが実在するか検査 |

## 変更の経緯 (旧 CLAUDE.md の更新記録)

CLAUDE.md の「更新記録」に書かれていた本文を、#3248 でここへ移した。**記述は当時のまま**で、文中の「Section N」は当時の CLAUDE.md の節を指す。新しいものが上。

- **2026-09-22**: `make gates` に `sqlbind-check` を追加。`make help` の target は 136 → 137。**値をクォートで囲んだリテラルへ差し込まず bind する**、を固定する。
  chart の unique 配列は**行の値そのもの** (外部由来の文字列を含みうる) なので、
  `internal/core/chart/repository.go` の `ApplyDeltas` が `?::varchar[]` で bind して
  いるのが唯一の防波堤になっている。初版 (`50ba697b`、2026-04-09) からその形だが、
  **担保が無かった** — 実測で、配列リテラルを書式へ差し込む形に変えても既存の実 DB
  テストは**緑のまま通った** (`TestIntegration_GormRepository_UniqueIncrementApplyDeltas`
  の値が `u1` / `u2` だけで、構造上意味を持つ文字を含んでいない)。
  **「これは SQL か」を判定しない。** 初版はキーワードの部分一致で SQL らしさを
  判定していたが、敵対的レビューで**両方向に壊れている**ことを実測された。偽陽性:
  `"plugin '%s' returned no values"` のような普通の英文が `values` に当たり、
  「プレースホルダで bind すること」という**事実と逆の診断**が出る (allowlist の理由欄は
  「なぜ SQL として解釈されないか」を書かせる形なので、非 SQL には書きようがない)。
  偽陰性: `'%s'::varchar[]` のような**断片**はキーワードに 1 つも当たらず**収集すら
  されない** — このリポジトリには SQL を断片ごとに組んで `strings.Join` する経路が
  (chart / fsck / maintenance など) あり、そこが丸ごと盲点だった。実際、`Insert` の
  placeholder を `fmt.Sprintf` へ変える変異は**静的ゲートも chart の全テストも
  緑のまま通った** (レビュー側が使い捨て probe で実測し、値が SQL テキストに載ることを確認)。
  判定を「クォートで開いた区間に動詞が在るか」だけにすると射程は広がり、偽陽性は減る。
  **代わりに網は広がる** — 値をクォートで囲んだだけのメッセージも該当しうるので、
  診断は「bind しろ」と決めつけず、SQL でない場合の逃げ道も示す。
  **「プレースホルダが在るか」では守れない。** ダミーの `?` を 1 つ足し、
  `pgArrayLiteral` とは**別名の独立したビルダ**で配列をリテラルへ畳み、要素数で
  分岐させる変異は、**静的ゲートも chart の全テストも緑のまま通った** (実測。
  リテラルが SQL テキストに載ることは手元で確認した。閾値をテストの値数より上に
  置くと振る舞い側も外れる)。
  `ApplyDeltas` が組む**書式集合そのものを pin** すると、書式を**変える**形は落ちる。
  **ただし pin は「その Sprintf が在るか」しか見ない** — 呼び出しを `_ =` にして
  残したまま、実際の組み立てを文字列連結へ移す変異は**静的ゲートを素通りし、
  実 DB の往復テストだけが落とす** (実測)。静的な走査で意図的な回避まで塞ぐことは
  できないので (#3135「名前で見る走査は名前で避けられる」)、ここが受け持つのは
  **普通に書いたときに踏む形**で、意図的な回避は振る舞い側が受ける。
  **書式は定数連結を畳んでから見る。** 長い SQL を `"..." + "..."` で折り返すのは
  普通の書き方で、第 1 引数が `*ast.BasicLit` のときだけ拾う形は**その書式を 1 件も
  収集しない** (実測)。違反ではなく**不在**になるので、違反集合の比較では気付けない。
  **名前を決め打ちにしない。** `pgArrayLiteral` の行き先を `append(args, ...)` で
  見ていたため、局所変数を `params` へ改名しただけでゲートが落ち、しかも**既に
  bind しているコードへ「バインド引数で渡すこと」と事実と逆の指示**を出した (実測)。
  `Exec` / `Raw` へ可変長展開している識別子を解決して突き合わせる形に直した。
  **名指しの一覧は `map` で回さない。** イテレーション順が非決定的なので、走査が
  縮んだときに報告されるサイトが実行ごとに変わる。ソート済みの slice で回し、
  未検出を集めてから 1 回で報告する。**違反の報告も `require` にしない** — 先に
  止まって allowlist の死んだ entry の診断が出ない (#3135 と同じ型)。
  **数え方**: `internal` / `cmd` / `plugin` の非テスト Go を AST で走査し、`fmt.Sprintf`
  の書式リテラル (定数連結は畳む) をサイト単位で数えて **186 件** (ユニークキー 103、
  うち人工ソース 6 サイト)。違反は **4 サイト / 3 キー**で、内訳は人工ソース 2 キーと
  `internal/server/frontend.go#renderFrontendShell` の 1 キー (JS 生成が 2 サイト。
  入るのは Vite manifest のエントリ名とビルド版数)。いずれも allowlist 済みで、
  **本番の未許可の違反は 0**。
  **検出の枝は人工ソース (`internal/entitycompat/sqlbindfixture`) で固定する。** 本番の
  違反が allowlist 済みの 1 キーしか無いので、本番だけを見ていると枝が一度しか通らない。
  `testdata/` に置くと Go ツールチェーンが無視して `go vet` の書式検査も通らないので、
  コンパイルされるパッケージに置く (`_test.go` を持たないパッケージは `go list` の
  テスト対象にも CI のカバレッジ閾値にも入らない)。
  **gofmt はトップレベル宣言の doc コメントの中のクォート 2 連だけを typographic quote へ書き換える**
  (関数の中のコメントには効かない。`b7c419f9` と同じ形)。この作業でも実際に踏んだ。
  **変異検証は 19 形 (production 13 / ゲート側 6)。** production: 素の補間 /
  クォートを書式の外へ出す / decoy (独立ビルダ + ダミーの `?` + 要素数で分岐) /
  `Insert` の placeholder を `Sprintf` 化 / `pgArrayLiteral` のエスケープを外す 3 形
  (`\` のみ・`"` のみ・両方) / 定数連結で折り返した違反形 / 値をクォートで囲んだ
  英文メッセージ (**検出される**。SQL でないので allowlist に値の出どころを書く形) /
  allowlist のキーと同名の宣言を同じファイルへ足す / **落ちてはいけない 2 形**
  (局所変数 `args` を `params` へ改名 / `lit := pgArrayLiteral(...)` と局所変数に受ける) /
  **既知の穴 1 形** (pin 対象の `Sprintf` を `_ =` で残したまま組み立てを文字列連結へ
  移すと**静的ゲートは素通りし、実 DB の往復テストだけが落とす**)。
  ゲート側: `%%` の skip を外す / クォートの状態を反転させない / 定数畳み込みを
  無効化 / 走査ルートから `internal` / `cmd` / `plugin` をそれぞれ外す 3 形
  (`cmd` と `plugin` は名指しの一覧が `internal` に偏っていると黙って通る。
  レビューで指摘されて両 root のアンカーを足した)。
  **クォートが釣り合わない書式は「判定できない」として報告する。** パリティの
  状態機械なので、`-- don't touch` を先頭に置くだけで以降の内外が反転し、**後続の
  本物の違反が 0 件に化ける** (実測)。逆に `can't resolve %s` のような英文は、
  存在しないリテラルの内側だと判定される。安全側へ倒さず、最初のクォートより後ろの
  動詞をすべて報告する形にした (corpus は不変)。
  **キーの曖昧さも見る。** `<file>#<func>` は Go がパッケージ関数と別型のメソッドに
  同名を許すので衝突しうる。ただし同名自体は正当なので (`internal/activitypub/types.go`
  は別々の型に `UnmarshalJSON` を持つ)、落とすのは**その key を allowlist か名指しの
  一覧が使っているときだけ**にしてある。
  **分担が分かれているのが要点** — 書式の退行は静的ゲートが落とし、エスケープの
  退行は書式の形が変わらないので静的ゲートには見えず、`TestPgArrayLiteral_*` と
  実 DB の往復テストが受け持つ。逆に述語の退行は実 DB では何も起きない。
  実 DB テストが担うのは **Go の golden と往復では表現できない側** — 配列リテラルの
  入力パーサは PostgreSQL が権威なので、`pgArrayLiteral` と `parsePgTextArray` を
  対称に壊して Go 内では辻褄が合う形でも、往復の値が変われば落ちる
  (`\` / `"` については `TestPgArrayLiteral_EscapesQuotesAndBackslashes` が
  書き側の出力を golden で pin しているので、そちらでも落ちる)。
  **射程外**: taint 解析はしない (#2644 と同じ理由)。書式を `const` や変数に入れた
  `Sprintf`、書式の途中に変数を連結した形、`fmt.Sprintf` 以外 (`Fprintf` / `Errorf`)、
  `fmt` の別名 import、文字列連結や `strings.Builder` で組む SQL、`Raw` / `Exec` へ渡す
  非リテラル、別 module の `plugins/`、`internal` / `cmd` / `plugin` 以外のツリー。
  **名前で見る走査は名前で避けられる** (#3135) ので、確実に落とすのは
  「普通に書いたときに踏む形」に寄せてある。
- **2026-09-21**: `make gates` に `ipshape-check` を追加 (#3136)。`make help` の target は 134 → 135。**#3066 の完了条件「IP 情報が一般ユーザー向け API や連合へ露出しない」の担保が shapecheck の golden 照合しか無かった** — `UserLite` に `json:"lastIPs"` を足して `make shapecheck` は PASS する (実測)。**いまは漏れていない**が、担保が無かった。
  **reflect で型を手で並べない。** 初版はそう書いて `entity.MeDetailed` (= `/api/i`) を落としていた。**AST で全 struct の json タグを読む**形にすると型を列挙しないので落としようがない。走査は `internal/entity` / `internal/activitypub` だけでは足りない — **handler が自分のファイルに宣言した response struct も stream のイベント payload も、そのまま wire の形になる**ので `internal/api` / `internal/server` / `internal/stream` も入れる。実測は**要素 2,332 / ユニークキー 755** (数え方: `scanJSONTags` が `publicShapeDirs` 全体で返した要素数と、その `Key` のユニーク数)。IP を指すキーを持つのは 11 件で、10 件は `internal/api/admin` の IP 照会 API (モデレーター + policy + scope の 3 段)、1 件はレスポンスに出ない入力構造体。
  **語の切り方は片側に寄せると必ず穴が開く。** 大文字のたびに割ると `lastIPs` が `last` + `i` + `ps` になって Go の命名規約に素直な名前だけが素通りし、「小文字/数字の直後の大文字」だけにすると `IPAddr` / `IPHash` / `IPList` が 1 語に潰れて素通りする。**2 稿にわたって片側ずつ落とした** (どちらも敵対的レビューで実測)。境界は 2 つ要る — 小文字/数字の直後と、**大文字が 2 つ以上続いた後の、小文字が続く大文字**。さらに **`address` 系の alternative も要る** — 割れるのは `IPAddress` だけで `IPaddress` / `ipaddress` は1 語に潰れる。「割るから要らない」と書いて一度落とし、3 周目で実測された。
  **キーの走査だけでは入れ子が見えない。** `Session *model.Signin` を 1 フィールド足すだけで `encoding/json` はその中の `ip` を出す。さらに **`model.User` は自分では IP を持たないのに `Avatar *model.DriveFile` 越しに `avatar.requestIp` を出す**ので、1 段だけでは足りない。`internal/` 全体から「自分の JSON キーに IP を持つ名前付き struct」を**導出して推移的に追う**。**interface と関数型のフィールドは伝播させない** — 混ぜると repository 一式が汚れて 18 件が誤検出になる (実測)。
  **収集ロジックは `encoding/json` の実挙動と突き合わせる。** 人工ソースは `testdata/` ではなく**コンパイルされるパッケージ**に置く — あちらは Go ツールチェーンが無視するので `json.Marshal` と比べられず、期待値を手で書くことになる。拾う形は 5 つ — タグ無し exported はフィールド名、`json:"-"` と非公開は出ない、**タグ無しの匿名埋め込みは昇格**、**タグ付きの匿名埋め込みはタグ名 1 つ**、**struct でない名前付き型の埋め込みは型名**。**昇格の枝は、埋め込まれる型が非公開でないと固定できない** — exported だとその型自身の宣言からも同じキーが出るので、埋め込みを消しても突き合わせが食い違わない (実測)。
  **「違反 0 件が正常」な検査は、抽出側にも下限が要る。** `scanJSONTags` には下限を置いたのに `typeRefs` (参照側) に置かず、**package 修飾の収集を落とす 1 行で `*model.Signin` の漏れごと素通りした** (敵対的レビュー 2 本が独立に実測)。allowlist にも**死んだ entry の検査を必ず付ける** — 付け忘れた側で、**実在しない entry を捏造した状態が全テスト緑のまま通った**。
  **走査が縮んだことは、件数と代表キーでは見られない。** 代表キーが大きな 2 ファイルに偏っていたため、**14 ファイルを落とす変異が本物の漏れごと素通りした**。`json:"` を含むファイルは 1 件以上寄与することを**ファイル単位**で要求し、truth は AST ではなくテキスト走査から採る (AST が壊れれば必ず食い違う)。**その truth 側にも下限が要る** — 1 ファイルに縮める変異で検査が丸ごと無意味になる。
  **射程外**: `map[string]any` を手で組む経路 (`entity.PackSignin` / nodeinfo)、`datatypes.JSON` の中身、**`publicShapeDirs` の外に宣言された型を handler が `c.JSON` にそのまま渡す形** (`/api/server-info` が返す `serverstats.PublicStats` が実例。`internal/core` を走査に足すのは採らなかった — IP を持つ内部の入力構造体が 11 件流れ込んで allowlist が倍増し、本物の signal が埋もれる)、`remoteAddr` / `CDNIPs` / `ip4s` のように語として `ip` を取り出せない綴り。
- **2026-09-21**: `make gates` に `iprecord-check` を追加 (#3135)。`make help` の target は 135 → 136 (同日に入れた `ipshape-check` の次)。**#3105 の関連アカウント検索は `user_ip` の観測だけを見るので、失敗したサインインの IP がそこに入ると第三者が他人の関連候補を作れる** — 攻撃者が対象アカウントの ID で自分の IP から失敗を繰り返せば、その IP が対象の「使用した IP」として記録され、攻撃者自身のアカウントが候補に並ぶ。**いまは入っていない**が、担保が無かった。
  **振る舞いテストだけでは守れない。** 守りたいのは「どこからも呼ばれていない」という構造的な性質で、endpoint ごとのテストは叩いた経路しか見ない。実際、`/api/signin` だけを叩くテストは `SigninFlow` (同梱フロントが実際に使うほう) に記録を足す変異を素通りさせた (実測)。非同期化 (`go h.ipRecorder.Record(...)`) は**初版の振る舞いテストでは不安定にしか捕まらなかった** (実測で `-race` 5 回中 3-4 回)。`fail()` 到達を `require.Eventually` で待つようにしてからは 10/10 で落ちる。
  **静的ゲートだけでも守れない。** allowlist のキーは `<file>#<func>` なので、**allowlist 済みの関数の中に `Record` を足す**変異も、そこから allowlist 済みの `RecordSuccessfulSignin` を呼ぶ変異も、call site の一覧としては何も変わらない。しかも `signin-with-passkey` は振る舞いテストを 1 つも持っていなかったので、**その組み合わせが両側とも空白だった** (敵対的レビュー 2 本が同じ穴を別経路で実測)。passkey の失敗 4 分岐にrecorder のアサーションを置き、`RecordSuccessfulSignin` の呼び出し側も別の allowlist で固定した。
  **名前で見る走査は名前で避けられる。** `rec := h.ipRecorder.Record; rec(a, b)` と書くと呼び出し側が`*ast.Ident` になり、引数 2 の `.Record(` としては現れない。**呼び出さずに値として持ち出す形も call site として数える。** package 変数に入れたクロージャの中も見る (`FuncDecl` だけを走査する形では見えない)。**この対処を片方の走査にしか入れず、2 周目で指摘された** — 成功入口 (`RecordSuccessfulSignin`) の走査に同じ枝が無く、**署名検証より前に呼ばれる `resolvePasskeyUser` (今の `resolvePasskeyKey`) にメソッド値で仕込む変異が静的ゲートも振る舞いテストも素通りした** (そこは攻撃者が送った値 (当時は `userHandle`、今は credential の `id`) で user を引く段階なので、被害者の `user_ip` に自分の IP を入れられる)。走査は 1 本に統合した。**型の位置とフィールドアクセスは除く** — `a.Record{}` / `h.Record.Val` まで call site にすると診断が事実と無関係なことを断定する。**枝そのものは人工ソース (`internal/entitycompat/recordfixture`) で固定する** — 実データにその形が無いので、枝を消しても実データからは何も起こらない。
  **call site の列挙だけでは順序が固定できない。** パスキーの記録を `fail()` より前へ動かしても場所は変わらないので allowlist は通る。順序テストは**最初の `Record`** を基準にする — 最後のものを見る形だと、既存の呼び出しを残したまま前にもう 1 つ**足す**変異が素通りする (実測)。
  **件数の下限は診断を壊す。** 「allowlist の長さ以上か」は dead-entry 検査より論理的に弱く(下限が落ちる状況では必ず dead-entry も落ちる)、しかも `require` なので**先に止めて正しい診断を奪う**。call site を正当に 1 つ消しただけでも「抽出が壊れている」と事実と逆を出した (実測)。実在する call site を名指しで要求する形 (`secretfield-check` の `mustDetectSecretFields` と同じ) に替えた。
  **allowlist の理由も裏取りする。** 「`RecordSuccessfulSignin` が唯一の成功入口」と書いたが、**すぐ下の entry 自身が「passkey は経由せず直接呼ぶ」と書いており矛盾していた**。同じ型で「呼ぶのは 2 箇所」も誤り (実際は 3 箇所)、`deliveryhealth` の関数名 `Record` も推測 (実際は `RecordDelivery`。ゲート自身に訂正された)。
  **「テストにできない」も裏取りする。** 失敗の記録が非同期だから `fail()` 到達を確かめられない、と書いたが誤りだった — `MockSigninRepository` は mutex 付きの `Len()` を公開しており、**同 package の既存テストが既に `require.Eventually` で待っている**。到達確認を入れないと、将来 `fail()` の手前で返すようになっても「IP を記録しない」が自明に真になって空虚化に気付けない。
  **射程外**: 見るのは `internal/` だけ。**`user_ip` に実際に書く `UserIPRepository.Observe` (3 引数) は引数の数で絞る走査に入らない**。
- **2026-09-17**: `make gates` に `nulparam-check` を追加 (#3025)。`make help` の target は 133 → 134。**認証済みの一般利用者が、パラメータに NUL を 1 文字入れるだけで 500 を起こせた** (`federation/*` や `users/clips` など**未認証**で叩けるものもあった)。NUL はどの列にも入らないうえ、**比較の右辺に置くだけで PostgreSQL がクエリごと落とす** (手元の simple protocol で SQLSTATE 08P01、本番の pgx extended protocol で 22021)。`IsNotFound` でもないので handler は `JSONInternalError` へ倒す。NUL は JSON のエスケープで普通に送れる。#3018 / #3022 は主に「列に**書く**値」を塞いだ (申請 ID のように引く側も一部含む) が、**カーソル / id / 検索語は系統的に残っていた**。
  **upstream は「全部 500」ではない。** ajv に `misskey:id` (`/^[a-zA-Z0-9]+$/`) を登録しているので、その format を持つ `sinceId` / `untilId` / `userId` / `noteId` は**列に届く前に 400 で弾かれる**。format を持たない値 (検索語や `users/show` の `username` など) だけが 500 になる。**この裏取りをせずに「upstream は 500」と書き、敵対的レビューで指摘された。**
  **役割ごとに答えが違う。** カーソルは 400 (空に倒すと「カーソル無し = 先頭から」になり、**利用者の指定と無関係なページを正しい応答として返す**)、単体 id と完全一致で引く値は not-found (一致しえない値は「無い」が事実)、検索語は**空の結果**(「その語を含む行が無い」が事実で、利用者の入力が壊れているわけではないので wire に新しいエラーコードを足さない)。**issue の完了条件は「4xx になる」だったが、検索語だけは 200 + 空にしてある** — 理由は docs/divergence.md に書いた。**単体 id は upstream と code / id が違い、status も一致しない経路がある** (`users/show` は mk-go 404 / upstream 400) ので、これも divergence として記録した。
  **1 箇所で塞げるものとそうでないものがある。** カーソルは `id.NormalizeCursor` を 39 ファイルが通るので choke point になる (数え方: 非テストの `internal/` で `id.NormalizeCursor(` を含むファイル)。**signature を 3 値にしてコンパイルで全件の書き換えを強制した** (呼び出しは実測 88)。id と完全一致の値は共通の入口が無いので、repository の**単一行 lookup** (`Find*` / `Get*` が `(*model.X, error)` を返すもの) と `*ByID*` の 118 メソッドに guard を置いた。検索語は `escapeSQLLikePattern` が NUL を見ていないので、LIKE を組み立てる 16 関数に置いた。
  **#2792 に反しない。** 丸めているのは DB 障害ではなく、**引く前に分かっている「一致しえない」**という事実で、クエリを投げていない以上そこに隠れる障害が無い。逆に言うと、この判定を「引いた後」へ動かすと #2792 違反になるので、gate は**順序も見る**。
  **関数の契約を上書きしない。** 「無い」を `(nil, nil)` で表す lookup (`FindActive` / `FindPendingInvitation`) に `ErrNotFound` を返す guard を入れたところ、呼び出し側が err として扱って **`roles/assignment-show` が 500 のまま残った** (敵対的レビュー 2 周目で実測)。guard はその関数が既に持っている「無い」の表現に合わせること。
  **共有の入力構造体を書き換えない。** `buildV2Query` で `fq.RoleIDs` をフィルタ済みの値に書き戻したら、handler が同じ filter で `ListV2` の直後に `CountV2` を呼ぶため、**2 回目は guard もフィルタも素通りして絞り前の件数を返した** (同 2 周目で実測)。`filter.Query` がポインタなのが効いている。局所変数に受けること。
  **mock では検出できない。** `internal/testutil` の mock repository は NUL を渡しても普通に「見つからない」を返すので、**guard を外しても handler テストは緑のまま通る**。だから (a) 静的な gate、(b) 実 PostgreSQL に対する「引く前に弾いている」テストの 2 本で押さえる。後者は `IsNotFound(err)` が真であることに加えて**普通の入力が引けたまま**であることまで見る (空を返すだけの実装でも「NUL で空になる」テストは通る)。
  **gate は「どの値を見ているか」まで照合する。** 呼び出しの有無だけを見る初版は、敵対的レビューで**バグを再導入する 3 変異が全て素通り**することを実測された — (a) 複数の値を取る lookup で片方だけ guard する、(b) `storableIDs(ids)` と書いて**戻り値を捨てる** (式文として合法で `go vet` も黙る)、(c) LIKE に載る値そのものの guard を落として別の値だけ見る。**実際にその形で本番コードが漏れていた** (`ListUsers` が `Username` は見て `Hostname` を見ていなかった)。パラメータを 1 つ残らず guard の引数と突き合わせ、`storableIDs` は代入し直していること、**guard の分岐が実際に return すること**、**順序はパラメータごとに見ること** (関数単位で「最初の guard」と比べると 2 つ目以降が SELECT の後ろでも通る) まで見る形に直した。
  **カーソル側の gate も 3 つ穴が開いていた。** 「関数のどこかに `!ok` があればよい」形にしていたため、(a) 手前の `pagination.ResolveLimit` の `limitOK` に受け直すだけで黙る (**全 handler がその形の `!limitOK` を持っている**)、(b) guard を DB 呼び出しの後ろへ動かしても緑、(c) `if !ok { sinceID, untilID = "", "" }` と**空に倒して 200 を返す**形も緑。**「カーソルの値を使う前に `!ok` を見て抜ける」**に限定して塞いだ。**「直後の 1 文」に狭めると今度は正当な書き方を落とす** — guard を 2 つ積む / 条件を束ねる / `switch` の case に置く / ループで `continue` する、のどれもが偽陽性になり、しかも診断が「guard が無い」と事実と逆を指す。
  **「呼び出し側を見る」だけでは片側しか塞がらない。** `id.NormalizeCursor` の呼び出しを見る gate は、**そもそも呼んでいない handler** を視界に入れられない。実際 `list-mine` (一般ユーザーが叩ける) と `admin/emoji-application/*` の 3 つが `untilId` をそのまま `"id" < ?` に載せていた。`untilId` / `sinceId` を bind する handler 側を数える gate (実測 44) を別に足してある。
  **「引く前に弾く」の判定に「組み込み以外の呼び出し」を使わない。** `time.Now()` や `r.normalizeHost(host)` を guard の前に 1 行置いただけで落ちるのでは述語と意図が食い違う。DB とみなすのは `tx.` / `db.` / `q.` と、レシーバの**フィールド**越しの呼び出し (`r.db.` / `c.inner.`) だけにする。
  **AND と OR で落とし方が違う。** AND で畳む検索語は 1 つでも一致しえなければ全体が空 (`allStorable`)、**OR で畳む語は要素ごとに落とす** (`storableIDs`)。`multipleWordsToQuery` は後者だが、**`roleIds` を前者で書いて実測で壊した** — `&&` は overlap = OR なので、一緒に指定した他の role の一致まで消えていた (書き込み側の `normalizeEmojiRoleIDs` は #3018 で既に要素ごとに落としている)。
  **gate の対象集合は「LIKE を作る側」だけにする。** `multipleWordsToQuery` を escape helper の集合に入れると、その呼び出し側 (`buildV2Query`) に判定を要求してしまう — あの関数は自分の中で語ごとに落としているので偽陽性になる。集合から外しても、あの関数自身が `escapeLike` を呼ぶので中の判定を外せば捕まる。
  **射程外**: gate が見るのはカーソル (呼び出し側 88 + bind 側 44) / 単一行 lookup + `*ByID*` 118 / LIKE 16 で、値を受ける一覧系は見ていない。**guard を呼ばずに DB を触るメソッドが 311 残っている** (数え方は docs/divergence.md)。**実際に届く経路は測って個別に塞いだ** — 未認証で叩けるものだけでも `federation/followers` / `following` / `users` の `host`、`hashtags/users` / `show` の `tag`、`notes/reactions` の `type`、`users/clips` / `flashs` / `gallery/posts` / `pages` の `userId`。網羅ではないので、一覧系を足すときは受け取る値を自分で弾くこと。
  **変異検証は 25 形** (production 21 + gate の述語 4)。production: `colfit.Storable` を常に真 / `mute.List` の `!cursorOK` を `_` / `ListMine` の `!cursorOK` を `_` / `userRepository.FindByID` の guard を外す / `storableIDs` を素通し / `instanceRepository.List` の guard を外す / `multipleWordsToQuery` の語ごと判定を外す / `hashtags/search` の guard を外す / `hashtags/show` と `users` の guard を外す / `FindByIDAndUserID` の `id` だけ外す / `storableIDs(ids)` の戻り値を捨てる / `SearchMessages` の `query` だけ外す / `ListUsers` の `Hostname` を外す / `following` の `host` guard を外す / `buildV2Query` の `roleIds` フィルタを無効化 / `registry.Get` の `key` の guard だけ SELECT の後ろへ / `SearchUsers` の `query` の guard だけ escape の後ろへ / `buildV2Query` が `fq.RoleIDs` へ書き戻す / `FindActive` の guard を `ErrNotFound` に戻す / `note_reaction.ListByNoteID` の guard を外す / `clip.ListPublicByUser` の guard を外す。gate: カーソルの抽出 (`isCursorCall`) / lookup の対象判定 (`isGuardedLookup`) / LIKE の escape 集合 / `paramKindOf` の `*string` 枝。**下限の件数を持たせているのが要点** — 違反 0 件が正常な状態なので、抽出を壊しても「検出 0 件」と区別が付かない。**`hashtags/show` の handler テストは置いていない** — あちらは `err != nil` を丸ごと 400 に潰す既存実装なので、guard の有無で外から見える応答が変わらず空虚になる (実測)。repository 側のテストで押さえてある。
- **2026-09-12**: `make gates` に `submodulepin-check` を追加 (#2969)。`make help` の target は 132 → 133。**fork frontend の pin が doc と gitlink で食い違ったまま緑になっていた。** #2963 で `third_party/misskey` に commit して fork へ push し、`docs/divergence.md` にも新しい tag を書いたのに、**親リポの gitlink だけ古いまま CI 28 チェックが全て緑でマージされた** (#2965 で解消)。気付いたのはマージ後に `git status` を見たときで、検出が人手に依存していた。
  **実害の経路もある。** `Makefile` の `REVISION_LDFLAGS` は `git -C third_party/misskey describe --tags` で `MkGoFrontendVersion` を作るが、これは **submodule の working tree** を見るので、gitlink が遅れている窓に develop からビルドしたバイナリは古い tag を名乗りつつ doc は新しい tag を書いている状態になる。
  **SHA で突き合わせるのが要点。** doc に書いてあるのは tag 名だが、tag から SHA を解くには**ネットワーク** (`git ls-remote`) か submodule の checkout が要り、`make gates` はどちらも前提にできない。**pin 行に短縮 SHA を併記して親リポだけで完結**させると、`git ls-files -s -- third_party/misskey` が submodule 未初期化の worktree でも gitlink を返すので `make gates` に載る (実測: submodule が空の worktree で PASS し、そこで doc の SHA を変えると落ちることまで確認した)。
  **gitlink は index から読む (`ls-files -s`)。** doc は working tree から読むので、gitlink を `ls-tree HEAD` (= 直前の commit) から読むと**読み元が非対称**になり、submodule を `git add` して doc も直した**コミット直前の状態で必ず落ちる** — しかも診断が「`git add` しろ」= もう済ませた操作を指示する。`make check` はコミット前に回す決まりなので bump のたびに踏む (実測: 直近 30 commit のうち 13 が gitlink を動かしている)。CI は checkout 直後で index == HEAD なので検査は弱まらない。**敵対的レビューで指摘されるまで `ls-tree HEAD` だった。**
  **tag 名の正しさは CI の `build` job で見る。** #2963 の事故は「**tag だけ直して gitlink を忘れた**」形だったので、SHA 側を書き換え忘れると `make gates` は素通りする。`Check submodule commit is pushed` step に `git ls-remote` で tag → commit を解いて gitlink と突き合わせる判定を足した (実測 1.0 秒。annotated / lightweight 両対応)。あわせて doc 内の整合 (pin 行の tag == §4-2 の表の最終行) を `make gates` 側でも見るので、ネットワーク無しでも気付ける経路が 1 本ある。**「別の場所 (`frontend-check` 側) の仕事」と書いたが、そんな実装はどこにも無かった** — 裏取り無しの主張だったのでこの形に直した。
  **pin 行はちょうど 1 件であることも要求する。** `FindSubmatch` で最初の一致だけを採ると、前方に書式の例を書いた瞬間に本物の pin 行が検査対象から外れる (このゲートの失敗メッセージ自身が書式を提示するので、doc へ写す動機がある)。
  **実害の向きに注意。** 「バイナリが古い tag を名乗る」は症状であって実害ではない — develop を clone して submodule を取れば working tree は gitlink (古い方) に置かれるので、`describe --tags` が返す古い tag は**事実として正しい**。嘘をついているのは doc の側で、本当の実害は「**入れたつもりの frontend の修正が develop のビルドに入っていない**」こと。
  **変異検証は 8 形**: doc の SHA を 1 文字変える / pin 行を消す / SHA の併記だけ消す / 正規表現を空振りさせる / pin 行を 2 件にする / pin 行の tag を古くする / CI 側で tag だけ古くする / CI 側で存在しない tag を書く。**「アサーションを外す」「`ls-files` の結果を無視する」はテスト自身の変異なので「検出した」とは言えない** — それが示すのは「そのアサーションが load-bearing である (空虚でない)」ことだけ。
- **2026-09-12**: `make gates` に `secretfield-check` を追加。`make help` の target は 131 → 132。**モデルをそのまま JSON 化する経路があるので、`json:"-"` が唯一の防波堤になっているフィールドがある。** 実測で `internal/model` の該当タグを外しても `make gates` も全テストも緑のままだった。**名前だけでは判定できない** — `Meta` の captcha secret は `admin/meta` が管理画面へ返すうえ moderation log にも載るし、drive の `accessKey` は URL の構成要素で秘密ではない。そこで #2792 と同じ **allowlist に書かせる**方式にした。
  **allowlist には「出してよい理由」ではなく「どの経路で実際に出るか」を書く。** 前者だと、タグが使われていないという誤った前提のまま動かしてしまう — 実際にそれで壊した (下記)。
  **モデルを直接 JSON 化する経路は 6 系統ある** (数え方: `internal/model` の型が `encoding/json` の Marshal/Unmarshal/Encode/Decode か `echo.Context.JSON` に静的型で到達する非テスト箇所)。(a) `internal/core/moderationlog/service.go` が `json.Marshal(info)` でモデルごと記録し (`*model.Meta` の before/after、`[]*model.RegistrationTicket`、`*model.Role`、`*model.Ad`、`*model.SystemWebhook` など)、`admin/show-moderation-logs` が `info` をそのまま返す。(b) `internal/core/ephemeral/store.go` が `model.Note` / `model.User` を Redis へ。(c) `internal/core/webpush/cache.go` が `[]*model.SwSubscription` を Redis へ入れて読み戻す。(d) **admin API のレスポンス本体** — `internal/api/admin/relays.go` が `*model.Relay` / `[]*model.Relay` を `c.JSON` にそのまま渡す。(e) **同** — `abuse_report_notification.go` の `packedRecipient` が `*model.AbuseReportNotificationRecipient` を埋め込んで返し、`admin/show-user` は `[]*model.Role` を入れた map を返す。(f) `drive/chunked_upload.go` が `[]model.ChunkedUploadPart` を jsonb 列へ往復し、`instance_service.go` が `[]model.SuspendedSoftwareEntry` を読み戻す。**「API は map を手で組むからタグは使われていない」も「API のレスポンスは必ず map か entity を通る」も誤り** — (d)(e) はモデルの json タグがそのままレスポンスの shape になる。**この数え落としは 3 度繰り返した** (2 箇所 → 3 系統 → 6 系統)。数えるなら grep ではなく型情報で追うこと。
  **敵対的レビュー 1 周目で 3 つの穴が実測された。** (a) 正規表現が `Pass` を見ておらず **`Meta.SmtpPass`** が検出集合にすら入っていなかった。(b) `Code` も見ておらず、**`SignupApplication.ClaimCodeHash`** — モデル側が「平文では持たない。DB が漏れた時点で全申請が乗っ取れる」と書いて `json:"-"` で守っているフィールド — のタグを外しても緑だった (**ゲートが存在する理由そのものの失敗形**)。(c) `fld.Tag == nil` を skip していたため、**タグを書き忘れた新規フィールドが原理的に見えなかった** (`encoding/json` はタグの無い exported フィールドを Go の名前でそのまま出す)。`Pass` / `Code` / `Auth` / `Hash` の追加による偽陽性は `NoteDraft.Hashtag` の 1 件だけで、allowlist 1 行で済む。
  **2 周目で、その修正が実挙動を壊していることが実測された。** 1 周目の指摘を受けて新検出の 4 件を `json:"-"` にしたが、うち **`Meta.SmtpPass` と `RegistrationTicket.Code` は moderation log の記録を欠けさせた** — 招待コードの監査記録が空になり、`update-meta` の記録も他の secret が全部残る中で smtpPass だけ消えるという不揃いになった。しかも `internal/api/admin/handler.go` には「upstream も mask しない。互換性最優先で同じ挙動」という**意図的な parity 判断のコメントが既にあった**。「挙動を書き換えるなら、それを固定しているテストが無いか先に読む」「コードコメントの既知乖離を見る」に反した形。**`SwSubscription.Auth` も危うく同じ経路で壊すところだった** ((c) の Redis キャッシュ)。タグは allowlist へ戻し、**出ることを固定するテスト** (`TestModelJSONKeepsAuditedFields`) を足した。さらに 3 周目の指摘で、**allowlist に載っているものが `json:"-"` になっていないかを 29 件一律に検査する**形に変えてある — 手で 3 件並べるだけでは、同じ壊れ方が残り 12 件で開いたままだった (実測で 4 形が素通り)。「allowlist に載せた = 出ることが前提」なので `json:"-"` は宣言との矛盾として落ちる。
  **allowlist の dead-entry 検査は万能ではない。** 「実在しないキーが残ると落ちる」形は、**allowlist に該当があるキーしか守らない**。`Pass` / `Code` は該当が全て `json:"-"` 側にあったため、**1 周目の指摘を塞いだ修正そのものが正規表現から外しても緑で巻き戻せる状態**だった (2 周目で実測)。検出集合そのものを `mustDetectSecretFields` で固定して塞いだ。
  **gate は `internal/model` には置けない。** あそこは `_test.go` を 1 つも持たないので、テストを足すと CI のカバレッジ閾値 (90%) の対象に**初めて**入り、wire 層と同じ理由で 0% に張り付いて落ちる (#462 と同型。実測で `coverage: 0.0%`)。`internal/entitycompat` に置き、対象のソースはファイルとして読む。
  **検出ロジックは人工のソースで固定する。** 実モデルは allowlist で全て許可済みなので leak は 0 件で、実モデルだけを見ていると検出の枝が一度も実行されない。静的なタグ検査に加えて代表的な型を実際に `json.Marshal` もする (`MarshalJSON` を自前で実装した型では静的検査が抜けるため)。逆に新しいフィールドを allowlist 無しで足す形は静的検査でしか捕まらない。
  **「N 形中 M 形」と書くなら変異集合そのものを書く。** 1 稿目は「20 形中 19 形」と書いて非検出形を 1 つ挙げたが、そこに上記 (a)(b)(c) は入っていなかった。2 稿目は非検出形を集合から外して「22 形すべて」にし、数字は上がったが情報は減った。**現在の集合は 27 形**: モデル側の `json:"-"` 剥がし 11 (= `json:"-"` を持つ全件) + タグ削除 3 + ゲート側 6 (tag==nil skip の復活 / 正規表現の空振り / allowlist の死んだキー / allowlist から 1 件削除 / 理由を空に / leak 収集を潰す) + 正規表現の alternative 剥がし 4 (`Pass` / `Code` / `Auth` / `Hash`) + 監査側の固定 3 (`Meta.SmtpPass` / `RegistrationTicket.Code` / `SwSubscription.Auth` を `json:"-"` にする)。検出対象は実測 40 件 (`json:"-"` 11 + allowlist 29)。**既知の非検出形**は「実モデルの leak 検査そのものを消す」形 (leak が 0 件である以上、実モデルからは捕まらない。人工ソースのテストが検出ロジック側を押さえる)。
  **既知の取りこぼし**: `Pass` / `Code` / `Auth` / `Hash` は部分一致なので `PassedAt` / `StatusCode` のような名前も拾う (fail-closed なので危険側には倒れない)。`datatypes.JSON` 列の中身、`internal/model` 直下以外 (glob が非再帰)、名前付き型の中の匿名 struct、`internal/queue` など他パッケージの構造体は見ない。`Key` 全体には広げていない — `PublicKey` / `*SiteKey` (captcha のサイトキーはフロントへ配る公開値) / `SortKeys` / `ExcludeKeywords` が誤検知で allowlist を埋め、本物が紛れるため。
- **2026-09-11**: `make gates` に `dockerignore-check` を追加し、`.dockerignore` の漏れを塞いだ (#2942)。`make help` の target は 130 → 131。**`drive-files` と operator-local な設定 (`.config/*.y*ml` / `deploy/uds/config/*.y*ml` / `compose.uds.yaml` / `.claude`) が除外されていなかった。**
  **「配る image に入る」ではない。** mk-go をビルドする Dockerfile はどれも最終 stage が**明示パスの `COPY --from=builder` しか持たない**ので、context に入ったファイルが配布物へ出ることはない (実測: 除外を外して build しても最終 image の `drive-files` は 0 件、builder stage には 2,790 件。修正前の `.dockerignore` でビルドされた本番 image の `/app/drive-files` も空)。**最初これを「image へ焼き込まれる」と裏取りせずに書き、7 箇所に伝播させた。** 守っているのは **build context と builder stage の layer** で、漏れる経路は (a) `cache-to` でキャッシュへ書き出したとき、(b) 手元の builder cache に滞留したとき、の 2 つ。加えて転送量にも効く。
  **`.dockerignore` のパターンはパス全体で照合される。** スラッシュを含まない `node_modules` は `node_modules` にしかマッチせず、**`third_party/misskey/node_modules` (実測 1,133 MB) は残る**。同じことが `.git` にも起きており、`plugins/*/.git` が 4 つ入っていた (#2940 の `pluginresolve` は clone した分だけ自分で消しており、コメントに「`.dockerignore` は `plugins/*/.git` を落とさない」と書いてあった = 既知のまま放置されていた)。入れ子にも効かせるものは `**/` を前置する。**ただし `built` は前置しない** — `third_party/misskey/built` は SPA の成果物で image に要る。
  **`**/node_modules` にもできない。** それだと `packages/backend/node_modules/` 配下の symlink まで落ち、Dockerfile が COPY する `emoji-assets/built/...` が解決できなくなる。`third_party/misskey/node_modules` を名指しで除外し、必要な emoji-assets (44 MB) だけを `!` で再包含する。**再包含は実体側に書く** — pnpm は実体を `.pnpm/` 配下に置き、`packages/backend/node_modules/...` はそこへの symlink なので、symlink 側を再包含しても効かない (実体側の再包含を外すと COPY が `not found` で落ちることを実測)。
  **実測は 1,512 MB → 421 MB。** 手元ではさらに `.pnpm-store` (3.8 GB) が消える。**`docker build --check` では分からない** — `--check` は context を 849B しか送らないので、転送量も COPY の成否も実ビルドでしか測れない。
  **gate はサイズを見ない。** コンテキストが太っても転送が遅くなるだけで、ビルドは通るし気付ける。見るのは「中身が読まれると困るもの」だけ。
  **`.dockerignore` を自前で解釈しない。** `moby/patternmatcher` + `ignorefile` (Docker 本体が使う実装) に「そのパスが除外されるか」を直接判定させる。**最初は文字列の正規化で近似して書き、敵対的レビューで 8 形中 7 形を見逃すことを実測された** (`!*/**` / `!drive-files*` / `!/drive-files` / `!.config/**` / `!.config/*` / `!deploy/uds/config/*` / `!compose.uds.yaml*` がどれも gate PASS のまま再包含される)。`**/` の前置・末尾スラッシュ・先頭スラッシュ・`*` を挟む形・`!` の後勝ちが絡むので、近似は必ず取りこぼす。#2857 が「Makefile を自前でパースせず `make -n` に解決させる」と結論したのと同じ形。**`ignorefile.ReadAll` と組にするのが要点** — 先頭スラッシュの除去はそちらの仕事で、`patternmatcher` 単体だと `!/drive-files` を取り逃がす。両者は既に `go.sum` にある (testcontainers 経由) ので、`go get` するだけで **`go.mod` を変えずに** import できる。**`// indirect` のコメントは残る** — 落とすのは `go mod tidy` の仕事で、このリポジトリでは tidy が使えないため。充足は `GOWORK=off go build ./...` が通ることで確かめる。
  **判定は字句だけで symlink を辿らない。** だから「Dockerfile が COPY する symlink 経路」と「pnpm が実体を置く `.pnpm/` 配下」の**両方**を一覧に入れる必要がある。実体側だけを守っていたときは、`third_party/misskey/node_modules` を `**/node_modules` に広げる変更が **gate 緑のまま全ビルドを壊した** (`!` の再包含は実体側だけを生かすので symlink が落ちる)。しかもそれは `.dockerignore` 自身が名指しで警告している形で、`Dockerfile` の guard は「pnpm install not run?」と**事実と逆**を出す。
  **本物の matcher に解かせると、肯定側のアサーションが書けるようになる。** 「`.config/docker.yml.example` は context に**残る**」「emoji-assets の twemoji は**残る**」を検査対象にできるので、除外を広げすぎて COPY を壊す変更 (`.config/*.yml` → `.config/*`、`built` → `**/built`) がその場で落ちる。**除外側の文字列一致しか見ない形では原理的に書けない検査**で、変異検証でも肯定側 3 件が検出できている (合計 17/17)。
  **`<Dockerfile名>.dockerignore` の存在も見る。** BuildKit はそれがあると root の `.dockerignore` を**一切見ない**ので、ファイル 1 つで全ての除外が静かに無効になる。
- **2026-09-10**: `make gates` に `pluginembed-check` を追加し、運営者向けの reusable workflow (`build-with-plugins.yml`) を新設 (#2940)。`make help` の target は 129 → 130。**`Dockerfile.bundled` が `pluginbuild` を呼んでいなかった** — `plugins/` に置いてビルドしても入らない image が黙って出来ており、しかもエラーにならないので運営者は「入ったつもり」で起動できた。`docs/plugins/operating.md` は「`Dockerfile` / `deploy/uds/Dockerfile.mkgo` の両方が生成ツールを実行する」と書いて bundled を挙げていなかったが、**除外とも書いていなかった**。
  **gate は 3 つの素通りを塞いである** (どれも敵対的レビューで実測された)。(a) **順序を見る** — `pluginbuild` を `go build` の後に置くと生成物が binary に入らないが、Dockerfile としては正当でビルドも成功する。(b) **builder の検出を 1 つの文字列に頼らない** — `./cmd/misskey` だけを探す形は module path (`github.com/shiroha-a/mk/cmd/misskey`) やワイルドカード (`./cmd/...`) で書かれた Dockerfile を検査対象から黙って落とす。**allowlist にも載らないので gate は鳴らない**まま検査が減る (「1 つも拾えなかったら落とす」は全部消えたときしか効かない)。(c) **RUN 内の行末 `#` も落とす** — シェルのコメントなので「書いてあるのに実行されない」状態になる (#2856 が `wiring-check` で `/* */` に対して踏んだのと同型)。(d) **行継続を畳んでから判定する** — `go build` と対象が同じ行にあることを要求すると、ldflags を 1 つ足して折り返した瞬間にその Dockerfile が検査対象から消える。(e) **動詞も 1 つに頼らない** (`go install` で外れる)。
  **落としすぎる strip を builder の判定に使わない。** 行末 `#` の除去は「落としすぎる」側に倒してあるので、`go build` の行にたまたま ` #` があるとその Dockerfile ごと builder 集合から消える。**検出は広い body で、実行されるかの判定は狭い body で**、と分けてある (敵対的レビュー 2 周目で、(c) の対処が (b) の穴を新しく開けていることが実測された)。
  **検出と順序判定でも広さを変える。** 検出は「ファイルのどこかに動詞と対象がある」で広く取る — 動詞と対象が同じコマンドに現れることを要求すると、対象を `ARG MK_MAIN=./cmd/misskey` のような変数に入れただけで検査対象から消える。逆に順序判定は「動詞と対象を**同時に含むコマンド**」だけを基準にする — 畳んだ RUN の中に無関係な `go build` / `go install` があると、そちらが基準点を前へ引っ張って**正しい Dockerfile が順序違反で落ちる** (しかも診断が事実と逆を指す)。**3 周目のレビューでこの 2 つが同時に指摘された** — (b)(d) の対処がそれぞれ別方向の穴を開けていた形。**残る既知の穴は「pluginbuild を使われない別 stage に置く」形** (存在判定はファイル全体を見るため素通りする)。テストの doc コメントに明記してある。
  **突き合わせに使う出力は、無検証の値より前に置く。** `pluginbuild` の行は `dir=` を `name=` より前に出す — `name` は `mk-plugin.yml` の無検証な YAML 文字列で括弧も改行も入れられるので、後ろに置くと `name: "x dir=plugins/victim "` のように**別プラグインの行を偽装でき、無効化されたプラグインが組み込まれたと判定される** (実測)。書式は `tools/pluginbuild` 側のテストで固定した — 呼び出し側が突き合わせに使う契約なので、片側だけ変えて気付かないのを防ぐ。
  **運用の動機は実測。** 定常運用は **343 MiB** (mk-go 91 / PostgreSQL 181 / valkey 69 / nginx 2) で 2GB VPS に載るのに、**Go のビルドは 1200MB 制限・既定の並列度で OOM する** (`-p 1` なら通る)。`/usr/bin/time -v` が出す 976MB は**単一プロセスの最大値**で、並列コンパイラの合計ではないので、コア数が多いホストほど OOM しやすい。さらに **BuildKit は dockerd に組み込まれている**ため、本番ホストでビルドするとヒープが膨らんだまま返らない — **ビルドキャッシュを 60.88GB 削除しても RSS は 4,465 → 4,485MB で不変**だった (削除処理自体で一時的に 6,798MB まで増え、2 分で戻った)。`builder.gc.defaultKeepStorage` はディスクにしか効かない。解放には dockerd の再起動が要る。
  **`ARG` は最初の `FROM` より前に置く。** `FROM assets-${ASSETS_SOURCE}` のような stage 名の展開に使えるのは global ARG だけで、stage 内で宣言したものは参加しない。しかも**`--build-arg` を渡しても救われない** (未宣言の build-arg は metaArgs に入らない) ので、置き場所を間違えると `assets-` という不正な stage 名になり、**プラグイン経路だけでなく既定のビルドまで落ちる**。
  **step の `if:` から `secrets` は参照できない** (使えるのは env / github / inputs / job / matrix / needs / runner / steps / strategy / vars)。書くと式の検証が `Unrecognized named-value` になり、**呼び出し元の job が 1 つも走らずに失敗する** — token を渡さない呼び出しでも同じ。判定は `run` の中で行う (step の `env` は `if` からは見えないが `run` からは見える)。
  **reusable workflow 側で `permissions` を宣言しない。** caller の権限以下にしか設定できないので、宣言した時点で caller がそれを持っていなければ run ごと拒否される (`push: false` でも回避できない)。publish する caller が自分で `packages: write` を書く。
  **要求したプラグインが実際に入ったかを突き合わせる。突き合わせるのはディレクトリ名。** `pluginbuild` が最初に出す名前は `mk-plugin.yml` の `name:` で、**置いたディレクトリ名とは限らない**。運営者が指定できるのはディレクトリ名だけなので、名前で照合すると**正しく組み込まれたプラグインで落ちる** (しかも診断が disabled を疑わせる方向になり事実と逆を指す)。`pluginbuild` の出力に `dir=` を足して、そちらで照合する。あわせて **`-dry-run` の stdout には spec 以外を書かない** — 「指定されたプラグインはありません」を stdout に出していたため、プラグインを全部コメントアウトすると案内文の 1 行目がプラグイン名として読まれて落ちた (どちらも敵対的レビュー 2 周目で、1 周目の修正が作った回帰として検出された)。`pluginbuild` は `mk-plugin.yml` が `disabled: true` のものを黙って skip して exit 0 で終わる。しかも `disabled: true` は #2701 でこのプロジェクト自身が同梱プラグインに要求している書き方なので、作者がそれに倣っていれば必ず踏む。見ないと**指定した N 個のうち 0 個しか入っていない image が緑で push される** = この issue が塞ごうとしている穴そのものになる。
  **新しい workflow は PR 上で発火させて確認する** (`docs/ci.md`)。`workflow_dispatch` だけだと default branch にあるものしか起動できず、マージ前に一度も検証できない。**この PR の High 3 件はそこを踏み外したまま書いたことで生まれた。**
- **2026-09-10**: `make gates` に `mdtable-check` を追加 (#2930)。`make help` の target は 128 → 129。**GFM は列が増えた行を「崩して描画」しない。溢れたセルを黙って捨てる。** ヘッダ行が列数を決め、それを超えたセルは破棄されるので、**ソースには書いてあるのに GitHub 上では読めない**という形で壊れる。ローカルで md を読んでいる限り気付けない。実際に踏んだのは `docs/divergence.md` の `2026.9.0-mk.7` の行で、コードスパンの中に書いた権限式 `$i.isModerator || $i.policies.canManageCustomEmojis` の `||` がセル区切りとして働き、**描画は 599 文字あるべきところ 394 文字で止まって 205 文字 (34.2%) が読めなかった** (数え方は `gh api /markdown --mode gfm` の出力からタグを除いた文字数)。消えた中に「純正へは還元できない行」という分類が入っており、この表を「還元不能な差分の一覧」として読む運用が成立していなかった。
  **コードスパンの中でもパイプは区切りとして働く。** GFM のエスケープ (`\|` → `|`) は inline の解析より**前**に効くので、表セルの中では `` `a \|\| b` `` と書けば区切りにならずコード中の `||` になる。リテラルの `\|` を見せたいときは `` `a \\| b` ``。**表の外にはこの前処理が無い**ので、コードスパンに `\|` と書くとバックスラッシュがそのまま出る (この entry の 1 稿目で実際に間違えた)。
  **見るのは列数だけにしてある。** 敵対的レビューで「列数が一致したままコードスパンが割れる形がある」(3 列の表の `` | `x|y` | z | `` は**セル数がヘッダと同じ 3 になる**) と指摘され、コードスパンの対応付けを自前で持つ実装と、外側パイプ省略に対応するため表の終端をブロック開始で判定する実装を足した。**どちらも次の周で正当な md を落とした** — 前者は**二重バッククォートのコードスパンを含む行**を、後者は**表の直後にリストを置くというごく普通の書き方**を偽陽性にした (どちらも GitHub では正常に描画されることを実測)。自前の inline / block パーサに継ぎ足す形は #2857 が「手当てするたびに隣の穴が開く」と結論した型なので、**列数という 1 つの条件だけ**に戻してある。取りこぼす側 (列数が一致したまま割れる形、外側パイプを省いた表、ヘッダ行自体が壊れた表) はテストの doc コメントに明記した。
  **表は「先頭パイプの行が続く間」とする** — GFM はパイプを含まない行も表の行にするが、そこまで追うには全ブロックの開始判定が要る。このリポジトリの表 227 個はすべて先頭パイプ付きなので取りこぼしは無い。**フェンスの検出は行頭 3 スペースまで** (`^\s*` にすると、フェンスの書き方をインデントブロックで見せているdoc で開いたまま閉じず、そのファイルの残りが未検査になる。現 corpus に該当は無いが仕様どおりにしてある)。**`git ls-files` で見る** (#2857 と同じ理由。submodule の中は出ないので fork frontend の md は対象外)。**1 つも拾えなかったら落とす。** 実測は tracked な md 57 ファイル / 表 227 個で、**現 corpus に対し偽陽性 0・検出 1 件** (= 上記の実バグ)。**あわせて `make gates` の一覧に `notiftype-check` が漏れていたのを Section 3 と `docs/development.md` の両方で直した** (2026-09-08 に追加したときの片側更新)。
- **2026-09-08**: `make gates` に `notiftype-check` を追加 (#2898)。`make help` の target は 126 → 127。通知タイプの一覧が **core の `Type` 定数と `internal/api/notifications` のリテラルの 2 箇所**にあり、片側更新が実際に起きていた (`importCompleted` が core にだけあった。発火箇所が無いので実害は出ていなかったが、固有型を足せば必ず踏む)。API 側を `internal/core/notification` の registry から導出する形にしたうえで、(a) registry と `Type` 定数が 1:1 か、(b) API 側がリテラルに書き戻されていないか、を検査する。**値の一致だけでは足りない** — リテラルに書き戻しても書いた時点の中身は同じなので値比較は通り、落ちるのは core に型を足した後 = 一番検出したい瞬間に検出できない。導出している「形」を AST で固定してある (中身が同一のリテラルへの書き戻しで落ちることを実測)。**固有型を全指定判定に含めるのが要点** — 含めないと upstream の 20 種を全て `excludeTypes` に並べただけで「全部除外」と判定され、除外指定していない固有型の通知まで返らなくなる。
- **2026-09-06**: `make gates` に `migrationdoc-check` を追加 (#2874)。`make help` の target は 119 → 120。**gate が見るのは 8 ファイル 22 箇所** (数え方: claim 20 + no-op down の一覧 1 + 破壊的マイグレーションの表 1)。1 本足したとき実際に動くのはその一部で、#2866 (000082 の追加) では 17 箇所 (total 4 + destructive 11 + 一覧 1 + 表 1。テーブルを作らず data loss 宣言も持たないので tables / dataloss は動かない) — #2866 の敵対的レビューで 5 箇所の漏れが見つかっている (`docs/api-compatibility.md` はリンク先と違う数を出したまま、`internal/testutil` の 2 箇所は分母が migration ファイル数。**PR は単一コミットに squash されているので、漏れていた中間状態は履歴に残っていない**)。**一覧の突き合わせが本体** — 件数だけだと「1 本足して 1 本消す」で素通りする (実測で確認)。**破壊的なマイグレーションの件数は doc 自身の表の行数を truth にする** — migration の中身から「共有テーブルに触るか」を機械的に判定しようとすると、upstream に無いテーブル (`signup_application`) を触るものまで拾って人手で外すことになる。表は 1 行 1 migration なので判断が要らない。**最初これを「機械化できない」と誤って結論し、#2866 で実際に壊れた 5 箇所のうち 3 箇所を検査対象から外していた** (敵対的レビューで指摘)。対象外にしたのは 3 つだけ — 「宣言が無いまま DROP する down が 51 本」(`architecture.md` と `migration-from-ts.md` で**定義が違うのに同じ 51** を出しており、どちらを truth にするか決められない。実測ではどちらの定義でも 51)、「102」(「上記 9 件」の定義に依存)、「データを不可逆に変えるのはこのうち 8 本」(機械判定できない)。**拾えなかったら落とす** — 書式を変えて正規表現が空振りすると、検査していないのに緑になる。#2644 が「doc の静的検査は測ったら使い物にならなかった」と結論しているが、あれは**存在しない Makefile target / パスの検出**で不在候補 298 件の大半が偽陽性だった話。件数は数え方が一意に定義でき、実測で偽陽性 0 / claim 20 個と truth・書式の変異を合わせて全件検出、しかも**生きた drift を 1 件見つけた** (`docs/deployment.md` の self-check 出力例が version 81 のままで、82 だと `selfcheck` は FAIL を返すので例として成立していなかった)。**この gate 自身も untracked のまま `make gates` に落とされた** — #2857 の `gaterun-check` が `git ls-files` で見るため。
- **2026-09-06**: `make gates` に `gaterun-check` を追加 (#2857)。`make help` の target は 118 → 119。**`go test -run` は該当が無くても exit 0 で通る** (`ok ... [no tests to run]`) ので、ゲートのテストが消えても `make gates` は緑のままだった。#2840 で実際に踏んでいる — 新設したゲートファイルが untracked のまま、`wiring-check` は PASS が 12 → 11 に減るだけで何も言わずに通った。**件数ではなく名前で突き合わせる** — 期待件数を別に持つと、それ自体が同期を要する第 2 の一覧になる。`-run` に書かれた名前がそのまま一覧なので「その名前に一致する tracked なテストが 1 つ以上あるか」だけを見る。**`git ls-files` で見るのが要点** — ディスクを走査すると `git add` を忘れた新規ゲートが手元では見つかり、CI で初めて落ちる。**完全一致にはしない** — `notfound-check` の `TestScanCollapsedLookups` は `_APILayer` / `_CoreLayer` をまとめて指す前方一致で、厳密にすると正当な書き方が落ちる (実測)。接頭辞を保つ rename は `-run` でも引き続き当たるので、検出したいのは「1 つも当たらなくなった」状態だけ。**`gates:` からの脱落も見る** — -run が解決しても一括実行から漏れていれば誰も回さない (同じ「黙って検査が止まる」型)。
  **Makefile を自前でパースしない** — 行継続・列 0 のコメント・recipe 中の空行・同一 target の複数ルール・集約 target は
  どれも make の仕様で、自前パーサに継ぎ足すと**手当てするたびに隣の穴が開く** (敵対的レビュー 3 周で毎周それを繰り返した)。
  `make -n <target>` と `make -pn` に解決させ、こちらは出力から `go test … -run …` を拾うだけにした
  (`$(shell …)` がこの Makefile に無いので `-n` に副作用も無い)。**make の出力にも行継続は残る**ので、そこだけは畳む。
  実装自体が最初 untracked で落ち、完了条件を自分で実証した。
- **2026-09-04**: Section 3 に `make compose-check` を追加し `make gates` の一括対象に入れた (#2828)。`make help` の target は 114 → 115。配布する compose 3 つ (`docker-compose.yml` / `docker-compose.image.yml` / `compose.uds.yaml.example`、計 12 サービス) が `logging:` を持たず、Docker 既定の `json-file` が**ローテーションなし**で動いていた。**サービスを足したときが危ない** — anchor (`*default-logging`) を書き忘れても compose は通るし起動もするので、ディスクが埋まるまで気付けない。gate は `max-size` / `max-file` の**値そのもの**を見る (「空でない」だけだと `max-size: 50g` のような「上限を書いたのに実質無制限」が素通りする、実測)。service を 1 つも読めなかったら落とす — 書式が変わって拾えなくなると、検査していないのに緑になるため。**コメントアウトされたサービスは見えない** (YAML パーサはコメントを読まない) ので、既定無効のテンプレート (video-thumb) は gate の対象外。**一覧は手で持つ** — root には検証用の compose が 8 つあるので `git ls-files` の列挙が使えない。**`max-size` は decimal** で読まれる (json-file は `units.FromHumanSize`) ので `50m` は 50,000,000 バイト = 47.7 MiB。`50mib` と書いても同じ扱いで MiB は表現できない。
- **2026-09-01**: Section 3 に `make notfound-check` を追加し `make gates` の一括対象に入れた (#2792)。`make help` の target は 113 → 114。**repository の lookup error を種別を見ずに 4xx へ潰している箇所が 107 件**あり (`internal/api` + `internal/server` の非テスト Go を AST で走査し、`Find` で始まるか `Get` の 単行 lookup の直後 3 文以内にある `if` が、not-found 述語を通さずに 4xx を返す形を数えた。issue 本文の「135 のうち 61」は `FindByID` に限った別の数え方)、DB 接続断が「そんなノートは無い」に化けていた。クライアントからは区別できず、監視でも 5xx が立たない。upstream は `.findOneBy` の結果が `null` かで判定するので障害は例外として 500 になる。一括変換はできない — `if err != nil || !list.IsPublic {` のように not-found 判定と権限判定が同じ条件に混ざる形があるため。gate で**新規流入を止めてから段階的に潰す**方針を採り、107 件すべてを潰して allowlist は空になった。**allowlist を件数で持つのが要点** — key は `<file>:<func>` なので、理由の文字列だけを持つ形だと**その関数に 1 つでも残っていれば何個足しても素通りする** (実測)。判定は条件と body の両方で not-found 述語を探す (正しい直し方は body の中で分けるので、条件だけ見ると**直したものを検出し続ける**)。err 変数は名前のパターンではなく**代入の左辺と突き合わせる** (`err2` を拾うために部分一致にすると `n, e :=` が漏れ、逆もまた然り)。
- **2026-08-31**: Section 3 に `make wiring-check` を追加 (#2762)。`make gates` の一括対象も 1 つ増えて `make help` の target は 111 → 112。router で配線しないと効かない設定 (今回は `meta.enableFanoutTimelineDbFallback`) が、**配線を消しても build もテストも通ってしまう**ため。`internal/server` は CI のカバレッジ対象外で router を組み立てるテストも無く、#2762 の穴 (列と admin 公開はあるが読み取り経路に配線されていない) がまさにこれだった。判定は router.go をソースとして読む文字列一致だが、**コメント行は数えない** (コメントアウトして残すのは消すのと同じ)。同 package の既存 gate が生ソースを見ているのに合わせてある。(**#2856 で AST 照合に変えた** — 行頭 `//` だけを除外する形は `/* */` で囲んだ配線を素通りさせていた。あわせて引数まで照合するようになったので、`WireMetaToggles(hook, nil, nil)` も落ちる)
