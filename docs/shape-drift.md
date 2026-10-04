# Entity shape drift gate (Layer 0 / 2 / 3)

mk-goのentity DTO構造体・packer出力・実HTTPレスポンスを、Misskey API契約(misskey-jsの`types.ts`=OpenAPI `components.schemas`のミラー)とフィールド単位で突き合わせ、**3rd-partyクライアントのshape crashを引き起こすドリフト**を検出するゲート。

サーバー/ブラウザ/Docker不要で、決定的・ミリ秒で動く。CIでは`go test ./...`の一部として自動実行される(`TestEntityShapeDrift` + `Test*ShapeL2` + 各handler testの`shapetest.Assert`)。

検出は3レイヤに分かれる。下に行くほど「実際に出る値」に近づき、上ほど網羅的:

| Layer | 何を見るか | どこで | 射程 |
|---|---|---|---|
| **L0** | 構造体の**宣言**(reflection) | `internal/entitycompat`の`TestEntityShapeDrift` | 全family網羅。ただし宣言しか見ない |
| **L2** | **packer出力**(fixtureでmap生成) | `Test*ShapeL2` | map-based packer / union / populate漏れ |
| **L3** | **実HTTPレスポンス**(handler test) | 各api packageの`shapetest.Assert` | handlerがアドホックに組む応答・実際の値 |

L0は宣言を全網羅、L2/L3は「実際に出た値」を見るので**宣言は正しいが実行時にnull/欠落するバグ**を捕まえる。L3が最も実態に近く、このセッションで30経路超へ拡大した(下記)。

## なぜ必要か

Misskey互換クライアント(Miria等)は、misskey-jsの型に従ってレスポンスをデシリアライズする。`text: string`(non-null)と宣言された欄をmk-goが`null`で返したり、必須欄を省略すると、クライアントは非nullキャストに失敗してクラッシュする。

これは本質的に**スキーマ(shape)の不一致**であって、Playwrightやdrop-in E2Eのようなブラウザ越しの挙動テストで間接的に検出する問題ではない。本ゲートは契約レベルで直接diffを取るため、flakyゼロ・全フィールド網羅で検出できる。

## 構成

| ファイル | 役割 |
|---|---|
| `internal/entitycompat/shapecheck.go` | reflection / types.tsパーサ(`FieldShape`/`Elem`)/ diffロジック |
| `internal/entitycompat/mapping.go` | entity構造体 ↔ golden schemaのマッピング表 |
| `internal/entitycompat/gate.go` | snapshot/allowlistのロード、ゲート判定 |
| `internal/entitycompat/runtime.go` | L2/L3 validator(`ValidateValue`/`ValidateUnionValue`)、`L2FlatSchemaNames()` |
| `internal/entitycompat/response.go` | embedded golden(`//go:embed`)+ `ValidateResponse`(L3入口) |
| `internal/entitycompat/shapetest/` | handler testから呼ぶ`shapetest.Assert`(L3)。`testing`importを持つためprod非依存 |
| `internal/entitycompat/testdata/golden_schemas.json` | golden契約のスナップショット(commit対象) |
| `internal/entitycompat/testdata/allowlist.json` / `allowlist_l2.json` | 既知/意図的ドリフトのallowlist(baseline backlog) |
| `tools/shapediff/` | snapshot再生成 + 全family drift report |
| `internal/entitycompat/plugin_surface_test.go` | 公開プラグイン API の面 (`TestPluginSurfaceDrift`) |
| `internal/entitycompat/plugin_doc_test.go` | `docs/plugins/authoring.md` の一覧 ↔ 公開面 golden (`TestPluginDoc_*` 5 本) |
| `internal/entitycompat/divergence_doc_test.go` | `docs/divergence.md` ↔ 実 schema / 生成物 / router.go (`TestDivergenceDoc_*` 6 本)、`docs/api-compat.md` ↔ router.go (`TestAPICompatDoc_MatchesRouter`) |
| `internal/entitycompat/schema_drift_test.go` | migration の列 ↔ upstream entity (`TestSchemaDrift_CreateOnlyColumns`) |
| `internal/entitycompat/migration_seed_test.go` | TypeORM `migrations` seed の網羅 (`TestMigrationSeed_CoversUpstream`) |

golden側は**commit済みスナップショット**を読むため、テスト時にsubmoduleを必要としない(hermetic)。

## 検出するドリフトの種類

| Kind | Severity | 意味 |
|---|---|---|
| `missing` (required) | HIGH | golden必須欄をmk-goが全く出さない |
| `missing` (optional) | LOW | golden optional欄が無い(非ブロッキング) |
| `nullable` | HIGH | goldenがnon-nullなのにmk-goが`null`を出しうる(omitempty無しポインタ) |
| `omit` | MED | goldenが必須なのにmk-goが`omitempty`で省略しうる |
| `extra` | INFO | mk-go独自欄(拡張/alias候補) |
| `layer` | INFO | 同family内の別layerに存在(配置違いだが出力上は存在) |

ゲートは**HIGH/MED**のみをブロック対象とする。LOW/INFOはレポートのみ。

## マッピングの考え方

goldenはユーザー shapeを合成可能な部品(`UserLite` / `UserDetailedNotMeOnly` / `MeDetailedOnly`)に分解し、mk-goは構造体埋め込みでこれを写している。よって各埋め込みlayerを対応する`*Only`スキーマに突き合わせる。standaloneなentity(Note等)はfamily of one。

### 対象外

- **Notification**: golden側がtype別のdiscriminated union、mk-go側も`PackNotification`が`map[string]any`を手組みするため、reflection(L0)は届かない。**L2**(fixtureでpacker出力を検証)と**L3**(`/api/i/notifications`の実HTTP応答を`ValidateResponse`のunion dispatchで検証)でカバーする。

## 運用

### 日常(CI)

`go test ./...`で`TestEntityShapeDrift`が走る。新しいドリフトが入ったPRはここで落ちる。ローカル確認は:

```bash
make shapecheck          # gateをCIと同じ判定で実行
make shapecheck-report   # 全familyのdrift一覧(severity付き)
```

### allowlist

ゲート導入時点で存在したドリフトはbaselineとして`allowlist.json`に記録済み。各エントリは**潰すべきbacklog**であって恒久免除ではない。新規ドリフトは:

1. 構造体を修正してドリフトを解消する、または
2. 理由を添えて`allowlist.json`に追加する(意図的拡張等)

allowlistに登録済みのドリフトを修正すると、そのエントリは**stale**になりゲートが落ちる(allowlistの掃除を促す)。

### upstream catch-up時

`UPSTREAM_MISSKEY_VERSION`を新バージョンに書き換えて`make upstream-fetch`で本家を取得したら、goldenスナップショットを再生成してcommitする(goldenは`.cache/misskey/<版>/`の本家から作る。#3378):

```bash
make shapecheck-gen   # testdata/golden_schemas.json を再生成
git add internal/entitycompat/testdata/golden_schemas.json
```

これで新バージョンで追加/変更された契約欄が次回のゲートに反映される。

## Layer 2: 実行時shape検証(map-based / union)

L0の静的reflectionは「宣言されたshape」しか見ない。以下はその射程外:

- `map[string]any`で手組みするpacker(`PackNotification`等)
- discriminated union(通知のtype別variant)
- 構造体は正しいが、handlerが実行時に値をpopulateし忘れるバグ

L2は**packerが実際に出力したJSON値**を契約に突き合わせてこれを埋める。二backend(TS)起動は重くflakyなので避け、**fixtureでpacker出力を生成 → golden契約に検証**する軽量版を採る。

| ファイル | 役割 |
|---|---|
| `internal/entitycompat/runtime.go` | union parser(`ParseUnion`)+ 実行時validator(`ValidateValue` / `ValidateUnionValue`) |
| `internal/entitycompat/runtime_test.go` | `Test*ShapeL2`(実packer出力を検証)+ 単体テスト |
| `testdata/golden_unions.json` | union契約のスナップショット(type literal別variant、commit対象) |
| `testdata/allowlist_l2.json` | L2 baseline backlog |

### 対象packer

L2は2種の検証関数を持つ:

- **`ValidateUnionValue`**: discriminated union(`Notification`)を`type`でvariant dispatchして検証。`UnionSchemaNames()`が対象。
- **`ValidateValue`**: flatなmap-based packer(`Announcement`等)を golden flat schema に対して検証。`L2FlatSchemaNames()`が対象。golden schemaは`golden_schemas.json`に同居(L0と同じflat形式)。

map-based packer(`map[string]any`を手組みするもの)はL0のreflectionが届かないため、L2で実出力を検証する。新しいmap-based packerを守りたいときは`L2FlatSchemaNames()`にgolden schema名を足し、fixtureで`Test*ShapeL2`を書く。

### ValidateUnionValue の判定

通知mapの`type`でgolden variantを引き、そのvariantに対して検証する:

- `type`に対応するvariantが無い → HIGH(strictクライアントがunion dispatchで弾く)
- variantの必須欄が実行時に欠落 / null → HIGH
- scalar型不一致 → MED
- golden未知の欄 → INFO(拡張)

### L2 baseline(実検出例)

L2導入時点で検出された実ドリフト:

- `type=pollVote` / `type=importCompleted`(HIGH): mk-goが出す通知typeがgolden union(`pollEnded` / `exportCompleted`)に無い。strictクライアントがdispatchできない。
- `noteId`(INFO): mk-goが通知に足す独自欄(契約外、非ブロッキング)。

### まだ残る射程外

- `/api/i`等がhandler層で`map[string]any`にアドホック追加する欄(再利用可能なpackerを介さないため、fixtureで再現できない)。これは下記**L3**で実レスポンスを直接検証して埋める。

## Layer 3: 実HTTPレスポンス検証(handler test)

L2のfixtureは「packerが正しく呼ばれれば」を見るが、handlerがpackerを介さず`map[string]any`を直接組む経路(`admin/*`の多く、`/api/meta`、chat/reversi等)や、handlerがrelationをattachし忘れる実行時バグはfixtureで再現しづらい。

L3は**handler unit testが実際に返したJSON**を golden に突き合わせる。各api packageの既存テストに1行足すだけ:

```go
import "github.com/shiroha-a/mk/internal/entitycompat/shapetest"

func TestCreate_Success(t *testing.T) {
    // ... handlerを叩いて rec.Body を得る ...
    var resp map[string]any
    require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
    shapetest.Assert(t, "Clip", resp) // golden "Clip" に対し HIGH/MED drift があれば t.Error
}
```

`shapetest.Assert(t, schemaName, actual)`は embedded golden(`//go:embed`)に対し`ValidateResponse`を回し、HIGH/MEDのfindingがあればテストを落とす。**packerは要らない**(`internal/entitycompat/shapetest`は`testing`importを持つがプロダクションには含まれない)。

### L3を足す手順

1. golden schema名を`L2FlatSchemaNames()`に追加(L2と共通の抽出リスト。L0 family済みの`UserLite`等は不要)。
2. `make shapecheck-gen`でsnapshot再生成。
3. 対象handlerのsuccess testで応答を`map[string]any`にdecodeし、`shapetest.Assert(t, "<Schema>", resp)`を足す。配列応答なら`resp[0]`を渡す。
4. テスト実行。drift が出たら**修正の前に現Misskey実装を確認**(下記lineage / verify-before-fix)。

### 配列応答・ネスト

- 配列を返すendpoint(`mute/list`等)は`rows[0]`をassert。
- ネストした子objectを検証したいとき(`/api/meta`の`counts`等)はその子だけ渡す: `shapetest.Assert(t, "QueueCount", resp["counts"].(map[string]any))`。
- 合成型(`MetaDetailed = MetaLite & MetaDetailedOnly`)はparserがflat抽出できないので、**構成要素を個別にassert**する(同じ応答に`MetaLite`と`MetaDetailedOnly`の2回)。

### discriminated union(`Notification`)

`shapetest.Assert(t, "Notification", resp[0])`は**flatでもunionでも同じ呼び方**で通る。`ValidateResponse`が **flat golden → union(`golden_unions.json`)→ not-found** の順でdispatchし、unionなら値の`type` discriminatorでvariantを引いて`ValidateUnionValue`に回す(未知typeはHIGH、variant必須欄の欠落/nullもHIGH)。

L2のfixtureは「packerが正しく呼ばれれば」を見るのに対し、L3は`/api/i/notifications`の**実HTTP応答**を直接unionに突合する。`type=follow`等のvariantはhandlerが`user`(reaction系は`note`も)を解決して埋める必要があるので、`SetRepos`をwireして対象userを seed したtestで配線する。

## array要素型の検出(`Elem`)

`string[]`と`{id,name}[]`はどちらもcoarseには`"array"`で、要素型を見ないと取り違えを見逃す(例: `EmojiDetailedAdmin.roleIdsThatCanBeUsedThisEmojiAsReaction`)。

parserは`FieldShape.Elem`に要素型(`string`/`number`/`boolean`/`object`/`array`/`other`)を記録し、`ValidateValue`は**非空配列の先頭要素**型をElemと突き合わせる(空配列は判定不能でskip、`json.RawMessage`はdecodeしてから判定)。多行`{...}[]`も正しく`array`+`elem:object`として抽出する。

## lineage判定: vanilla か cherrypick 派生か

goldenは**vanilla Misskey**のmisskey-jsから生成される。一方mk-goの一部endpointは**yojo-art/cherrypick**由来で、vanillaと契約が異なる:

- **`/api/chat/*`**: cherrypick federated chat由来。ただし`ChatRoom`/`ChatMessage`はvanillaとshapeが一致したのでgate済み。
- **`/api/reversi/*`**: yojo-art/cherrypick + **連合対戦拡張**。`crc32`等のvanilla goldenに無い独自field・federation関連で乖離が大きく、**vanilla golden gateの対象外**。

**新しいendpointをgateする前に、それがvanilla由来か派生由来かを確認する。** 派生由来でvanilla goldenと乖離するものは、無理にgateすると「lineage差」を「drift」と誤認する。

### verify-before-fix(driftを見つけたら直す前に確認)

L0/L2/L3がdriftを出しても、**修正の前に現misskey-ts実装を確認する**。golden(契約)とMisskeyの実packer出力がズレているケースがあるため:

- **vestigial field**: 契約には残るが機能削除済みのfield(例: `antenna.notify`はカラム削除済でpackerが定数`false`を返す)。goldenにあってもmk-goで実装し直すのは誤り。
- **endpoint取り違え**: goldenの同名schemaが別endpointの契約のことがある(例: `EmojiDetailed`は`admin/emoji/list`、`EmojiDetailedAdmin`は`v2/admin/emoji/list`)。
- 確認先: `.cache/misskey/<版>/.../core/entities/*EntityService.ts`(packer)、endpoint定義の`res`スキーマ、migration。

## gateの盲点と補い方

- **`Record<string, never>`等は`"other"`型**になり、`ValidateValue`の型検査をskipする(`QueueJob.progress`/`data`等)。ただし**required欄の欠落**と**non-null欄のnull**は型に関係なく検出される。`progress`がnumberかobjectかのような「otherの中身」はtest側で明示assertして補う。

  ただし**goldenがupstreamの実装と食い違う場合は、goldenの方に従わない**。`QueueJob.failedReason`/`returnValue`はschema上required だが、Bullのjobは失敗するまで`failedReason`を持たないのでupstreamの`packJobData`は`undefined`を返し、JSONから消える。frontendは`v-if="job.failedReason != null"`で行の有無を決めるため、goldenに寄せて空文字を常に出すと**成功したjobにも赤い警告アイコン付きの空行**が出た(#2689)。goldenは生成物なので直さず、`shapetest.AssertExcept`で理由付きに例外化する。
- **lite/detail等のfield partition**は集合演算で体系的に差分を取る: goldenの該当schema(`MetaLite`/`MetaDetailedOnly`)のtop-level fieldをparserで抽出し、handlerの応答キーと突き合わせて「必須なのに欠落」「detail専用なのにleak」を算出する(`/api/meta`の`#1306`で使用)。

## 既知の修正済みドリフト(本ゲートで検出)

L3拡大の過程で検出・修正した実ドリフトの代表例(いずれも現misskey-ts確認の上で対応):

| 対象 | ドリフト | 種別 |
|---|---|---|
| `pages` (PackPage) | `content`が空時null(golden `PageBlock[]`非null) | null-array |
| `channels` | `createdAt`/`bannerUrl`欠落、`pinnedNoteIds`がnull | 欠落 + null-array |
| `chat/rooms/create` | `owner`(UserLite)未attach | embed漏れ |
| `chat/messages/create` | `fromUser`未attach | embed漏れ |
| `admin/abuse-report/notification-recipient` | `updatedAt`列欠落、`userId`/`systemWebhookId`がnull | schema gap + null |
| `v2/admin/emoji/list` | `roleIds`が`string[]`(golden `{id,name}[]`) | array要素型 |
| `admin/queue/show-job` | `progress`/`data` | 型 + 欠落 |
| `/api/meta` (lite) | MetaLite必須5欄をomit、MetaDetailedOnly 3欄をleak | partition |

## Error-id drift gate（error.id の per-endpoint 整合）

shape gateがレスポンス**ボディ**の契約を守るのに対し、これは**エラーレスポンスのid**を守る別ゲート(`TestErrorIDDrift`)。

Misskeyのエラーは`{code, message, id}`形式で、クライアントは`code`だけでなく**endpoint固有のUUID `id`**でエラーを識別する。mk-goは「1 code = 1 UUID使い回し」になりがちだが、Misskeyは**endpointごとに別id**を割り当てる(例: `NO_SUCH_WEBHOOK`はshow/update/delete/testで4つ別id)。idがズレると`code`が正しくてもdrop-inクライアントがエラーを誤分類する。

### 仕組み

完全に静的(サーバ起動不要)。`internal/api/**/*.go`を走査し、各handler **method**が返すエラーの`(code, id)`を4経路で抽出する:

- **inline literal**: `apierr.Error("CODE", msg, "uuid")`
- **UUID定数参照**: `apierr.Error("CODE", msg, apierr.UUIDxxx)`(`errors.go`の定数表で解決)
- **helper呼び出し**: `apierr.NoSuchUser()`等(`errors.go`のhelper表で`(code, uuid)`に解決)
- **echo wrapper**: `apierr.JSONNoSuchNote(c)`等(`echo.go`の`JSONXxx`→内部helper→`(code, uuid)`に解決)

echo wrapperはhandlerの最頻送出経路なので外すとgateが大半のrouteを素通しする(実例: これを足すと解決数が577→652に増え、未検出だったdrift 25件が露出した)。

**`internal/server/router.go`のインラインendpointは0件**。#2791で全14件を`internal/api`配下へ移設したので、上のapi walkがそのまま拾う。

gateは「golden と突合できた数の下限」から**「inlineが復活したら落とす」向きに反転**させた。判定に使うのは`countInlineHandlers`で、`/api` group tree配下のroute登録のうち**`handlerVar.Method`で解決できない形**を数える:

- closure直書き (`api.POST("/x", func(c echo.Context) error {...})`)
- ident束縛のclosure (`h := func(...); api.GET("/x", h)`)
- **コンストラクタ直渡し** (`api.POST("/x", pkg.NewHandler(a).Read)`)。closureではないが`parseRoutes`の解決からも外れるので、error idが丸ごと無検査になる

レシーバは`api`から派生したgroupを推移的に辿り、**チェーン呼び出し** (`api.Group("/x").POST(...)`) も見る。メソッドは`Match` / `Any` / `Add`も対象 (GET+POSTの両登録に使われうる)。**`/api`配下に限る** — レシーバを一切問わないと`/healthz`やpprof、frontendのcatchallまで拾い、あれらは移設対象ではない。

`scanInlineRoutes` / `scanBodyEmissions`は**inlineが復活したときのdrift突合用**に残してある (現在の寄与は0件)。復活の検出そのものは`countInlineHandlers`が担う。抽出器自体は`inline_scan_test.go`でテストしてある — guardが「0件ならPASS」の向きなので、**抽出が壊れると空振りする**。

移設先を書くときは**ハンドラを変数に代入してから渡す**。gateは`handlerVar.Method`の形しか解決しない。

なお上の**インライン復活の検出は id gateだけ**が持つ。status / kind gateは`internal/api`のみを見る。

### golden

`tools/erroriddiff`がMisskeyの`endpoints/*.ts`の`meta.errors`から`endpoint → {code: id}`を抽出し、`internal/entitycompat/testdata/golden_error_ids.json`へ生成・embedする(third_party非依存でCI実行可)。

### 除外（lineage / upstream typo）

- **reversi/* ・ chat/***: cherrypick派生。idもvanillaへ寄せない(`errorIDExcludedPrefixes`)。
- **不正UUIDのgolden値はskip**: upstreamのtypoでidが壊れている箇所(`sw/update-registration`は先頭スペース、`i/2fa/update-key`等は非hex文字を含む)は揃える対象が無いため、`validUUID`で弾く(自己文書化された除外)。
- **mk-go独自code・route未解決**: 対応するMisskey契約が無いので対象外(driftではない)。

確実に解決できたケースのみgateする方針なので、誤検出より見落としに倒している。

### ep-aware helper(gate射程外だが構築により整合)

同じcodeをendpointごとに別idで返す必要がある一方、複数endpointが**共有helper**を呼ぶケース(`drive`の`mapFileError`/`mapFolderError`、`users`の`listRelations`、`notes`の`serveList`、`i/2fa`の`requireWebAuthn`等)は、helperに**endpoint識別の引数**(enum/bool/id literal)を渡して各endpoint固有idを選択する。これらのidはhelper内で動的選択されるためstatic gateはdataflowを追えず**検証対象外**だが、`switch ep { ... default: panic }`等で網羅を強制し**構築により**driftを防ぐ(#977 `mapFolderError`が初出、#1336で他helperへ展開)。新規endpointを共有helperに足すときはこの分岐を必ず更新する。

### 運用

```bash
make errorid-check    # id gate + HTTP status gate + kind gate をローカル実行
make shapecheck-gen   # shape golden と合わせて error id / status / kind golden も再生成
```

upstream bump時は`make shapecheck-gen`で全goldenを再生成してcommit。新しいerror idを返すendpointを足すときは、verify-before-fixに従い対応するMisskey endpointの`meta.errors`のidを確認してから実装する。

## Error HTTP status drift gate（明示ステータスのみ）

`TestErrorHTTPStatusDrift`は、エラーレスポンスの**HTTPステータス**をgateする。ただし**Misskeyが明示的にステータスを固定しているエラーだけ**を対象にする。

Misskeyの`ApiCallService`は`httpStatusCode` → 無ければ`kind`既定(`client`→400 / `permission`→403 / `server`→500)でステータスを決め、`kind`既定値は`client`。実態として**459エラー中431件(93.9%)が未指定=400**で、mk-goは`NO_SUCH_*`に404、`ACCESS_DENIED`に403というセマンティックなステータスを返す。misskey-jsは`status===200`以外を一律errorとしてbodyを読むため400/404を区別せず、**全部400に倒すのは設計上の損失**(REST的セマンティクスを失う)。

そこで本gateは「Misskeyが`httpStatusCode`または非デフォルト`kind`を明示している」契約(28件)だけを golden 化(`tools/erroriddiff`が`golden_error_status.json`に出力、暗黙400は記録しない)。mk-goが各endpointで返すステータス(inline `c.JSON(http.StatusX, ...)` / `JSONXxx` wrapper)を解決して突合する。

検出・整合した実例(本gate新設時):

| endpoint | code | mk旧 | Misskey明示 |
|---|---|---|---|
| `drive/files/create` | `MAX_FILE_SIZE_EXCEEDED` | 400 | 413 (`httpStatusCode`) |
| `users/show` | `FAILED_TO_RESOLVE_REMOTE_USER` | 404 | 500 (`kind:'server'`) |

## Error kind drift gate

`TestErrorKindDrift`は、エラーenvelopeの`kind` discriminator(`client`/`server`/`permission`)をgateする(#1608)。

Misskeyの`ApiCallService.send()`は全エラーenvelopeに`kind`を必ず含め(`ApiError`の既定は`client`)、`#sendApiError`が`kind`からWWW-Authenticateヘッダを導出する(`client`→`error="invalid_request"`、`permission`+`PERMISSION_DENIED`→`error="insufficient_scope"`)。mk-go側は`apierr.Error()`が`kind:"client"`を、明示が要る箇所は`apierr.ErrorWithKind()`が任意のkindを出す。ヘッダ付与は`/api` groupの`WWWAuthenticate` middlewareが横断で行う。

gateは2方向:

- **明示kind**: `tools/erroriddiff`がupstreamのエラー定義から`kind`明示エントリだけを`golden_error_kinds.json`へ抽出(現行9件: `NO_SUCH_ABUSE_REPORT`等が`server`、`i`の`USER_IS_DELETED`が`permission`)。解決できたemissionのkindはこれと一致しなければならない。
- **暗黙client**: kind goldenに無くても`golden_error_ids.json`にcodeがある(=upstreamがそのendpointで定義する)エラーは、既定の`client`を要求する。mk-go独自code・route未解決はid gateと同じ方針で対象外。

実行・golden再生成はid gateと同じ`make errorid-check` / `make shapecheck-gen`。

## Pagination limit-spec drift gate

`TestLimitSpecDrift`は、list endpointの`limit`の**default / maximum**をMisskeyの`paramDef`と整合させるgate。

Misskeyは`limit: { type:'integer', minimum, maximum, default }`を宣言し、ajvが**default補完 + 範囲外reject**する。mk-goはhandlerでimperativeにclampしていたため、default/max値がupstreamとずれると「limit省略時の件数」や「上限」が変わる(`limit`省略時に10件返すべきが30件返る等)。

### 仕組み

handlerは`pagination.ClampLimit(limit, def, max)`でlimitを正規化する。gateは各call siteの`def`/`max`**リテラル**を読み、`router.go`で囲むmethodをendpointに解決し、Misskey paramDefから生成したgolden(`golden_limit_specs.json`、`tools/limitspec`が生成)と突合する。L3と同様**段階的**で、`ClampLimit`へ移行済みのcall siteだけがgate対象(未移行は素通し)。

clampロジック自体(mk=clamp / Misskey=範囲外reject)は揃えない — mk側のclampの方が寛容で、揃えると既存クライアントを壊しうる。**default/max値のみ**が契約として gate される。

### 運用

```bash
make limitspec-check  # gate をローカル実行
make shapecheck-gen   # golden_limit_specs.json も再生成
```

## Permission drift gate（アクセス制御）

`TestPermissionDrift`は、mk-goのrouter middlewareがMisskeyの宣言する**アクセス要件より緩くない**ことを検証するセキュリティgate。

Misskeyは各endpointのmetaで`requireAdmin`/`requireModerator`/`requireCredential`を宣言する(階層: public < auth < moderator < admin)。mk-goはrouterで`middleware.RequireAuth`/`RequireModerator`/`RequireAdmin`/`RequireRolePolicy`を適用する。`tools/permspec`がMisskey metaから`endpoint→level`のgolden(`golden_permissions.json`)を生成し、gateがrouterのmiddleware levelと突合する。

### looser方向のみgate

mk-goが**Misskeyより緩い**(= 権限昇格 / 認証欠落)ケースのみを失敗扱いにする:

- mk public だが Misskey requireCredential → 匿名アクセス可(認証欠落)
- mk moderator だが Misskey requireAdmin → moderatorがadmin専用に到達(権限昇格)

逆に mk が**厳しい**(Misskey public を auth 要求、Misskey auth を RolePolicy 要求等)のは防御的強化として許容し、flagしない。router parseは複数行inline handler(`}, middleware.RequireAuth())`)も括弧バランスで登録全体を読み、閉じ行のmiddlewareを取りこぼさない。

### gate新設時に検出・修正した実drift (10件)

| 種別 | endpoint | 修正 |
|---|---|---|
| 権限昇格 (admin→moderator) | `admin/accounts/{delete,find-by-email}`, `admin/captcha/save`, `admin/{delete-account,delete-all-files-of-a-user,get-user-ips,show-moderation-logs}` | RequireModerator→**RequireAdmin** |
| 認証欠落 (auth→public) | `notes/polls/recommendation`, `roles/list`, `roles/notes` | **RequireAuth** 追加 |

### 運用

```bash
make perm-check       # gate をローカル実行
make shapecheck-gen   # golden_permissions.json も再生成
```

## Secure drift gate（app token 制限）

`TestSecureDrift`は、Misskeyの`secure: true` endpoint(password変更 / 2FA / data export-import / authorized-apps 等のaccount-security系、51件)が、mk-goで`middleware.RequireSecure`を適用していることを検証する。

**逆向きも見る (#2877)。** golden に無い endpoint に `RequireSecure` が付いていたら落とす。片方向のままだと、upstream が `secure` を外したとき (2026.9.0 の `i/revoke-token` がまさにそれ) に golden から消えるだけで、mk-go 側に `RequireSecure` が残っていても緑のまま通る = サードパーティアプリから叩けるようにする変更が**機能していなくても検出できない**。意図的に厳しくする場合は `secureStricterThanUpstream` に理由付きで登録する (現在は空)。

Misskeyの`secure`は「native session token のみ許可、第三者app/OAuth/MiAuth access token 不可」(ApiCallServiceの`isSecure = user != null && token == null`)。これが無いと、有効なaccess tokenを持つ第三者appがpassword変更や2FA解除を駆動できてしまう。

mk-goは全認証がtoken経由(session無し)で、native token = `users.token`、app/MiAuthは別の`access_tokens`行。`RequireSecure`は`*user.Token == GetToken(c)`でnative判定し、一致しなければ403 ACCESS_DENIED(Misskey id `56f35758-...`)。`tools/securespec`が`secure: true` endpointのgolden(`golden_secure_endpoints.json`)を生成し、gateがrouter登録(複数行inline含む括弧バランスparse)に`RequireSecure`があるか突合する。

## OAuth scope drift gate（meta.kind）

`TestPermissionDrift` が見るのは `public|auth|moderator|admin` の粗い段だけで、**OAuth scope (`meta.kind`) の取り違えは素通りする**。実際 `admin/queue/stats` は `read:admin:emoji` のまま出荷されていた (upstream 自身の typo をそのまま移植したもので、upstream は 2026.9.0 で修正)。絵文字の scope しか持たないアプリが queue の統計を読め、queue の scope を持つアプリが読めない状態だった。

`tools/permspec` が upstream の `meta.kind` を `golden_oauth_kinds.json` (297件) に出し、router の `middleware.RequireScope` と突き合わせる。**値の不一致と、kind があるのに `RequireScope` が無い方の両方**を見る (後者は app token が scope 無しで叩ける)。意図的に付けない場合は `kindMissingScope` に upstream の kind を添えて登録する (現在は空)。

数え方: golden は endpoint 単位の map で、`kind` は **meta 直下 (タブ 1 つ) の宣言のみ**を拾う (`meta.errors` の中にも `kind: 'server'` があるため)。**クォートは 2 種類あることに注意** — `endpoints/i.ts` だけが `kind: "read:account"` とダブルクォートで、シングル限定にすると**最も広く叩かれる `/api/i` が丸ごと検査対象から外れる** (#2877 で実際に取りこぼし、scope を変異させても gate が緑のままだった)。

### 運用

```bash
make perm-check       # permission / secure / OAuth scope の 3 gate をローカル実行
make shapecheck-gen   # golden_permissions.json / golden_secure_endpoints.json /
                      # golden_oauth_kinds.json をまとめて再生成
```

## Schema drift gate（drop-in で生えない列）

`TestSchemaDrift_CreateOnlyColumns`は、mk-goのmigrationが**`CREATE TABLE IF NOT EXISTS`の中でしか定義していない列**のうち、upstreamのentityに存在しないものを検出する。

Misskey TSが既に作ったテーブルに対して`CREATE TABLE IF NOT EXISTS`はno-opになるため、この形の列は**TS製DBにだけ生えない**。upstreamにも同名の列があればTS側が作っているので問題ないが、mk-go独自の列は生えず、読み書きすると drop-in 環境でのみ`column "..." of relation "..." does not exist`で落ちる。新規にmk-goから作ったDBでは`CREATE TABLE`が実際に走るため再現せず、通常のテストでは踏まない。

実際に`app.createdAt` / `auth_session.createdAt` / `clip.notesCount`の3本がこの形で紛れ込んでいた(#2243)。`ALTER TABLE ... ADD COLUMN`で追加した列は両方のshapeで冪等に効くのでgateの対象外。

検出時の解消方法は2つ:

- **(a) その列への依存を外す** — upstreamに無いということは本来不要なはず。#2243はこちらを採った(createdAt 2本はそもそも読んでいなかった、notesCountは`clip_note`の実カウントに寄せた)
- **(b) ALTERで足す** — どうしても必要なら`ALTER TABLE ... ADD COLUMN IF NOT EXISTS`のmigrationを追加する(`000039_dropin_compat.up.sql`と同じ方式)。既存行のbackfillも併せて検討すること

`tools/schemadrift`がupstream entityの`@Entity('table')` + `@Column`系decoratorからテーブル別の列一覧をgolden(`golden_upstream_columns.json`)に出力し、gateがmigrationのparse結果と突合する。

fresh な mk-go DB にだけ残るが誰も読み書きしない列は、テスト内の`createOnlyAllowlist`に理由付きで登録する。**その列を使い始めるときは必ずallowlistから外すこと**(使うなら(b)のmigrationが要る)。

### 運用

```bash
go test ./internal/entitycompat/ -run TestSchemaDrift   # gate をローカル実行
make shapecheck-gen                                     # golden_upstream_columns.json も再生成
```

## Migration seed gate（drop-in 復路）

**復路は保証しない(#3191)が、このgateはrequiredのまま残す。** seedは、どこまで戻れるかを測る`mkgo-born`の前提で、足す手間も小さいため。

`TestMigrationSeed_CoversUpstream`は、TypeORMのbookkeepingテーブル`migrations`へのseedが、upstreamの全migrationを網羅していることを検証する。

mk-goで動かしたDBに本家Misskeyを繋ぎ直したとき、TypeORMは

```js
allMigrations.filter(m => !executed.find(e => e.name === m.name))
```

で未実行migrationを選ぶ(`MigrationExecutor`)。比較キーは**`name`列の文字列一致**で、比較対象は`migration.name ?? migration.constructor.name`(= `DeleteCreatedAt1697420555911`形式)。seedに漏れがあるとそのmigrationが再実行され、適用済みDDLへの`ADD COLUMN`重複や`DROP COLUMN`によるデータ喪失につながりうる(#2244)。

nameは原則class名だが、`name = '...'`プロパティがあればそちらが優先される。`1690796169261-play-visibility.js`はclass名(`PlayVisibility1689102832143`)とnameプロパティ(`PlayVisibility1690796169261`)が食い違う唯一の例で、実DBには後者が入る。

`tools/schemadrift`がupstreamのmigrationファイルからname一覧をgolden(`golden_upstream_migrations.json`)に出力し、gateがmigration SQL中のTypeORM形式name literalと突合する。

**seedを追加する前に、そのmigrationのDDLがmk-go側にも入っているか必ず確認すること。** 入っていないままseedすると、TS側が「適用済み」と誤認してskipし、schemaがずれたまま放置される。

`make dropin-swap-test`のstage 8dにも、TS復帰後に`migrations`テーブルが変化していないことのassertを入れている(ただしこちらはTS製DBから始まるshapeなので general guard であって、本gateの代替にはならない)。

### 運用

```bash
go test ./internal/entitycompat/ -run TestMigrationSeed   # gate をローカル実行
make shapecheck-gen                                        # golden_upstream_migrations.json も再生成
```

## divergence doc gate（差分の一次資料が実態からずれない）

`docs/divergence.md` は upstream との差分の一次資料で、`diff-e2e` の ignore-list を足すときに「対応する記述があるか」を確認する運用になっている。**実態より少ない表を信じると、そこに載っていない差分を差分として認識しないまま調査が進む。**

実際に 3 箇所が静かにずれていた (#2634)。#2313 で分割アップロードの endpoint 4 件を足したときに冒頭サマリだけ更新して §1-1 の内訳表を更新せず、#2332 / #2340 / `instance_secret` でも同じことが起きた。§2-2 に至っては見出しの「実使用 14」と直後の散文の「15 件」が**隣接 2 行で矛盾**していた。どれも人が数え直さない限り気付けない。

### `TestDivergenceDoc_EndpointCountMatchesTable`

§1-1 の見出しの件数 == 表の件数列の合計 == 冒頭サマリの和の式。

### `TestDivergenceDoc_EndpointCountMatchesAPICompat`

§1-1 の見出しの件数 == `docs/api-compat.md` の `mk-go only (TS spec 外)`。

**内部整合だけでは足りない。** 上の gate は 3 箇所が互いに一致することしか見ないので、**3 つが揃って同じだけ間違っている**状態を通す。実際 develop では §1-1 が 53、生成物が 49、真値が 58 だった (#2640)。§1-1 の内訳表に数えられていなかったのは `admin/server-plugins` / `admin/server-metrics` / `admin/self-check` / `admin/federation/{delivery,inbox}-health` の 5 件で、うち 4 件は生成物の側には載っていた (= 突き合わせていれば気付けた)。

upstream の endpoint 一覧を `tools/apicompat` から直接引くことはできない (**`test-shards` job は submodule を checkout しない**。#3379 で frontend を `frontend/` へ取り込んでからは、submodule を checkout する job は無い)。ただし `make apicompat` の生成物は commit されているので、そちらを経由すれば submodule 無しで突き合わせられる。

この gate が落ちたとき**どちらが古いかは中身を見ないと決まらない**。api-compat.md 側が古いなら `make apicompat` で再生成する (route dump に stack が要る)。divergence.md 側が古いなら §1-1 の表・見出し・冒頭サマリの 3 箇所すべてを直す。

### `TestDivergenceDoc_ForkFrontendTagsMatchTable`

冒頭サマリの `N tag (-mk.X ～ -mk.Y)` == §4-2 の表の行数と範囲。tag 番号が連番であることも見る。

サマリは 10 tag と言い、表には 11 行あり、実際の submodule には 23 個の tag があった (#2640)。**この gate が捕まえるのは前 2 つの食い違いだけ**で、3 つ目 (= submodule 側が進んだこと) は検出できない — `test-shards` job は submodule を checkout しないため、サマリと表を両方据え置けば submodule が先に進んでもすり抜ける。submodule bump の PR で表を足すのは人の仕事。

### `TestDivergenceDoc_StreamChannelsMatchRegistry`

§4-1 のチャンネル一覧 == `router.go` の `streamRegistry.Register*` の登録名。

**チャンネル名はソースのファイル名と違う。** upstream のソースは `chat-room.ts` だが
wire 上の名前 (`chName`) は `chatRoom`。#2640 の初稿はファイル名をそのまま
「チャンネル名も upstream に揃えてある」として並べており、**18 件中 11 件が実在
しない名前**だった。人が目で照合すると通る類の誤り。

doc 側は §4-1 の表 (mk-go 独自) と ```text フェンス (upstream 由来) の和を母集団に、
実装側は `internal/server/` 非テスト `.go` 全体の `streamRegistry.Register*` を見る。
呼び出しの総数と抽出できた数が一致することも見る (名前が定数経由だったり英字以外を
含むと registry から丸ごと消えるため)。

**固定できるのは「doc の一覧 == mk-go の登録」だけ。** §4-1 のもう半分の主張
「upstream は 18」「名前も upstream に揃えてある」は検証していない — `test-shards`
job は submodule を checkout しないため。doc と実装を同時に間違った名前へ変えれば
この gate は通る。upstream が 19 個目を足した / 名前を変えた場合も検出できないので、
**submodule bump の PR で人が見る**。

### `TestAPICompatDoc_MatchesRouter`

`docs/api-compat.md` の endpoint 行 == `router.go` が静的登録する `/api/*`。POST と GET の
両方を見る (「endpoint を足す操作は必ず POST を伴う」は成り立たない —
`/api/v1/instance/peers` は upstream 側も `get()` 直登録で mk-go も `api.GET` 一本)。

**錨そのものが腐ると、それを見る gate も一緒に無力化する。** 上の
`EndpointCountMatchesAPICompat` は divergence.md と api-compat.md の一致しか見ないので、
endpoint を足して**どちらも更新しない**と両方が古いまま緑になる (develop では
`mk-go version: 1.1.2` / `mk-go only: 49` のまま腐っていた)。

route dump には stack が要るのでテストからは呼べない。代わりに router.go の
`api.POST(` / `api.GET(` / `api.Match(chartMethods, ` を静的に抽出して突き合わせる。
同梱プラグインのルート (`/api/plugin/*`) は literal で現れないので母集団から外す。
生成物側は「mk-go 側にしかない」「両方に存在する」の 2 セクションだけを見る
(「TS 側に存在するが mk-go で未実装」を混ぜると、upstream が endpoint を足した直後 =
生成物が正しい状態で落ちる)。

**静的抽出は取りこぼすと gate が緩くなる方向に倒れる**ので、fail-closed を 3 段に
している。

1. どちらかの集合が 0 件なら `t.Fatal` (書式が変わって空集合同士が一致するのを防ぐ)
2. 抽出できた数 == 呼び出しの総数 (path が定数 / 変数経由、複数行に分かれている形を検出)
3. `/api` 配下を生やす別経路を想定件数で固定 — `api.Group(` は 1
   (`plugin_wiring.go` がプラグイン用に使う)、`s.echo.Group("/api"` と catchall の
   `api.Any(` は各 1、`api.Add(` / `api.PUT|DELETE|PATCH|HEAD|CONNECT|TRACE(` /
   `s.echo.<METHOD>("/api` は 0

**0 件チェックだけでは足りない** — 部分的な取りこぼしは router 側の集合が小さく
なるだけで、生成物との差が出ずに黙って一致する。走査は `internal/server/` の
非テスト `.go` 全体で、router.go だけを見ると別ファイルからの登録を取りこぼす。

3 は「正当な理由でその形を使うことになったら、抽出をそちらへ拡張してから固定を
解除する」という運用。`Static` / `File` / `RouteNotFound` も対象に含める
(`echo.File` はそのパスに GET を 1 本生やす)。

**塞ぎきれてはいない** — `*echo.Group` を helper に渡してその中で登録する形
(`func(g *echo.Group){ g.POST(…) }(api)`) は静的には追えない。

### `TestDivergenceDoc_TableCountMatchesSchema` / `TestDivergenceDoc_ColumnCountMatchesSchema`

§2-1 / §2-2 の見出しの件数を**実 schema と突き合わせる**。母集団の作り方は 2 つで違う。

- §2-1 は `parseMigrations` が返す `CREATE TABLE` 由来のテーブル名から、golden にあるものを引いたもの。`__chart__*` / `__chart_day__*` は upstream でも定義位置が違うだけ (`models/` ではなく `core/chart/charts/entities/`) なので、prefix で明示的に除く
- §2-2 は **`CREATE TABLE` 本体の列を起点に、全 `ALTER` を出現順に適用した後の列**と golden の差分。`DROP COLUMN` した列は入らない。**`CREATE TABLE` 由来を除くと成立しない** — 「未使用の残存 3」(`app.createdAt` / `auth_session.createdAt` / `clip.notesCount`) はいずれも `ALTER` ではなく `CREATE TABLE` の中でだけ定義されている列で、これがそのまま `TestSchemaDrift_CreateOnlyColumns` の allowlist と対応する

`ALTER TABLE` は**文単位で切ってから句を拾う**。`ADD COLUMN` を 1 文 1 句として数える正規表現だと複数句形式を取りこぼし、**独自カラムを足しても gate が落ちない**方向に倒れる。実測では `ADD COLUMN` 102 句のうち 63 句 (62%) が複数句の文 (11 文) にあり、1 文 1 句として数えると 52 句 (51%) を落としていた。

**コメント・文字列リテラル・ドル引用符の中身は解析前に空白へ潰す。** 文の終端を素の `;` で決めているので、`CREATE TABLE` の本体に `);` を含むコメントや `DEFAULT 'f(x);'` が 1 つあるだけで body がそこで切れ、そのテーブルの列が母集団から丸ごと消える (= gate が黙って通る)。`ALTER` 側も同じで、途中で文が切れると 2 句目以降を落とす。**読み飛ばすのではなく空白化する**のが要点で、読み飛ばすとその範囲のコメントが素通りして幻の DDL として拾われる (`DO $$` の中に例示として `ALTER TABLE ... ADD COLUMN` を書くと、drop-in gate がそれを「ALTER で足してあるから安全」と誤認する)。

既知の制約。**いずれも現行 migration に該当が 0 件**だが、踏むと母集団が過小になる (= gate が落ちない方向) ので、migration を書くときは避けること。

- `RENAME COLUMN` を追跡しない
- **`DO $$ ... $$` の中の DDL は見えない。** 中身を空白化しているため (幻の DDL を拾わないための選択で、実 DDL も一緒に見えなくなる)。`TestMigrationIdempotency_RequiresIfExists` も `DO` を含むファイルを丸ごと skip するので、**この形はどの gate にも捕まらない**。CLAUDE.md が enum 作成で `DO $$ ... EXCEPTION` を求めている都合上 `DO` 自体は日常的に書かれるので、その中で列を足さないこと
- ネストしたブロックコメント `/* /* */ */` を 1 段しか閉じない
- `COLUMN` を省いた `ADD "x"` と、引用符なしの識別子を拾わない
- 同一ファイル内で `ALTER` が `CREATE TABLE` より前にある場合は `CREATE` を先に処理する (現行の 2 ファイルは対象テーブルが重ならない)

件数だけでなく**行の存在**も見る。§2-2 は table と column の両方で照合する — 列名だけで探すと、`createdAt` のように複数テーブルにある名前は他の行に残っているせいで行が丸ごと消えても素通りする。

**`golden_upstream_columns.json` を撮り直すとこの gate が動く。** upstream が列を DROP するとその列が「mk-go 独自」に転じて §2-2 の件数が増えるので、submodule bump の PR で落ちる (`note_favorite.createdAt` がまさにその経緯で独自列になっている)。落ちたら doc の件数と行を実態に合わせること。

## Index naming gate / migration idempotency gate（drop-in で二重化・停止しない）

drop-in では mk-go の migration が **Misskey TS の作った既存 DB** にも流れる(`docs/migration-from-ts.md`)。upstream が既に作った構造と衝突しないよう、2 つの静的 gate を置いている。

### `TestMigrationIdempotency_RequiresIfExists`

migration の DDL に `IF NOT EXISTS` / `IF EXISTS` が付いていることを強制する。条件なしの DDL は upstream が作った列 / テーブル / index と衝突して

```
column "category" of relation "avatar_decoration" already exists
```

で migration が dirty 停止し、**drop-in 手順そのものが完走しない**。実際 `000048` が upstream 2026.5.0 の `AddCategoryToAvatarDecorations` と衝突して、2026.5.0 以降の TS 製 DB からの drop-in が不可能な状態だった(#2246)。新規 DB でしか試さないと絶対に踏まない。

### `TestIndexNaming_NoNewUpstreamDuplicates`

mk-go が upstream と同内容の index を**別名で**追加するのを防ぐ。

mk-go は `IDX_<table>_<col>`、upstream (TypeORM) は `IDX_e5848eac4940934e23dbc17581` のような hash 名を使う。`CREATE INDEX IF NOT EXISTS` は **index 名**でしか存在判定しないので、定義が同一でも名前が違えば新規作成され、TS 製 DB では index が二重化する。

実測 (Misskey TS 2026.7.0 が作った DB に mk-go の全 migration を適用):

| | index 数 |
|---|---|
| TS のみ | 442 |
| mk-go migration 適用後 | 639 (+197) |
| `000068` 適用後 | 474 (165 本を削除、upstream 由来は 0 本削除) |

`note` は最大テーブルなので GIN index の二重化は INSERT / UPDATE のたびに 2 本分の更新コストがかかる。読み取り性能には効かないが書き込みスループットと容量に効く。

**新しく index を足すとき、upstream に同じ (table, unique, method, columns) の index があるなら upstream の index 名をそのまま使うこと。** `000058_channel_muting_expires_at.up.sql` が前例。そうすれば `IF NOT EXISTS` が効いて二重化しない。

既存の 167 本は `testdata/known_duplicate_indexes.json` に記録し、`000068` が実行時に落とす。意図的に定義を変えている場合 (partial 化等) も同ファイルに追加する。

### golden の再生成

index の golden は**実 DB から**採る (TypeORM の decorator からは正規形を再現できないため)。upstream を bump したら以下で撮り直す:

```bash
# 任意の隔離 stack で misskey/misskey:<version> を clean DB に対して起動し、
# migration が完走したあとに実行する
psql -t -A -F'|' -c "SELECT tablename, indexname, indexdef FROM pg_indexes \
  WHERE schemaname='public' ORDER BY tablename, indexname;"
```

出力を `internal/entitycompat/testdata/golden_upstream_indexes.json` の形式
(`{table, name, def}`、`def` は `CREATE (UNIQUE )?INDEX <name> ON ` を `UNIQUE|` / `|` に潰したもの) に変換して commit する。
