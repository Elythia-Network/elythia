# プラグイン — 作者向け

## 最初に知っておくこと

**プラグインはサーバーと同じ権限で動く。** サンドボックスは無い。運営者はあなたを信頼して組み込む。

これは自由であると同時に責任でもある。外部への通信、ファイルの読み書き、実行時間、どれも mk-go は制限しない。

## 作る

```
plugins/myplugin/
├── mk-plugin.yml       これがあるものだけがプラグインとして扱われる
├── go.mod              **必須**
├── plugin.go
└── frontend/           UI が要る場合だけ
    └── index.ts
```

### `mk-plugin.yml`

```yaml
name: myplugin
apiVersion: 1
```

`apiVersion` はコンパイル前に検査される。合わないとビルドが止まり、理由が出る。

`disabled: true` を書くとビルドから除外される（詳細は[運営者向け](operating.md)）。この判定は `apiVersion` の検査より先に行われるので、ビルドが通らない状態のプラグインでも無効化はできる。

### `go.mod`

```
module github.com/you/mk-plugin-myplugin

go 1.27.1

require github.com/elythia-network/elythia v0.0.0

replace github.com/elythia-network/elythia => ../..
```

**`replace` は要る。** `make build` は生成される `go.work` で解決できるが、**`make plugin-test` は `GOWORK=off` で回す**ので、これが無いと `missing go.sum entry` で落ちる。同梱プラグインは全部この形。

**独立した Go module である必要がある。** これは形式ではなく、Go の internal ルールにより「mk-go の内部パッケージを import できない」ことを保証する仕組み。`go.mod` を持たないディレクトリはビルド時にエラーになる。

依存を足すときは自分の module の中で `go get` する。**リポジトリのルートで `go mod tidy` を走らせないこと** — 生成物 `cmd/elythia/plugins_generated.go` が各プラグインのモジュール (上の例では `github.com/you/mk-plugin-myplugin`) を import しており、private repo だと解決に失敗する。

### `plugin.go`

`Plugin` という名前のパッケージ変数を公開する。生成される登録コードがこれを参照する。

```go
package myplugin

import "github.com/elythia-network/elythia/plugin"

var Plugin = plugin.Definition{
	Name:       "myplugin",
	Version:    "1.0.0",
	APIVersion: plugin.APIVersion,
	Migrations: migrations,
	Routes:     routes,
	Jobs:       jobs,
}
```

名前は URL path / queue の task type / PostgreSQL schema の 3 箇所で使われるので、**小文字英数字とハイフン**のみ。

## 開発しながら動かす

```bash
make plugin-dev PLUGIN=plugins/myplugin
```

ソースを監視して 生成 → ビルド → 再起動 を繰り返す。frontend の HMR を使うには
Vite dev server も立てる。詳細は[開発環境](development.md)。

## 動く実例

[`plugins/status/`](../../plugins/status/) が公開面のほとんどを使う。外部サービスに依存しないので、組み込んで動かしながら読める。

以下は要点だけを抜き出したもの。

## ルート

```go
func routes(ctx plugin.Context, r plugin.Router) error {
	r.POST("/me", func(req plugin.Request) (any, error) {
		me := req.UserID()          // 未認証なら空文字
		if me == "" {
			return nil, plugin.Errorf(http.StatusUnauthorized, "ログインが必要です")
		}
		return map[string]any{"ok": true}, nil
	})
	return nil
}
```

`/api/plugin/<name>/` の下に生える。戻り値は JSON エンコードされ、`nil` なら 204。

**フロントから呼ぶものは POST にする。** `host.api`（= `misskeyApi`）は POST 固定で、Misskey 本体の API も POST 基本。`GET` は `<img>` から読ませる場合などに使う。

### エラー

```go
return nil, plugin.Errorf(http.StatusBadRequest, "%d 文字以内にしてください", c.MaxLength)
return nil, plugin.ErrNotFound("見つかりません")
```

**素の `error` を返すとメッセージは外に出ない**（ログに残り、利用者には汎用メッセージが返る）。プラグインの内部事情が利用者に見えるのを防ぐため。利用者が直せるものだけ `StatusError` で返す。

clientが分岐する安定codeも必要なら`plugin.NewCodedStatusError(status, message, code)`を返す。codeは英大文字で始まり、英大文字・数字・underscoreだけを使う。空codeまたは書式不正codeはcodeなしの`StatusError`へ降格し、statusとmessageは維持する。errorをwrapした場合はerror treeの外側から最初の`StatusError`またはcoded errorを返すため、外側で丸めたstatus/messageを内側のcodeが上書きしない。

### バイナリ応答

```go
return plugin.Blob{
	ContentType:  "image/png",
	Body:         data,
	CacheControl: "public, max-age=86400",
}, nil
```

主な用途は画像のプロキシ。本体の CSP は `img-src` を `'self' data: blob:` + 固定 2 host (upstream のクレジットページ用、#2892) に絞っているので、**プラグインが指す外部の画像は `<img>` で直接読めない**。同一オリジンで配信すれば CSP を緩めずに済む。

mk-go は `filesHandler` と同じ 3 点を必ず付ける。

- **`Content-Type` を allowlist に通す。** image / audio / video 以外は `application/octet-stream` に矯正される（upstream `FileServerUtils.getSafeContentType` と同じ規則）。`text/html` や `image/svg+xml` をそのまま流すと**同一オリジンの XSS** になり、Misskey のフロントは `account` を localStorage に置くのでアカウント乗っ取りと同じになるため。`nosniff` はブラウザの MIME 推測を止めるだけで、Content-Type が**実際に** `text/html` のときには何も止めない
- `Content-Security-Policy: default-src 'none'; img-src 'self'; media-src 'self'; style-src 'unsafe-inline'`
- `X-Content-Type-Options: nosniff` と `Content-Disposition: inline`

JSON を返したいときは `Blob` ではなく素の値を返す（本体が JSON 化する）。`text/plain` 等は octet-stream に落ちるので、ブラウザでは表示ではなくダウンロードになる。

それでも**取得元の `Content-Type` をそのまま流さず、扱う型を決めて検証すること。** 読み込みの上限（`io.LimitReader` など）もプラグイン側の責務。

## ストレージ

自分専用の PostgreSQL schema が渡される。

```go
var migrations = []plugin.Migration{
	{Version: 1, SQL: `CREATE TABLE items (id serial PRIMARY KEY)`},
}
```

**`Definition.Migrations` で宣言する。** `Routes` の中で `Storage().Migrate()` を呼ぶと、ロールを分割したとき（`MK_ONLY_SERVER` / `MK_ONLY_QUEUE`）片方でしか走らず、queue 側がテーブルの無い schema でジョブを回すことになる。

```go
db := ctx.Storage().DB()   // *sql.DB (search_path は自分の schema に固定)
if _, err := db.ExecContext(req.Context(), `INSERT INTO items DEFAULT VALUES`); err != nil {
    return nil, err
}
```

GORM を使いたければプラグイン側で包む（`postgres.New(postgres.Config{Conn: db})`）。mk-go が GORM を使っているのは内部の選択であって契約ではない。

### 守ること

- **mk-go 本体のテーブルに触れない。** ノートの可視性判定はアプリケーション側にあり DB には無いので、`SELECT * FROM note` は非公開ノートを含む。本体のデータは API 経由で取る
- **冪等に書く。** ストレージへの書き込みと API 呼び出しは同じトランザクションに入れられない
- 一度適用した version の SQL を書き換えても再実行されない。変更は新しい version で

## 本体の API を呼ぶ

```go
// 匿名で呼ぶ
raw, err := ctx.API().Anonymous().Call(req.Context(), "users/show", map[string]any{"userId": id})
if err != nil {
    return nil, err
}

// その利用者として呼ぶ
if _, err := ctx.API().AsUser(userID).Call(req.Context(), "notes/create", params); err != nil {
    return nil, err
}
return raw, nil
```

mk-go の既存エンドポイントをプロセス内で呼ぶ。**可視性・権限・レート制限が自動的に効く**ので、モデレーション状態などを自前で持たなくてよい。

**`Call` の第 1 引数は `context.Context`。** `plugin.Context` は満たさないので `ctx` を渡すとコンパイルできない。ルートの中なら `req.Context()`、ジョブの中なら受け取った `context.Context` を渡す。

`AsUser` はその利用者として振る舞う（レート制限もその利用者のものが適用される）。「すべてを迂回する」経路は用意していない。管理操作が必要なら管理者の ID を渡す。

**`AsUser` が載せるのはその利用者の native token で、OAuth の scope は付かない。** つまり `AsUser(req.UserID())` は「呼び出し元が持っていた権限」ではなく「その利用者が持つ全権」で呼ぶ。だから mk-go は**プラグインのルートに第三者アプリのアクセストークンを入れない**（本体が 403 `PERMISSION_DENIED` を返す）。ネイティブのログイントークン（同梱フロントエンドや公式アプリ）と未認証だけが到達する。

upstream が `kind` を宣言しない資格情報必須エンドポイントで app token を一律拒否するのと同じ規則で、プラグインのルートには `kind` にあたる宣言が無いためこちら側に倒してある。第三者アプリから叩かせたい処理は、プラグインのルートではなく本体のエンドポイント（scope が宣言されている）に置くこと。

非 2xx は `*plugin.APIError` になる。本文が入っているので Misskey のエラーコードで分岐できる。

### ロールの付与を確認する

利用者に特定のロールが**手動で付与されているか**を、メンバー一覧を走査せずに 1 回で確認できる。条件つきロールは対象外（後述）。

返ってくるのはこの形。

```json
{ "assigned": true, "expiresAt": "2026-08-20T03:04:05.000Z",
  "role": { "id": "...", "target": "manual", "isPublic": true, "canEditMembersByModerator": false } }
```

**`assigned` が答えるのは手動付与だけで、`target` が `conditional` のロールでは常に `false` になる。** 条件つきロールは付与のレコードを持たず、条件式を読み取り時に評価して決まるため。**`target` を必ず見ること** — `conditional` なら、返ってきた `false` は「条件を満たしていない」ではなく「この API では分からない」という意味になる。

```go
// AsUser に渡した利用者自身について答える。
// 任意の利用者を見るなら "admin/roles/assignment-show" に
// {"userId": ..., "roleId": ...} を渡す (AsUser はモデレーター以上の ID)。
raw, err := ctx.API().AsUser(userID).Call(req.Context(), "roles/assignment-show", map[string]any{
    "roleId": roleID,
})
if err != nil {
    return nil, err
}

var res struct {
    Assigned bool `json:"assigned"`
    Role     struct {
        Target string `json:"target"`
    } `json:"role"`
}
if err := json.Unmarshal(raw, &res); err != nil {
    return nil, err
}
if res.Role.Target == "conditional" {
    // assigned は当てにならない。この endpoint では判定できない
    return nil, nil
}
return res.Assigned, nil
```

条件つきロールまで含めた実効的な判定は用意していない。条件式の評価は利用者ごとに全ロールを読み込む必要があり、この API の利点である「1 回引くだけで終わる」性質が失われるため。

非公開ロールで `roles/assignment-show` を使うと、**付与されていない利用者には `NO_SUCH_ROLE`** が返る（存在の有無を漏らさないため）。条件つきかつ非公開のロールは、条件を満たす利用者にもこうなる。`admin/roles/assignment-show` にこの秘匿は無く、モデレーター以上なら非公開ロールも読める。

## ジョブ

```go
func jobs(ctx plugin.Context, j plugin.Jobs) error {
	j.Handle("prune", func(c context.Context, payload json.RawMessage) error {
		return nil
	})
	j.Schedule("0 * * * *", "prune", nil)
	return nil
}
```

task type は `plugin:<name>:<job>` として名前空間が付き、**プラグインごとの専用キュー
`plugin:<name>`** で動く。遅い処理が連合の配送を止めることはないし、他のプラグインの
巻き添えにもならない。admin/queue から per-queue に一時停止・再開もできる (名前が動的なのでヘッダのタブには出ない。概要タブのカードから辿る)。

**`Schedule` を消したり名前を変えたりすると、次の起動で旧いスケジュールは撤去される**
(#3173)。起動時の登録が全部終わった後、登録されなかったスケジュールを消す。ただし
既に積まれている次回分は消えないので、**最大 1 回は旧い名前で発火する** (handler を
消していればその 1 回は失敗する)。完了・失敗の記録は 7 日で刈られる (#3171)。

- **撤去されるのはプラグインが有効なままのときだけ。** 無効化・ビルドから外したプラグインの
  キューは走査しないので、そのスケジュールは残る (worker が居ないので発火もしない)。
- **登録が 1 件でも失敗した起動では撤去しない** (一時的な失敗で正当なスケジュールを消さない)。
- **queue ノードが複数あるときは、ビルドとプラグインの設定を揃えること。** 各ノードは自分が
  登録しなかったものを消すので、`Schedule` を条件付きで呼ぶプラグイン (設定が無ければ
  登録しない、など) を一部のノードでだけ未設定にすると、他のノードの登録を消す。

### 任意のタイミングで積む

`ctx.Queue()` から積める。**`Routes` からも `Jobs` からも呼べる**ので、HTTP ハンドラの
中で重い処理を後回しにできる。

```go
func routes(ctx plugin.Context, r plugin.Router) error {
	r.POST("/refresh", func(req plugin.Request) (any, error) {
		err := ctx.Queue().Enqueue(req.Context(), "prune", map[string]string{"uid": req.UserID()},
			plugin.WithDedup(5*time.Minute),  // 同じ中身の二重起動を抑える
			plugin.WithDelay(10*time.Second), // すぐには走らせない
			plugin.WithMaxAttempts(3),        // 初回を含む回数
		)
		return nil, err
	})
	return nil
}
```

| | |
|---|---|
| 名前 | `Jobs.Handle` に登録したものと同じもの。登録が無い名前で積むと処理者なしとして失敗する (再試行はしない) |
| `Definition.Jobs` | **宣言していないと積めない** (エラーになる)。専用キューを作らないので、積めても誰も処理しないため |
| 再試行 | **既定は無し。** 冪等かどうかはプラグインしか知らないので、`WithMaxAttempts` で明示する。頼むと指数バックオフ (10 秒起点) が自動で付く — 付けないと落ちている取得先を遅延 0 で連打する |
| 重複排除 | `WithDedup` で抑制されたときも `Enqueue` は `nil` を返す。**積めたかどうかは区別できない** |
| ロール | 積むのはどのプロセスからでもできる。処理するのは queue ロールのプロセスだけ |

worker の起動・停止時の待ち合わせ・実行時間の上限は mk-go が持つ。**プラグインが自分で
worker を起こす経路は用意しない** — プラグインの数だけ同じバグを書くことになる。

**ただし 1 回の実行に上限がある。** mk-go は job の handler を既定 1 時間で
打ち切る (#2658、`queueHandlerDeadlineSeconds`)。超えると job は失敗扱いになり、
cron と `WithMaxAttempts` を付けていない enqueue はそこで捨てられる。**打ち切られても
handler 自体は止まらない** (Go では goroutine を殺せない) ので、DB 接続を
掴んだまま走り続ける — 再試行を頼んでいると、**止まらない実行が重なる**ことになる。

1 時間を超えうる処理は**分割して複数回に分ける**こと。この上限は全キュー共通の
設定なので、プラグインのために延ばすと inbox / deliver の保護も一緒に緩む。
`ctx` は必ず尊重すること — 尊重していれば打ち切り時に正常終了でき、goroutine が
残らない。

キューに載せるほどでもない、プロセス内で完結する非同期処理は `ctx.Go()` を使う（recover 付き）。**再起動をまたがない**ので、跨いでほしい処理はキューへ。

起動中に呼んだ`ctx.Go()`は、全pluginのstorage、migration、`EffectivePolicies`、`Routes`、`Jobs`とhost側のroute配線が成功するまで開始されない。起動に失敗した場合は保留した処理を破棄する。起動成功後の呼び出しは直ちに開始する。cancelやdrainは提供しないため、正常終了時の停止が必要な処理はplugin側で終了条件を持つこと。

> **プラグインが素の `go` を書くとプロセスごと落ちる。** Go は他 goroutine の panic を回収できず、mk-go 側の recover では止められない。

## 効果ポリシー

`Definition.EffectivePolicies`で、native policyの解決に参加するproviderを宣言できる。有効なpluginは、全pluginのstorage/migration/provider登録が成功した後にだけ`Routes`と`Jobs`を有効化する。

権限に関わる判定は、既存roleで表現できるなら`admin/roles/assign`などでnative roleとして永続化する方を優先する。providerの寄与はplugin停止・buildからの除外・Misskey TSへの切り戻しで即座に消えるため、停止後も維持すべき昇格・制限には向かない。`EffectivePolicies`はlevelに応じた連続値など、role付与だけでは表現できない解決時の寄与に限定する。`plugins/trustlevel`が引き続きrole付与を使うのは、切り戻し後も確定済みの昇格を残すためである。

```go
func effectivePolicies(ctx plugin.Context, inv plugin.EffectivePolicyInvalidator) (plugin.EffectivePolicyRegistration, error) {
	return plugin.EffectivePolicyRegistration{
		Keys: []string{"driveCapacityMb"},
		Resolve: func(c context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			return []plugin.EffectivePolicyContribution{
				{Key: "driveCapacityMb", Priority: 1, Value: 1000},
			}, nil
		},
	}, nil
}
```

`Keys`は空・空文字・重複を許さず、hostが持つnative policy keyだけを宣言できる。`Resolve`は必須。resolverは`req.UserID`と、activeなnative role IDをソート・重複除去した`req.RoleIDs`を受け取る。匿名解決では`UserID`が空文字で、`RoleIDs`はnilではない空sliceになる。入力sliceはproviderごとに複製されるが、resolver側でも変更しないこと。

resolverは`req.UserID`のほかに、activeな手動ロールのassignmentを`req.ActiveAssignments`で受け取る。`RoleIDs`は従来どおりconditionalロールを含むが、`ActiveAssignments`は`role_assignment`の行を持つ手動ロールだけなので、`ActiveAssignments`の各`RoleID`は必ず`RoleIDs`に含まれる。ロールごとに高々1件で、並びは`RoleID`順、匿名解決では非nilの空sliceになる。期限切れ・削除済み・`role`行が無いorphanは含まれない。rolesとassignmentはhostの同じ1回の読取から同時に作られるので、両者の間に食い違いの窓は無い。`RoleIDs`は減っていないので、`ActiveAssignments`を読まないproviderの挙動は変わらない。

`AssignmentID`は**不透明なID**として扱う — 内容は解釈せず、plugin storageの行と対応させるだけにする。unassign → re-assignでは別IDになるので、assignmentに紐づく状態を復活させられない。

resolverの実行token取得待ちとresolver実行の期限はそれぞれ1秒。tokenを期限内に取得できないrequestはnative fallbackへ戻るが、providerは無効化されない。token取得後にresolver専用の新しい1秒deadlineが始まり、この実行期限を超えたproviderだけがprocess再起動まで無効化される。resolverへ渡されたcontextをStorage I/Oにも必ず渡すこと。contextを無視する処理はhostから強制終了できないが、hostは同じproviderの実行をcapacity 1に制限するため、timeout後に残留するresolver goroutineはproviderごと最大1本になる。

resolverから本体のpolicy解決を呼び戻してはならない。同じ入力では自分のin-flight結果を待ち、異なる入力では自分が保持しているcapacity 1 tokenを待つため、いずれも外側のresolver deadlineを使い切ってproviderがprocess再起動まで無効化される。

contributionの`Priority`は`0..2`で、大きいpriorityのgroupだけをnative roleと同じ規則で集約する。同じprovider内での重複判定はcontributionの種別で違う — **追加contribution（`ReplaceRoleID`なし）は`Key`と`Order`の組**で一意で、`Order`は同一`Key`の複数寄与の並びを決める。**置換contribution（`ReplaceRoleID`あり）は`Order`を0に固定したうえで`Key`と`ReplaceRoleID`の組**で一意になる（後述）— 置換は`Order`を選べないので、同じ`Key`の別ロール置換を`Order`で区別すると必ず衝突する。`UseDefault: true`では`Value`を無視し、そのkeyのnative defaultを同じpriorityへ参加させる。

`ReplaceRoleID`にロールIDを入れると、そのcontributionは「追加」ではなく**置換**になる — 指定したactive manualロールが`Key`にネイティブに持つcontributionだけを、1対1で差し替える。追加だけだとboolのORや数値のmaxでネイティブの値が生き残ってしまうので、計算した値を対象ロールの値として出したい用途には置換が必要になる。

**置換の契約**（`ReplaceRoleID`が空でない場合）:

- 対象は`req.ActiveAssignments`に現れるロールだけ。conditionalロールは`role_assignment`の行を持たないため置換対象にできない
- 置換後のentryは**元のネイティブentryの`priority`を引き継ぐ**。自分で選ぶと二重定義になるので`Priority`と`Order`はどちらも`0`のまま書く
- 置換後も、他のロールのcontribution・providerの通常contribution・instance / server capと同じpriority cascadeと型集約を行う
- `explicit`も`UseDefault`に従う。`UseDefault: true`の置換は「このロールのoverrideをベース値に戻す」= ネイティブの`useDefault`と同じ扱い
- administrator / moderator判定は対象外のまま

```go
func effectivePolicies(ctx plugin.Context, inv plugin.EffectivePolicyInvalidator) (plugin.EffectivePolicyRegistration, error) {
	return plugin.EffectivePolicyRegistration{
		Keys: []string{"mentionLimit"},
		Resolve: func(c context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			out := []plugin.EffectivePolicyContribution{}
			for _, a := range req.ActiveAssignments {
				// ロールごとに1件の置換を出す。Priority / Order は 0 のまま渡す =
				// 元のネイティブentryのpriorityを引き継ぐ。
				out = append(out, plugin.EffectivePolicyContribution{
					Key:           "mentionLimit",
					Value:         40,
					ReplaceRoleID: a.RoleID,
				})
			}
			return out, nil
		},
	}, nil
}
```

値はnative keyの型に一致させる。boolはOR、integer-native policyは最大値、`chatAvailability`は`available`、`readonly`、`unavailable`の順で寛容な値、`uploadableFileTypes`はtrim後のset unionを使う。`optOutNotificationTypes`は**set intersectionを使う** — 「受け取らない」一覧なのでunionにすると複数ロールに属するほど通知が減り、他のpolicyが緩い方へ倒れるのと向きが食い違うため (#2898)。型不一致の候補は集約から除外し、有効な候補が1件も無ければnative defaultへ戻す。integer-native policyは`int`、host `int`範囲内の`int64`、または有限かつhost `int`範囲内の`float64`を受理する。`float64`の小数部は拒否・切り捨てず、結果のpolicy mapでも小数として維持する。typed integerは`2^53`を超えても`float64`へ変換せず比較する。

受理されたinteger-native policyはpolicy map内ではhost `int`の精度を維持する。consumerが分・MBなどを`time.Duration`、byte数、件数などの固定幅表現へ変換するときにだけ、consumer固有の境界処理を行う。容量・件数などは表現可能範囲へ飽和し、rate limitの最小間隔が正方向overflowする場合は実質的な無期限拒否を避けるため元の間隔へ戻す。大きな正数がwrapして負数・無制限扱いになることはなく、通常範囲の値・単位・instance/server capの優先順位は変わらない。

未宣言key、unknown key、priority範囲外、order重複、型不一致、NaN、infinity、範囲外整数、enum外の値、空または非文字列の配列要素が1件でもあればprovider全体を失敗として扱う。resolverのerrorやpanicも同様。失敗providerが宣言したkeyはnative結果へ戻し、同じkeyに対する他providerの貢献も破棄する。宣言していないkeyには成功providerの貢献を適用し続ける。成功値はproviderごとのLRUへ保存し、evictionまたはinvalidation後のresolver失敗はcacheせずnativeへ戻す。fallbackはproviderごとの累積回数が1、2、4、8...回になった時だけ匿名warningとして記録し、恒常障害でrequestごとにlogを増やさない。診断errorとwarningはplugin名、user/role/policy ID、provider output、panic値を含まない。

**置換が複数providerから重なったときはmalformedではない。** 上に挙げた失敗条件は「provider自身の出力が壊れている」場合で、置換の競合はそうではない。同じ`Key`と`ReplaceRoleID`を複数providerが置換した場合は**競合**になる。hostはそのpairを置換として受け入れず、ネイティブcontributionへ戻す — どちらの値も採らないので、管理者が設定したロールの値が残る。provider全体は失敗扱いにしないので、**ネイティブへ戻るのはそのpairだけで、他のkeyの追加contributionも他のロールの置換も通常どおり効く**。checked解決はこの結果を競合を表す固定errorと伴って返し、unchecked解決は同じ結果を返して競合errorだけを捨てる。provider失敗と併発した場合は両方のerrorが`errors.Is`で辿れる。provider失敗は宣言keyをネイティブへ戻すので、他のproviderの置換も同じkeyなら巻き戻る。

instance/server capはplugin集約の後に適用する。`maxFileSizeMb`、`chunkedUploadMaxConcurrentSessions`、`chunkedUploadMaxPendingMb`へ`0`以下の無制限値を返しても、positiveなcapが設定されていればcap値になる。

`Resolve`は、明示的なinvalidationの間は`UserID`、sorted active `RoleIDs`、`ActiveAssignments`だけで結果が決まる純粋関数として実装する。時刻、request固有情報、未通知の外部状態へ依存してはならない。**同じ`RoleIDs`でも`ActiveAssignments`が違えば結果が違ってもよい**ので、hostはproviderごとの成功結果cache keyにassignment IDを含める（付け外し / 再割り当ての直後に前の結果を返さないため）。cacheはoperator設定`effectivePolicyProviderCacheEntries`（既定10000件、providerごと）のLRUであり、eviction時は同じ入力を再解決する。

plugin独自の書き込みでpolicy入力が変わった場合は、永続化のcommit成功後にだけ`inv.InvalidateUser`または`inv.InvalidateRole`を呼ぶ。失敗結果はcacheしない。in-flightの古いnative role/provider結果はinvalidation後のrequestへ返さず、cacheにも戻さない。conditional roleの対象userはassignment rowから列挙できないため、role invalidationは全userのrole/policy cacheを保守的に破棄する。匿名（空`UserID`）にはper-user invalidationが無いため、匿名結果に影響する状態変更では`InvalidateRole`を使う。

## 設定

```go
type config struct {
	MaxLength int `json:"maxLength"`
}

c := config{MaxLength: 30}          // 既定値を入れてから渡す
if err := ctx.Config().Unmarshal(&c); err != nil {
	return err
}
```

設定が書かれていなければ `v` は変更されないので、既定値がそのまま残る。

**キーの大文字小文字に注意。** 読み込みに使っている Viper はキーを小文字化する（`apiKey` → `apikey`）。構造体のフィールドは `encoding/json` が大文字小文字を無視して照合するので camelCase のタグで問題ないが、**map で受ける設定はキーの大小が復元されない**。

## 外へ HTTP を出す

**自分で `&http.Client{}` を作らないこと。** `ctx.HTTP()` が返す client を使う。

```go
hreq, err := http.NewRequestWithContext(req.Context(), http.MethodGet, "https://example.com/api", nil)
if err != nil {
	return err
}
res, err := ctx.HTTP().Do(hreq)
if err != nil {
	return err
}
defer res.Body.Close() //nolint:errcheck // 読み捨て
```

この client は次を既に持っている。

| | |
|---|---|
| SSRF ガード | プライベート IP / 非 http(s) への接続を落とす |
| 運営者の設定 | `proxy` / `outgoingAddress` / `outgoingAddressFamily` |
| timeout | 1 リクエスト 30 秒 |

素の client を使うと、運営者が proxy を設定していても**そのプラグインだけがサーバーの素の IP で外へ出る**。リモートに「このインスタンスの所在」を渡すことになるので、匿名性を期待している運営者の前提が崩れる。

**`Transport` を差し替えないこと。** 差し替えると上の 2 つが消える。もっと短い / 長い期限が要るなら `http.NewRequestWithContext` で per-request に付ける。

これは**セキュリティ境界ではない** — プラグインは `net/http` を直接使えるので、これは「間違えにくい既定」を配るもの。

テストでは `plugintest.Harness.WithHTTPClient` で差し替える。設定しないと `ctx.HTTP()` は必ず失敗する client を返す (既定を素の client にすると、テストが気付かないうちに本物の外部サービスへ出ていくため)。

## フロントエンド

`frontend/index.ts` を置くと Vite のビルドに取り込まれる。

```ts
import { definePlugin } from '@/plugin-api.js';
import MyCard from './MyCard.vue';

export default definePlugin({
	name: 'myplugin',
	setup(host) {
		host.slot('profile:info', { component: MyCard });
	},
});
```

### スロット

| 名前 | 位置 |
|---|---|
| `profile:info` | ユーザーのプロフィール（`ctx.user` が渡る） |
| `settings:profile` | 設定 > プロフィール |
| `admin:federation` | コントロールパネル > 連合（一覧の手前） |
| `admin:instance-info` | インスタンス情報の概要タブ（`ctx.host` が渡る） |
| `admin:user` | ユーザーのモデレーション画面の概要タブ（`ctx.user` が渡る）。`profile:info` と違い**モデレーターにしか見えない**ので、裁く材料はこちらへ置く |

**位置は意味で定義されている。** upstream がコンポーネント名を変えても壊れない。

**描画されることは権限の保証ではない。** `admin:` で始まるスロットはモデレーター
向けのページにしか無いが、それは UI の都合でしかない。バックエンドは
`Request.IsModerator()` (公開面は[一覧](#公開面の一覧)を参照) で必ず自分で守ること。

管理向けのスロットでは、**権限が無いときに何も描かないこと**も考える。空の枠が
挟まると、本体の UI の邪魔になる。

```ts
// 403 は「モデレーターでない」なので黙って消える。それ以外は理由を出す
// (読み込み失敗と権限不足で対応が変わる)。
try {
    data.value = await api('overview', {});
    hidden.value = false;
} catch (err) {
    if ((err as { message?: string })?.message?.includes('権限')) {
        hidden.value = true;
        return;
    }
    error.value = '読み込めませんでした';
}
```

### 独自ページ・管理画面

**`setup` ではなく `definePlugin` で宣言する。**

```ts
export default definePlugin({
	name: 'myplugin',
	pages: [
		{ path: '/', component: TopPage, navTitle: 'マイプラグイン', navIcon: 'ti ti-puzzle' },
		{ path: '/', component: AdminPage, admin: true },
	],
	setup(host) { ... },
});
```

| | パス |
|---|---|
| 通常 | `/plugin/<name><path>` |
| `admin: true` | `/admin/plugin/<name><path>` |

**名前空間を切るのは意図的**で、本体のパスと混ざると upstream が同名のページを足したときに衝突する。

`navTitle` を指定すると**メニューに項目が出る**。

| | 出る場所 |
|---|---|
| 通常のページ | **「もっと」（ランチパッド）**。利用者が設定でサイドバーに常設できる |
| `admin: true` | コントロールパネルの左メニュー（「プラグイン」の節） |

**省くとルートは生えるがメニューには出ない。** 別の画面から遷移させる場合はそれでよいが、管理画面で省くと URL を直打ちするしかなくなる。

> **既定のサイドバーには入らない。** サイドバーの並びは利用者ごとの設定 (`prefer.r.menu`) で決まり、そこに列挙された項目だけが描画される。プラグインが勝手に常設されると、利用者の持ち物を運営者が占領することになるので、そうしていない。欲しい利用者は「ナビゲーションバーを編集」から追加できる。

> **なぜ宣言なのか。** ルーターは**モジュール読み込み時**に現在の URL を解決する。`setup` で登録すると、その時点では未登録なので**直接 URL を開いたときだけ 404 になる**（画面遷移では動くので気付きにくい）。宣言なら読み込み順に依存しない。

管理画面は `/admin` の配下に入るので**モデレーター以上にしか表示されない**。

> **画面を隠すのは UI の都合でしかない。** バックエンドは別途守ること。

```go
r.POST("/admin/stats", func(req plugin.Request) (any, error) {
	if !req.IsModerator() {
		return nil, plugin.Errorf(http.StatusForbidden, "権限がありません")
	}
	...
})
```

`Request.IsModerator()` / `IsAdministrator()` が使える。**未認証・判定不能なときは false** を返す（判定できないときに通すと、権限の穴が「動いているように見える」形で残る）。

### レート制限は掛からない

本体の per-endpoint テーブルはプラグインのパスを持たないので、**登録したルートには上限が無い**。認証と body の上限（1 MiB）までは本体が見るが、呼び出し回数は見ない。

重い処理（画像の生成、外部への問い合わせ、集計クエリ）を含むなら自分で持つこと。素直なのは結果をキャッシュすることで、`Storage()` に置くか、プロセス内に持つ。

`API().Anonymous()` / `AsUser()` 越しに本体のエンドポイントを呼ぶ分には、本体のレート制限がそのまま効く（`AsUser` ならその利用者の枠）。自前で DB や外部を叩く経路だけが素通しになる。

### 2 つの形式

```ts
host.slot('profile:info', { component: MyCard });        // Vue コンポーネント
host.slot('profile:info', (el, ctx) => { /* ... */ });   // 素の DOM
```

Misskey のコンポーネント（`MkInput` など）を使うなら前者。ホストのアプリ内で描画されるので、テーマも provide/inject も本体と同じものが効く。

### 使えるもの

```ts
import { MkInput, MkButton, MkFolder, MkLoading } from '@/plugin-api.js';
// 2026.10.0-mk.5 から
import { MkSelect, MkSwitch, MkAvatar, MkUserName, MkTime, PageWithHeader, useMkSelect, definePage, getUsers } from '@/plugin-api.js';
```

`getUsers(ids)` は ID の重複を除き、`users/show` を 100 件ずつ呼んで公開ユーザー情報 (`PluginUser`) を返す。見えないユーザー (存在しない・凍結中など) は結果から黙って消えるので、件数は入力以下になり順序も保証しない。形式が不正な ID が混ざると、その 100 件のまとまりごと失敗する。`PluginUser` は misskey-js の `UserLite` そのままで (MkAvatar / MkUserName にそのまま渡すため)、upstream が型を変えると一緒に変わる。

**ここに出ているものだけが「壊さないと約束する範囲」。** プラグインは同じバンドルに入るので技術的には何でも import できるが、それ以外は upstream のリファクタで黙って壊れる。必要なものがあれば mk-go 側に要求すること。

### API 呼び出し

```ts
const res = await host.api<T>('plugin/myplugin/me', { ... });
```

**POST 固定。** 利用者のセッションがそのまま使われる。

## 他のインスタンスとやりとりする

同じプラグインを入れている **mk-go 同士**でだけ通信できる (#2537)。wire の形は
[peer プロトコル](../plugin-peer-protocol.md) にある。

```go
var Plugin = plugin.Definition{
    Name:       "demo",
    Version:    "1.0.0",
    APIVersion: plugin.APIVersion,
    Peered:     true,   // ← 宣言したものだけが使える

    // **登録は Peer の中で。** ロールに関係なく呼ばれる。
    Peer: func(ctx plugin.Context, peer plugin.Peer) error {
        // 相手から届いたとき。from は署名で確定したホスト。
        peer.Handle(func(c context.Context, from string, payload json.RawMessage) (any, error) {
            var req struct{ User string `json:"user"` }
            if err := json.Unmarshal(payload, &req); err != nil {
                return nil, err
            }
            return map[string]any{"score": lookup(req.User)}, nil
        })

        // Send の応答が返ってきたとき。
        peer.OnReply(func(c context.Context, from, id string, reply json.RawMessage) error {
            return save(from, reply)
        })
        return nil
    },

    Routes: func(ctx plugin.Context, r plugin.Router) error {
        r.POST("/ask", func(req plugin.Request) (any, error) {
            id, err := ctx.Peer().Send(req.Context(), "other.example", map[string]any{"user": "alice"})
            if err != nil {
                return nil, err
            }
            return map[string]any{"id": id}, nil
        })
        return nil
    },
}
```

**`Handle` と `OnReply` を `Routes` の中で登録しない (#2819)。** `Routes` は server
ロールでしか走らないのに、送信の POST は queue ロールで走る。ロールを分割した構成
(`MK_ONLY_SERVER` / `MK_ONLY_QUEUE`) では応答が届かなくなる。登録が無いロールでは
起動時に warn が出る。

**ActivityPub には出ない。** AP に載せると不具合の症状が他人のサーバー側に出るうえ、
一度公開した形は後から塞げないため、mk-go 専用の経路に閉じてある。相手が Misskey や
Mastodon でも影響しない代わりに、**相手も同じプラグインを持っていることが前提**になる。

mk-go が面倒を見るもの:

| | |
|---|---|
| 宛先 | ブロック / 連合の許可設定 / 停止 (`suspensionState`) / SSRF / 自分自身への送信を弾く。判定は AP の deliver と同じなので、運営者が相手を停止すれば peer も止まる。ホストは既定ポートを剥がした正規形で比べる (`example:443` は `example` へのブロック指定に当たる) |
| 送信元 | HTTP Signature で確定する。`from` は名乗りではない |
| 大きさ | 要求も応答も `Definition.PeerMaxBody` まで (既定 64 KiB) |
| 再送 | 数回まで自動で試す (`429` / 5xx / 接続エラーのみ。それ以外の 4xx は 1 回で止める)。**キューに載るので再起動をまたぐ** |
| 回数 | 受け口は毎秒 6 / バースト 60 で throttle する (超過は `429`)。**全 peered プラグインで 1 つの表を共有**し、IP 側の枠も同時に消える |
| 相手の確認 | nodeinfo に同じプラグインの宣言が無ければ送らない |

プラグインが面倒を見るもの:

- **payload の中身と値域**。相手は同じプラグインを持っているだけで、善良とは限らない
- **payload の版**。mk-go は中身を解釈しないので、形を変えたときの互換は自分で保つ
- **`OnReply` の冪等性**。キューに載るので、worker が途中で落ちれば同じ交換が
  積み直される。**複数回呼ばれうる**ので、加算や追記はそのままでは二重になる
- **取り直し**。`OnReply` は**届かないことがある** (相手が落ちている / 再送の上限)。
  「いつか必ず届く」ことは保証しない。期限を持って要求し直すこと

送信そのものは裏で走るので、相手の遅さはこちらに伝播しない。ただし `Send` が
**即座に返るとは限らない** — 相手の nodeinfo がキャッシュに無いと、その取得
(最大 10 秒) だけは呼び出し元で待つ。戻り値の id が `OnReply` に渡るので、要求と
応答を対応づけられる。

`OnReply` が呼ばれるのは**その POST の HTTP 応答**が返ったとき。別のリクエストで
返ってくるわけではないので、相手が `Send` を呼び返す必要は無い。status やエラーの
形など wire の詳細は[peer プロトコル](../plugin-peer-protocol.md)。

### 取り寄せた結果をキャッシュする

リモート利用者のプロフィールにプラグインの情報を出すなら、**非同期取り寄せ + TTL +
空振りの記憶 + 初回は空**という型が要る。`Send` は応答が返るまで待てず、失敗すると最大 105 秒かけて再送するので、描画に
そのままは使えない。

`plugin/peercache` がこの型を持っている。**キャッシュはプラグイン自身の schema に
置く**ので、プラグインを消せばデータも消える。

```go
cache, err := peercache.New(peercache.Options{
    Context: ctx,
    DB:      ctx.Storage().DB(),
    Request: func(key string) any { return map[string]string{"username": key} },
    // 省略すると DefaultTTL / DefaultNegativeTTL。
})
if err != nil {
    return nil, err
}

// 初回は nil。取り寄せは裏で走り、次の表示から出る。
profile, err := cache.Lookup(req.Context(), "other.example", "alice")
if err != nil {
    return nil, err
}
_ = profile
```

| | |
|---|---|
| テーブル | `Migrations(n)` が返す DDL を自分の migration に混ぜる。**消費する version は常に 1 つ**で、`peer_cache` / `peer_cache_pending` / `peer_cache_ask` が予約 |
| 読む | `Lookup(ctx, host, key)`。**初回は nil**、取り寄せは裏で走る。期限切れでも古いものは返る |
| 書く | `OnReply` から `Store(ctx, id, payload, found)`。`found` が false なら否定 TTL で覚える |
| 掃除 | cron から `Sweep(ctx)` |

**空振りを覚えるのが要点。** 覚えないと、そのプラグインを使っていない利用者の
プロフィールを開くたびに相手へ問い合わせることになる。

応答が届く前に大勢が同じプロフィールを開いても、問い合わせは **1 分に 1 回**まで。
判定は 1 文の `INSERT ... ON CONFLICT ... WHERE ... RETURNING` で取るので、同時に
走っても勝つのは 1 つだけ。**取り寄せに失敗しても印は残る**ので、相手が落ちて
いる場合も繰り返し接続しない。

`plugintest` からは `h.Peer(def)` で登録し、`h.PeerSends()` で出た問い合わせを、
`h.DeliverPeerReply(host, id, ...)` で応答を試せる。

### リモート利用者を引くときは `AsUser` で

**踏みやすい。** 相手のホストを知るために `users/show` を呼ぶことになるが、
`Anonymous()` で呼ぶと `ugcVisibilityForVisitor` が `local` (既定) のインスタンス
では**リモート利用者が `NO_SUCH_USER` になる** (#2106 のゲート)。ホストが引けない
ので `Send` にも辿り着けず、「相互にプラグインを入れたのに永久に何も出ない」という
形で現れる。

```go
// 閲覧者として引く。匿名だとリモート利用者が見えない。
caller := ctx.API().Anonymous()
if req.UserID() != "" {
    caller = ctx.API().AsUser(req.UserID())
}
raw, err := caller.Call(req.Context(), "users/show", map[string]any{"userId": id})
if err != nil {
    return nil, err
}
return raw, nil
```

未ログインの閲覧者では引けないままだが、それは「未ログインにリモートの情報を
見せない」という運営者の設定どおりの挙動なので、**無理に権限を上げないこと**。

受信側 (`Handle` の中) で**自分のところの**利用者を引くのは匿名でよい。ゲートが
効くのはリモート利用者を引くときだけ。

### 相手が返した URL をそのまま使わない

画像などの URL を payload に載せる場合、受け取った側は**そのまま `<img>` に
渡さない**。本体の CSP は `img-src` を自オリジン中心に絞っている (#2892) ので表示できないうえ、閲覧者の
接続先が相手のサーバーになる。名前だけを取り出して自分のプロキシ経由に組み直し、
**想定した命名規則から外れていれば捨てる** (相手が渡した文字列をそのまま URL に
しない)。

`Peered` を立てると nodeinfo の `metadata.mkGoPlugins` にプラグイン名が出る。宣言して
いないプラグインは名前も出ない (入れている拡張を全部晒さないため)。

`_` で始まるパスは mk-go の予約なので、プラグインからは登録できない (受け口を奪えない
ようにするため)。

### テストする

`plugintest` から両方向を叩ける。**実際には送らない**ので、テストが外に出ない。

```go
h := plugintest.New(t).WithPeers("other.example")
h.Peer(def)          // Handle / OnReply の登録 (本番はロールに関係なく呼ばれる)
routes := h.Routes(def)

// 送信側: Send した内容を検査する
_, err := routes.Call(t, "POST /ask", plugintest.Request{UserID: "u1", Body: `{}`})
require.NoError(t, err)
sends := h.PeerSends()
require.Len(t, sends, 1)
assert.Equal(t, "other.example", sends[0].Host)

// 受信側: 相手から届いたことにする
_, err = h.DeliverPeer("other.example", map[string]any{"user": "alice"})
require.NoError(t, err)

// 応答が返ってきたことにする
err = h.DeliverPeerReply("other.example", sends[0].ID, map[string]any{"score": 1})
```

`WithPeers` に無いホストへの `Send` は本番と同じくエラーになる。宣言を忘れたテストが、
本番では通らない経路を試していることに気付ける。

## テスト

`plugin/plugintest` でルートとジョブを直接叩ける。

```go
h := plugintest.New(t).
	WithDB(db).
	WithConfig(map[string]any{"maxLength": 50}).
	WithAPI(&stubAPI{}).
	Routes(Plugin)

res, err := h.Call(t, "POST /me/set", plugintest.Request{UserID: "u1", Body: `{"text":"x"}`})
require.NoError(t, err)
require.NotNil(t, res)
```

```go
jobs := plugintest.New(t).WithDB(db).Jobs(Plugin)
require.NoError(t, jobs.Run(t, "prune", ""))
```

**DB はフェイクにしない。** SQL の挙動を模した偽物は本物とずれ、通ったのに本番で落ちるテストになる。`plugintest` も migration の適用は mk-go 本体と同じ実装を使っている。

## 公開面の一覧

以下が「壊さないと約束する範囲」のすべて。`plugin` パッケージの分は**手で書いているが、`internal/entitycompat/testdata/golden_plugin_surface.txt` の `plugin:` 行と突き合わせる gate が CI で回る** (`TestPluginDoc_*`)。見るのは識別子の有無・宣言の有無・interface の method の署名・トップレベル func / const の行・struct のフィールドの型。**`type X func(...)` の署名だけは対象外** — golden が `type Handler func` としか出さず、照合する相手が無いため。

`plugin/plugintest` の分はこの一覧に入れていない (テストの書き方は[テスト](#テスト)の節)。

**gate が照合するのは `plugin` パッケージの分だけ。** `plugin/peercache` と
`plugin/plugintest` は golden (`TestPluginSurfaceDrift`) の対象ではあるが、doc との
突き合わせは行われない — この 2 つは **golden の diff をレビューで見ること**。

### Go (`github.com/elythia-network/elythia/plugin/peercache`)

```
const DefaultTTL
const DefaultNegativeTTL

type Options struct
  Context plugin.Context
  DB *sql.DB
  Request func(key string) any
  TTL time.Duration
  NegativeTTL time.Duration

type Cache struct

func New(Options) (*Cache, error)
func Migrations(int) []plugin.Migration
func (*Cache) Lookup(context.Context, string, string) (json.RawMessage, error)
func (*Cache) Store(context.Context, string, any, bool) error
func (*Cache) Sweep(context.Context) error
```

### Go (`github.com/elythia-network/elythia/plugin/imagedecode`)

取得した画像を本体と同じ上限でデコードする。**`plugin` 本体とは別パッケージ**
なので、使うプラグインだけが画像ライブラリの依存を持つ (`plugin/peercache` が
`pgx` を持つのと同じ切り方)。

```
func MaxImagePixels() int64
func DecodeImage([]byte) (image.Image, error)
func DecodeImageWithPixelCap([]byte, int64) (image.Image, error)
```

### Go (`github.com/elythia-network/elythia/plugin`)

```
const APIVersion

type Definition struct
  Name       string
  Version    string
  APIVersion int
  Peered     bool
  PeerMaxBody int64
  Peer       func(Context, Peer) error
  Migrations []Migration
  Routes     func(Context, Router) error
  Jobs       func(Context, Jobs) error
  EffectivePolicies func(Context, EffectivePolicyInvalidator) (EffectivePolicyRegistration, error)

func (Definition) Validate() error

type ActiveRoleAssignment struct
  RoleID string
  AssignmentID string

type EffectivePolicyRequest struct
  UserID string
  RoleIDs []string
  ActiveAssignments []ActiveRoleAssignment

type EffectivePolicyContribution struct
  Key string
  Priority int
  UseDefault bool
  Value any
  Order int
  ReplaceRoleID string

type EffectivePolicyResolver func(context.Context, EffectivePolicyRequest) ([]EffectivePolicyContribution, error)

type EffectivePolicyRegistration struct
  Keys []string
  Resolve EffectivePolicyResolver

func (EffectivePolicyRegistration) Validate() error

type EffectivePolicyInvalidator interface
  InvalidateUser(context.Context, string) error
  InvalidateRole(context.Context, string) error

type Context interface
  Name() string
  Logger() *slog.Logger
  API() API
  Storage() Storage
  Config() Config
  Peer() Peer
  Queue() Queue
  HTTP() *http.Client
  Go(func())

type Peer interface
  Send(context.Context, string, any) (string, error)
  Handle(PeerHandler)
  OnReply(PeerReplyHandler)
  Has(context.Context, string) (bool, error)

type PeerHandler func(context.Context, string, json.RawMessage) (any, error)
type PeerReplyHandler func(context.Context, string, string, json.RawMessage) error

type Queue interface
  Enqueue(context.Context, string, any, ...EnqueueOption) error

type EnqueueOptions struct
  Delay time.Duration
  MaxAttempts int
  DedupTTL time.Duration

type EnqueueOption func(*EnqueueOptions)

func WithDelay(time.Duration) EnqueueOption
func WithMaxAttempts(int) EnqueueOption
func WithDedup(time.Duration) EnqueueOption

type Router interface
  GET(string, Handler)
  POST(string, Handler)

type Request interface
  Context() context.Context
  Bind(any) error
  Param(string) string
  Query(string) string
  UserID() string
  IsModerator() bool
  IsAdministrator() bool

type Storage interface
  DB() *sql.DB
  Migrate(context.Context, []Migration) error

type API interface
  Anonymous() Caller
  AsUser(string) Caller

type Caller interface
  Call(context.Context, string, any) (json.RawMessage, error)

type Jobs interface
  Handle(string, JobHandler)
  Schedule(string, string, any)

type Config interface
  Unmarshal(any) error

type Handler func(Request) (any, error)
type JobHandler func(context.Context, json.RawMessage) error
type Migration struct { Version int; SQL string }
type Blob struct { ContentType string; Body []byte; CacheControl string }
type StatusError struct { Status int; Message string }
type APIError struct { Endpoint string; Status int; Body json.RawMessage }

func (*StatusError) Error() string
func (*APIError) Error() string

func Errorf(int, string, ...any) *StatusError
func ExtractStatusError(error) (*StatusError, string)
func NewCodedStatusError(int, string, string) error
func ErrNotFound(string, ...any) *StatusError
func Register(Definition)
func Registered() []Definition
```

### TypeScript (`@/plugin-api.js`)

```
definePlugin(def)
host.name / host.me
definePlugin({ name, pages?, setup })
host.slot(name, renderer)
host.api<T>(endpoint, params)

PluginPage: { path, component, navTitle?, navIcon?, admin? }

型: SlotName / SlotUser / SlotContext / SlotMount / SlotComponent / SlotRenderer
    PluginPage / PageRegistration / PluginUser
関数: getUsers(ids)
再公開: MkInput / MkButton / MkFolder / MkLoading
        MkSelect / MkSwitch / MkAvatar / MkUserName / MkTime / PageWithHeader
        useMkSelect / definePage
```

## やってはいけないこと

| | 理由 |
|---|---|
| ActivityPub に関わるものを触る | 公開していない。不具合の症状が他人のサーバー側に出て、自分では気づけない |
| mk-go 本体のテーブルを読む | 可視性判定を迂回する。非公開ノートが混ざる |
| `plugin-api.ts` に無いコンポーネントを import する | upstream のリファクタで黙って壊れる |
| 取得元の `Content-Type` をそのまま `Blob` に流す | 本体が allowlist で矯正するので XSS にはならないが、画像のつもりが `application/octet-stream` になってダウンロードになる |
| 素の `go` で goroutine を起動する | panic でプロセスごと落ちる。`ctx.Go()` を使う |
| 管理用の API を `IsModerator()` で守らない | 画面を隠しても API は誰でも叩ける |

## 公開と互換性

`plugin/` は semver に従う。破壊的変更では `APIVersion` が上がり、合わないプラグインは**ビルド時にエラーになる**（黙って動かない状態にはならない）。

詳細は[互換性ポリシー](compatibility.md)を参照。
